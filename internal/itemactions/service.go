package itemactions

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
)

// Reader checks one expected revision without opening a write transaction.
// A stale read returns the current complete state with ErrStaleRevision.
type Reader interface {
	Read(ctx context.Context, characterID uuid.UUID, expectedRevision int64) (State, error)
}

// Service applies the retail item verbs. Repository and Reader are normally
// one charstore adapter; the split keeps Use from issuing a no-op save.
type Service struct {
	repository Repository
	reader     Reader
	catalog    Catalog
	use        UseAuthority
	events     EquipEventSink
}

func NewService(
	repository Repository,
	reader Reader,
	catalog Catalog,
	use UseAuthority,
	events EquipEventSink,
) (*Service, error) {
	switch {
	case repository == nil:
		return nil, errors.New("item actions: repository is required")
	case reader == nil:
		return nil, errors.New("item actions: reader is required")
	case catalog == nil:
		return nil, errors.New("item actions: catalog is required")
	}
	return &Service{repository: repository, reader: reader, catalog: catalog, use: use, events: events}, nil
}

func (service *Service) Split(ctx context.Context, actor Actor, command SplitCommand) (Result, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, err
	}
	if command.MoveNoMore < 0 || command.MoveNoMore > MaxMoveNoMore {
		return Result{}, fmt.Errorf("%w: move_no_more %d outside 0 through %d",
			ErrInvalidCommand, command.MoveNoMore, MaxMoveNoMore)
	}
	return service.update(ctx, actor, command.ExpectedRevision, func(state State) (State, []EquipChanged, error) {
		moved, err := transfer(state, command.FromSlot, command.ToSlot,
			command.MoveNoMore, service.catalog)
		if err != nil {
			return State{}, nil, err
		}
		state.Inventory = moved
		return advance(state)
	})
}

func (service *Service) Drop(ctx context.Context, actor Actor, command DropCommand) (Result, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, err
	}
	if command.Count <= 0 {
		return Result{}, fmt.Errorf("%w: drop count %d is not positive", ErrInvalidCommand, command.Count)
	}
	return service.update(ctx, actor, command.ExpectedRevision, func(state State) (State, []EquipChanged, error) {
		if err := validateBagSlot(command.Slot, state.Layout); err != nil {
			return State{}, nil, err
		}
		index := stackIndex(state.Inventory, command.Slot)
		if index < 0 {
			return State{}, nil, fmt.Errorf("%w: inventory slot %d", ErrEmptySlot, command.Slot)
		}
		stack := state.Inventory[index]
		definition, ok := service.catalog.ItemDefinition(stack.ItemID)
		if !ok || !definition.Droppable {
			return State{}, nil, fmt.Errorf("%w: %q cannot be dropped", ErrUnsupportedAction, stack.ItemID)
		}
		if command.Count > stack.Count {
			return State{}, nil, fmt.Errorf("%w: drop %d from stack of %d", ErrInvalidCommand, command.Count, stack.Count)
		}
		state.Inventory = append([]inventory.Stack(nil), state.Inventory...)
		if command.Count == stack.Count {
			state.Inventory = slices.Delete(state.Inventory, index, index+1)
		} else {
			state.Inventory[index].Count -= command.Count
		}
		return advance(state)
	})
}

