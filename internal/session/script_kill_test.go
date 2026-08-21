package session_test

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"
	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/world"
)

// The count-special slice, spelled with quest 1-30's real shape: startImpacts
// resolve a spawn table, attach RatKiller to every live rat and tag each; a
// rat's death crosses FullHealthCalcer(multiplier=0) and the ReturningImpact
// inside RatKiller lands the increment on the killer.
const (
	scriptQuestID    = "quest.inst-league1.quest-1-30"
	scriptCountID    = "questcount.inst-league1.quest-1-30.count-id-1"
	scriptTriggerID  = "trigger.il-questspells.rat-killer"
	scriptTableID    = "spawntable.inst-league1.rat1-1"
	scriptRatWorldID = "mob.inst-league1.rat"
	scriptDressID    = "trigger.inst-league1.quest-1-20.dress-trigger"
	scriptEquipQuest = "quest.inst-league1.quest-1-20"
	scriptEquipCount = "questcount.inst-league1.quest-1-20.count-id-1"
)

// scriptNode builds a fixture node. The trees below mirror the authored
// documents and are also compiled into .sptbl rows by the pack-backed test.
func scriptNode(family script.Family, key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: family, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func scriptRef(id string) script.Value {
	return script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: id}}
}

func scriptNodes(children ...*script.Node) script.Value {
	values := make([]script.Value, 0, len(children))
	for _, child := range children {
		values = append(values, script.Value{Kind: script.ValueNode, Node: child})
	}
	return script.Value{Kind: script.ValueList, List: values}
}

// ratKillerDocument mirrors IL_QuestSpells/RatKiller.(TriggerResource).xdb.
func ratKillerDocument() *script.Node {
	return scriptNode(script.FamilyTrigger, "rat-killer", "TriggerResource",
		script.Field{Name: "effects", Value: scriptNodes(
			scriptNode(script.FamilyEffect, "rat-killer/effects[0]", "HealthTrigger",
				script.Field{Name: "healthOn", Value: script.Value{
					Kind: script.ValueNode,
					Node: scriptNode(script.FamilyCalcer, "rat-killer/effects[0]/healthOn", "FullHealthCalcer",
						script.Field{Name: "multiplier", Value: script.Value{Kind: script.ValueInteger}},
					),
				}},
				script.Field{Name: "impactsOn", Value: scriptNodes(
					scriptNode(script.FamilyImpact, "rat-killer/effects[0]/impactsOn[0]", "ReturningImpact",
						script.Field{Name: "impact", Value: scriptNodes(
							scriptNode(script.FamilyImpact, "rat-killer/effects[0]/impactsOn[0]/impact",
								"ImpactIncreaseQuestCount",
								script.Field{Name: "id", Value: scriptRef(scriptCountID)},
							),
						)},
					),
				)},
			),
		)},
	)
}

// dressTriggerDocument mirrors Quest_1_20/DressTrigger.(TriggerResource).xdb,
// reduced to its MAINHAND branch.
func dressTriggerDocument() *script.Node {
	return scriptNode(script.FamilyTrigger, "dress-trigger", "TriggerResource",
		script.Field{Name: "effects", Value: scriptNodes(
			scriptNode(script.FamilyEffect, "dress-trigger/effects[0]", "EquipTrigger",
				script.Field{Name: "slot", Value: script.Value{Kind: script.ValueText, Text: "MAINHAND"}},
				script.Field{Name: "effects", Value: scriptNodes(
					scriptNode(script.FamilyEffect, "dress-trigger/effects[0]/switch", "Switch",
						script.Field{Name: "impactsOn", Value: scriptNodes(
							scriptNode(script.FamilyImpact, "dress-trigger/effects[0]/switch/impactsOn[0]",
								"ImpactIncreaseQuestCount",
								script.Field{Name: "id", Value: scriptRef(scriptEquipCount)},
							),
						)},
					),
				)},
			),
		)},
	)
}

// fixtureScripts is the QuestScriptSource the pack will one day be.
type fixtureScripts struct{}

