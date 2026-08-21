package inventory

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidMove    = errors.New("inventory: invalid move")
	ErrEmptySource    = errors.New("inventory: source slot is empty")
	ErrSlotOutOfRange = errors.New("inventory: slot is outside the bag layout")
	ErrStackAtLimit   = errors.New("inventory: destination stack is full")
)

// Move relocates one occupied slot inside an authored bag layout.
//
// An empty destination receives the source. A matching item absorbs only what
// its content-authored stack limit permits, leaving any remainder in the
// source slot. A different item swaps with the source. Errors return no result
// and never mutate slots.
func Move(slots []Stack, from, to int32, limits Limits, layout BagLayout) ([]Stack, error) {
	if err := layout.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMove, err)
	}
	if limits == nil {
		return nil, fmt.Errorf("%w: stack-limit source is nil", ErrInvalidMove)
	}
	capacity := layout.Capacity()
	if from < 0 || from >= capacity {
		return nil, fmt.Errorf("%w: source %d outside [0,%d)", ErrSlotOutOfRange, from, capacity)
	}
	if to < 0 || to >= capacity {
		return nil, fmt.Errorf("%w: destination %d outside [0,%d)", ErrSlotOutOfRange, to, capacity)
	}
	if from == to {
		return nil, fmt.Errorf("%w: source and destination are both %d", ErrInvalidMove, from)
	}

	working, stackLimits, err := checkedStacks(slots, limits, capacity)
	if err != nil {
		return nil, err
	}
	source, occupied := working[from]
	if !occupied {
		return nil, fmt.Errorf("%w: %d", ErrEmptySource, from)
	}
	destination, occupied := working[to]
	if !occupied {
		delete(working, from)
		source.Slot = to
		working[to] = source
		return ordered(working), nil
	}
	if destination.ItemID != source.ItemID {
		source.Slot = to
		destination.Slot = from
		working[to] = source
		working[from] = destination
		return ordered(working), nil
	}

	limit := stackLimits[source.ItemID]
	room := limit - destination.Count
	if room <= 0 {
		return nil, fmt.Errorf("%w: slot %d holds %d of %q", ErrStackAtLimit, to, destination.Count, destination.ItemID)
	}
	moved := source.Count
	if moved > room {
		moved = room
	}
	destination.Count += moved
	source.Count -= moved
	working[to] = destination
	if source.Count == 0 {
		delete(working, from)
	} else {
		working[from] = source
	}
	return ordered(working), nil
}

func checkedStacks(
	slots []Stack,
	limits Limits,
	capacity int32,
) (map[int32]Stack, map[string]int32, error) {
	working := make(map[int32]Stack, len(slots))
	stackLimits := make(map[string]int32)
	for _, stack := range slots {
		if stack.Slot < 0 || stack.Slot >= capacity {
			return nil, nil, fmt.Errorf(
				"%w: occupied slot %d outside [0,%d)",
				ErrSlotOutOfRange,
				stack.Slot,
				capacity,
			)
		}
		if _, duplicate := working[stack.Slot]; duplicate {
			return nil, nil, fmt.Errorf("%w: slot %d appears twice", ErrInvalidMove, stack.Slot)
		}
		if stack.ItemID == "" || stack.Count <= 0 {
			return nil, nil, fmt.Errorf(
				"%w: slot %d holds %d of %q",
				ErrInvalidMove,
				stack.Slot,
				stack.Count,
				stack.ItemID,
			)
		}
		limit, known := stackLimits[stack.ItemID]
		if !known {
			var ok bool
			limit, ok = limits.StackLimit(stack.ItemID)
			if !ok {
				return nil, nil, fmt.Errorf("%w: %q", ErrUnknownItem, stack.ItemID)
			}
			stackLimits[stack.ItemID] = limit
		}
		if limit < 1 || stack.Count > limit {
			return nil, nil, fmt.Errorf(
				"%w: slot %d holds %d of %q with limit %d",
				ErrInvalidMove,
				stack.Slot,
				stack.Count,
				stack.ItemID,
				limit,
			)
		}
		working[stack.Slot] = stack
	}
	return working, stackLimits, nil
}
