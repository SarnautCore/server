package inventory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/store"
)

// newCharacter seeds one character with an empty bag and a zero purse, which is
// what checkpoint L1 leaves behind for a fresh login.
func newCharacter(t *testing.T, repository store.Repository) uuid.UUID {
	t.Helper()
	characterID := uuid.New()
	err := store.SaveCharacter(context.Background(), repository, store.Snapshot{
		State: store.CharacterState{
			CharacterID: characterID,
			ZoneID:      "PaperHarbor",
			Level:       2,
			Health:      120,
			SaveSeq:     1,
		},
	})
	if err != nil {
		t.Fatalf("seed character: %v", err)
	}
	return characterID
}

func newService(t *testing.T, repository store.Repository, slots int32) *inventory.Service {
	t.Helper()
	service, err := inventory.NewService(repository, fixtureLimits(), slots)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

// TestAwardCommitsSlotsAndPurseTogether is rule 5.6: one transaction writes the
// bag and the purse, and what it returns is what it committed.
func TestAwardCommitsSlotsAndPurseTogether(t *testing.T) {
	repository := store.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	result, err := service.Award(context.Background(), characterID, inventory.Award{
		Money:  17,
		Grants: []inventory.Grant{{ItemID: tonic, Count: 45}},
	})
	if err != nil {
		t.Fatalf("Award() error = %v", err)
	}
	if got := counts(result.Slots); !equal(got, []int32{20, 20, 5}) {
		t.Errorf("result.Slots = %v, want [20 20 5]", got)
	}
	if result.Currency != 17 {
		t.Errorf("result.Currency = %d, want 17", result.Currency)
	}

	stored, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if got := counts(inventory.FromStore(stored)); !equal(got, []int32{20, 20, 5}) {
		t.Errorf("stored counts = %v, want what the award reported", got)
	}
	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Currency != 17 {
		t.Errorf("stored currency = %d, want 17", state.Currency)
	}
	if state.SaveSeq != result.SaveSeq {
		t.Errorf("stored save_seq = %d, award reported %d", state.SaveSeq, result.SaveSeq)
	}
}

// TestAwardWithAFullBagWritesNothing is rule 5.6.3: not the items, and not the
// money either.
func TestAwardWithAFullBagWritesNothing(t *testing.T) {
	repository := store.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 2)

	_, err := service.Award(context.Background(), characterID, inventory.Award{
		Money:  99,
		Grants: []inventory.Grant{{ItemID: tonic, Count: 45}},
	})
	if !errors.Is(err, inventory.ErrBagFull) {
		t.Fatalf("Award() error = %v, want ErrBagFull", err)
	}

	stored, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("stored inventory = %+v, want nothing", stored)
	}
	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Currency != 0 {
		t.Errorf("stored currency = %d, want 0: the money is part of the same all-or-nothing", state.Currency)
	}
}

// abortingRepository fails one named write, so a test can cut a transaction in
// half at a chosen point.
type abortingRepository struct {
	store.Repository
	failStateSave bool
}

var errInjected = errors.New("injected mid-transaction failure")

func (repository *abortingRepository) SaveCharacterState(ctx context.Context, state store.CharacterState) error {
	if repository.failStateSave {
		return errInjected
	}
	return repository.Repository.SaveCharacterState(ctx, state)
}

func (repository *abortingRepository) RunInTx(
	ctx context.Context,
	fn func(ctx context.Context, tx store.Repository) error,
) error {
	return repository.Repository.RunInTx(ctx, func(ctx context.Context, tx store.Repository) error {
		return fn(ctx, &abortingRepository{Repository: tx, failStateSave: repository.failStateSave})
	})
}

// TestAnAbortedAwardRollsTheInventoryBack is the crash-consistency case.
//
// The award writes the inventory and then credits the purse. Failing the second
// write is the worst moment there is: if the transaction were not one unit of
// work the character would have the items and the corpse would still have them
// too. The assertion is that the stored bag is exactly as it was, so the item
// is in the corpse and nowhere else.
func TestAnAbortedAwardRollsTheInventoryBack(t *testing.T) {
	repository := &abortingRepository{Repository: store.NewMemory(), failStateSave: true}
	characterID := newCharacter(t, &abortingRepository{Repository: repository.Repository})
	service := newService(t, repository, 8)

	_, err := service.Award(context.Background(), characterID, inventory.Award{
		Money:  5,
		Grants: []inventory.Grant{{ItemID: feather, Count: 3}},
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("Award() error = %v, want the injected failure", err)
	}

	stored, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("stored inventory = %+v after an aborted award, want nothing", stored)
	}
	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Currency != 0 {
		t.Errorf("stored currency = %d after an aborted award, want 0", state.Currency)
	}
}

// TestTwoAwardsStackIntoTheSameSlots is what a second kill on the same table
// looks like: the merge in rule 5.7.5 happens against what the first award
// persisted, not against an in-memory view.
func TestTwoAwardsStackIntoTheSameSlots(t *testing.T) {
	repository := store.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	for range 2 {
		if _, err := service.Award(context.Background(), characterID, inventory.Award{
			Money:  10,
			Grants: []inventory.Grant{{ItemID: feather, Count: 6}},
		}); err != nil {
			t.Fatalf("Award() error = %v", err)
		}
	}

	stored, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if got := counts(inventory.FromStore(stored)); !equal(got, []int32{10, 2}) {
		t.Errorf("stored counts = %v, want [10 2]: twelve units at a limit of ten", got)
	}
	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Currency != 20 {
		t.Errorf("stored currency = %d, want both credits", state.Currency)
	}
}

// TestAwardAdvancesTheSaveSequence. Two awards in a row must not both write at
// the same sequence, or the second would be refused as a stale save.
func TestAwardAdvancesTheSaveSequence(t *testing.T) {
	repository := store.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	first, err := service.Award(context.Background(), characterID, inventory.Award{Money: 1})
	if err != nil {
		t.Fatalf("first Award() error = %v", err)
	}
	second, err := service.Award(context.Background(), characterID, inventory.Award{Money: 1})
	if err != nil {
		t.Fatalf("second Award() error = %v", err)
	}
	if second.SaveSeq <= first.SaveSeq {
		t.Errorf("save sequence went %d then %d, want it to advance", first.SaveSeq, second.SaveSeq)
	}
}