func (fixtureScripts) QuestActivation(questID string) (session.QuestActivation, bool) {
	switch questID {
	case scriptQuestID:
		return session.QuestActivation{
			StartImpacts: []*script.Node{
				scriptNode(script.FamilyImpact, "quest-1-30/startImpacts[0]", "ImpactFindSpawnTable",
					script.Field{Name: "spawnResource", Value: scriptRef(scriptTableID)},
					script.Field{Name: "impacts", Value: scriptNodes(
						scriptNode(script.FamilyImpact, "quest-1-30/startImpacts[0]/impacts[0]",
							"ImpactAttachTrigger",
							script.Field{Name: "trigger", Value: scriptRef(scriptTriggerID)},
						),
						scriptNode(script.FamilyImpact, "quest-1-30/startImpacts[0]/impacts[1]", "TagMobForKill"),
					)},
				),
			},
		}, true
	case scriptEquipQuest:
		return session.QuestActivation{
			TriggerAgents: []*script.Node{
				scriptNode(script.FamilyTrigger, "quest-1-20/triggerAgents[0]", "TriggerAgentSelf",
					script.Field{Name: "trigger", Value: scriptRef(scriptDressID)},
				),
			},
		}, true
	default:
		return session.QuestActivation{}, false
	}
}

func (fixtureScripts) Trigger(ref script.Ref) (*script.Node, bool) {
	switch ref.ID {
	case scriptTriggerID:
		return ratKillerDocument(), true
	case scriptDressID:
		return dressTriggerDocument(), true
	default:
		return nil, false
	}
}

func (fixtureScripts) Counter(ref script.Ref) (session.CounterBinding, bool) {
	switch ref.ID {
	case scriptCountID:
		return session.CounterBinding{QuestID: scriptQuestID, ObjectiveIndex: 0}, true
	case scriptEquipCount:
		return session.CounterBinding{QuestID: scriptEquipQuest, ObjectiveIndex: 0}, true
	default:
		return session.CounterBinding{}, false
	}
}

func (fixtureScripts) SpawnTableMobs(ref script.Ref) []string {
	if ref.ID == scriptTableID {
		return []string{scriptRatWorldID}
	}
	return nil
}

// countSpecialDefinitions is the definition slice both halves of the flag test
// share: three rats for quest 1-30, one equip for quest 1-20.
func countSpecialDefinitions() []pack.Quest {
	return []pack.Quest{
		{
			ID: scriptQuestID,
			Objectives: []pack.QuestObjective{
				{Kind: pack.QuestObjectiveCountSpecial, Limit: 3, ShowCount: true},
			},
		},
		{
			ID: scriptEquipQuest,
			Objectives: []pack.QuestObjective{
				{Kind: pack.QuestObjectiveCountSpecial, Limit: 1},
			},
		},
	}
}

// scriptZoneFixture is one zone with a quest module, a script driver and three
// live rats.
type scriptZoneFixture struct {
	zone     *world.Zone
	quests   *quests.Module
	driver   *session.ScriptDriver
	sink     combat.KillSink
	playerID uint64
	ratIDs   []uint64
}

func newScriptZoneFixture(t *testing.T) *scriptZoneFixture {
	t.Helper()
	return newScriptZoneFixtureWith(t, countSpecialDefinitions(), fixtureScripts{})
}

