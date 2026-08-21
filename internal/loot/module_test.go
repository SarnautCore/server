package loot_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/world"
)

// TestAKillStandsUpACorpseHoldingTheRolledDrop is rule 5.1.1: the roll happens
// at corpse creation, so what the corpse holds is knowable before anybody opens
// it.
func TestAKillStandsUpACorpseHoldingTheRolledDrop(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	containerID := fixture.kill()

	offer, refusal := fixture.loot.Look(fixture.ownerEntity, containerID)
	if refusal != loot.RefusalNone {
		t.Fatalf("Look() refused with %s", refusal)
	}
	if offer.LootTableID != flatTableID {
		t.Errorf("offer.LootTableID = %q, want the sparrow's %q", offer.LootTableID, flatTableID)
	}
	if len(offer.Items) != 1 || offer.Items[0].ItemID != tonicItemID || offer.Items[0].Count != dropCount {
		t.Fatalf("offer.Items = %+v, want %d of %s", offer.Items, dropCount, tonicItemID)
	}

	// Looking twice cannot change what is there, which is the property rolling
	// at creation buys.
	again, _ := fixture.loot.Look(fixture.ownerEntity, containerID)
	if len(again.Items) != 1 || again.Items[0] != offer.Items[0] {
		t.Errorf("a second look reported %+v, want %+v", again.Items, offer.Items)
	}
}

// TestTakingSplitsTheDropIntoStacks joins the two halves of the spec: the roll
// produces a count, and rule 5.7 turns it into stacks the pack's stack limit
// decides.
func TestTakingSplitsTheDropIntoStacks(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	containerID := fixture.kill()

	result, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	if result.Refusal != loot.RefusalNone {
		t.Fatalf("Take() refused with %s", result.Refusal)
	}

	stored := fixture.inventoryOf(fixture.ownerID)
	if got := unitsOf(stored, tonicItemID); got != dropCount {
		t.Errorf("bag holds %d of %s, want %d", got, tonicItemID, dropCount)
	}
	if len(stored) != 3 {
		t.Errorf("bag uses %d slots, want 3 (worked example 6.2)", len(stored))
	}
	for _, item := range stored[:2] {
		if item.Quantity != 20 {
			t.Errorf("slot %d holds %d, want a full stack of 20", item.Slot, item.Quantity)
		}
	}
	if stored[2].Quantity != 5 {
		t.Errorf("the remainder stack holds %d, want 5", stored[2].Quantity)
	}
}

// TestASecondCharacterCannotOpenSomebodyElsesCorpse is rule 5.8.2, and the
// refusal is typed: the session stays up and the corpse is untouched.
func TestASecondCharacterCannotOpenSomebodyElsesCorpse(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	containerID := fixture.kill()

	result, err := fixture.loot.Take(context.Background(), fixture.strangerEntity, containerID)
	if !errors.Is(err, loot.ErrNotYourLoot) {
		t.Fatalf("Take() error = %v, want ErrNotYourLoot", err)
	}
	if result.Refusal != loot.RefusalNotYourLoot {
		t.Errorf("result.Refusal = %s, want not_your_loot", result.Refusal)
	}
	if _, refusal := fixture.loot.Look(fixture.strangerEntity, containerID); refusal != loot.RefusalNotYourLoot {
		t.Errorf("Look() refused with %s, want not_your_loot", refusal)
	}
	if items := fixture.inventoryOf(fixture.strangerID); len(items) != 0 {
		t.Errorf("the stranger's bag holds %+v, want nothing", items)
	}

	// The corpse is intact, so the character who earned it still gets it.
	owned, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if err != nil {
		t.Fatalf("the owner's Take() error = %v", err)
	}
	if owned.Refusal != loot.RefusalNone {
		t.Fatalf("the owner's Take() refused with %s", owned.Refusal)
	}
	if got := unitsOf(fixture.inventoryOf(fixture.ownerID), tonicItemID); got != dropCount {
		t.Errorf("the owner's bag holds %d, want the whole drop", got)
	}
}

