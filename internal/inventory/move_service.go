package inventory

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// MoveState is the persisted part of a character that one inventory move may
// replace. The repository keeps the inventory and SaveSeq in the same
// transaction as the callback.
type MoveState struct {
	Items   []InventoryItem
	SaveSeq int64
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
	layout     BagLayout
}

func NewMoveService(repository MoveRepository, limits Limits, layout BagLayout) (*MoveService, error) {
	if repository == nil {
		return nil, errors.New("inventory: a move repository is required")
	}
	if limits == nil {
		return nil, errors.New("inventory: a stack-limit source is required")
	}
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	return &MoveService{repository: repository, limits: limits, layout: cloneBagLayout(layout)}, nil
}

// Layout returns the authored layout enforced by the service.
func (service *MoveService) Layout() BagLayout { return cloneBagLayout(service.layout) }

// Move commits a slot move and the next save sequence as one replacement.
func (service *MoveService) Move(
	ctx context.Context,
	characterID uuid.UUID,
	from,
	to int32,
) (MoveResult, error) {
	committed, err := service.repository.UpdateInventory(
		ctx,
		characterID,
		func(state MoveState) (MoveState, error) {
			if state.SaveSeq < 0 || state.SaveSeq == math.MaxInt64 {
				return MoveState{}, fmt.Errorf("%w: save sequence %d", ErrInvalidMove, state.SaveSeq)
			}
			moved, err := Move(FromStore(state.Items), from, to, service.limits, service.layout)
			if err != nil {
				return MoveState{}, err
			}
			return MoveState{Items: ToStore(moved), SaveSeq: state.SaveSeq + 1}, nil
		},
	)
	if err != nil {
		return MoveResult{}, err
	}
	return MoveResult{Slots: FromStore(committed.Items), SaveSeq: committed.SaveSeq}, nil
}