func newScriptZoneFixtureWith(
	t *testing.T,
	definitions []pack.Quest,
	source session.QuestScriptSource,
) *scriptZoneFixture {
	t.Helper()

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "ScriptIntegration",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}

	catalog, err := quests.NewCatalogWithOptions(
		definitions, nil, quests.CatalogOptions{AllowCountSpecial: true},
	)
	if err != nil {
		t.Fatalf("NewCatalogWithOptions() error = %v", err)
	}
	questModule := quests.New(slog.New(slog.DiscardHandler), zone, catalog, nil)
	driver := session.NewScriptDriver(
		slog.New(slog.DiscardHandler), zone, questModule, source, script.Options{Enabled: true},
	)
	binding := session.ZoneBinding{World: zone, Quests: questModule, Scripts: driver}

	var ratIDs []uint64
	for index := 0; index < 3; index++ {
		ratIDs = append(ratIDs, zone.SpawnNPC(world.NPCSpec{
			ContentID: scriptRatWorldID,
			Level:     1,
			MaxHealth: 40,
			Position:  world.Vec3{X: float32(index)},
		}))
	}
	playerID, _ := zone.Join()

	return &scriptZoneFixture{
		zone:     zone,
		quests:   questModule,
		driver:   driver,
		sink:     binding.KillSink(),
		playerID: playerID,
		ratIDs:   ratIDs,
	}
}

// admit loads the character holding both count-special quests in `accepted`.
func (fixture *scriptZoneFixture) admit(t *testing.T, characterID uuid.UUID) {
	t.Helper()
	err := fixture.quests.Admit(fixture.playerID, characterID, quests.Character{
		Level: 1,
		Quests: []quests.QuestState{
			{QuestID: scriptQuestID, State: "accepted"},
			{QuestID: scriptEquipQuest, State: "accepted"},
		},
	})
	if err != nil {
		t.Fatalf("quests.Admit() error = %v", err)
	}
}

