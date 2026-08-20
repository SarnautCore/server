package combat_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// TestWorkedExampleKillsTheMobInSixCasts is mechanics/combat.md section 6.1,
// asserted number for number.
//
// The spec calls this out as the acceptance test for the M2 combat loop:
// damage is exactly 20, the sixth cast and only the sixth emits the death
// event, and the mob dies on tick 150 relative to the first cast.
func TestWorkedExampleKillsTheMobInSixCasts(t *testing.T) {
	t.Parallel()

	// The scenario input: the player stands 6.0 m away when the first cast
	// goes out. Everything else comes from the pack.
	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})

	mob := fixture.state(fixture.mobID)
	if mob.level != 2 {
		t.Fatalf("mob level = %d, want 2", mob.level)
	}
	if mob.maxHealth != 120 {
		t.Fatalf("mob max_hp = %d, want 120: (HP_BASE + HP_PER_LEVEL) * hp_mod 1.0", mob.maxHealth)
	}

	firstCastTick := fixture.tick()
	var total int32
	var deaths int
	expectedHealth := []int32{100, 80, 60, 40, 20, 0}
	for cast := 0; cast < 6; cast++ {
		if cast > 0 {
			// Rule 5.4.2: thirty ticks, which is exactly one second.
			fixture.step(30)
		}
		event, err := fixture.cast(baseAbility, fixture.mobID)
		if err != nil {
			t.Fatalf("cast %d rejected: %v", cast+1, err)
		}
		if event.Damage != 20 {
			t.Fatalf("cast %d damage = %d, want 20", cast+1, event.Damage)
		}
		if event.TargetHealth != expectedHealth[cast] {
			t.Errorf("health after cast %d = %d, want %d",
				cast+1, event.TargetHealth, expectedHealth[cast])
		}
		if event.KillingBlow {
			deaths++
			if cast != 5 {
				t.Errorf("cast %d was the killing blow, want the sixth", cast+1)
			}
		}
		total += event.Damage
	}

	if total != 120 {
		t.Errorf("total damage = %d, want 120, the mob's whole health bar", total)
	}
	if deaths != 1 {
		t.Errorf("killing blows = %d, want exactly 1", deaths)
	}
	if elapsed := fixture.tick() - firstCastTick; elapsed != 150 {
		t.Errorf("the mob died %d ticks after the first cast, want 150", elapsed)
	}

	death := fixture.events.waitFor(t, combat.EventKindDeath)
	if death.TargetID != fixture.mobID || death.CasterID != fixture.playerID {
		t.Errorf("death event = %+v, want victim %d killed by %d",
			death, fixture.mobID, fixture.playerID)
	}
	if death.VictimContentID != targetMob {
		t.Errorf("death event victim content id = %q, want %q", death.VictimContentID, targetMob)
	}
	// Rule 5.9.4: 150 + ceil(30 s / 33.333333 ms) = 150 + 900.
	if death.CorpseDespawnTick != death.ServerTick+900 {
		t.Errorf("corpse despawn tick = %d, want %d", death.CorpseDespawnTick, death.ServerTick+900)
	}
	if extra := fixture.events.count(combat.EventKindDeath); extra != 0 {
		t.Errorf("%d further death events arrived, want exactly one in total", extra)
	}
}

// TestDeadTargetTakesNoFurtherDamage is rule 5.2.4 after the fact: the corpse
// is still in the world, still replicated, and no longer a combatant.
func TestDeadTargetTakesNoFurtherDamage(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	killTarget(t, fixture)
	// Synchronise on the one death this kill is entitled to before counting
	// whether any others follow.
	fixture.events.waitFor(t, combat.EventKindDeath)

	before := fixture.state(fixture.mobID)
	if before.alive {
		t.Fatal("the mob is still alive after the killing blow")
	}
	if !before.replicated {
		t.Error("the corpse stopped replicating immediately; rule 5.9.4 gives it a timer")
	}

	fixture.step(30)
	event, err := fixture.cast(baseAbility, fixture.mobID)
	if !errors.Is(err, combat.ErrTargetDead) {
		t.Fatalf("casting at a corpse returned %v, want ErrTargetDead", err)
	}
	if event.Damage != 0 {
		t.Errorf("a rejected cast dealt %d damage", event.Damage)
	}
	if after := fixture.state(fixture.mobID); after.health != before.health {
		t.Errorf("corpse health moved from %d to %d", before.health, after.health)
	}
	if fixture.events.count(combat.EventKindDeath) != 0 {
		t.Error("a second death event was emitted for one corpse")
	}
}

// killTarget lands the exact number of casts the pack's numbers require,
// without the test knowing what that number is.
func killTarget(t *testing.T, fixture *harness) int {
	t.Helper()
	for cast := 1; cast <= 100; cast++ {
		event, err := fixture.cast(baseAbility, fixture.mobID)
		if err != nil {
			t.Fatalf("cast %d rejected: %v", cast, err)
		}
		if event.KillingBlow {
			return cast
		}
		fixture.step(30)
	}
	t.Fatal("the mob would not die in 100 casts")
	return 0
}
