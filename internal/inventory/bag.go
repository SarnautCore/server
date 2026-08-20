// Package inventory holds item instances in bag slots and money in a purse.
//
// It owns rule 5.7 of mechanics/loot.md — stack splitting, merging into
// partially filled stacks, and the all-or-nothing insertion of rule 5.6.3 — and
// the persistence of both through `internal/store`. Every stack limit it uses
// comes from the content pack; there is no constant in this package that a
// second item could contradict.
//
// The bag arithmetic in this file is pure. It takes a slot list and a list of
// grants and returns a new slot list, so a test can assert worked example 6.2
// without a database, and the transactional part in service.go has nothing left
// to decide.
package inventory

import (
	"errors"
	"fmt"
	"sort"
)

// ErrBagFull reports rule 5.6.3: the bag cannot hold every stack, so nothing is
// inserted, no money is credited, and the corpse keeps the whole drop.
//
// Partial looting is a deliberate non-goal for M2 (loot.md section 7.4), which
// is why this is one error and not a partial result.
var ErrBagFull = errors.New("inventory: bag is full")

// DefaultSlots is how many bag slots a character has.
//
// It is a curated SarnautCore decision, not a value from reference data: bag
// capacity in retail is a property of the bags a character has equipped, and no
// spec covers equipment yet. It lives here so that the one place to change it
// is the one place that reads it.
const DefaultSlots = 16

// Stack is one occupied bag slot: `1 <= Count <= item.stack_limit`.
type Stack struct {
	Slot   int32
	ItemID string
	Count  int32
}

// Grant is one item and count to insert, as `internal/loot` rolled it.
type Grant struct {
	ItemID string
	Count  int32
}

// Limits answers what one item's stack limit is. `internal/pack` implements it;
// a test implements it in three lines.
//
// It is an interface rather than a *pack.Pack so that this package can be
// tested against a limit of 1, of 20 and of a million without authoring a
// content pack for each, and so that nothing here can reach for a second field
// of an item record by accident.
type Limits interface {
	// StackLimit reports the maximum units per stack for one item id, and false
	// when the content does not describe the item at all.
	StackLimit(itemID string) (int32, bool)
}

// ErrUnknownItem reports a grant naming an item the pack does not carry. It is
// fatal to the insertion: an item with no stack limit has no defined number of
// slots, so guessing would be inventing content.
var ErrUnknownItem = errors.New("inventory: item is not in the content pack")

// Insert applies `grants` to `slots` and returns the resulting slot list in
// ascending slot order.
//
// It is all or nothing (rule 5.6.3): on ErrBagFull the input is untouched and
// the returned list is nil. `capacity` is the number of addressable slots, so
// slot indices run from 0 to capacity-1.
//
// The order of operations is rule 5.7.5's requirement and not an optimisation:
// existing stacks are topped up first, and only what is left needs new slots.
// Computing the slot requirement before merging is how an implementation ends
// up telling a player with a half-full stack that the bag is full when it is
// not.
func Insert(slots []Stack, grants []Grant, limits Limits, capacity int32) ([]Stack, error) {
	if capacity < 0 {
		return nil, fmt.Errorf("inventory: capacity %d is negative", capacity)
	}
	working := make(map[int32]Stack, len(slots)+len(grants))
	for _, stack := range slots {
		if stack.Count <= 0 {
			return nil, fmt.Errorf("inventory: slot %d holds %d of %q", stack.Slot, stack.Count, stack.ItemID)
		}
		if stack.Slot < 0 || stack.Slot >= capacity {
			return nil, fmt.Errorf("inventory: slot %d is outside a %d slot bag", stack.Slot, capacity)
		}
		working[stack.Slot] = stack
	}

	for _, grant := range merge(grants) {
		limit, ok := limits.StackLimit(grant.ItemID)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownItem, grant.ItemID)
		}
		if limit < 1 {
			// A Limits implementation is allowed to be wrong; the bag is not
			// allowed to divide by it. `pack.Item.Stack` already floors at one,
			// so this only catches a hand-written double.
			limit = 1
		}
		if err := place(working, grant, limit, capacity); err != nil {
			return nil, err
		}
	}
	return ordered(working), nil
}

// place inserts one merged grant, topping up before opening new slots.
func place(working map[int32]Stack, grant Grant, limit, capacity int32) error {
	remaining := grant.Count

	// Rule 5.7.5, and it runs first. Ascending slot order makes the result a
	// function of the bag's contents rather than of map iteration.
	for _, slot := range sortedSlots(working) {
		if remaining <= 0 {
			break
		}
		stack := working[slot]
		if stack.ItemID != grant.ItemID || stack.Count >= limit {
			continue
		}
		room := limit - stack.Count
		if room > remaining {
			room = remaining
		}
		stack.Count += room
		working[slot] = stack
		remaining -= room
	}

	// Rules 5.7.2 to 5.7.4. What is left becomes full stacks and one remainder,
	// which is the same thing as filling free slots to the limit until it runs
	// out — written that way so the loop and the arithmetic cannot disagree.
	for next := int32(0); remaining > 0; next++ {
		if next >= capacity {
			return fmt.Errorf(
				"%w: %d of %q will not fit in %d slots", ErrBagFull, grant.Count, grant.ItemID, capacity,
			)
		}
		if _, occupied := working[next]; occupied {
			continue
		}
		count := limit
		if count > remaining {
			count = remaining
		}
		working[next] = Stack{Slot: next, ItemID: grant.ItemID, Count: count}
		remaining -= count
	}
	return nil
}

// merge folds duplicate item ids into one grant, keeping first-appearance
// order.
//
// This is where rule 5.5.5's deferred merge happens. The evaluator leaves
// duplicates alone because it does not know stack limits; here they are known,
// and two grants of the same item have to share stacks or the slot requirement
// comes out too high.
func merge(grants []Grant) []Grant {
	merged := make([]Grant, 0, len(grants))
	at := make(map[string]int, len(grants))
	for _, grant := range grants {
		if grant.Count <= 0 {
			continue
		}
		if index, seen := at[grant.ItemID]; seen {
			merged[index].Count += grant.Count
			continue
		}
		at[grant.ItemID] = len(merged)
		merged = append(merged, grant)
	}
	return merged
}

func sortedSlots(working map[int32]Stack) []int32 {
	slots := make([]int32, 0, len(working))
	for slot := range working {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(left, right int) bool { return slots[left] < slots[right] })
	return slots
}

func ordered(working map[int32]Stack) []Stack {
	result := make([]Stack, 0, len(working))
	for _, slot := range sortedSlots(working) {
		result = append(result, working[slot])
	}
	return result
}

// Split reports the stacks one count of one item becomes, per rules 5.7.2 and
// 5.7.3, ignoring anything already in the bag.
//
// Nothing in the insertion path calls it: [Insert] tops up first, so the split
// it performs is over what is left rather than over the whole count. It exists
// because worked example 6.2 states the split on its own, and a rule stated in
// the spec deserves a function a test can point at.
func Split(count, limit int32) []int32 {
	if count <= 0 {
		return nil
	}
	if limit < 1 {
		limit = 1
	}
	stacks := make([]int32, 0, count/limit+1)
	for remaining := count; remaining > 0; remaining -= limit {
		if remaining < limit {
			stacks = append(stacks, remaining)
			break
		}
		stacks = append(stacks, limit)
	}
	return stacks
}
