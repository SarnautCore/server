package combat

import (
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

const worldStateTestMob = "mob.paper-harbor.tide-crab"

func newWorldStateTestModule(t *testing.T) (*world.Zone, *Module, *pack.Pack) {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "WorldStateFixture", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second / 15, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	rules, err := RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	return zone, New(slog.New(slog.DiscardHandler), zone, rules, Options{Seed: 7}), content
}

func TestTurnMobUpdatesTheStationaryAnchorYaw(t *testing.T) {
	t.Parallel()
	zone, module, content := newWorldStateTestModule(t)
	if err := module.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		var mob *gametypes.EntityData
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.ContentID == worldStateTestMob {
				mob = entity
				return false
			}
			return true
		})
		if mob == nil {
			t.Fatal("fixture has no target mob")
		}
		position := tick.Position(mob)
		if err := module.TurnMob(tick, mob.ID, position.Add(gametypes.Vec3{Y: 10})); err != nil {
			t.Fatalf("TurnMob() error = %v", err)
		}
		if got := module.mobs[mob.ID].anchorYaw; math.Abs(float64(got-math.Pi/2)) > 0.000001 {
			t.Fatalf("anchor yaw = %.7f, want pi/2", got)
		}

		mob.Heading = 0
		module.mobs[mob.ID].phase = phaseReturning
		module.walkHome(tick, mob, module.mobs[mob.ID])
		if math.Abs(float64(mob.Heading-math.Pi/2)) > 0.000001 {
			t.Fatalf("heading after returning home = %.7f, want authored pi/2 anchor", mob.Heading)
		}
		return nil
	}); err != nil {
		t.Fatalf("zone.GameCommand() error = %v", err)
	}
}

func TestTurnMobRefusesAMovingMobWithoutChangingItsAnchor(t *testing.T) {
	t.Parallel()
	zone, module, content := newWorldStateTestModule(t)
	if err := module.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		var mob *gametypes.EntityData
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.ContentID == worldStateTestMob {
				mob = entity
				return false
			}
			return true
		})
		if mob == nil {
			t.Fatal("fixture has no target mob")
		}
		before := module.mobs[mob.ID].anchorYaw
		mob.Velocity = gametypes.Vec3{X: 1}
		err := module.TurnMob(tick, mob.ID, tick.Position(mob).Add(gametypes.Vec3{Y: 10}))
		if err == nil || !strings.Contains(err.Error(), "moving or in combat") {
			t.Fatalf("TurnMob() error = %v, want moving refusal", err)
		}
		if after := module.mobs[mob.ID].anchorYaw; after != before {
			t.Fatalf("refused turn changed anchor yaw from %v to %v", before, after)
		}
		return nil
	}); err != nil {
		t.Fatalf("zone.GameCommand() error = %v", err)
	}
}

func TestKilledSummonDespawnsWithoutRespawning(t *testing.T) {
	t.Parallel()
	zone, module, content := newWorldStateTestModule(t)
	mob, ok := content.Mob(worldStateTestMob)
	if !ok {
		t.Fatalf("fixture pack has no %s", worldStateTestMob)
	}
	mob.ID = "mob.fixture.one-shot-summon"
	var summonedID uint64
	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		entity, err := module.Summon(
			tick, mob, "script-summon|one-shot", gametypes.Vec3{X: 4, Y: 5, Z: 6}, 0,
		)
		if err != nil {
			return err
		}
		summonedID = entity.ID
		module.kill(tick, entity, 0)
		return nil
	}); err != nil {
		t.Fatalf("summon and kill: %v", err)
	}

	for range ticksIn(corpseTimer, zone.TickInterval()) + 1 {
		zone.Step()
	}
	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		if tick.Entity(summonedID) != nil {
			t.Fatalf("killed summon %d remains after corpse despawn", summonedID)
		}
		if _, ok := module.mobs[summonedID]; ok {
			t.Fatalf("killed summon %d retained combat state", summonedID)
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect despawned summon: %v", err)
	}

	for range 3000 {
		zone.Step()
	}
	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		if tick.Entity(summonedID) != nil {
			t.Fatalf("killed summon %d respawned", summonedID)
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect no-respawn window: %v", err)
	}
}
