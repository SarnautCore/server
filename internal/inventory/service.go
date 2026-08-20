package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/store"
)

// Award is what one loot take asks the inventory to absorb: a purse credit and
// a list of grants, exactly as `internal/loot` rolled them.
type Award struct {
	Money  int64
	Grants []Grant
}

// Result is the character's inventory after an award, as it was committed.
//
// It is returned rather than left for the caller to re-read because the session
// holds its own view of the inventory for checkpointing, and a view that has to
// be refreshed by a second query is a view that is stale between the two.
type Result struct {
	Slots    []Stack
	Currency int64
	// SaveSeq is the sequence the award committed at. The session adopts it, so
	// its next checkpoint advances past this write instead of being rejected as
	// stale (ADR 0031 §6).
	SaveSeq int64
}

// Awarder is the seam `internal/loot` calls across.
//
// It exists so the loot module can be tested against a bag that is always full,
// or one whose transaction fails halfway, without a database — and so that
// nothing in the loot module can reach a repository directly.
type Awarder interface {
	Award(ctx context.Context, characterID uuid.UUID, award Award) (Result, error)
}

// Service commits awards through `internal/store`.
type Service struct {
	repository store.Repository
	limits     Limits
	slots      int32
}

// NewService binds a repository to the content that decides stack limits.
//
// `slots` of zero means [DefaultSlots]. The limits come from the loaded content
// pack: there is no fallback, because an item with no stack limit has no
// defined slot cost and a guess would be inventing content.
func NewService(repository store.Repository, limits Limits, slots int32) (*Service, error) {
	if repository == nil {
		return nil, errors.New("inventory: a repository is required")
	}
	if limits == nil {
		return nil, errors.New("inventory: a stack-limit source is required")
	}
	if slots <= 0 {
		slots = DefaultSlots
	}
	return &Service{repository: repository, limits: limits, slots: slots}, nil
}

// Slots is the bag capacity this service enforces.
func (service *Service) Slots() int32 { return service.slots }

// Award commits one loot take as a single transaction (loot.md rule 5.6).
//
// One transaction is the whole point. The bag is read, the placement is
// computed, the slots are written and the purse is credited inside it, so a
// crash anywhere in the middle leaves the stored inventory exactly as it was
// and the caller still holding an un-cleared corpse. There is no window in
// which the item is in both places, and none in which it is in neither.
//
// On [ErrBagFull] nothing at all is written: rule 5.6.3 makes the insertion all
// or nothing, money included.
func (service *Service) Award(
	ctx context.Context,
	characterID uuid.UUID,
	award Award,
) (Result, error) {
	var result Result
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx store.Repository) error {
		state, err := tx.LoadCharacterState(ctx, characterID)
		if err != nil {
			return fmt.Errorf("load character state: %w", err)
		}
		stored, err := tx.LoadInventory(ctx, characterID)
		if err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}

		placed, err := Insert(FromStore(stored), award.Grants, service.limits, service.slots)
		if err != nil {
			return err
		}
		if err := tx.ReplaceInventory(ctx, characterID, ToStore(placed)); err != nil {
			return fmt.Errorf("write inventory: %w", err)
		}

		state.Currency += award.Money
		// The sequence is taken from the row this transaction just read, not
		// from a counter the session keeps. Two writers racing therefore both
		// try to advance to the same number, and the loser is rejected by the
		// anti-clobber rule rather than silently overwriting the winner.
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("credit purse: %w", err)
		}

		result = Result{Slots: placed, Currency: state.Currency, SaveSeq: state.SaveSeq}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// FromStore adapts persisted rows into bag stacks.
func FromStore(items []store.InventoryItem) []Stack {
	stacks := make([]Stack, 0, len(items))
	for _, item := range items {
		stacks = append(stacks, Stack{Slot: item.Slot, ItemID: item.ItemID, Count: item.Quantity})
	}
	return stacks
}

// ToStore adapts bag stacks into persisted rows.
func ToStore(stacks []Stack) []store.InventoryItem {
	items := make([]store.InventoryItem, 0, len(stacks))
	for _, stack := range stacks {
		items = append(items, store.InventoryItem{
			Slot:     stack.Slot,
			ItemID:   stack.ItemID,
			Quantity: stack.Count,
		})
	}
	return items
}