// TestTakingTheSameCorpseTwiceYieldsTheItemOnce. Rule 5.6.4 leaves the corpse
// standing but empty, so the second take is an ordinary refusal and not a
// second copy of the drop.
func TestTakingTheSameCorpseTwiceYieldsTheItemOnce(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	containerID := fixture.kill()

	if _, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID); err != nil {
		t.Fatalf("first Take() error = %v", err)
	}
	result, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if !errors.Is(err, loot.ErrAlreadyLooted) {
		t.Fatalf("second Take() error = %v, want ErrAlreadyLooted", err)
	}
	if result.Refusal != loot.RefusalAlreadyLooted {
		t.Errorf("result.Refusal = %s, want already_looted", result.Refusal)
	}

	if got := unitsOf(fixture.inventoryOf(fixture.ownerID), tonicItemID); got != dropCount {
		t.Errorf("bag holds %d of %s after two takes, want exactly one drop of %d", got, tonicItemID, dropCount)
	}
	// The corpse is still there, and still empty.
	if _, refusal := fixture.loot.Look(fixture.ownerEntity, containerID); refusal != loot.RefusalAlreadyLooted {
		t.Errorf("Look() after a take refused with %s, want already_looted", refusal)
	}
}

// TestAFullBagLeavesTheCorpseIntact is rule 5.6.3. The drop needs three slots
// and the bag has two, so nothing is inserted, nothing is destroyed, and the
// client is told why.
func TestAFullBagLeavesTheCorpseIntact(t *testing.T) {
	fixture := newHarness(t, harnessOptions{slots: 2})
	containerID := fixture.kill()

	result, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if !errors.Is(err, loot.ErrBagFull) {
		t.Fatalf("Take() error = %v, want ErrBagFull", err)
	}
	if result.Refusal != loot.RefusalBagFull {
		t.Errorf("result.Refusal = %s, want bag_full", result.Refusal)
	}
	if items := fixture.inventoryOf(fixture.ownerID); len(items) != 0 {
		t.Errorf("bag holds %+v after a refused take, want nothing", items)
	}

	offer, refusal := fixture.loot.Look(fixture.ownerEntity, containerID)
	if refusal != loot.RefusalNone {
		t.Fatalf("Look() after a bag-full refusal returned %s; the corpse should be intact", refusal)
	}
	if len(offer.Items) != 1 || offer.Items[0].Count != dropCount {
		t.Errorf("the corpse holds %+v, want the whole drop still on it", offer.Items)
	}
}

// abortingRepository fails the purse write, cutting the award transaction in
// half at the worst possible moment.
type abortingRepository struct {
	charstore.Repository
	armed bool
}

var errInjected = errors.New("injected mid-transaction failure")

func (repository *abortingRepository) SaveCharacterState(ctx context.Context, state charstore.CharacterState) error {
	if repository.armed {
		return errInjected
	}
	return repository.Repository.SaveCharacterState(ctx, state)
}

func (repository *abortingRepository) RunInTx(
	ctx context.Context,
	fn func(ctx context.Context, tx charstore.Repository) error,
) error {
	return repository.Repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		return fn(ctx, &abortingRepository{Repository: tx, armed: repository.armed})
	})
}

// TestAnAbortedTakeLeavesTheItemInExactlyOnePlace is the crash-consistency
// claim, and it is the reason Take is three phases rather than one.
//
// The award writes the inventory and then credits the purse. Failing the second
// write rolls the first one back, and the corpse is un-reserved rather than
// cleared, so the item is on the corpse and not in the bag. Not both. Not
// neither.
func TestAnAbortedTakeLeavesTheItemInExactlyOnePlace(t *testing.T) {
	// The failure is armed after the harness has seeded its characters, so what
	// it cuts in half is the award and nothing else.
	repository := &abortingRepository{Repository: charstore.NewMemory()}
	fixture := newHarness(t, harnessOptions{repository: repository})
	containerID := fixture.kill()
	repository.armed = true

	result, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if err == nil {
		t.Fatal("Take() error = nil, want the injected failure")
	}
	if result.Refusal != loot.RefusalInternal {
		t.Errorf("result.Refusal = %s, want internal", result.Refusal)
	}

	inBag := unitsOf(fixture.inventoryOf(fixture.ownerID), tonicItemID)
	offer, refusal := fixture.loot.Look(fixture.ownerEntity, containerID)
	var onCorpse int32
	if refusal == loot.RefusalNone {
		for _, grant := range offer.Items {
			if grant.ItemID == tonicItemID {
				onCorpse += grant.Count
			}
		}
	}

	switch {
	case inBag > 0 && onCorpse > 0:
		t.Fatalf("the item is in both places: %d in the bag and %d on the corpse", inBag, onCorpse)
	case inBag == 0 && onCorpse == 0:
		t.Fatal("the item is in neither place; the aborted take destroyed it")
	case onCorpse != dropCount:
		t.Errorf("the corpse holds %d, want the whole drop of %d", onCorpse, dropCount)
	}

	// And the corpse is takeable again once the store recovers, which is what
	// makes the un-reservation more than a comment.
	repository.armed = false
	retry, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, containerID)
	if err != nil {
		t.Fatalf("Take() after recovery error = %v", err)
	}
	if retry.Refusal != loot.RefusalNone {
		t.Fatalf("Take() after recovery refused with %s", retry.Refusal)
	}
	if got := unitsOf(fixture.inventoryOf(fixture.ownerID), tonicItemID); got != dropCount {
		t.Errorf("bag holds %d after the retry, want %d", got, dropCount)
	}
}

