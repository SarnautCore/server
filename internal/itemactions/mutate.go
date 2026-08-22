package itemactions

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/SarnautCore/server/internal/inventory"
)

func transfer(
	state State,
	from,
	to,
	moveNoMore int32,
	catalog Catalog,
) ([]inventory.Stack, error) {
	if err := validateBagSlot(from, state.Layout); err != nil {
		return nil, err
	}
	if err := validateBagSlot(to, state.Layout); err != nil {
		return nil, err
	}
	if from == to {
		return nil, fmt.Errorf("%w: source and destination are both %d", ErrInvalidCommand, from)
	}
	working := append([]inventory.Stack(nil), state.Inventory...)
	sourceIndex := stackIndex(working, from)
	if sourceIndex < 0 {
		return nil, fmt.Errorf("%w: inventory slot %d", ErrEmptySlot, from)
	}
	source := working[sourceIndex]
	amount := source.Count
	if moveNoMore > 0 && moveNoMore < amount {
		amount = moveNoMore
	}
	if amount <= 0 {
		return nil, fmt.Errorf("%w: source count %d", ErrInvalidCommand, source.Count)
	}

	destinationIndex := stackIndex(working, to)
	if destinationIndex < 0 {
		working[sourceIndex].Count -= amount
		moved := source
		moved.Slot = to
		moved.Count = amount
		if amount < source.Count {
			moved.InstanceID = 0
			created := []inventory.Stack{moved}
			if err := assignMissingInstanceIDs(created, state); err != nil {
				return nil, err
			}
			moved = created[0]
		}
		if working[sourceIndex].Count == 0 {
			working = append(working[:sourceIndex], working[sourceIndex+1:]...)
		}
		working = append(working, moved)
		sort.Slice(working, func(left, right int) bool { return working[left].Slot < working[right].Slot })
		return working, nil
	}

	destination := working[destinationIndex]
	if sameMutableItem(source, destination) {
		limit, ok := limits{catalog: catalog}.StackLimit(source.ItemID)
		if !ok {
			return nil, fmt.Errorf("%w: %q", inventory.ErrUnknownItem, source.ItemID)
		}
		room := limit - destination.Count
		if room <= 0 {
			return nil, inventory.ErrStackAtLimit
		}
		if amount > room {
			amount = room
		}
		working[sourceIndex].Count -= amount
		working[destinationIndex].Count += amount
		if working[sourceIndex].Count == 0 {
			working = append(working[:sourceIndex], working[sourceIndex+1:]...)
		}
		return working, nil
	}

	if amount != source.Count {
		return nil, fmt.Errorf("%w: a partial stack cannot swap with occupied slot %d", ErrDestinationOccupied, to)
	}
	working[sourceIndex].Slot = to
	working[destinationIndex].Slot = from
	sort.Slice(working, func(left, right int) bool { return working[left].Slot < working[right].Slot })
	return working, nil
}

func sameMutableItem(left, right inventory.Stack) bool {
	// Slot, identity and count describe the container position and stack. Every
	// other field is item-instance state and must match before two identities
	// can merge. Reflection keeps this rule closed when the persisted mutable
	// instance gains another field.
	left.Slot, right.Slot = 0, 0
	left.InstanceID, right.InstanceID = 0, 0
	left.Count, right.Count = 0, 0
	return reflect.DeepEqual(left, right)
}

func validateBagSlot(slot int32, layout inventory.BagLayout) error {
	if err := layout.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}
	if slot < 0 || slot >= layout.Capacity() {
		return fmt.Errorf("%w: %d outside [0,%d)", ErrSlotOutOfRange, slot, layout.Capacity())
	}
	return nil
}

func stackIndex(stacks []inventory.Stack, slot int32) int {
	for index, stack := range stacks {
		if stack.Slot == slot {
			return index
		}
	}
	return -1
}

func equipmentIndex(equipment []EquippedItem, slot EquipmentSlot) int {
	for index, equipped := range equipment {
		if equipped.Slot == slot {
			return index
		}
	}
	return -1
}

func removeAt(stacks []inventory.Stack, slot, count int32) ([]inventory.Stack, error) {
	index := stackIndex(stacks, slot)
	if index < 0 {
		return nil, fmt.Errorf("%w: inventory slot %d", ErrEmptySlot, slot)
	}
	if count <= 0 || stacks[index].Count < count {
		return nil, fmt.Errorf("%w: remove %d from slot %d with %d", ErrInvalidCommand, count, slot, stacks[index].Count)
	}
	if stacks[index].Count == count {
		return append(stacks[:index], stacks[index+1:]...), nil
	}
	stacks[index].Count -= count
	return stacks, nil
}