// kill delivers one death through the zone's kill fan-out, from inside a tick,
// exactly as combat does.
func (fixture *scriptZoneFixture) kill(t *testing.T, victimID uint64) {
	t.Helper()
	err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.sink.MobKilled(tick, combat.Kill{
			VictimEntityID:  victimID,
			KillerEntityID:  fixture.playerID,
			VictimContentID: scriptRatWorldID,
			DeathTick:       tick.Number(),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("GameCommand(kill) error = %v", err)
	}
}

// questRow reads one persisted quest row back, with its counters decoded.
func (fixture *scriptZoneFixture) questRow(t *testing.T, characterID uuid.UUID, questID string) (string, []int32) {
	t.Helper()
	for _, row := range fixture.quests.Rows(characterID) {
		if row.QuestID != questID {
			continue
		}
		var record struct {
			Counters []int32 `json:"counters"`
		}
		if err := json.Unmarshal(row.Objectives, &record); err != nil {
			t.Fatalf("decode counters: %v", err)
		}
		return row.State, record.Counters
	}
	t.Fatalf("character holds no row for %s", questID)
	return "", nil
}

// TestACountSpecialQuestProgressesFromAKillChainFlagOn is the end-to-end claim
// for the flag-on composition, at the highest seam that exists without pack
// script rows: quest activation evaluates the impact tree, the attach commands
// land in the zone's trigger registry, the kill fan-out fires them, and the
// increment travels through quests.CreditSpecial into the same instance state
// a kill counter uses. Three rats die, the counter walks 1-2-3, and the
// instance is completable; a fourth death does not overrun the limit.
func TestACountSpecialQuestProgressesFromAKillChainFlagOn(t *testing.T) {
	t.Parallel()

	fixture := newScriptZoneFixture(t)
	characterID := uuid.New()
	fixture.admit(t, characterID)

	// The session reader calls this after the accept commits; the admitted row
	// stands in for the commit here.
	fixture.driver.QuestActivated(fixture.playerID, scriptQuestID)

	for index, ratID := range fixture.ratIDs {
		fixture.kill(t, ratID)
		_, counters := fixture.questRow(t, characterID, scriptQuestID)
		if len(counters) != 1 || counters[0] != int32(index+1) {
			t.Fatalf("counters after kill %d = %v, want [%d]", index+1, counters, index+1)
		}
	}

	state, counters := fixture.questRow(t, characterID, scriptQuestID)
	if state != "completable" || counters[0] != 3 {
		t.Fatalf("state = %q counters = %v, want completable [3]", state, counters)
	}

	// A dead rat's attachment is gone with it: re-announcing the same death
	// moves nothing, and neither does a fourth kill of an untracked mob.
	fixture.kill(t, fixture.ratIDs[0])
	if state, counters = fixture.questRow(t, characterID, scriptQuestID); counters[0] != 3 || state != "completable" {
		t.Fatalf("state = %q counters = %v after a replayed death, want completable [3]", state, counters)
	}
}

// TestACompiledPackDrivesCountSpecialOneTwoThree proves the production source
// path. The test writes protobuf rows into real .sptbl containers, loads them
// through pack.Load, then uses PackQuestScriptSource without fixture lookups.
func TestACompiledPackDrivesCountSpecialOneTwoThree(t *testing.T) {
	t.Parallel()

	content := loadCompiledScriptPack(t)
	if err := content.ValidateQuestScriptCoverage(); err != nil {
		t.Fatalf("ValidateQuestScriptCoverage() error = %v", err)
	}
	definitions := make([]pack.Quest, 0, len(content.QuestIDs()))
	for _, id := range content.QuestIDs() {
		definition, _ := content.Quest(id)
		definitions = append(definitions, definition)
	}
	source := session.NewPackQuestScriptSource(content)
	binding, ok := source.Counter(script.Ref{ID: scriptCountID})
	if !ok || binding.ObjectiveID != scriptQuestID+".objective."+strings.Repeat("a", 64) || binding.ObjectiveIndex != 0 {
		t.Fatalf("compiled counter binding = %#v, %v", binding, ok)
	}
	fixture := newScriptZoneFixtureWith(t, definitions, source)
	characterID := uuid.New()
	fixture.admit(t, characterID)
	fixture.driver.QuestActivated(fixture.playerID, scriptQuestID)

	for index, ratID := range fixture.ratIDs {
		fixture.kill(t, ratID)
		_, counters := fixture.questRow(t, characterID, scriptQuestID)
		if len(counters) != 1 || counters[0] != int32(index+1) {
			t.Fatalf("compiled-pack counters after kill %d = %v, want [%d]", index+1, counters, index+1)
		}
	}
	if state, counters := fixture.questRow(t, characterID, scriptQuestID); state != "completable" || counters[0] != 3 {
		t.Fatalf("compiled-pack state = %q counters = %v, want completable [3]", state, counters)
	}
	census := fixture.driver.Census()
	if census.Inert["FutureAmbientImpact"] != 1 {
		t.Errorf("inert census = %v, want FutureAmbientImpact:1", census.Inert)
	}
	if census.Refused["FutureAuthoritativeImpact"] != 1 {
		t.Errorf("refused census = %v, want FutureAuthoritativeImpact:1", census.Refused)
	}
}

// TestAnEquipTriggerCountsThroughTheAdapterFlagOn walks shape A across the
// same seam: TriggerAgentSelf binds DressTrigger to the player at activation,
// and the equip event — delivered through the driver's seam, since no
// equipment module publishes it yet — completes the objective.
func TestAnEquipTriggerCountsThroughTheAdapterFlagOn(t *testing.T) {
	t.Parallel()

	fixture := newScriptZoneFixture(t)
	characterID := uuid.New()
	fixture.admit(t, characterID)
	fixture.driver.QuestActivated(fixture.playerID, scriptEquipQuest)

	// An unrelated slot moves nothing.
	fixture.driver.EquipChanged(fixture.playerID, "FEET", true)
	if state, counters := fixture.questRow(t, characterID, scriptEquipQuest); counters[0] != 0 || state != "accepted" {
		t.Fatalf("state = %q counters = %v after an unrelated equip, want accepted [0]", state, counters)
	}

	fixture.driver.EquipChanged(fixture.playerID, "MAINHAND", true)
	state, counters := fixture.questRow(t, characterID, scriptEquipQuest)
	if state != "completable" || counters[0] != 1 {
		t.Fatalf("state = %q counters = %v, want completable [1]", state, counters)
	}
}

// TestCountSpecialStaysRefusedFlagOff pins the other half of the flag: without
// AllowCountSpecial the catalog refuses the definition outright — and under
// the skip opt-in it is skipped, which existing pack tests cover — so the
// flag-off composition can never offer a quest whose counters nothing drives.
func TestCountSpecialStaysRefusedFlagOff(t *testing.T) {
	t.Parallel()

	_, err := quests.NewCatalog(countSpecialDefinitions(), nil)
	var unsupported *quests.UnsupportedObjectiveError
	if !errors.As(err, &unsupported) {
		t.Fatalf("NewCatalog() error = %v, want an UnsupportedObjectiveError", err)
	}
	if unsupported.Kind != pack.QuestObjectiveCountSpecial {
		t.Errorf("refused kind = %v, want count-special", unsupported.Kind)
	}
}

type compiledManifest struct {
	SchemaVersion int                     `json:"schema_version"`
	Ruleset       string                  `json:"ruleset"`
	Zone          string                  `json:"zone"`
	PackID        string                  `json:"pack_id"`
	Builder       json.RawMessage         `json:"builder"`
	Source        json.RawMessage         `json:"source"`
	KeepExtra     bool                    `json:"keep_extra"`
	Tables        []compiledManifestTable `json:"tables"`
}

type compiledManifestTable struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	RowType string `json:"row_type"`
	Rows    uint32 `json:"rows"`
	Bytes   uint64 `json:"bytes"`
	Blake3  string `json:"blake3"`
}

