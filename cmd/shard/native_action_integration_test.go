package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/zeebo/blake3"
	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/world"
)

const (
	nativeCombatActionID      = "action.fixture.native-auto-attack"
	nativeUnavailableActionID = "action.fixture.requires-ranged"
)

func TestNativeActionAdmissionUsesAuthoredPhysicalScalerChain(t *testing.T) {
	stats := &pack.StartingCombatStats{
		Resource: pack.StartingResource{
			Kind: "energy", Initial: pack.Decimal{Mantissa: 100}, Maximum: pack.Decimal{Mantissa: 100},
		},
		Innate:           []pack.ExactStatValue{{Stat: "strength", Value: pack.Decimal{Mantissa: 21}}},
		BaseStatValue:    pack.Decimal{Mantissa: 20},
		WeaponDPSDefault: pack.Decimal{Mantissa: 4_472_136, Scale: 6},
		FairyScaler:      pack.Decimal{Mantissa: 33_333_334, Scale: 8},
		Mainhand: pack.WeaponProfile{
			MinimumDamage: pack.Decimal{Mantissa: 39_637_733, Scale: 7},
			MaximumDamage: pack.Decimal{Mantissa: 66_062_884, Scale: 7},
			SpeedMS:       pack.Decimal{Mantissa: 2000},
		},
		Ranged: pack.WeaponProfile{
			MinimumDamage: pack.Decimal{Mantissa: 1}, MaximumDamage: pack.Decimal{Mantissa: 1},
			SpeedMS: pack.Decimal{Mantissa: 2000},
		},
	}
	resource, combatant, err := nativeActionAdmission(pack.ChargenOption{
		ID: "chargen.league.warrior", CombatStats: stats,
	})
	if err != nil {
		t.Fatalf("nativeActionAdmission() error = %v", err)
	}
	if resource != (combat.ActionResource{
		Kind: "energy", CurrentMilli: 100_000, MaximumMilli: 100_000,
	}) {
		t.Fatalf("resource = %+v", resource)
	}
	if combatant.PhysicalScale != (script.Decimal{Mantissa: 165_447_637, Scale: 9}) ||
		combatant.PhysicalRangedScale != (script.Decimal{Mantissa: 31_304_952, Scale: 9}) {
		t.Fatalf("physical scales = melee %+v ranged %+v",
			combatant.PhysicalScale, combatant.PhysicalRangedScale)
	}
	if got := combatant.WeaponSpeedScale["Mainhand"]; got != (script.Decimal{Mantissa: 2}) {
		t.Fatalf("mainhand weapon speed scale = %+v", got)
	}
}

