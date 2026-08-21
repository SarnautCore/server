package trade_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/trade"
)

var errSaveInterrupted = errors.New("save interrupted")

type failCharacterSave struct {
	charstore.Repository
	characterID uuid.UUID
}

func (repository *failCharacterSave) SaveCharacterState(
	ctx context.Context,
	state charstore.CharacterState,
) error {
	if state.CharacterID == repository.characterID {
		return errSaveInterrupted
	}
	return repository.Repository.SaveCharacterState(ctx, state)
}

func (repository *failCharacterSave) RunInTx(
	ctx context.Context,
	fn func(context.Context, charstore.Repository) error,
) error {
	return repository.Repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		return fn(ctx, &failCharacterSave{Repository: tx, characterID: repository.characterID})
	})
}

func TestTransferRollsBackBothBagsWhenSecondPurseWriteFails(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	repository := &failCharacterSave{
		Repository:  fixture.repository,
		characterID: fixture.second,
	}
	store, err := trade.NewRepositoryStore(
		repository,
		limits{"sword": 1, "gem": 20, "tonic": 20},
		fixture.capacity,
		fixture.bindings,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Transfer(fixture.ctx, trade.Transfer{
		FirstCharacterID:  fixture.first,
		SecondCharacterID: fixture.second,
		FirstItems:        []trade.Item{{BagSlot: 0, ItemID: "sword", Count: 1}},
		SecondItems:       []trade.Item{{BagSlot: 0, ItemID: "gem", Count: 3}},
		FirstMoney:        10,
		SecondMoney:       5,
	})
	if !errors.Is(err, errSaveInterrupted) {
		t.Fatalf("Transfer() error = %v, want interrupted save", err)
	}

	firstState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.first)
	secondState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.second)
	if firstState.Currency != 100 || secondState.Currency != 50 {
		t.Fatalf("purses changed after rollback: %d/%d", firstState.Currency, secondState.Currency)
	}
	firstItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.first)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if _, found := storedItem(firstItems, "sword"); !found {
		t.Fatalf("first inventory lost sword: %+v", firstItems)
	}
	if _, found := storedItem(secondItems, "gem"); !found {
		t.Fatalf("second inventory lost gem: %+v", secondItems)
	}
}

func TestConcurrentTransfersCommitTheSameOfferAtMostOnce(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	store := mustRepositoryStore(t, fixture)
	request := trade.Transfer{
		FirstCharacterID:  fixture.first,
		SecondCharacterID: fixture.second,
		FirstItems:        []trade.Item{{BagSlot: 0, ItemID: "sword", Count: 1}},
	}

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Transfer(fixture.ctx, request)
			results <- err
		}()
	}
	wait.Wait()
	close(results)

	succeeded := 0
	changed := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, trade.ErrHoldingsChanged):
			changed++
		default:
			t.Fatalf("Transfer() error = %v", err)
		}
	}
	if succeeded != 1 || changed != 1 {
		t.Fatalf("results success/changed = %d/%d, want 1/1", succeeded, changed)
	}
	firstItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.first)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if _, found := storedItem(firstItems, "sword"); found {
		t.Fatalf("first still holds transferred sword: %+v", firstItems)
	}
	if item, found := storedItem(secondItems, "sword"); !found || item.Quantity != 1 {
		t.Fatalf("second sword = %+v/%v, want one", item, found)
	}
}

func TestMoneyOverflowRejectsTheWholeTransfer(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	state, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.first)
	state.Currency = math.MaxInt64
	state.SaveSeq++
	if err := fixture.repository.SaveCharacterState(fixture.ctx, state); err != nil {
		t.Fatal(err)
	}
	store := mustRepositoryStore(t, fixture)

	_, err := store.Transfer(fixture.ctx, trade.Transfer{
		FirstCharacterID:  fixture.first,
		SecondCharacterID: fixture.second,
		SecondMoney:       1,
	})
	if !errors.Is(err, trade.ErrHoldingsChanged) {
		t.Fatalf("Transfer() error = %v, want holdings changed", err)
	}
	firstState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.first)
	secondState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.second)
	if firstState.Currency != math.MaxInt64 || secondState.Currency != 50 {
		t.Fatalf("purses changed on overflow: %d/%d", firstState.Currency, secondState.Currency)
	}
}

func TestRepositoryStoreRequiresEveryAuthority(t *testing.T) {
	repository := charstore.NewMemory()
	_, err := trade.NewRepositoryStore(repository, nil, trade.FixedCapacity(8), trade.UnboundItems{})
	if err == nil {
		t.Fatal("NewRepositoryStore accepted nil stack limits")
	}
	_, err = trade.NewRepositoryStore(repository, limits{"sword": 1}, nil, trade.UnboundItems{})
	if err == nil {
		t.Fatal("NewRepositoryStore accepted nil capacity")
	}
	_, err = trade.NewRepositoryStore(repository, limits{"sword": 1}, trade.FixedCapacity(8), nil)
	if err == nil {
		t.Fatal("NewRepositoryStore accepted nil item binding authority")
	}
}