func assignMissingInstanceIDs(stacks []inventory.Stack, state State) error {
	var highest uint64
	visit := func(instanceID uint64) {
		if instanceID > highest {
			highest = instanceID
		}
	}
	for _, stack := range state.Inventory {
		visit(stack.InstanceID)
	}
	for _, equipped := range state.Equipment {
		visit(equipped.Item.InstanceID)
	}
	if state.Bag != nil {
		visit(state.Bag.InstanceID)
	}
	for _, stack := range stacks {
		visit(stack.InstanceID)
	}
	for index := range stacks {
		if stacks[index].InstanceID != 0 {
			continue
		}
		if highest == math.MaxUint64 {
			return errorsInstanceIDExhausted
		}
		highest++
		stacks[index].InstanceID = highest
	}
	return nil
}

var errorsInstanceIDExhausted = fmt.Errorf("%w: item instance id space is exhausted", ErrInvalidCommand)

func definitionAllowsSlot(definition Definition, slot EquipmentSlot) bool {
	for _, allowed := range definition.EquipSlots {
		if allowed == slot {
			return true
		}
	}
	return false
}

func replaceEquipment(
	equipment []EquippedItem,
	slot EquipmentSlot,
	item inventory.Stack,
) ([]EquippedItem, *inventory.Stack) {
	result := append([]EquippedItem(nil), equipment...)
	index := equipmentIndex(result, slot)
	if index >= 0 {
		previous := result[index].Item
		result[index].Item = item
		return result, &previous
	}
	result = append(result, EquippedItem{Slot: slot, Item: item})
	sort.Slice(result, func(left, right int) bool { return result[left].Slot < result[right].Slot })
	return result, nil
}

func firstFreeSlot(stacks []inventory.Stack, capacity int32) int32 {
	for slot := int32(0); slot < capacity; slot++ {
		if stackIndex(stacks, slot) < 0 {
			return slot
		}
	}
	return -1
}

func strandedSlot(stacks []inventory.Stack, capacity, replacementSlot int32) bool {
	if replacementSlot < 0 || replacementSlot >= capacity {
		return true
	}
	for _, stack := range stacks {
		if stack.Slot >= capacity {
			return true
		}
	}
	return false
}

func cloneLayout(layout inventory.BagLayout) inventory.BagLayout {
	return inventory.BagLayout{ID: layout.ID, Partitions: append([]int32(nil), layout.Partitions...)}
}

func equipEvents(
	actor Actor,
	requestID uint64,
	slot EquipmentSlot,
	removed,
	added *inventory.Stack,
	catalog Catalog,
) []EquipChanged {
	events := make([]EquipChanged, 0, 2)
	appendEvent := func(item *inventory.Stack, equipped bool) {
		if item == nil {
			return
		}
		definition, ok := catalog.ItemDefinition(item.ItemID)
		if !ok || definition.TriggerSlot == "" {
			return
		}
		events = append(events, EquipChanged{
			RequestID: requestID, CharacterID: actor.CharacterID, EntityID: actor.EntityID, Slot: slot,
			TriggerSlot: definition.TriggerSlot, Equipped: equipped, ItemID: item.ItemID,
		})
	}
	appendEvent(removed, false)
	appendEvent(added, true)
	return events
}

func itemAt(state State, location ItemLocation) (inventory.Stack, error) {
	if !location.HasSlot {
		return inventory.Stack{}, fmt.Errorf("%w: item use has no slot", ErrInvalidCommand)
	}
	switch location.Kind {
	case LocationItemBag:
		if err := validateBagSlot(location.Slot, state.Layout); err != nil {
			return inventory.Stack{}, err
		}
		index := stackIndex(state.Inventory, location.Slot)
		if index < 0 {
			return inventory.Stack{}, fmt.Errorf("%w: inventory slot %d", ErrEmptySlot, location.Slot)
		}
		return state.Inventory[index], nil
	case LocationDress:
		slot := EquipmentSlot(location.Slot)
		if slot == EquipmentBag {
			if state.Bag == nil {
				return inventory.Stack{}, fmt.Errorf("%w: bag equipment slot", ErrEmptySlot)
			}
			return *state.Bag, nil
		}
		index := equipmentIndex(state.Equipment, slot)
		if index < 0 {
			return inventory.Stack{}, fmt.Errorf("%w: equipment slot %d", ErrEmptySlot, slot)
		}
		return state.Equipment[index].Item, nil
	case LocationDeposit:
		return inventory.Stack{}, fmt.Errorf("%w: deposit item use has no admitted server state", ErrUnsupportedAction)
	default:
		return inventory.Stack{}, fmt.Errorf("%w: location type %d", ErrInvalidCommand, location.Kind)
	}
}

