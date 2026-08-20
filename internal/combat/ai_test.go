package combat_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// TestMobAggroesOnlyInsideItsPackAuthoredRadius is rule 5.7.
//
// The radius is not in this test. It is read from the pack and the player is
// walked to either side of it, so authoring a different radius changes what
// the test does without changing what it asserts.
func TestMobAggroesOnlyInsideItsPackAuthoredRadius(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 30}, combat.Options{})
	mob, ok := fixture.content.Mob(targetMob)
	if !ok {
		t.Fatalf("the pack does not describe %q", targetMob)
	}
	anchor := fixture.state(fixture.mobID).position

	// Just outside the radius, and well outside the ability range the mob
	// would stop at, so a mob that pulled would visibly move.
	var seq uint64
	outside := mob.AggroRadiusM + 2
	fixture.walk(&seq, world.Vec3{X: -1}, 6, ticksToCover(30-outside, 6))
	fixture.step(20)
	if moved := fixture.state(fixture.mobID).position; moved != anchor {
		t.Fatalf("the mob left its anchor at %.1f m, outside its %.1f m aggro radius",
			distanceToPlayer(fixture), mob.AggroRadiusM)
	}

	// Now step across it.
	fixture.walk(&seq, world.Vec3{X: -1}, 6, ticksToCover(4, 6))
	fixture.step(20)
	if moved := fixture.state(fixture.mobID).position; moved == anchor {
		t.Fatalf("the mob stayed at its anchor with the player %.1f m away, inside its %.1f m radius",
			distanceToPlayer(fixture), mob.AggroRadiusM)
	}
}

// TestMobLeashesHomeWhenThePlayerLeaves is rule 5.8.1 into 5.8.5 and 5.8.7:
// the aggro target is gone, so the mob walks back to its anchor and arrives at
// full health.
func TestMobLeashesHomeWhenThePlayerLeaves(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	anchor := fixture.state(fixture.mobID).position

	// Hit it once, which pulls it whatever the distance (rule 5.6.3), then
	// walk away so it follows and leaves its anchor.
	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Fatalf("cast rejected: %v", err)
	}
	hurt := fixture.state(fixture.mobID)
	if hurt.health >= hurt.maxHealth {
		t.Fatal("the cast did no damage; the health restoration below would prove nothing")
	}

	var seq uint64
	fixture.walk(&seq, world.Vec3{X: 1}, 6, 90)
	if pulled := fixture.state(fixture.mobID).position; pulled == anchor {
		t.Fatal("the mob never chased, so leashing home is not being tested")
	}

	fixture.leave(fixture.playerID)
	// It walks home under its own steam; nothing teleports it.
	for step := 0; step < 2000; step++ {
		if fixture.state(fixture.mobID).position == anchor {
			break
		}
		fixture.step(1)
	}

	home := fixture.state(fixture.mobID)
	if home.position != anchor {
		t.Errorf("the mob stopped at %+v, want its anchor %+v", home.position, anchor)
	}
	if home.health != home.maxHealth {
		t.Errorf("the mob came home at %d/%d health, want full", home.health, home.maxHealth)
	}
}

// TestMobLeashesWhenItIsDraggedPastItsLeashRadius is rules 5.8.2 and 5.8.5:
// the player is still there, and the mob gives up anyway because it has come
// too far from home.
func TestMobLeashesWhenItIsDraggedPastItsLeashRadius(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	mob, ok := fixture.content.Mob(targetMob)
	if !ok {
		t.Fatalf("the pack does not describe %q", targetMob)
	}
	anchor := fixture.state(fixture.mobID).position

	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Fatalf("cast rejected: %v", err)
	}

	// Retreat at the mob's own speed so it keeps following without ever
	// catching up or being left behind. It has to cover its whole leash radius
	// before it gives up.
	var seq uint64
	var farthest float32
	for step := 0; step < 4000; step++ {
		fixture.walk(&seq, world.Vec3{X: 1}, mob.WalkSpeed, 1)
		distance := world.Distance(fixture.state(fixture.mobID).position, anchor)
		if distance > farthest {
			farthest = distance
		}
		if distance > mob.LeashRadiusM {
			break
		}
	}
	if farthest <= mob.LeashRadiusM {
		t.Fatalf("the mob only reached %.1f m from its anchor, never crossing its %.1f m leash",
			farthest, mob.LeashRadiusM)
	}

	// From here it ignores the player entirely and goes home.
	for step := 0; step < 4000; step++ {
		if fixture.state(fixture.mobID).position == anchor {
			break
		}
		fixture.step(1)
	}
	home := fixture.state(fixture.mobID)
	if home.position != anchor {
		t.Errorf("the mob stopped at %+v, want its anchor %+v", home.position, anchor)
	}
	if home.health != home.maxHealth {
		t.Errorf("the mob came home at %d/%d health, want full", home.health, home.maxHealth)
	}
}

// TestReturningMobIsUntargetable is rule 5.8.6.
func TestReturningMobIsUntargetable(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	anchor := fixture.state(fixture.mobID).position
	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Fatalf("cast rejected: %v", err)
	}

	// Pull it off its anchor, then let the chase target vanish so it turns
	// round while the second character is standing next to it.
	var seq uint64
	fixture.walk(&seq, world.Vec3{X: 1}, 6, 90)
	if fixture.state(fixture.mobID).position == anchor {
		t.Fatal("the mob never chased")
	}
	watcher := fixture.join()
	fixture.leave(fixture.playerID)
	fixture.playerID = watcher
	fixture.step(1)

	if _, err := fixture.cast(baseAbility, fixture.mobID); !errors.Is(err, combat.ErrInvalidTarget) {
		t.Errorf("casting at a returning mob returned %v, want ErrInvalidTarget", err)
	}
}

func distanceToPlayer(fixture *harness) float32 {
	return world.Distance(fixture.state(fixture.mobID).position, fixture.state(fixture.playerID).position)
}

// ticksToCover is how many ticks of walking at `speed` cover `metres`, rounded
// up so the walk finishes past the mark rather than short of it.
func ticksToCover(metres, speed float32) int {
	if metres <= 0 {
		return 0
	}
	seconds := float64(metres) / float64(speed)
	ticks := seconds / tickInterval.Seconds()
	return int(ticks) + 1
}
