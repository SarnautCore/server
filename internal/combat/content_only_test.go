package combat_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// TestASecondAbilityAndASecondMobAreContentOnly is the exit criterion of this
// task.
//
// `demo-extended` is `demo` with one overlay directory layered on: an ability,
// a mob kind, a mob and a placement, in YAML. No line of Go distinguishes the
// two packs, and this test drives the second one through the whole loop — a
// different damage number, a different level, a different health pool, a
// different range and a cooldown the base ability does not have — using the
// same module, the same entry point and the same rules.
//
// If a gameplay rule ever becomes a Go constant, this is the test that fails,
// because the constant will be the base pack's value.
func TestASecondAbilityAndASecondMobAreContentOnly(t *testing.T) {
	t.Parallel()

	base := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	extended := newHarness(t, extendedPack, extendedMob, world.Vec3{X: 6}, combat.Options{})

	// The overlay's mob is nowhere in the base pack, and the base pack's mob
	// is still in the extended one: an overlay adds, it does not replace.
	if _, ok := base.content.Mob(extendedMob); ok {
		t.Fatalf("%q is in the base pack; the two fixtures are not distinct", extendedMob)
	}
	if _, ok := extended.content.Mob(targetMob); !ok {
		t.Fatalf("the overlay dropped %q", targetMob)
	}

	baseMob := base.state(base.mobID)
	extendedState := extended.state(extended.mobID)
	if extendedState.level == baseMob.level || extendedState.maxHealth == baseMob.maxHealth {
		t.Fatalf("the two mobs are indistinguishable: %+v and %+v", baseMob, extendedState)
	}

	// Derived from the pack: level 3, hp_mod 1.5, so 210 max health; and a
	// 30-damage 0.25-coefficient ability against level 3 armour, so 25.
	if extendedState.level != 3 || extendedState.maxHealth != 210 {
		t.Errorf("overlay mob = level %d with %d health, want level 3 with 210",
			extendedState.level, extendedState.maxHealth)
	}

	event, err := extended.cast(extendedAbility, extended.mobID)
	if err != nil {
		t.Fatalf("the overlay ability was rejected: %v", err)
	}
	if event.Damage != 25 {
		t.Errorf("overlay ability damage = %d, want 25", event.Damage)
	}
	if event.AbilityID != extendedAbility {
		t.Errorf("the event names ability %q, want %q", event.AbilityID, extendedAbility)
	}

	// Its range is the pack's, not the base ability's: a distance that is out
	// of range for one is comfortably inside the other.
	far := newHarness(t, extendedPack, extendedMob, world.Vec3{X: 20}, combat.Options{})
	if _, err := far.cast(baseAbility, far.mobID); !errors.Is(err, combat.ErrOutOfRange) {
		t.Errorf("the 10 m ability at 20 m returned %v, want ErrOutOfRange", err)
	}
	if _, err := far.cast(extendedAbility, far.mobID); err != nil {
		t.Errorf("the 25 m ability at 20 m returned %v, want a hit", err)
	}

	// And the whole kill loop runs on it, at its own cooldown.
	casts := 1
	for casts < 100 {
		next, elapsed := extended.castUntilReady(extendedAbility, extended.mobID, 200)
		casts++
		if elapsed != 90 {
			t.Errorf("cast %d came %d ticks after the last, want the pack's 90", casts, elapsed)
		}
		if next.KillingBlow {
			break
		}
	}
	if casts != 9 {
		t.Errorf("the overlay mob died in %d casts, want ceil(210 / 25) = 9", casts)
	}
	death := extended.events.waitFor(t, combat.EventKindDeath)
	if death.VictimContentID != extendedMob || death.VictimLevel != 3 {
		t.Errorf("death event = %+v, want the overlay mob at level 3", death)
	}
}

// TestPerMobAggroAndLeashRadiiComeFromThePack is the AI half of the same
// claim: two mobs in one pack with different radii behave differently, with no
// branch in Go that knows either of them.
func TestPerMobAggroAndLeashRadiiComeFromThePack(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, extendedPack, extendedMob, world.Vec3{X: 6}, combat.Options{})
	first, ok := fixture.content.Mob(targetMob)
	if !ok {
		t.Fatalf("the pack does not describe %q", targetMob)
	}
	second, ok := fixture.content.Mob(extendedMob)
	if !ok {
		t.Fatalf("the pack does not describe %q", extendedMob)
	}
	if first.AggroRadiusM == second.AggroRadiusM || first.LeashRadiusM == second.LeashRadiusM {
		t.Fatalf("the two mobs share a radius: %+v and %+v", first, second)
	}

	// The overlay mob's leash is the shorter of the two, so dragging it breaks
	// sooner than the base mob's would.
	anchor := fixture.state(fixture.mobID).position
	if _, err := fixture.cast(extendedAbility, fixture.mobID); err != nil {
		t.Fatalf("cast rejected: %v", err)
	}
	var seq uint64
	var farthest float32
	for step := 0; step < 4000; step++ {
		fixture.walk(&seq, world.Vec3{X: 1}, second.WalkSpeed, 1)
		distance := world.Distance(fixture.state(fixture.mobID).position, anchor)
		if distance > farthest {
			farthest = distance
		}
		if distance > second.LeashRadiusM {
			break
		}
	}
	if farthest <= second.LeashRadiusM {
		t.Fatalf("the overlay mob reached %.1f m, never crossing its %.1f m leash",
			farthest, second.LeashRadiusM)
	}
	if farthest > first.LeashRadiusM {
		t.Errorf("the overlay mob ran to %.1f m, past the other mob's %.1f m leash; "+
			"one leash radius is being applied to both", farthest, first.LeashRadiusM)
	}
}