func TestCompiledChargenNativeActionExecutesAndRefusesWithoutMutation(t *testing.T) {
	directory := nativeCombatPack(t)
	content, err := pack.Load(directory, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	loadouts, err := combatLoadouts(content)
	if err != nil {
		t.Fatalf("combatLoadouts() error = %v", err)
	}
	loadout, ok := loadouts.StartingCombat("chargen.fixture.native-combat")
	if !ok {
		t.Fatal("compiled chargen combat loadout is missing")
	}

	spawn := nativeTargetSpawn(t, content)
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "NativeActionFixture", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second / 15, MaxMoveSpeed: 6,
		PlayerSpawn: spawn.Position.Add(gametypes.Vec3{X: 1}),
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("combat.RulesFromPack() error = %v", err)
	}
	module := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})
	if err := module.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("combat.Populate() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go module.Run(ctx)

	playerID, _ := zone.Join()
	if err := module.Admit(playerID, combat.PlayerAdmission{
		Level: 1, Health: loadout.MaxHealth, MaxHealth: loadout.MaxHealth,
		AbilityIDs: loadout.AbilityIDs, ActionBindings: loadout.ActionBindings,
		ActionResource: loadout.ActionResource,
	}); err != nil {
		t.Fatalf("combat.Admit() error = %v", err)
	}
	t.Cleanup(func() {
		module.Release(playerID)
		zone.Leave(playerID)
	})
	targetID := nativeTargetEntity(t, zone, spawn.MobID)

	beforeHealth := entityHealth(t, zone, targetID)
	beforeResource, err := module.ActionResourceState(playerID)
	if err != nil {
		t.Fatalf("ActionResourceState() error = %v", err)
	}
	if _, err := module.UseAbility(playerID, combat.AbilityRequest{
		Seq: 1, TargetID: targetID, AbilityID: nativeCombatActionID,
	}); !errors.Is(err, combat.ErrUnknownAbility) {
		t.Fatalf("unwired native action error = %v, want ErrUnknownAbility", err)
	}
	assertNativeActionState(t, module, zone, playerID, targetID, beforeHealth, beforeResource)

	driver := session.NewScriptDriver(
		nil, zone, nil, session.NewPackQuestScriptSource(content), script.Options{Enabled: true},
	)
	driver.BindCombat(module)
	if err := driver.SetWarriorCombatant(playerID, loadout.Combatant); err != nil {
		t.Fatalf("SetWarriorCombatant() error = %v", err)
	}
	profile, ok := driver.Profile(playerID, nativeCombatActionID)
	if !ok || profile.Cooldown != 2*time.Second {
		t.Fatalf("compiled native cooldown = %s, %v, want 2s from base 1 x speed 2",
			profile.Cooldown, ok)
	}
	t.Cleanup(func() { driver.ReleaseWarriorCombatant(playerID) })
	if _, err := module.SelectTarget(playerID, targetID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	event, err := module.ActivateSlot(playerID, 0, 1)
	if err != nil {
		t.Fatalf("ActivateSlot() error = %v", err)
	}
	if event.AbilityID != nativeCombatActionID || event.Damage != 1 || event.TargetID != targetID {
		t.Fatalf("native action event = %#v, want authored action and 1 damage", event)
	}
	afterResource, err := module.ActionResourceState(playerID)
	if err != nil || afterResource.CurrentMilli != beforeResource.CurrentMilli-25_000 {
		t.Fatalf("resource after native action = %#v, %v", afterResource, err)
	}
	afterHealth := entityHealth(t, zone, targetID)

	if _, err := module.UseAbility(playerID, combat.AbilityRequest{
		Seq: 2, TargetID: targetID, AbilityID: nativeUnavailableActionID,
	}); !errors.Is(err, combat.ErrInvalidTarget) {
		t.Fatalf("unavailable action error = %v, want ErrInvalidTarget", err)
	}
	assertNativeActionState(t, module, zone, playerID, targetID, afterHealth, afterResource)
	if _, err := module.UseAbility(playerID, combat.AbilityRequest{
		Seq: 2, TargetID: targetID, AbilityID: "action.fixture.not-granted",
	}); !errors.Is(err, combat.ErrUnknownAbility) {
		t.Fatalf("unknown action error = %v, want ErrUnknownAbility", err)
	}
	assertNativeActionState(t, module, zone, playerID, targetID, afterHealth, afterResource)
	if _, err := module.ActivateSlot(playerID, 0, 2); !errors.Is(err, combat.ErrOnCooldown) {
		t.Fatalf("cooldown action error = %v, want ErrOnCooldown", err)
	}
	assertNativeActionState(t, module, zone, playerID, targetID, afterHealth, afterResource)
}

func assertNativeActionState(
	t *testing.T,
	module *combat.Module,
	zone *world.Zone,
	playerID, targetID uint64,
	wantHealth int32,
	wantResource combat.ActionResource,
) {
	t.Helper()
	if health := entityHealth(t, zone, targetID); health != wantHealth {
		t.Fatalf("refused action changed target health to %d, want %d", health, wantHealth)
	}
	resource, err := module.ActionResourceState(playerID)
	if err != nil || resource != wantResource {
		t.Fatalf("refused action changed resource to %#v, want %#v, error %v",
			resource, wantResource, err)
	}
}

func nativeTargetSpawn(t *testing.T, content *pack.Pack) gametypes.NPCSpawn {
	t.Helper()
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID == "mob.paper-harbor.tide-crab" {
			return spawn
		}
	}
	t.Fatal("fixture pack has no tide-crab spawn")
	return gametypes.NPCSpawn{}
}