type compiledPackRow struct {
	id      string
	message proto.Message
}

func loadCompiledScriptPack(t *testing.T) *pack.Pack {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "pack")
	source := filepath.Join("..", "..", "testdata", "packs", "demo")
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(directory, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, payload, 0o600)
	}); err != nil {
		t.Fatalf("copy fixture pack: %v", err)
	}

	definitions := countSpecialDefinitions()
	questRows := make([]compiledPackRow, 0, len(definitions))
	for _, definition := range definitions {
		wire := &contentv1.Quest{Id: definition.ID, ZoneId: "zone.inst-league1", Level: 1, Rewards: &contentv1.QuestRewards{}}
		for _, objective := range definition.Objectives {
			wire.Objectives = append(wire.Objectives, &contentv1.QuestObjective{
				Kind:  contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_COUNT_SPECIAL,
				Limit: objective.Limit, Internal: objective.Internal, ShowCount: objective.ShowCount,
			})
		}
		questRows = append(questRows, compiledPackRow{id: wire.GetId(), message: wire})
	}
	replaceSessionPackTable(t, directory, "quests", contentv1.RowType_ROW_TYPE_QUEST, questRows)

	spawn := &contentv1.SpawnTable{Id: scriptTableID, Entries: []*contentv1.SpawnTableEntry{{
		ObjectId: scriptRatWorldID, Group: "commons", Chance: 1,
	}}}
	replaceSessionPackTable(t, directory, "spawn-tables", contentv1.RowType_ROW_TYPE_SPAWN_TABLE,
		[]compiledPackRow{{id: spawn.GetId(), message: spawn}})

	scriptRows := make([]compiledPackRow, 0, 2)
	for _, questID := range []string{scriptQuestID, scriptEquipQuest} {
		activation, _ := (fixtureScripts{}).QuestActivation(questID)
		row := &contentv1.QuestScript{Id: "script." + questID, QuestId: questID}
		if questID == scriptQuestID {
			row.Counters = []*contentv1.QuestCounterBinding{{
				CountId: scriptCountID, ObjectiveIndex: 0,
				ObjectiveId: scriptQuestID + ".objective." + strings.Repeat("a", 64),
			}}
			activation.StartImpacts = append(activation.StartImpacts,
				&script.Node{Key: "future/inert", Family: script.FamilyImpact, Opcode: "FutureAmbientImpact", Tier: script.TierInert},
				&script.Node{Key: "future/refused", Family: script.FamilyImpact, Opcode: "FutureAuthoritativeImpact", Tier: script.TierRefused},
			)
		} else {
			row.Counters = []*contentv1.QuestCounterBinding{{
				CountId: scriptEquipCount, ObjectiveIndex: 0,
				ObjectiveId: scriptEquipQuest + ".objective." + strings.Repeat("b", 64),
			}}
		}
		for _, node := range activation.StartImpacts {
			row.StartImpacts = append(row.StartImpacts, scriptNodeToProto(node))
		}
		for _, node := range activation.TriggerAgents {
			row.TriggerAgents = append(row.TriggerAgents, scriptNodeToProto(node))
		}
		scriptRows = append(scriptRows, compiledPackRow{id: row.GetId(), message: row})
	}
	replaceSessionPackTable(t, directory, "quest-scripts", contentv1.RowType_ROW_TYPE_QUEST_SCRIPT, scriptRows)

	triggerRows := []compiledPackRow{
		{id: scriptTriggerID, message: &contentv1.ScriptTrigger{Id: scriptTriggerID, Root: scriptNodeToProto(ratKillerDocument())}},
		{id: scriptDressID, message: &contentv1.ScriptTrigger{Id: scriptDressID, Root: scriptNodeToProto(dressTriggerDocument())}},
	}
	replaceSessionPackTable(t, directory, "script-triggers", contentv1.RowType_ROW_TYPE_SCRIPT_TRIGGER, triggerRows)
	resealSessionPack(t, directory)

	content, err := pack.Load(directory, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load(compiled scripts) error = %v", err)
	}
	return content
}

