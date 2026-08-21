package session_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
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
// documents; the pack does not carry script rows yet (M3-09), which is exactly
// why the source seam exists.
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
		countSpecialDefinitions(), nil, quests.CatalogOptions{AllowCountSpecial: true},
	)
	if err != nil {
		t.Fatalf("NewCatalogWithOptions() error = %v", err)
	}
	questModule := quests.New(slog.New(slog.DiscardHandler), zone, catalog, nil)
	driver := session.NewScriptDriver(
		slog.New(slog.DiscardHandler), zone, questModule, fixtureScripts{}, script.Options{Enabled: true},
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
		Quests: []store.QuestState{
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
	err := fixture.zone.Command(func(tick *world.Tick) error {
		fixture.sink.MobKilled(tick, combat.Kill{
			VictimEntityID:  victimID,
			KillerEntityID:  fixture.playerID,
			VictimContentID: scriptRatWorldID,
			DeathTick:       tick.Number(),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Command(kill) error = %v", err)
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
