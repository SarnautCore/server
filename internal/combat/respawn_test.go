package combat_test

import (
	"math"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// TestCorpseDespawnsAndTheSlotRefillsInsideThePacksWindow is rules 5.9.4 to
// 5.9.7.
//
// The window is the placement's, not this test's: it is read back from the
// pack and the observed delay is asserted to fall inside it. Authoring a
// different window changes the bound without changing the assertion.
func TestCorpseDespawnsAndTheSlotRefillsInsideThePacksWindow(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{Seed: 20260820})
	anchor := fixture.state(fixture.mobID).position
	spawn, ok := spawnOf(fixture.content, targetMob)
	if !ok {
		t.Fatalf("the pack has no placement for %q", targetMob)
	}

	killTarget(t, fixture)
	death := fixture.events.waitFor(t, combat.EventKindDeath)
	corpseTicks := death.CorpseDespawnTick - death.ServerTick

	// The corpse is there for the whole of its timer and gone on the tick the
	// death event predicted.
	fixture.step(int(corpseTicks) - 1)
	if state := fixture.state(fixture.mobID); !state.replicated {
		t.Error("the corpse vanished before its despawn tick")
	}
	fixture.step(1)
	if state := fixture.state(fixture.mobID); state.replicated {
		t.Error("the corpse is still replicated on its despawn tick")
	}

	despawnedAt := fixture.tick()
	var respawnedAt uint64
	for step := 0; step < 3000; step++ {
		fixture.step(1)
		if fixture.state(fixture.mobID).alive {
			respawnedAt = fixture.tick()
			break
		}
	}
	if respawnedAt == 0 {
		t.Fatal("the mob never came back")
	}

	delay := respawnedAt - despawnedAt
	low, high := tickBounds(spawn.RespawnMin), tickBounds(spawn.RespawnMax)
	if delay < low || delay > high+1 {
		t.Errorf("respawn took %d ticks, want the pack's window of [%d, %d] (%v to %v)",
			delay, low, high+1, spawn.RespawnMin, spawn.RespawnMax)
	}

	back := fixture.state(fixture.mobID)
	if back.position != anchor {
		t.Errorf("the mob respawned at %+v, want its anchor %+v", back.position, anchor)
	}
	if back.health != back.maxHealth || back.maxHealth == 0 {
		t.Errorf("the mob respawned at %d/%d health, want full", back.health, back.maxHealth)
	}
	if !back.replicated {
		t.Error("the respawned mob is not being replicated")
	}
	// The level is redrawn from the zone spawn stream, and this mob's content
	// pins the range, so the redraw has to land on the same number.
	if back.level != 2 {
		t.Errorf("the respawned mob is level %d, want 2 from a [2, 2] draw", back.level)
	}
}

// TestTheSpawnStreamIsSeeded is what makes every number above reproducible: two
// runs of the same seed over the same pack schedule the same respawn.
func TestTheSpawnStreamIsSeeded(t *testing.T) {
	t.Parallel()

	measure := func(seed uint64) uint64 {
		fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{Seed: seed})
		killTarget(t, fixture)
		death := fixture.events.waitFor(t, combat.EventKindDeath)
		fixture.step(int(death.CorpseDespawnTick - death.ServerTick))
		from := fixture.tick()
		for step := 0; step < 3000; step++ {
			fixture.step(1)
			if fixture.state(fixture.mobID).alive {
				return fixture.tick() - from
			}
		}
		t.Fatal("the mob never came back")
		return 0
	}

	first, second := measure(4242), measure(4242)
	if first != second {
		t.Errorf("one seed produced respawn delays of %d and %d ticks", first, second)
	}
	if other := measure(99); other == first {
		t.Log("two seeds happened to draw the same delay; that is possible, not a failure")
	}
}

// tickBounds converts a millisecond window bound into ticks. It floors rather
// than reproducing the module's own conversion, so a rounding change in the
// implementation cannot make this assertion agree with itself.
func tickBounds(duration time.Duration) uint64 {
	if duration <= 0 {
		return 0
	}
	return uint64(math.Floor(duration.Seconds() / tickInterval.Seconds()))
}