func scriptNodeToProto(node *script.Node) *contentv1.ScriptNode {
	wire := &contentv1.ScriptNode{
		NodeKey: node.Key, Family: string(node.Family), Opcode: node.Opcode,
		Tier: contentv1.CoverageTier(int32(node.Tier) + 1),
	}
	for _, field := range node.Fields {
		wire.Fields = append(wire.Fields, &contentv1.ScriptField{Name: field.Name, Value: scriptValueToProto(field.Value)})
	}
	sort.Slice(wire.Fields, func(left, right int) bool { return wire.Fields[left].GetName() < wire.Fields[right].GetName() })
	return wire
}

func scriptValueToProto(value script.Value) *contentv1.ScriptValue {
	switch value.Kind {
	case script.ValueInteger:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Integer{Integer: value.Integer}}
	case script.ValueDecimal:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Decimal{Decimal: &contentv1.Decimal{Mantissa: value.Mantissa, Scale: value.Scale}}}
	case script.ValueBool:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Boolean{Boolean: value.Bool}}
	case script.ValueText:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Text{Text: value.Text}}
	case script.ValueRef:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Reference{Reference: &contentv1.ContentRef{Id: value.Ref.ID, RowType: value.Ref.RowType}}}
	case script.ValueDurationMS:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_DurationMs{DurationMs: value.DurationMS}}
	case script.ValueNode:
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Node{Node: scriptNodeToProto(value.Node)}}
	case script.ValueList:
		list := &contentv1.ScriptValueList{}
		for _, entry := range value.List {
			list.Values = append(list.Values, scriptValueToProto(entry))
		}
		return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_List{List: list}}
	default:
		panic("test script contains an unspecified value")
	}
}

func replaceSessionPackTable(
	t *testing.T,
	directory, name string,
	rowType contentv1.RowType,
	rows []compiledPackRow,
) {
	t.Helper()
	payload := encodeSessionPackTable(t, rowType, rows)
	file := filepath.ToSlash(filepath.Join("tables", name+".sptbl"))
	if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(file)), payload, 0o600); err != nil {
		t.Fatalf("write table %s: %v", name, err)
	}
	document := readCompiledManifest(t, directory)
	entry := compiledManifestTable{Name: name, File: file, RowType: rowType.String(), Rows: uint32(len(rows))}
	replaced := false
	for index := range document.Tables {
		if document.Tables[index].Name == name {
			document.Tables[index] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		document.Tables = append(document.Tables, entry)
	}
	writeCompiledManifest(t, directory, document)
}