func (service *Service) OpenBox(ctx context.Context, actor Actor, command OpenBoxCommand) (Result, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, err
	}
	return service.update(ctx, actor, command.ExpectedRevision, func(state State) (State, []EquipChanged, error) {
		if err := validateBagSlot(command.BoxSlot, state.Layout); err != nil {
			return State{}, nil, err
		}
		boxIndex := stackIndex(state.Inventory, command.BoxSlot)
		if boxIndex < 0 {
			return State{}, nil, fmt.Errorf("%w: box slot %d", ErrEmptySlot, command.BoxSlot)
		}
		box := state.Inventory[boxIndex]
		definition, ok := service.catalog.ItemDefinition(box.ItemID)
		if !ok || definition.Box == nil {
			return State{}, nil, fmt.Errorf("%w: %q is not an openable box", ErrUnsupportedAction, box.ItemID)
		}

		keySlot := command.KeySlot
		switch {
		case definition.Box.KeyItemID == "" && keySlot != -1:
			return State{}, nil, fmt.Errorf("%w: box %q does not accept a key", ErrWrongKey, box.ItemID)
		case definition.Box.KeyItemID != "" && keySlot == -1:
			return State{}, nil, fmt.Errorf("%w: box %q requires %q", ErrWrongKey, box.ItemID, definition.Box.KeyItemID)
		case keySlot != -1:
			if err := validateBagSlot(keySlot, state.Layout); err != nil {
				return State{}, nil, err
			}
			keyIndex := stackIndex(state.Inventory, keySlot)
			if keyIndex < 0 {
				return State{}, nil, fmt.Errorf("%w: key slot %d", ErrEmptySlot, keySlot)
			}
			if state.Inventory[keyIndex].ItemID != definition.Box.KeyItemID {
				return State{}, nil, fmt.Errorf("%w: slot %d holds %q, want %q", ErrWrongKey,
					keySlot, state.Inventory[keyIndex].ItemID, definition.Box.KeyItemID)
			}
		}

		working := append([]inventory.Stack(nil), state.Inventory...)
		var err error
		working, err = removeAt(working, command.BoxSlot, 1)
		if err != nil {
			return State{}, nil, err
		}
		if keySlot != -1 && definition.Box.ConsumeKey {
			working, err = removeAt(working, keySlot, 1)
			if err != nil {
				return State{}, nil, err
			}
		}
		placed, err := insertDefaultRewards(working, definition.Box.Rewards, service.catalog, state.Layout.Capacity())
		if err != nil {
			return State{}, nil, err
		}
		if err := assignMissingInstanceIDs(placed, state); err != nil {
			return State{}, nil, err
		}
		state.Inventory = placed
		return advance(state)
	})
}

func (service *Service) Equip(ctx context.Context, actor Actor, command EquipCommand) (Result, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, err
	}
	return service.update(ctx, actor, command.ExpectedRevision, func(state State) (State, []EquipChanged, error) {
		if err := validateBagSlot(command.BagSlot, state.Layout); err != nil {
			return State{}, nil, err
		}
		index := stackIndex(state.Inventory, command.BagSlot)
		if index < 0 {
			return State{}, nil, fmt.Errorf("%w: inventory slot %d", ErrEmptySlot, command.BagSlot)
		}
		incoming := state.Inventory[index]
		definition, ok := service.catalog.ItemDefinition(incoming.ItemID)
		if !ok || !definitionAllowsSlot(definition, command.DressSlot) {
			return State{}, nil, fmt.Errorf("%w: %q in dress slot %d", ErrIncompatibleSlot, incoming.ItemID, command.DressSlot)
		}
		if incoming.Count != 1 {
			return State{}, nil, fmt.Errorf("%w: equippable instance %d has count %d", ErrInvalidCommand, incoming.InstanceID, incoming.Count)
		}
		incoming.Slot = -1
		if definition.BindOnEquip {
			incoming.Bound = true
		}

		state.Inventory = append([]inventory.Stack(nil), state.Inventory...)
		state.Inventory = slices.Delete(state.Inventory, index, index+1)
		var outgoing *inventory.Stack
		if command.DressSlot == EquipmentBag {
			if definition.BagLayout == nil {
				return State{}, nil, fmt.Errorf("%w: %q has no bag layout", ErrIncompatibleSlot, incoming.ItemID)
			}
			if strandedSlot(state.Inventory, definition.BagLayout.Capacity(), command.BagSlot) {
				return State{}, nil, ErrBagWouldShrink
			}
			outgoing = state.Bag
			bag := incoming
			state.Bag = &bag
			state.Layout = cloneLayout(*definition.BagLayout)
		} else {
			if !isRegularEquipmentSlot(command.DressSlot) {
				return State{}, nil, fmt.Errorf("%w: dress slot %d", ErrInvalidCommand, command.DressSlot)
			}
			var previous *inventory.Stack
			state.Equipment, previous = replaceEquipment(state.Equipment, command.DressSlot, incoming)
			outgoing = previous
		}
		if outgoing != nil && outgoing.Cursed {
			return State{}, nil, fmt.Errorf("%w: instance %d", ErrCursed, outgoing.InstanceID)
		}
		if outgoing != nil {
			returned := *outgoing
			returned.Slot = command.BagSlot
			state.Inventory = append(state.Inventory, returned)
		}
		sort.Slice(state.Inventory, func(left, right int) bool { return state.Inventory[left].Slot < state.Inventory[right].Slot })

		events := equipEvents(actor, command.RequestID, command.DressSlot, outgoing, &incoming, service.catalog)
		state, _, err := advance(state)
		return state, events, err
	})
}

