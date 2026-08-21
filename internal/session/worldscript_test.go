package session

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/world"
	"github.com/SarnautCore/server/internal/worldscript"
)

type worldScriptSourceStub struct{}

func (worldScriptSourceStub) QuestActivation(string) (QuestActivation, bool) {
	return QuestActivation{}, false
}
func (worldScriptSourceStub) Trigger(script.Ref) (*script.Node, bool) { return nil, false }
func (worldScriptSourceStub) Counter(script.Ref) (CounterBinding, bool) {
	return CounterBinding{}, false
}
func (worldScriptSourceStub) SpawnTableMobs(script.Ref) []string { return nil }

func newWorldScriptAdapterFixture(t *testing.T) (*world.Zone, *ScriptDriver, *worldscript.Module, uint64) {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	heights := make([]float32, 121)
	for y := 0; y < 11; y++ {
		for x := 0; x < 11; x++ {
			heights[y*11+x] = float32(x) * 0.5
		}
	}
	ground, err := world.NewHeightfield(world.HeightfieldSpec{
		CellSize: 1, Width: 11, Height: 11, Heights: heights, MaxGrade: 1,
	})
	if err != nil {
		t.Fatalf("NewHeightfield() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "zone.fixture.world-script-adapter", TickInterval: 100 * time.Millisecond,
		SnapshotInterval: time.Second, MaxMoveSpeed: 10,
		PlayerSpawn: gametypes.Vec3{X: 0.5, Y: 0.5}, Ground: ground,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("combat.RulesFromPack() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{Seed: 5})
	spawns := content.NPCSpawns()
	mobContentID := ""
	for _, spawn := range spawns {
		mob, ok := rules.Mob(spawn.MobID)
		if ok && mob.WalkSpeed > 0 {
			mobContentID = spawn.MobID
			break
		}
	}
	if mobContentID == "" {
		t.Fatal("fixture pack carries no moving mob")
	}
	mobID := zone.SpawnNPC(world.NPCSpec{
		ContentID: mobContentID, PlacementID: "placement.fixture.path-mob",
		Position: gametypes.Vec3{X: 1, Y: 1},
	})
	driver := NewScriptDriver(
		slog.New(slog.DiscardHandler), zone, nil, worldScriptSourceStub{}, script.Options{Enabled: true},
	)
	driver.BindCombat(combatModule)
	module, err := worldscript.New(zone, worldscript.Content{
		Devices: []worldscript.DeviceSpec{{
			ID: "device.fixture.elixir", PlacementID: "placement.fixture.elixir",
			MapID: "map.fixture", ScriptID: "ElixirChest",
			PresentationID: "presentation.fixture.elixir", Kind: worldscript.DeviceKindElixirChest,
			Position: gametypes.Vec3{X: 2, Y: 2}, LootTableID: "loot.fixture",
			InteractRadius: 3, OneShot: true,
		}},
		ScriptZones: []worldscript.ScriptZoneSpec{{
			ID: "script-zone.fixture", Bounds: worldscript.Bounds{
				Min: gametypes.Vec3{}, Max: gametypes.Vec3{X: 4, Y: 4, Z: 4},
			},
		}},
		Variables: []worldscript.VariableSpec{{ID: "variable.fixture", Initial: 4}},
	}, worldscript.Options{Host: driver})
	if err != nil {
		t.Fatalf("worldscript.New() error = %v", err)
	}
	driver.BindWorldScripts(module)
	return zone, driver, module, mobID
}

func applyWorldCommand(t *testing.T, zone *world.Zone, driver *ScriptDriver, command script.Command) error {
	t.Helper()
	return zone.GameCommand(func(tick gametypes.Tick) error {
		driver.tick = tick
		defer func() { driver.tick = nil }()
		return scriptHost{driver: driver}.Apply(context.Background(), command)
	})
}

func TestScriptHostAppliesWorldVariablesZonesAndCuesWithoutReentry(t *testing.T) {
	t.Parallel()
	zone, driver, module, _ := newWorldScriptAdapterFixture(t)
	if err := applyWorldCommand(t, zone, driver, script.Command{
		Kind:     script.CommandAddScriptZoneVariable,
		OtherRef: script.Ref{ID: "variable.fixture", RowType: "variable"},
		Count:    3, ExecutionKey: "adapter|variable|1",
	}); err != nil {
		t.Fatalf("Apply(variable) error = %v", err)
	}
	if value, _ := module.Variable("variable.fixture"); value != 7 {
		t.Fatalf("variable = %d, want 7", value)
	}
	if err := applyWorldCommand(t, zone, driver, script.Command{
		Kind: script.CommandSetScriptZoneDisabled,
		Ref:  script.Ref{ID: "script-zone.fixture", RowType: "script-zone"},
		Bool: true, ExecutionKey: "adapter|zone|1",
	}); err != nil {
		t.Fatalf("Apply(zone disabled) error = %v", err)
	}
	if err := applyWorldCommand(t, zone, driver, script.Command{
		Kind: script.CommandClientData, EntityID: "7",
		Ref:          script.Ref{ID: "client-data.fixture.hint", RowType: "client-data"},
		ExecutionKey: "adapter|cue|1",
	}); err != nil {
		t.Fatalf("Apply(client data) error = %v", err)
	}
	cues := module.DrainCues()
	if len(cues) != 1 || cues[0].ActorEntityID != 7 || cues[0].ResourceID != "client-data.fixture.hint" {
		t.Fatalf("client cues = %#v", cues)
	}
}

func TestScriptHostResolvesDeviceLocatorsAndRunsGroundedPath(t *testing.T) {
	t.Parallel()
	zone, driver, module, mobID := newWorldScriptAdapterFixture(t)
	var found []string
	err := zone.GameCommand(func(tick gametypes.Tick) error {
		driver.tick = tick
		defer func() { driver.tick = nil }()
		var resolveErr error
		found, resolveErr = scriptHost{driver: driver}.Resolve(context.Background(), script.ResolveRequest{
			Finder: "ImpactFindPermanentDevice",
			Locator: &script.MapLocator{
				Map: script.Ref{ID: "map.fixture", RowType: "map"}, ScriptID: "ElixirChest",
			},
		})
		return resolveErr
	})
	if err != nil || len(found) != 1 {
		t.Fatalf("Resolve(device) = %v, %v", found, err)
	}
	clientImpact := &script.Node{
		Key: "trigger.fixture/impact", Family: script.FamilyImpact,
		Opcode: "ImpactClientData", Tier: script.TierImplemented,
		Fields: []script.Field{{Name: "data", Value: script.Value{
			Kind: script.ValueRef, Ref: script.Ref{ID: "client-data.fixture.path-complete", RowType: "client-data"},
		}}},
	}
	finder := &script.Node{
		Key: "trigger.fixture/source", Family: script.FamilyFinder,
		Opcode: "AddresseeFinderSelf", Tier: script.TierImplemented,
	}
	effect := &script.Node{
		Key: "trigger.fixture/effect", Family: script.FamilyEffect,
		Opcode: "EffectTrigger", Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "eventClasses", Value: script.Value{Kind: script.ValueList, List: []script.Value{{
				Kind: script.ValueText, Text: goneThroughPathEventClass,
			}}}},
			{Name: "eventsSource", Value: script.Value{Kind: script.ValueNode, Node: finder}},
			{Name: "impacts", Value: script.Value{Kind: script.ValueNode, Node: clientImpact}},
		},
	}
	trigger := &script.Node{
		Key: "trigger.fixture", Family: script.FamilyTrigger,
		Opcode: "TriggerResource", Tier: script.TierImplemented,
		Fields: []script.Field{{Name: "effects", Value: script.Value{Kind: script.ValueNode, Node: effect}}},
	}
	driver.attachments[mobID] = []script.Attachment{{
		ID: "attachment.fixture.path", TriggerRef: script.Ref{ID: "trigger.fixture", RowType: "trigger"},
		Trigger: trigger, EntityID: formatEntityID(mobID),
		Frame: script.Frame{
			EvaluationID: "attachment.fixture.path", SourceID: "trigger.fixture",
			ZoneID: zone.ID(), Addressee: formatEntityID(mobID),
		},
	}}
	if err := applyWorldCommand(t, zone, driver, script.Command{
		Kind: script.CommandGoThroughPath, EntityID: formatEntityID(mobID),
		Destinations: []script.Destination{
			{Position: script.Position{X: 2, Y: 1, Z: 90}},
			{Position: script.Position{X: 4, Y: 1, Z: -90}},
		},
		ExecutionKey: "adapter|path|1",
	}); err != nil {
		t.Fatalf("Apply(path) error = %v", err)
	}
	for index := 0; index < 200; index++ {
		zone.Step()
	}
	_ = zone.GameCommand(func(tick gametypes.Tick) error {
		position := tick.Position(tick.Entity(mobID))
		if position.X != 4 || position.Y != 1 || position.Z != 2 {
			t.Fatalf("path endpoint = %#v, want grounded (4,1,2)", position)
		}
		return nil
	})
	cues := module.DrainCues()
	if len(cues) != 3 || cues[1].ResourceID != "client-data.fixture.path-complete" ||
		cues[2].Kind != worldscript.CuePathCompleted {
		t.Fatalf("path cues = %#v", cues)
	}
}
