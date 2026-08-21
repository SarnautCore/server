package worldscript_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/world"
	"github.com/SarnautCore/server/internal/worldscript"
)

func fixtureGround(t testing.TB) world.Ground {
	t.Helper()
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
	return ground
}

func fixtureContent() worldscript.Content {
	return worldscript.Content{
		Devices: []worldscript.DeviceSpec{
			{
				ID: "device.fixture.elixir-chest", PlacementID: "placement.fixture.chest.2",
				PresentationID: "presentation.fixture.elixir-chest", Kind: worldscript.DeviceKindElixirChest,
				Position: gametypes.Vec3{X: 3, Y: 2, Z: -50}, LootTableID: "loot.fixture.heal-elixir",
				InteractRadius: 3, OneShot: true,
			},
			{
				ID: "device.fixture.elixir-chest", PlacementID: "placement.fixture.chest.1",
				PresentationID: "presentation.fixture.elixir-chest", Kind: worldscript.DeviceKindElixirChest,
				Position: gametypes.Vec3{X: 1, Y: 2, Z: 90}, LootTableID: "loot.fixture.heal-elixir",
				InteractRadius: 3, OneShot: true,
			},
		},
		ScriptZones: []worldscript.ScriptZoneSpec{
			{ID: "script-zone.fixture.gate", Bounds: worldscript.Bounds{
				Min: gametypes.Vec3{X: 4, Y: 4, Z: -10},
				Max: gametypes.Vec3{X: 6, Y: 6, Z: 10},
			}},
		},
		Paths: []worldscript.PathSpec{
			{ID: "path.fixture.tutorial", DefaultSpeed: 2, Waypoints: []gametypes.Vec3{
				{X: 1, Y: 1, Z: 700}, {X: 3, Y: 1, Z: -700}, {X: 5, Y: 1, Z: 0},
			}},
		},
		Variables: []worldscript.VariableSpec{
			{ID: "variable.fixture.demons-total", Initial: 2},
			{ID: "variable.fixture.elementals-current", Initial: 0},
		},
	}
}

type fixture struct {
	zone   *world.Zone
	module *worldscript.Module
	host   *captureHost
	loot   *captureLoot
}

func newFixture(t testing.TB, state worldscript.State, teleport bool) *fixture {
	t.Helper()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "zone.fixture.tutorial", TickInterval: 100 * time.Millisecond,
		SnapshotInterval: time.Second, MaxMoveSpeed: 20,
		PlayerSpawn: gametypes.Vec3{X: 0.5, Y: 0.5}, Ground: fixtureGround(t),
		GroundSampleStep: 0.25,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	host := new(captureHost)
	loot := new(captureLoot)
	module, err := worldscript.New(zone, fixtureContent(), worldscript.Options{
		InitialState: state, TeleportPaths: teleport, Host: host, Loot: loot,
	})
	if err != nil {
		t.Fatalf("worldscript.New() error = %v", err)
	}
	return &fixture{zone: zone, module: module, host: host, loot: loot}
}

type captureHost struct {
	events     []worldscript.Event
	rejectNext bool
}

func (host *captureHost) HandleWorldEvent(_ gametypes.Tick, event worldscript.Event) bool {
	host.events = append(host.events, event)
	if event.Kind == worldscript.EventScriptZoneEntering && host.rejectNext {
		host.rejectNext = false
		return false
	}
	return true
}

type captureLoot struct {
	opens []worldscript.DeviceInteraction
	fail  error
}

func (loot *captureLoot) OpenDevice(tick gametypes.Tick, interaction worldscript.DeviceInteraction) (uint64, error) {
	if loot.fail != nil {
		return 0, loot.fail
	}
	loot.opens = append(loot.opens, interaction)
	container := tick.SpawnNPC(gametypes.NPCSpec{
		ContentID: "loot-container.fixture.heal-elixir",
		Position:  tick.Position(tick.Entity(interaction.DeviceEntityID)),
	})
	container.Alive = false
	return container.ID, nil
}