func encodeSessionPackTable(t *testing.T, rowType contentv1.RowType, rows []compiledPackRow) []byte {
	t.Helper()
	const headerBytes, keyBytes = 40, 12
	encoded := make([][]byte, len(rows))
	dataBytes := 0
	for index, row := range rows {
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(row.message)
		if err != nil {
			t.Fatalf("marshal row %s: %v", row.id, err)
		}
		encoded[index] = payload
		dataBytes += len(payload)
	}
	keyOffset := uint32(headerBytes)
	rowOffset := keyOffset + uint32(keyBytes*len(rows))
	dataOffset := rowOffset + uint32(4*(len(rows)+1))
	payload := make([]byte, int(dataOffset)+dataBytes)
	copy(payload, "SPK1")
	binary.LittleEndian.PutUint16(payload[4:], 1)
	binary.LittleEndian.PutUint16(payload[6:], 1)
	binary.LittleEndian.PutUint32(payload[8:], uint32(rowType))
	binary.LittleEndian.PutUint32(payload[12:], uint32(len(rows)))
	binary.LittleEndian.PutUint32(payload[16:], keyOffset)
	binary.LittleEndian.PutUint32(payload[20:], rowOffset)
	binary.LittleEndian.PutUint32(payload[24:], dataOffset)
	binary.LittleEndian.PutUint32(payload[28:], uint32(dataBytes))

	type key struct {
		hash    uint64
		ordinal uint32
	}
	keys := make([]key, len(rows))
	for index, row := range rows {
		sum := blake3.Sum256([]byte(row.id))
		keys[index] = key{hash: binary.LittleEndian.Uint64(sum[:8]), ordinal: uint32(index)}
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].hash < keys[right].hash || keys[left].hash == keys[right].hash && keys[left].ordinal < keys[right].ordinal
	})
	for index, entry := range keys {
		base := int(keyOffset) + index*keyBytes
		binary.LittleEndian.PutUint64(payload[base:], entry.hash)
		binary.LittleEndian.PutUint32(payload[base+8:], entry.ordinal)
	}
	offset := 0
	for index, row := range encoded {
		binary.LittleEndian.PutUint32(payload[int(rowOffset)+index*4:], uint32(offset))
		copy(payload[int(dataOffset)+offset:], row)
		offset += len(row)
	}
	binary.LittleEndian.PutUint32(payload[int(rowOffset)+len(rows)*4:], uint32(offset))
	return payload
}

func resealSessionPack(t *testing.T, directory string) {
	t.Helper()
	document := readCompiledManifest(t, directory)
	type named struct {
		name    string
		payload []byte
	}
	namedTables := make([]named, 0, len(document.Tables))
	for index, table := range document.Tables {
		payload, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(table.File)))
		if err != nil {
			t.Fatalf("read table %s: %v", table.Name, err)
		}
		sum := blake3.Sum256(payload)
		document.Tables[index].Bytes = uint64(len(payload))
		document.Tables[index].Blake3 = hex.EncodeToString(sum[:])
		namedTables = append(namedTables, named{name: table.Name, payload: payload})
	}
	sort.Slice(namedTables, func(left, right int) bool { return namedTables[left].name < namedTables[right].name })
	hasher := blake3.New()
	scratch := make([]byte, 8)
	for _, table := range namedTables {
		binary.LittleEndian.PutUint32(scratch[:4], uint32(len(table.name)))
		_, _ = hasher.Write(scratch[:4])
		_, _ = hasher.Write([]byte(table.name))
		binary.LittleEndian.PutUint64(scratch, uint64(len(table.payload)))
		_, _ = hasher.Write(scratch)
		_, _ = hasher.Write(table.payload)
	}
	document.PackID = hex.EncodeToString(hasher.Sum(nil))
	writeCompiledManifest(t, directory, document)
}

func readCompiledManifest(t *testing.T, directory string) compiledManifest {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var document compiledManifest
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return document
}

func writeCompiledManifest(t *testing.T, directory string, document compiledManifest) {
	t.Helper()
	path := filepath.Join(directory, "manifest.json")
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
