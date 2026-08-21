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
		Layout: inventory.BagLayout{
			ID: state.Layout.ID, Partitions: append([]int32(nil), state.Layout.Partitions...),
		},
	}
}

func newMoveService(t *testing.T, repository inventory.MoveRepository) *inventory.MoveService {
	t.Helper()
	service, err := inventory.NewMoveService(repository, fixtureLimits())
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
		Layout:  moveLayout(t),
	}}
	service := newMoveService(t, repository)

	result, err := service.Move(context.Background(), uuid.New(), 41, 2, 7)
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
		Layout:  moveLayout(t),
	}
	repository := &atomicMoveRepository{state: cloneMoveState(before), failCommit: true}
	result, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 9, 2, 7)
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
		Layout:  moveLayout(t),
	}
	repository := &atomicMoveRepository{state: cloneMoveState(before)}
	_, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 9, 4, 7)
	if !errors.Is(err, inventory.ErrEmptySource) {
		t.Fatalf("Move() error = %v, want ErrEmptySource", err)
	}
	if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
		t.Errorf("stored state = %+v after refused move, want %+v", stored, before)
	}
}

func TestMoveServiceRejectsAStaleExpectedRevisionBeforeMutation(t *testing.T) {
	before := inventory.MoveState{
		Items:   []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}},
		SaveSeq: 9,
		Layout:  moveLayout(t),
	}
	repository := &atomicMoveRepository{state: cloneMoveState(before)}
	_, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), 8, 2, 7)
	if !errors.Is(err, inventory.ErrStaleRevision) {
		t.Fatalf("Move() error = %v, want ErrStaleRevision", err)
	}
	if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
		t.Fatalf("stale move mutated state to %+v, want %+v", stored, before)
	}
}

func TestMoveServiceRejectsConcurrentCommandsAtTheSameRevision(t *testing.T) {
	const moves = 128
	repository := &atomicMoveRepository{state: inventory.MoveState{
		Items: []inventory.InventoryItem{
			{Slot: 0, ItemID: tonic, Quantity: 7},
			{Slot: 1, ItemID: feather, Quantity: 9},
		},
		SaveSeq: 100,
		Layout:  moveLayout(t),
	}}
	service := newMoveService(t, repository)
	characterID := uuid.New()
	errorsSeen := make(chan error, moves)
	var callers sync.WaitGroup
	for range moves {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, err := service.Move(context.Background(), characterID, 100, 0, 1)
			errorsSeen <- err
		}()
	}
	callers.Wait()
	close(errorsSeen)
	succeeded := 0
	stale := 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, inventory.ErrStaleRevision):
			stale++
		default:
			t.Fatalf("concurrent Move() error = %v", err)
		}
	}

	stored := repository.snapshot()
	want := []inventory.Stack{
		{Slot: 0, ItemID: feather, Count: 9},
		{Slot: 1, ItemID: tonic, Count: 7},
	}
	if got := inventory.FromStore(stored.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("stored slots = %+v after %d swaps, want %+v", got, moves, want)
	}
	if succeeded != 1 || stale != moves-1 || stored.SaveSeq != 101 {
		t.Errorf("success/stale/sequence = %d/%d/%d, want 1/%d/101", succeeded, stale, stored.SaveSeq, moves-1)
	}
}

func TestMoveServiceRejectsInvalidSaveSequenceWithoutMutation(t *testing.T) {
	for _, sequence := range []int64{-1, int64(^uint64(0) >> 1)} {
		repository := &atomicMoveRepository{state: inventory.MoveState{
			Items:   []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}},
			SaveSeq: sequence,
			Layout:  moveLayout(t),
		}}
		before := repository.snapshot()
		_, err := newMoveService(t, repository).Move(context.Background(), uuid.New(), sequence, 2, 7)
		if !errors.Is(err, inventory.ErrInvalidMove) {
			t.Errorf("sequence %d: Move() error = %v, want ErrInvalidMove", sequence, err)
		}
		if stored := repository.snapshot(); !reflect.DeepEqual(stored, before) {
			t.Errorf("sequence %d: state mutated to %+v", sequence, stored)
		}
	}
}

func TestNewMoveServiceRejectsMissingDependencies(t *testing.T) {
	repository := &atomicMoveRepository{}
	if _, err := inventory.NewMoveService(nil, fixtureLimits()); err == nil {
		t.Error("NewMoveService(nil repository) succeeded")
	}
	if _, err := inventory.NewMoveService(repository, nil); err == nil {
		t.Error("NewMoveService(nil limits) succeeded")
	}
	service, err := inventory.NewMoveService(repository, fixtureLimits())
	if err != nil {
		t.Fatalf("NewMoveService() error = %v", err)
	}
	repository.state = inventory.MoveState{
		Items: []inventory.InventoryItem{{Slot: 2, ItemID: tonic, Quantity: 3}}, SaveSeq: 1,
	}
	if _, err := service.Move(context.Background(), uuid.New(), 1, 2, 7); !errors.Is(err, inventory.ErrInvalidMove) {
		t.Errorf("Move() with bad persisted layout error = %v, want ErrInvalidMove", err)
	}
}
