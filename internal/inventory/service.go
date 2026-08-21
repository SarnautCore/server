package inventory

import (
	"context"

	"github.com/google/uuid"
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

type InventoryItem struct {
	Slot     int32
	ItemID   string
	Quantity int32
}

// FromStore adapts persisted rows into bag stacks.
func FromStore(items []InventoryItem) []Stack {
	stacks := make([]Stack, 0, len(items))
	for _, item := range items {
		stacks = append(stacks, Stack{Slot: item.Slot, ItemID: item.ItemID, Count: item.Quantity})
	}
	return stacks
}

// ToStore adapts bag stacks into persisted rows.
func ToStore(stacks []Stack) []InventoryItem {
	items := make([]InventoryItem, 0, len(stacks))
	for _, stack := range stacks {
		items = append(items, InventoryItem{
			Slot:     stack.Slot,
			ItemID:   stack.ItemID,
			Quantity: stack.Count,
		})
	}
	return items
}
