package inventory

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// ErrStaleRevision reports a move based on an inventory revision that no
// longer matches the locked character save sequence.
var ErrStaleRevision = errors.New("inventory: stale revision")

// MoveState is the persisted part of a character that one inventory move may
// replace. The repository keeps the inventory and SaveSeq in the same
// transaction as the callback.
type MoveState struct {
	Items   []InventoryItem
	SaveSeq int64
	Layout  BagLayout
}

// MoveRepository owns the atomic read-modify-write used by [MoveService]. It
// exposes no account, quest, character creation, or protocol operations.
type MoveRepository interface {
	UpdateInventory(
		ctx context.Context,
		characterID uuid.UUID,
		update func(MoveState) (MoveState, error),
	) (MoveState, error)
}

// MoveResult is the inventory state committed by a move.
type MoveResult struct {
	Slots   []Stack
	SaveSeq int64
}

// MoveService applies pure bag moves inside a repository transaction.
type MoveService struct {
	repository MoveRepository
	limits     Limits
}

func NewMoveService(repository MoveRepository, limits Limits) (*MoveService, error) {
	if repository == nil {
		return nil, errors.New("inventory: a move repository is required")
	}
	if limits == nil {
		return nil, errors.New("inventory: a stack-limit source is required")
	}
	return &MoveService{repository: repository, limits: limits}, nil
}

// Move commits a slot move and the next save sequence as one replacement.
func (service *MoveService) Move(
	ctx context.Context,
	characterID uuid.UUID,
	expectedSaveSeq int64,
	from,
	to int32,
) (MoveResult, error) {
	committed, err := service.repository.UpdateInventory(
		ctx,
		characterID,
		func(state MoveState) (MoveState, error) {
			if state.SaveSeq != expectedSaveSeq {
				return MoveState{}, fmt.Errorf("%w: expected %d, stored %d",
					ErrStaleRevision, expectedSaveSeq, state.SaveSeq)
			}
			if state.SaveSeq < 0 || state.SaveSeq == math.MaxInt64 {
				return MoveState{}, fmt.Errorf("%w: save sequence %d", ErrInvalidMove, state.SaveSeq)
			}
			if err := state.Layout.Validate(); err != nil {
				return MoveState{}, fmt.Errorf("%w: persisted layout: %w", ErrInvalidMove, err)
			}
			moved, err := Move(FromStore(state.Items), from, to, service.limits, state.Layout)
			if err != nil {
				return MoveState{}, err
			}
			return MoveState{Items: ToStore(moved), SaveSeq: state.SaveSeq + 1, Layout: state.Layout}, nil
		},
	)
	if err != nil {
		return MoveResult{}, err
	}
	return MoveResult{Slots: FromStore(committed.Items), SaveSeq: committed.SaveSeq}, nil
}