type discardSnapshot struct{}

func (discardSnapshot) OfferSnapshot(world.Snapshot) {}

func admitPlayer(t testing.TB, zone *world.Zone, position gametypes.Vec3) uint64 {
	t.Helper()
	entityID, _ := zone.JoinAt(position, 0)
	if err := zone.Subscribe(entityID, discardSnapshot{}); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	return entityID
}

func movePlayer(t testing.TB, zone *world.Zone, entityID uint64, to gametypes.Vec3) {
	t.Helper()
	if err := zone.Command(func(tick *world.Tick) error {
		tick.MoveTo(tick.Entity(entityID), to)
		return nil
	}); err != nil {
		t.Fatalf("move player: %v", err)
	}
}

func TestScriptZoneEntryConditionLeaveAndDisable(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, false)
	playerID := admitPlayer(t, fixture.zone, gametypes.Vec3{X: 1, Y: 1})
	fixture.host.rejectNext = true

	movePlayer(t, fixture.zone, playerID, gametypes.Vec3{X: 5, Y: 5})
	fixture.zone.Step()
	if got := countEvents(fixture.host.events, worldscript.EventScriptZoneEntered); got != 0 {
		t.Fatalf("entered events after rejected conditionsIn = %d, want 0", got)
	}
	fixture.zone.Step()
	if got := countEvents(fixture.host.events, worldscript.EventScriptZoneEntered); got != 1 {
		t.Fatalf("entered events after admitted retry = %d, want 1", got)
	}
	movePlayer(t, fixture.zone, playerID, gametypes.Vec3{X: 2, Y: 2})
	fixture.zone.Step()
	if got := countEvents(fixture.host.events, worldscript.EventScriptZoneLeft); got != 1 {
		t.Fatalf("left events = %d, want 1", got)
	}
	if err := fixture.module.SetZoneDisabled("script-zone.fixture.gate", true, "disable|1"); err != nil {
		t.Fatalf("SetZoneDisabled() error = %v", err)
	}
	movePlayer(t, fixture.zone, playerID, gametypes.Vec3{X: 5, Y: 5})
	fixture.zone.Step()
	if got := countEvents(fixture.host.events, worldscript.EventScriptZoneEntered); got != 1 {
		t.Fatalf("disabled zone entered events = %d, want unchanged 1", got)
	}
}

func countEvents(events []worldscript.Event, kind worldscript.EventKind) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func TestVariablesAndDisabledZonesSurviveRestart(t *testing.T) {
	t.Parallel()
	first := newFixture(t, worldscript.State{}, false)
	if err := first.module.AddVariable("variable.fixture.demons-total", 3, "quest|summand|1"); err != nil {
		t.Fatalf("AddVariable() error = %v", err)
	}
	if err := first.module.AddVariable("variable.fixture.demons-total", 3, "quest|summand|1"); err != nil {
		t.Fatalf("replayed AddVariable() error = %v", err)
	}
	if err := first.module.SetZoneDisabled("script-zone.fixture.gate", true, "quest|disable|1"); err != nil {
		t.Fatalf("SetZoneDisabled() error = %v", err)
	}
	state := first.module.Snapshot()

	second := newFixture(t, state, false)
	value, ok := second.module.Variable("variable.fixture.demons-total")
	if !ok || value != 5 {
		t.Fatalf("restored variable = %d, %t, want 5, true", value, ok)
	}
	if err := second.module.AddVariable("variable.fixture.demons-total", 3, "quest|summand|1"); err != nil {
		t.Fatalf("restart replay AddVariable() error = %v", err)
	}
	value, _ = second.module.Variable("variable.fixture.demons-total")
	if value != 5 {
		t.Fatalf("restart replay changed variable to %d", value)
	}
	if got, want := second.module.StateDigestInput(), first.module.StateDigestInput(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("restart digest lines = %v, want %v", got, want)
	}
}