func nativeTargetEntity(t *testing.T, zone *world.Zone, contentID string) uint64 {
	t.Helper()
	var entityID uint64
	_ = zone.GameCommand(func(tick gametypes.Tick) error {
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.ContentID == contentID {
				entityID = entity.ID
				return false
			}
			return true
		})
		return nil
	})
	if entityID == 0 {
		t.Fatalf("zone has no entity for %q", contentID)
	}
	return entityID
}

func entityHealth(t *testing.T, zone *world.Zone, entityID uint64) int32 {
	t.Helper()
	var health int32
	_ = zone.GameCommand(func(tick gametypes.Tick) error {
		entity := tick.Entity(entityID)
		if entity == nil {
			return gametypes.ErrUnknownEntity
		}
		health = entity.Health
		return nil
	})
	return health
}

func nativeCombatPack(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.CopyFS(directory, os.DirFS(filepath.Join("..", "..", "testdata", "packs", "demo"))); err != nil {
		t.Fatalf("copy fixture pack: %v", err)
	}
	slot := uint32(0)
	chargen := &contentv1.ChargenOption{
		Id: "chargen.fixture.native-combat", StartingLevel: 1,
		Stats: nativeStartingStats(),
		StartingActions: []*contentv1.StartingAction{{
			SlotIndex: &slot, ActionId: nativeCombatActionID,
		}, {ActionId: nativeUnavailableActionID}},
	}
	action := nativeCombatAction()
	unavailable := nativeUnavailableAction()
	replaceNativeTable(t, directory, "chargen", contentv1.RowType_ROW_TYPE_CHARGEN_OPTION,
		[]nativeCompiledRow{{id: chargen.GetId(), message: chargen}})
	replaceNativeTable(t, directory, "native-actions", contentv1.RowType_ROW_TYPE_NATIVE_ACTION,
		[]nativeCompiledRow{
			{id: action.GetId(), message: action},
			{id: unavailable.GetId(), message: unavailable},
		})
	resealNativePack(t, directory)
	return directory
}

func nativeUnavailableAction() *contentv1.NativeAction {
	action := proto.Clone(nativeCombatAction()).(*contentv1.NativeAction)
	action.Id = nativeUnavailableActionID
	action.ActionGroupId = ""
	action.Cooldown = nil
	action.Resource = nil
	action.TriggersGcd = false
	action.TargetImpacts[0].NodeKey = nativeUnavailableActionID + "/targetImpacts[0]"
	action.TargetImpacts[0].Fields[2].GetValue().GetNode().NodeKey =
		nativeUnavailableActionID + "/targetImpacts[0]/scaler"
	action.CasterConditions = []*contentv1.ScriptNode{{
		NodeKey: nativeUnavailableActionID + "/casterConditions[0]", Family: "predicate",
		Opcode: "PredicateEquipped", Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
		Fields: []*contentv1.ScriptField{
			{Name: "dressType", Value: nativeTextValue("ranged")},
			{Name: "toLog", Value: nativeBoolValue(false)},
			{Name: "weaponRequired", Value: nativeBoolValue(true)},
		},
	}}
	return action
}

