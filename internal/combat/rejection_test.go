package combat_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// TestRangeBoundaryIsTheAbilityRangePlusTolerance is rules 5.3.2 and 6.2. The
// numbers come from the pack: the ability says 10 m and the tolerance is the
// spec's 0.5.
func TestRangeBoundaryIsTheAbilityRangePlusTolerance(t *testing.T) {
	t.Parallel()

	inside := newHarness(t, basePack, targetMob, world.Vec3{X: 10.4}, combat.Options{})
	if _, err := inside.cast(baseAbility, inside.mobID); err != nil {
		t.Errorf("a cast at 10.4 m was rejected with %v, want a hit", err)
	}

	outside := newHarness(t, basePack, targetMob, world.Vec3{X: 10.6}, combat.Options{})
	before := outside.state(outside.mobID)
	event, err := outside.cast(baseAbility, outside.mobID)
	if !errors.Is(err, combat.ErrOutOfRange) {
		t.Fatalf("a cast at 10.6 m returned %v, want ErrOutOfRange", err)
	}
	if event.Rejection != combat.RejectionOutOfRange {
		t.Errorf("event rejection = %v, want RejectionOutOfRange", event.Rejection)
	}
	if after := outside.state(outside.mobID); after.health != before.health {
		t.Errorf("a refused cast changed target health from %d to %d", before.health, after.health)
	}
	// Rule 6.2: a rejected ability costs the player nothing, so the very next
	// tick's cast from inside range must still be free to go.
	if _, err := outside.cast(baseAbility, outside.mobID); !errors.Is(err, combat.ErrOutOfRange) {
		t.Errorf("the second refusal returned %v, want the same reason and no cooldown", err)
	}
}

// TestGlobalCooldownRejectsASecondCast is rule 5.4.1, and the measurement of
// how long the gate lasts is rule 5.4.2's thirty ticks.
func TestGlobalCooldownRejectsASecondCast(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Fatalf("first cast rejected: %v", err)
	}

	before := fixture.state(fixture.mobID)
	if _, err := fixture.cast(baseAbility, fixture.mobID); !errors.Is(err, combat.ErrOnCooldown) {
		t.Fatalf("an immediate second cast returned %v, want ErrOnCooldown", err)
	}
	if after := fixture.state(fixture.mobID); after.health != before.health {
		t.Errorf("a cast refused for the cooldown still dealt damage: %d to %d",
			before.health, after.health)
	}

	// Spam: nothing gets through and nothing changes, however hard the client
	// tries.
	for attempt := 0; attempt < 50; attempt++ {
		if _, err := fixture.cast(baseAbility, fixture.mobID); !errors.Is(err, combat.ErrOnCooldown) {
			t.Fatalf("spam attempt %d returned %v, want ErrOnCooldown", attempt, err)
		}
	}
	if after := fixture.state(fixture.mobID); after.health != before.health {
		t.Errorf("fifty refused casts moved health from %d to %d", before.health, after.health)
	}

	fixture.step(29)
	if _, err := fixture.cast(baseAbility, fixture.mobID); !errors.Is(err, combat.ErrOnCooldown) {
		t.Errorf("a cast 29 ticks later returned %v, want the cooldown to still be running", err)
	}
	fixture.step(1)
	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Errorf("a cast 30 ticks later returned %v, want a hit", err)
	}
}

// TestPerAbilityCooldownComesFromThePack proves that the second gate of rule
// 5.4 is content. The base ability has no cooldown of its own and the
// overlay's has one; nothing in Go distinguishes them.
func TestPerAbilityCooldownComesFromThePack(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, extendedPack, extendedMob, world.Vec3{X: 6}, combat.Options{})
	authored, ok := fixture.module.Rules().Ability(extendedAbility)
	if !ok {
		t.Fatalf("the overlay ability %q is not in the pack", extendedAbility)
	}
	if authored.Cooldown == 0 {
		t.Fatal("the overlay ability has no cooldown; this test would prove nothing")
	}

	if _, err := fixture.cast(extendedAbility, fixture.mobID); err != nil {
		t.Fatalf("first cast rejected: %v", err)
	}
	_, elapsed := fixture.castUntilReady(extendedAbility, fixture.mobID, 200)
	// 3000 ms at a 33.333333 ms tick.
	if elapsed != 90 {
		t.Errorf("the ability came back after %d ticks, want 90 for a %v cooldown", elapsed, authored.Cooldown)
	}
	if elapsed <= 30 {
		t.Error("the per-ability cooldown did not outlast the global one")
	}
}