func TestGoThroughPathTracksGroundAndPublishesCompletion(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, false)
	mobID := fixture.zone.SpawnNPC(world.NPCSpec{
		ContentID: "mob.fixture.path-runner", PlacementID: "placement.fixture.path-runner",
		Position: gametypes.Vec3{X: 1, Y: 1, Z: 800},
	})
	if err := fixture.module.StartPath(worldscript.PathRequest{
		EntityID: mobID, PathID: "path.fixture.tutorial", ExecutionKey: "quest|path|1",
	}); err != nil {
		t.Fatalf("StartPath() error = %v", err)
	}
	for index := 0; index < 6; index++ {
		fixture.zone.Step()
	}
	fixture.module.PausePath(mobID)
	var paused gametypes.Vec3
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		paused = tick.Position(tick.Entity(mobID))
		return nil
	})
	for index := 0; index < 5; index++ {
		fixture.zone.Step()
	}
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		if got := tick.Position(tick.Entity(mobID)); got != paused {
			t.Fatalf("paused route moved from %#v to %#v", paused, got)
		}
		return nil
	})
	fixture.module.ResumePath(mobID)
	for index := 0; index < 30; index++ {
		fixture.zone.Step()
	}
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		position := tick.Position(tick.Entity(mobID))
		if position.X != 5 || position.Y != 1 || position.Z != 2.5 {
			t.Fatalf("path endpoint = %#v, want ground-clamped (5,1,2.5)", position)
		}
		return nil
	})
	if got := countEvents(fixture.host.events, worldscript.EventPathCompleted); got != 1 {
		t.Fatalf("path completion events = %d, want 1", got)
	}
	cues := fixture.module.DrainCues()
	if len(cues) != 2 || cues[0].Kind != worldscript.CuePathStarted || cues[1].Kind != worldscript.CuePathCompleted {
		t.Fatalf("path cues = %#v", cues)
	}
}

func TestGoThroughPathTeleportFallbackKeepsCompletionContract(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, true)
	mobID := fixture.zone.SpawnNPC(world.NPCSpec{ContentID: "mob.fixture.teleport", Position: gametypes.Vec3{X: 1, Y: 1}})
	if err := fixture.module.StartPath(worldscript.PathRequest{
		EntityID: mobID, PathID: "path.fixture.tutorial", ExecutionKey: "quest|path|teleport",
	}); err != nil {
		t.Fatalf("StartPath() error = %v", err)
	}
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		position := tick.Position(tick.Entity(mobID))
		if position.X != 5 || position.Z != 2.5 {
			t.Fatalf("teleport endpoint = %#v", position)
		}
		return nil
	})
	if got := countEvents(fixture.host.events, worldscript.EventPathCompleted); got != 1 {
		t.Fatalf("teleport completion events = %d, want 1", got)
	}
}

func TestElixirChestResolutionInteractionAndRestart(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, false)
	playerID := admitPlayer(t, fixture.zone, gametypes.Vec3{X: 1, Y: 2})
	all := fixture.module.ResolveDevices(worldscript.DeviceQuery{
		ContentID: "device.fixture.elixir-chest", Permanent: true,
	})
	if len(all) != 2 || all[0] >= all[1] {
		t.Fatalf("permanent device resolution = %v, want two placement-ordered entities", all)
	}
	single := fixture.module.ResolveDevices(worldscript.DeviceQuery{
		ContentID: "device.fixture.elixir-chest", Origin: gametypes.Vec3{X: 1, Y: 2}, Radius: 0.5,
	})
	if len(single) != 1 || single[0] != all[0] {
		t.Fatalf("single device resolution = %v, want [%d]", single, all[0])
	}
	result, err := fixture.module.Interact(playerID, all[0])
	if err != nil || result.Refusal != worldscript.InteractAllowed || result.ContainerEntityID == 0 {
		t.Fatalf("Interact() = %#v, %v", result, err)
	}
	if len(fixture.loot.opens) != 1 || fixture.loot.opens[0].LootTableID != "loot.fixture.heal-elixir" {
		t.Fatalf("loot opens = %#v", fixture.loot.opens)
	}
	replay, err := fixture.module.Interact(playerID, all[0])
	if err != nil || replay.Refusal != worldscript.InteractAlreadyUsed {
		t.Fatalf("replayed Interact() = %#v, %v", replay, err)
	}

	restarted := newFixture(t, fixture.module.Snapshot(), false)
	remaining := restarted.module.ResolveDevices(worldscript.DeviceQuery{
		ContentID: "device.fixture.elixir-chest", Permanent: true,
	})
	if len(remaining) != 1 {
		t.Fatalf("devices after restart = %v, want one unopened chest", remaining)
	}
}

