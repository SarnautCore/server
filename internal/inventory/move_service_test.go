package inventory_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
)

type atomicMoveRepository struct {
	mu         sync.Mutex
	state      inventory.MoveState
	failCommit bool
}

var errMoveCommit = errors.New("injected move commit failure")

func (repository *atomicMoveRepository) UpdateInventory(
	ctx context.Context,
	_ uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return inventory.MoveState{}, err
	}
	working := cloneMoveState(repository.state)
	replacement, err := update(working)
	if err != nil {
		return inventory.MoveState{}, err
	}
	if repository.failCommit {
		return inventory.MoveState{}, errMoveCommit
	}
	repository.state = cloneMoveState(replacement)
	return cloneMoveState(repository.state), nil
}

func (repository *atomicMoveRepository) snapshot() inventory.MoveState {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return cloneMoveState(repository.state)
}

func cloneMoveState(state inventory.MoveState) inventory.MoveState {
	return inventory.MoveState{
		Items:   append([]inventory.InventoryItem(nil), state.Items...),
		SaveSeq: state.SaveSeq,
	}
}

func newMoveService(t *testing.T, repository inventory.MoveRepository) *inventory.MoveService {
	t.Helper()
	service, err := inventory.NewMoveService(repository, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("NewMoveService() error = %v", err)
	}
	return service
}

func TestMoveServiceCommitsReplacementAndSaveSequenceTogether(t *testing.T) {
	repository := &atomicMoveRepository{state: inventory.MoveState{
		Items: []inventory.InventoryItem{
			{Slot: 2, ItemID: tonic, Quantity: 13},
			{Slot: 7, ItemID: tonic, Quantity: 12},
		},
		SaveSeq: 41,
	}}
	service := newMoveService(t, repository)

	result, err := service.Move(context.Background(), uuid.New(), 2, 7)
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	wantSlots := []inventory.Stack{
		{Slot: 2, ItemID: tonic, Count: 5},
		{Slot: 7, ItemID: tonic, Count: 20},
	}
	if !reflect.DeepEqual(result.Slots, wantSlots) || result.SaveSeq != 42 {
		t.Fatalf("Move() = %+v, want slots %+v at sequence 42", result, wantSlots)
	}
	stored := repository.snapshot()
	if !reflect.DeepEqual(inventory.FromStore(stored.Items), wantSlots) || stored.SaveSeq != 42 {
		t.Fatalf("stored state = %+v, want committed result", stored)
	}
}

func TestMoveServiceRollsBackReplacementWhenCommitFails(t *testing.T) {
	before := inventory.MoveState{
		Items:   []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}},
		SaveSeq: 9,
	}
	repository := &atomicMoveRepository{state: cloneMoveState(before), failCommit: true}
	result, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 2, 7)
	if !errors.Is(err, errMoveCommit) {
		t.Fatalf("Move() error = %v, want commit failure", err)
	}
	if !reflect.DeepEqual(result, inventory.MoveResult{}) {
		t.Errorf("Move() result = %+v on failure", result)
	}
	if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
		t.Errorf("stored state = %+v after failed commit, want %+v", stored, before)
	}
}

func TestMoveServiceRejectsInvalidMoveBeforeCommit(t *testing.T) {
	before := inventory.MoveState{
		Items:   []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}},
		SaveSeq: 9,
	}
	repository := &atomicMoveRepository{state: cloneMoveState(before)}
	_, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 4, 7)
	if !errors.Is(err, inventory.ErrEmptySource) {
		t.Fatalf("Move() error = %v, want ErrEmptySource", err)
	}
	if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
		t.Errorf("stored state = %+v after refused move, want %+v", stored, before)
	}
}

func TestMoveServiceSerializesConcurrentMovesWithoutLosingItems(t *testing.T) {
	const moves = 128
	repository := &atomicMoveRepository{state: inventory.MoveState{
		Items: []inventory.InventoryItem{
			{Slot: 0, ItemID: tonic, Quantity: 7},
			{Slot: 1, ItemID: feather, Quantity: 9},
		},
		SaveSeq: 100,
	}}
	service := newMoveService(t, repository)
	characterID := uuid.New()
	errorsSeen := make(chan error, moves)
	var callers sync.WaitGroup
	for range moves {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, err := service.Move(context.Background(), characterID, 0, 1)
			errorsSeen <- err
		}()
	}
	callers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Move() error = %v", err)
		}
	}

	stored := repository.snapshot()
	want := []inventory.Stack{
		{Slot: 0, ItemID: tonic, Count: 7},
		{Slot: 1, ItemID: feather, Count: 9},
	}
	if got := inventory.FromStore(stored.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("stored slots = %+v after %d swaps, want %+v", got, moves, want)
	}
	if stored.SaveSeq != 100+moves {
		t.Errorf("save sequence = %d, want %d", stored.SaveSeq, 100+moves)
	}
}

func TestMoveServiceRejectsInvalidSaveSequenceWithoutMutation(t *testing.T) {
	for _, sequence := range []int64{-1, int64(^uint64(0) >> 1)} {
		repository := &atomicMoveRepository{state: inventory.MoveState{
			Items:   []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}},
			SaveSeq: sequence,
		}}
		before := repository.snapshot()
		_, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 2, 7)
		if !errors.Is(err, inventory.ErrInvalidMove) {
			t.Errorf("sequence %d: Move() error = %v, want ErrInvalidMove", sequence, err)
		}
		if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
			t.Errorf("sequence %d: state mutated to %+v", sequence, stored)
		}
	}
}

func TestNewMoveServiceRejectsMissingDependencies(t *testing.T) {
	layout := moveLayout(t)
	repository := &atomicMoveRepository{}
	if _, err := inventory.NewMoveService(nil, fixtureLimits(), layout); err == nil {
		t.Error("NewMoveService(nil repository) succeeded")
	}
	if _, err := inventory.NewMoveService(repository, nil, layout); err == nil {
		t.Error("NewMoveService(nil limits) succeeded")
	}
	if _, err := inventory.NewMoveService(repository, fixtureLimits(), inventory.BagLayout{}); !errors.Is(err, inventory.ErrInvalidBagLayout) {
		t.Errorf("NewMoveService(bad layout) error = %v", err)
	}
}