func (service *Service) Unequip(ctx context.Context, actor Actor, command UnequipCommand) (Result, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, err
	}
	return service.update(ctx, actor, command.ExpectedRevision, func(state State) (State, []EquipChanged, error) {
		if command.DressSlot == EquipmentBag {
			return State{}, nil, fmt.Errorf("%w: a bag must be replaced, not removed", ErrUnsupportedAction)
		}
		if !isRegularEquipmentSlot(command.DressSlot) {
			return State{}, nil, fmt.Errorf("%w: dress slot %d", ErrInvalidCommand, command.DressSlot)
		}
		index := equipmentIndex(state.Equipment, command.DressSlot)
		if index < 0 {
			return State{}, nil, fmt.Errorf("%w: equipment slot %d", ErrEmptySlot, command.DressSlot)
		}
		item := state.Equipment[index].Item
		if item.Cursed {
			return State{}, nil, fmt.Errorf("%w: instance %d", ErrCursed, item.InstanceID)
		}
		destination := command.BagSlot
		if destination == -1 {
			destination = firstFreeSlot(state.Inventory, state.Layout.Capacity())
			if destination == -1 {
				return State{}, nil, inventory.ErrBagFull
			}
		} else if err := validateBagSlot(destination, state.Layout); err != nil {
			return State{}, nil, err
		}

		state.Inventory = append([]inventory.Stack(nil), state.Inventory...)
		destinationIndex := stackIndex(state.Inventory, destination)
		if destinationIndex >= 0 {
			stack := state.Inventory[destinationIndex]
			limit, ok := limits{catalog: service.catalog}.StackLimit(item.ItemID)
			if !ok || !sameMutableItem(stack, item) || stack.Count > limit-item.Count {
				return State{}, nil, fmt.Errorf("%w: inventory slot %d", ErrDestinationOccupied, destination)
			}
			state.Inventory[destinationIndex].Count += item.Count
		} else {
			item.Slot = destination
			state.Inventory = append(state.Inventory, item)
			sort.Slice(state.Inventory, func(left, right int) bool { return state.Inventory[left].Slot < state.Inventory[right].Slot })
		}
		state.Equipment = append([]EquippedItem(nil), state.Equipment...)
		state.Equipment = slices.Delete(state.Equipment, index, index+1)
		events := equipEvents(actor, command.RequestID, command.DressSlot, &item, nil, service.catalog)
		state, _, err := advance(state)
		return state, events, err
	})
}