// TestFriendlyTargetIsRejected is rule 5.2.5. Two characters share a faction,
// and the pack says that faction is friendly to itself and unattackable.
func TestFriendlyTargetIsRejected(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	ally := fixture.join()

	event, err := fixture.cast(baseAbility, ally)
	if !errors.Is(err, combat.ErrInvalidTarget) {
		t.Fatalf("casting at a same-faction character returned %v, want ErrInvalidTarget", err)
	}
	if event.Damage != 0 {
		t.Errorf("a refused cast dealt %d damage to an ally", event.Damage)
	}
	if state := fixture.state(ally); state.health != state.maxHealth {
		t.Errorf("the ally is at %d/%d health", state.health, state.maxHealth)
	}
}

// TestInvalidTargetsAreRejectedWithoutMutatingTheWorld covers the rest of rule
// 5.2 in one place: no target, an unknown target, the caster itself, and an
// ability the pack does not carry.
func TestInvalidTargetsAreRejectedWithoutMutatingTheWorld(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	before := fixture.state(fixture.mobID)
	player := fixture.state(fixture.playerID)

	cases := []struct {
		name     string
		ability  string
		targetID uint64
		want     error
	}{
		{"no target", baseAbility, 0, combat.ErrNoTarget},
		{"unknown target", baseAbility, 999999, combat.ErrNoTarget},
		{"self target", baseAbility, fixture.playerID, combat.ErrInvalidTarget},
		{"unknown ability", "ability.magic.not-in-this-pack", fixture.mobID, combat.ErrUnknownAbility},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			event, err := fixture.cast(testCase.ability, testCase.targetID)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("cast returned %v, want %v", err, testCase.want)
			}
			if event.Damage != 0 || event.KillingBlow {
				t.Errorf("a refused cast produced %+v", event)
			}
		})
	}

	if after := fixture.state(fixture.mobID); after != before {
		t.Errorf("the mob changed from %+v to %+v across four refusals", before, after)
	}
	if after := fixture.state(fixture.playerID); after != player {
		t.Errorf("the caster changed from %+v to %+v across four refusals", player, after)
	}
	// Nothing above consumed the cooldown, so a valid cast still lands at once.
	if _, err := fixture.cast(baseAbility, fixture.mobID); err != nil {
		t.Errorf("a valid cast after four refusals returned %v; a refusal burnt the cooldown", err)
	}
}

// TestRetransmittedCommandIsDiscarded is the deduplication half of the
// validated-input contract the movement path already had.
func TestRetransmittedCommandIsDiscarded(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	request := combat.AbilityRequest{Seq: 7, TargetID: fixture.mobID, AbilityID: baseAbility}
	if _, err := fixture.module.UseAbility(fixture.playerID, request); err != nil {
		t.Fatalf("first use rejected: %v", err)
	}
	after := fixture.state(fixture.mobID)

	fixture.step(60)
	// The same frame arriving twice is a retransmit, not a refusal: it is
	// dropped without a rejection and without touching the world.
	if _, err := fixture.module.UseAbility(fixture.playerID, request); !errors.Is(err, combat.ErrDuplicateCommand) {
		t.Fatalf("the retransmit returned %v, want ErrDuplicateCommand", err)
	}
	if now := fixture.state(fixture.mobID); now.health != after.health {
		t.Errorf("the retransmit dealt damage: %d to %d", after.health, now.health)
	}
	request.Seq = 8
	if _, err := fixture.module.UseAbility(fixture.playerID, request); err != nil {
		t.Errorf("the next sequence number returned %v, want a hit", err)
	}
}