func nativeStartingStats() *contentv1.StartingCharacterStats {
	return &contentv1.StartingCharacterStats{
		Health: 146, MaxHealth: 146,
		Resource: &contentv1.StartingResource{
			Kind: "energy", Initial: nativeDecimal(100, 0), Maximum: nativeDecimal(100, 0),
		},
		Innate: []*contentv1.ExactStatEntry{{Stat: "strength", Value: nativeDecimal(21, 0)}},
		Armor:  nativeDecimal(0, 0), HitDice: nativeDecimal(1, 0), ManaDice: nativeDecimal(1, 0),
		BaseStatValue: nativeDecimal(20, 0), WeaponDpsDefault: nativeDecimal(4_472_136, 6),
		FairyScaler: nativeDecimal(33_333_334, 8),
		Mainhand: &contentv1.WeaponProfile{
			MinimumDamage: nativeDecimal(39_637_733, 7), MaximumDamage: nativeDecimal(66_062_884, 7),
			SpeedMs: nativeDecimal(2000, 0),
		},
		Ranged: &contentv1.WeaponProfile{
			MinimumDamage: nativeDecimal(1, 0), MaximumDamage: nativeDecimal(1, 0),
			SpeedMs: nativeDecimal(2000, 0),
		},
	}
}

func nativeCombatAction() *contentv1.NativeAction {
	return &contentv1.NativeAction{
		Id: nativeCombatActionID, TargetPolicy: "current-target", RangeM: nativeDecimal(5, 0),
		RequiresLos: true, IsAggro: true, TriggersGcd: true, IgnoresGcd: true,
		ActionGroupId: "action-group.fixture.native-auto-attack",
		Cooldown: &contentv1.ActionCooldown{
			DurationMs: 2000, GroupId: "action-group.fixture.native-auto-attack",
			Scaler: "weapon-speed", Base: nativeDecimal(1, 0),
		},
		Resource: &contentv1.ActionResource{
			Kind: "energy", Cost: nativeDecimal(125, 1), ScaleByWeaponSpeed: true, Source: "mainhand",
		},
		TargetImpacts: []*contentv1.ScriptNode{{
			NodeKey: nativeCombatActionID + "/targetImpacts[0]", Family: "impact",
			Opcode: "ScaledPhysicalWeaponDamage", Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
			Fields: []*contentv1.ScriptField{
				{Name: "avgDamage", Value: nativeDecimalValue(875, 2)},
				{Name: "canBeAvoided", Value: nativeBoolValue(true)},
				{Name: "scaler", Value: nativeNodeValue(&contentv1.ScriptNode{
					NodeKey: nativeCombatActionID + "/targetImpacts[0]/scaler", Family: "scaler",
					Opcode: "PhysicalScaler", Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
				})},
				{Name: "source", Value: nativeTextValue("Mainhand")},
				{Name: "threatMultiplier", Value: nativeIntegerValue(1)},
			},
		}},
	}
}

type nativeCompiledRow struct {
	id      string
	message proto.Message
}

func replaceNativeTable(
	t *testing.T,
	directory, name string,
	rowType contentv1.RowType,
	rows []nativeCompiledRow,
) {
	t.Helper()
	payload := encodeNativeTable(t, rowType, rows)
	path := filepath.Join(directory, "tables", name+".sptbl")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func encodeNativeTable(t *testing.T, rowType contentv1.RowType, rows []nativeCompiledRow) []byte {
	t.Helper()
	const headerBytes = 40
	const keyBytes = 12
	encoded := make([][]byte, len(rows))
	type key struct {
		hash    uint64
		ordinal uint32
	}
	keys := make([]key, len(rows))
	dataBytes := 0
	for index, row := range rows {
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(row.message)
		if err != nil {
			t.Fatalf("marshal row %q: %v", row.id, err)
		}
		encoded[index] = payload
		dataBytes += len(payload)
		sum := blake3.Sum256([]byte(row.id))
		keys[index] = key{hash: binary.LittleEndian.Uint64(sum[:8]), ordinal: uint32(index)}
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].hash < keys[j].hash || keys[i].hash == keys[j].hash && keys[i].ordinal < keys[j].ordinal
	})
	keyOffset := uint32(headerBytes)
	rowOffset := keyOffset + uint32(len(rows)*keyBytes)
	dataOffset := rowOffset + uint32((len(rows)+1)*4)
	payload := make([]byte, int(dataOffset)+dataBytes)
	copy(payload[:4], "SPK1")
	binary.LittleEndian.PutUint16(payload[4:], 1)
	binary.LittleEndian.PutUint16(payload[6:], 1)
	binary.LittleEndian.PutUint32(payload[8:], uint32(rowType))
	binary.LittleEndian.PutUint32(payload[12:], uint32(len(rows)))
	binary.LittleEndian.PutUint32(payload[16:], keyOffset)
	binary.LittleEndian.PutUint32(payload[20:], rowOffset)
	binary.LittleEndian.PutUint32(payload[24:], dataOffset)
	binary.LittleEndian.PutUint32(payload[28:], uint32(dataBytes))
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

type nativeManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	Ruleset       string                `json:"ruleset"`
	Zone          string                `json:"zone"`
	PackID        string                `json:"pack_id"`
	Builder       json.RawMessage       `json:"builder"`
	Source        json.RawMessage       `json:"source"`
	KeepExtra     bool                  `json:"keep_extra"`
	Tables        []nativeManifestTable `json:"tables"`
}

type nativeManifestTable struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	RowType string `json:"row_type"`
	Rows    uint32 `json:"rows"`
	Bytes   uint64 `json:"bytes"`
	Blake3  string `json:"blake3"`
}

func resealNativePack(t *testing.T, directory string) {
	t.Helper()
	manifestPath := filepath.Join(directory, "manifest.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest nativeManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]nativeManifestTable, len(manifest.Tables)+1)
	for _, entry := range manifest.Tables {
		entries[entry.Name] = entry
	}
	entries["chargen"] = nativeManifestTable{
		Name: "chargen", File: "tables/chargen.sptbl", RowType: contentv1.RowType_ROW_TYPE_CHARGEN_OPTION.String(), Rows: 1,
	}
	entries["native-actions"] = nativeManifestTable{
		Name: "native-actions", File: "tables/native-actions.sptbl", RowType: contentv1.RowType_ROW_TYPE_NATIVE_ACTION.String(), Rows: 2,
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	manifest.Tables = manifest.Tables[:0]
	hasher := blake3.New()
	for _, name := range names {
		entry := entries[name]
		tableBytes, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(entry.File)))
		if err != nil {
			t.Fatal(err)
		}
		sum := blake3.Sum256(tableBytes)
		entry.Bytes = uint64(len(tableBytes))
		entry.Blake3 = hex.EncodeToString(sum[:])
		manifest.Tables = append(manifest.Tables, entry)
		var scratch [8]byte
		binary.LittleEndian.PutUint32(scratch[:4], uint32(len(name)))
		_, _ = hasher.Write(scratch[:4])
		_, _ = hasher.Write([]byte(name))
		binary.LittleEndian.PutUint64(scratch[:], uint64(len(tableBytes)))
		_, _ = hasher.Write(scratch[:])
		_, _ = hasher.Write(tableBytes)
	}
	manifest.PackID = hex.EncodeToString(hasher.Sum(nil))
	sealed, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sealed = append(sealed, '\n')
	if err := os.WriteFile(manifestPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nativeDecimal(mantissa int64, scale int32) *contentv1.Decimal {
	return &contentv1.Decimal{Mantissa: mantissa, Scale: scale}
}

func nativeDecimalValue(mantissa int64, scale int32) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Decimal{Decimal: nativeDecimal(mantissa, scale)}}
}

func nativeBoolValue(value bool) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Boolean{Boolean: value}}
}

func nativeNodeValue(value *contentv1.ScriptNode) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Node{Node: value}}
}

func nativeTextValue(value string) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Text{Text: value}}
}

func nativeIntegerValue(value int64) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Integer{Integer: value}}
}