func (service *Service) Use(ctx context.Context, actor Actor, command UseCommand) (Result, *Cooldown, error) {
	if err := validateRequest(actor, command.RequestID, command.ExpectedRevision); err != nil {
		return Result{}, nil, err
	}
	if service.use == nil {
		return Result{}, nil, fmt.Errorf("%w: no item-use authority is installed", ErrUnsupportedAction)
	}
	state, err := service.reader.Read(ctx, actor.CharacterID, command.ExpectedRevision)
	if err != nil {
		return Result{State: state}, nil, err
	}
	item, err := itemAt(state, command.Location)
	if err != nil {
		return Result{State: state}, nil, err
	}
	definition, ok := service.catalog.ItemDefinition(item.ItemID)
	if !ok || definition.Use == nil || definition.Use.ProductActionID == "" {
		return Result{State: state}, nil, fmt.Errorf("%w: %q has no complete native use rule", ErrUnsupportedAction, item.ItemID)
	}
	if !allowsLocation(*definition.Use, command.Location.Kind) {
		return Result{State: state}, nil, fmt.Errorf("%w: %q cannot be used from location %d",
			ErrUnsupportedAction, item.ItemID, command.Location.Kind)
	}
	prepared, err := service.use.PrepareItemAction(ctx, actor.EntityID, item.InstanceID,
		definition.Use.ProductActionID, command.RequestID)
	if err != nil {
		return Result{State: state}, nil, err
	}
	resource := prepared.Resource()
	if resource.Count > 0 {
		committed, err := service.update(ctx, actor, command.ExpectedRevision, func(current State) (State, []EquipChanged, error) {
			currentItem, err := itemAt(current, command.Location)
			if err != nil {
				return State{}, nil, err
			}
			if currentItem.InstanceID != item.InstanceID || currentItem.ItemID != item.ItemID {
				return State{}, nil, itemactionsChangedUnderUse(item, currentItem)
			}
			var removalErr error
			current.Inventory, removalErr = consumeActionResource(current, command.Location, currentItem, resource)
			if removalErr != nil {
				return State{}, nil, removalErr
			}
			return advance(current)
		})
		if err != nil {
			prepared.Abort()
			return committed, nil, err
		}
		state = committed.State
	}
	cooldown := prepared.Commit()
	return Result{State: state}, &cooldown, nil
}

func (service *Service) update(
	ctx context.Context,
	actor Actor,
	expected int64,
	mutate func(State) (State, []EquipChanged, error),
) (Result, error) {
	state, events, err := service.repository.Update(ctx, actor.CharacterID, expected, mutate)
	result := Result{State: state, Events: append([]EquipChanged(nil), events...)}
	if err != nil {
		return result, err
	}
	if service.events != nil {
		for _, event := range events {
			service.events.EnqueueEquipChanged(event)
		}
	}
	return result, nil
}

func validateRequest(actor Actor, requestID uint64, expected int64) error {
	switch {
	case actor.CharacterID == ([16]byte{}):
		return fmt.Errorf("%w: actor has no character id", ErrInvalidCommand)
	case actor.EntityID == 0:
		return fmt.Errorf("%w: actor has no world entity id", ErrInvalidCommand)
	case requestID == 0:
		return fmt.Errorf("%w: request id is zero", ErrInvalidCommand)
	case expected < 0:
		return fmt.Errorf("%w: expected revision %d is negative", ErrInvalidCommand, expected)
	default:
		return nil
	}
}

func advance(state State) (State, []EquipChanged, error) {
	if state.Revision < 0 || state.Revision == math.MaxInt64 {
		return State{}, nil, fmt.Errorf("%w: revision %d cannot advance", ErrInvalidCommand, state.Revision)
	}
	state.Revision++
	return state, nil, nil
}

func allowsLocation(definition UseDefinition, location LocationKind) bool {
	for _, allowed := range definition.AllowedLocations {
		if allowed == location {
			return true
		}
	}
	return false
}

func itemactionsChangedUnderUse(expected, current inventory.Stack) error {
	return fmt.Errorf("%w: item instance changed from %d/%q to %d/%q",
		ErrStaleRevision, expected.InstanceID, expected.ItemID, current.InstanceID, current.ItemID)
}