// TestAnUnlootedCorpseDespawnsCompletely is rule 5.1.2, asserted by entity
// count so that a container left in the registry cannot hide behind a map that
// forgot about it.
func TestAnUnlootedCorpseDespawnsCompletely(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	before := fixture.zone.EntityCount()

	containerID := fixture.kill()
	if got := fixture.zone.EntityCount(); got != before+1 {
		t.Fatalf("entity count = %d after a kill, want %d: the corpse container", got, before+1)
	}
	if fixture.loot.CorpseCount() != 1 {
		t.Fatalf("the module holds %d corpses, want 1", fixture.loot.CorpseCount())
	}

	// The corpse timer is thirty seconds at a thirtieth of a second per tick,
	// and mechanics/loot.md section 3 sets LOOT_OWNERSHIP_S equal to it. A few
	// extra ticks cover the rounding.
	fixture.step(int(30*time.Second/tickInterval) + 5)

	if got := fixture.zone.EntityCount(); got != before {
		t.Errorf("entity count = %d after the despawn, want %d", got, before)
	}
	if got := fixture.loot.CorpseCount(); got != 0 {
		t.Errorf("the module still holds %d corpses, want 0", got)
	}
	if _, ok := fixture.loot.CorpseFor(fixture.mobEntity); ok {
		t.Error("the victim still resolves to a corpse container")
	}
	if _, refusal := fixture.loot.Look(fixture.ownerEntity, containerID); refusal != loot.RefusalNoCorpse {
		t.Errorf("Look() at a despawned corpse refused with %s, want no_corpse", refusal)
	}

	var present bool
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		present = tick.Entity(containerID) != nil
		return nil
	})
	if present {
		t.Error("the container entity is still in the registry")
	}
}

// TestOwnershipSurvivesAReconnect is rule 5.8.4. The entity and the session are
// both destroyed by zone.Leave; the character id is not, and ownership is keyed
// on it.
func TestOwnershipSurvivesAReconnect(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})
	containerID := fixture.kill()

	// Disconnect: the combat and loot modules release the entity, and the zone
	// removes it.
	fixture.combat.Release(fixture.ownerEntity)
	fixture.loot.Release(fixture.ownerEntity)
	fixture.zone.Leave(fixture.ownerEntity)

	// Reconnect: a new entity for the same character.
	reconnected, _ := fixture.zone.Join()
	if err := fixture.combat.Admit(reconnected); err != nil {
		t.Fatalf("Admit() error = %v", err)
	}
	fixture.loot.Admit(reconnected, fixture.ownerID)

	result, err := fixture.loot.Take(context.Background(), reconnected, containerID)
	if err != nil {
		t.Fatalf("Take() after a reconnect error = %v", err)
	}
	if result.Refusal != loot.RefusalNone {
		t.Fatalf("Take() after a reconnect refused with %s", result.Refusal)
	}
	if got := unitsOf(fixture.inventoryOf(fixture.ownerID), tonicItemID); got != dropCount {
		t.Errorf("bag holds %d, want the drop the pre-disconnect kill earned", got)
	}
}

// TestTakingSomethingThatIsNotACorpseIsRefused. A client naming an entity it
// invented gets a typed refusal, not a crash and not a protocol error.
func TestTakingSomethingThatIsNotACorpseIsRefused(t *testing.T) {
	fixture := newHarness(t, harnessOptions{})

	for _, entityID := range []uint64{0, fixture.mobEntity, 999_999} {
		result, err := fixture.loot.Take(context.Background(), fixture.ownerEntity, entityID)
		if !errors.Is(err, loot.ErrNoCorpse) {
			t.Errorf("Take(%d) error = %v, want ErrNoCorpse", entityID, err)
		}
		if result.Refusal != loot.RefusalNoCorpse {
			t.Errorf("Take(%d) refusal = %s, want no_corpse", entityID, result.Refusal)
		}
	}
}