func TestConcurrentChestInteractionAwardsOnce(t *testing.T) {
	fixture := newFixture(t, worldscript.State{}, false)
	playerID := admitPlayer(t, fixture.zone, gametypes.Vec3{X: 1, Y: 2})
	deviceID := fixture.module.ResolveDevices(worldscript.DeviceQuery{ContentID: "device.fixture.elixir-chest"})[0]

	const contenders = 32
	results := make(chan worldscript.InteractResult, contenders)
	errors := make(chan error, contenders)
	var group sync.WaitGroup
	for index := 0; index < contenders; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := fixture.module.Interact(playerID, deviceID)
			results <- result
			errors <- err
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	allowed := 0
	for err := range errors {
		if err != nil {
			t.Errorf("Interact() error = %v", err)
		}
	}
	for result := range results {
		if result.Refusal == worldscript.InteractAllowed {
			allowed++
		} else if result.Refusal != worldscript.InteractAlreadyUsed {
			t.Errorf("race refusal = %d, want already used", result.Refusal)
		}
	}
	if allowed != 1 || len(fixture.loot.opens) != 1 {
		t.Fatalf("allowed interactions = %d, loot opens = %d", allowed, len(fixture.loot.opens))
	}
}

func TestTypedClientCueIsIdempotentAndPathFree(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, false)
	cue := worldscript.Cue{
		Kind: worldscript.CueClientData, ActorEntityID: 7,
		ResourceID: "client-data.fixture.tutorial-hint", ExecutionKey: "quest|cue|1",
	}
	if err := fixture.module.EmitCue(cue); err != nil {
		t.Fatalf("EmitCue() error = %v", err)
	}
	if err := fixture.module.EmitCue(cue); err != nil {
		t.Fatalf("replayed EmitCue() error = %v", err)
	}
	got := fixture.module.DrainCues()
	if len(got) != 1 || got[0] != cue {
		t.Fatalf("cues = %#v, want one exact typed cue", got)
	}
}

func TestDeviceLootFailureLeavesChestUsable(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, worldscript.State{}, false)
	fixture.loot.fail = errors.New("fixture loot failure")
	playerID := admitPlayer(t, fixture.zone, gametypes.Vec3{X: 1, Y: 2})
	deviceID := fixture.module.ResolveDevices(worldscript.DeviceQuery{ContentID: "device.fixture.elixir-chest"})[0]
	result, err := fixture.module.Interact(playerID, deviceID)
	if err == nil || result.Refusal != worldscript.InteractUnavailable {
		t.Fatalf("Interact() = %#v, %v", result, err)
	}
	fixture.loot.fail = nil
	result, err = fixture.module.Interact(playerID, deviceID)
	if err != nil || result.Refusal != worldscript.InteractAllowed {
		t.Fatalf("retry Interact() = %#v, %v", result, err)
	}
}

func BenchmarkScriptZoneAndPathStep(b *testing.B) {
	fixture := newFixture(b, worldscript.State{}, false)
	for index := 0; index < 64; index++ {
		admitPlayer(b, fixture.zone, gametypes.Vec3{X: 1, Y: 1})
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		fixture.zone.Step()
	}
}

var _ = time.Second