func consumeActionResource(
	state State,
	location ItemLocation,
	active inventory.Stack,
	resource ActionResource,
) ([]inventory.Stack, error) {
	working := append([]inventory.Stack(nil), state.Inventory...)
	switch resource.Kind {
	case ActionResourceActiveItem:
		if location.Kind != LocationItemBag {
			return nil, fmt.Errorf("%w: active-item resource outside the item bag", ErrUnsupportedAction)
		}
		return removeAt(working, location.Slot, resource.Count)
	case ActionResourceItem:
		if resource.ItemID == active.ItemID && location.Kind == LocationItemBag {
			return removeAt(working, location.Slot, resource.Count)
		}
		return inventory.Remove(working, []inventory.Grant{{ItemID: resource.ItemID, Count: resource.Count}})
	case ActionResourceNone:
		return working, nil
	default:
		return nil, fmt.Errorf("%w: action resource kind %d", ErrUnsupportedAction, resource.Kind)
	}
}

// insertDefaultRewards keeps instance-local flags meaningful. A new unbound
// box reward may top up another default instance, but it cannot disappear into
// a bound, cursed, quest-operator, timed, or runed instance of the same item.
func insertDefaultRewards(
	stacks []inventory.Stack,
	grants []inventory.Grant,
	catalog Catalog,
	capacity int32,
) ([]inventory.Stack, error) {
	working := make(map[int32]inventory.Stack, len(stacks)+len(grants))
	for _, stack := range stacks {
		if stack.Slot < 0 || stack.Slot >= capacity || stack.Count <= 0 || stack.ItemID == "" {
			return nil, fmt.Errorf("%w: invalid existing stack %+v", ErrInvalidCommand, stack)
		}
		if _, duplicate := working[stack.Slot]; duplicate {
			return nil, fmt.Errorf("%w: duplicate inventory slot %d", ErrInvalidCommand, stack.Slot)
		}
		working[stack.Slot] = stack
	}
	merged := make([]inventory.Grant, 0, len(grants))
	byItem := make(map[string]int, len(grants))
	for _, grant := range grants {
		if grant.Count <= 0 {
			continue
		}
		if index, found := byItem[grant.ItemID]; found {
			if merged[index].Count > math.MaxInt32-grant.Count {
				return nil, fmt.Errorf("%w: reward count for %q overflows int32", ErrInvalidCommand, grant.ItemID)
			}
			merged[index].Count += grant.Count
			continue
		}
		byItem[grant.ItemID] = len(merged)
		merged = append(merged, grant)
	}
	for _, grant := range merged {
		limit, ok := limits{catalog: catalog}.StackLimit(grant.ItemID)
		if !ok {
			return nil, fmt.Errorf("%w: %q", inventory.ErrUnknownItem, grant.ItemID)
		}
		remaining := grant.Count
		for slot := int32(0); slot < capacity && remaining > 0; slot++ {
			stack, occupied := working[slot]
			if !occupied || stack.ItemID != grant.ItemID || !defaultMutable(stack) || stack.Count >= limit {
				continue
			}
			moved := min(remaining, limit-stack.Count)
			stack.Count += moved
			remaining -= moved
			working[slot] = stack
		}
		for slot := int32(0); slot < capacity && remaining > 0; slot++ {
			if _, occupied := working[slot]; occupied {
				continue
			}
			count := min(remaining, limit)
			working[slot] = inventory.Stack{Slot: slot, ItemID: grant.ItemID, Count: count}
			remaining -= count
		}
		if remaining > 0 {
			return nil, inventory.ErrBagFull
		}
	}
	result := make([]inventory.Stack, 0, len(working))
	for slot := int32(0); slot < capacity; slot++ {
		if stack, occupied := working[slot]; occupied {
			result = append(result, stack)
		}
	}
	return result, nil
}

func defaultMutable(stack inventory.Stack) bool {
	return stack.CounterValue == 0 && !stack.Bound && !stack.Cursed && !stack.QuestOperator &&
		stack.RemoveTime == nil && stack.RuneResourceID == nil && stack.RuneSlotResourceID == nil
}
