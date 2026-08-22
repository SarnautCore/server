package charstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/itemactions"
)

// ItemActionRepository adapts full character persistence to the narrow item
// action transaction. It owns the only mapping between persistence rows and
// the item-action domain.
type ItemActionRepository struct{ repository Repository }

func NewItemActionRepository(repository Repository) (*ItemActionRepository, error) {
	if repository == nil {
		return nil, errors.New("charstore: an item action repository is required")
	}
	return &ItemActionRepository{repository: repository}, nil
}

func (adapter *ItemActionRepository) Read(
	ctx context.Context,
	characterID uuid.UUID,
	expectedRevision int64,
) (itemactions.State, error) {
	state, err := adapter.loadCurrent(ctx, characterID)
	if err != nil {
		return itemactions.State{}, err
	}
	if state.Revision != expectedRevision {
		return state, itemactions.ErrStaleRevision
	}
	return state, nil
}

func (adapter *ItemActionRepository) Update(
	ctx context.Context,
	characterID uuid.UUID,
	expectedRevision int64,
	mutate func(itemactions.State) (itemactions.State, []itemactions.EquipChanged, error),
) (itemactions.State, []itemactions.EquipChanged, error) {
	var current itemactions.State
	var events []itemactions.EquipChanged
	err := adapter.repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		loaded, err := adapter.load(ctx, tx, characterID)
		if err != nil {
			return err
		}
		current = loaded
		if loaded.Revision != expectedRevision {
			return itemactions.ErrStaleRevision
		}
		replacement, emitted, err := mutate(loaded)
		if err != nil {
			return err
		}
		if replacement.Revision != loaded.Revision+1 {
			return fmt.Errorf("item action replacement revision %d does not advance %d once",
				replacement.Revision, loaded.Revision)
		}

		state, err := tx.LoadCharacterState(ctx, characterID)
		if err != nil {
			return err
		}
		state.SaveSeq = replacement.Revision
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			if errors.Is(err, ErrStaleSave) {
				return itemactions.ErrStaleRevision
			}
			return fmt.Errorf("save item action revision: %w", err)
		}
		if err := tx.ReplaceInventory(ctx, characterID, inventory.ToStore(replacement.Inventory)); err != nil {
			return fmt.Errorf("replace item action inventory: %w", err)
		}
		hud, err := tx.LoadCharacterHUD(ctx, characterID)
		if err != nil {
			return err
		}
		applyActionState(&hud, replacement)
		if err := tx.SaveCharacterHUD(ctx, characterID, hud); err != nil {
			return fmt.Errorf("replace item action equipment: %w", err)
		}
		current = replacement
		events = append([]itemactions.EquipChanged(nil), emitted...)
		return nil
	})
	if err == nil {
		return current, events, nil
	}
	if errors.Is(err, itemactions.ErrStaleRevision) {
		latest, loadErr := adapter.loadCurrent(ctx, characterID)
		if loadErr != nil {
			return current, nil, fmt.Errorf("reload stale item action state: %w", loadErr)
		}
		return latest, nil, itemactions.ErrStaleRevision
	}
	return current, nil, err
}

func (adapter *ItemActionRepository) loadCurrent(
	ctx context.Context,
	characterID uuid.UUID,
) (itemactions.State, error) {
	var result itemactions.State
	err := adapter.repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		loaded, err := adapter.load(ctx, tx, characterID)
		if err == nil {
			result = loaded
		}
		return err
	})
	return result, err
}

func (adapter *ItemActionRepository) load(
	ctx context.Context,
	repository Repository,
	characterID uuid.UUID,
) (itemactions.State, error) {
	state, err := repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		return itemactions.State{}, err
	}
	items, err := repository.LoadInventory(ctx, characterID)
	if err != nil {
		return itemactions.State{}, err
	}
	hud, err := repository.LoadCharacterHUD(ctx, characterID)
	if err != nil {
		return itemactions.State{}, err
	}
	return actionState(state.SaveSeq, items, hud)
}

func actionState(
	revision int64,
	items []InventoryItem,
	hud CharacterHUDState,
) (itemactions.State, error) {
	layout := inventory.BagLayout{
		ID:         inventory.BagLayoutID(hud.BagLayout.LayoutID),
		Partitions: make([]int32, len(hud.BagLayout.Partitions)),
	}
	for index, partition := range hud.BagLayout.Partitions {
		if partition.Ordinal != int16(index) {
			return itemactions.State{}, fmt.Errorf("character HUD bag partition %d carries ordinal %d", index, partition.Ordinal)
		}
		layout.Partitions[index] = partition.Capacity
	}
	if err := layout.Validate(); err != nil {
		return itemactions.State{}, err
	}
	result := itemactions.State{
		Revision: revision, Inventory: inventory.FromStore(items), Layout: layout,
		Equipment: make([]itemactions.EquippedItem, len(hud.Equipment)),
	}
	for index, equipped := range hud.Equipment {
		result.Equipment[index] = itemactions.EquippedItem{
			Slot: itemactions.EquipmentSlot(equipped.Slot),
			Item: stackFromInstance(-1, equipped.ItemInstance),
		}
	}
	if hud.Bag != nil {
		bag := stackFromInstance(-1, *hud.Bag)
		result.Bag = &bag
	}
	return result, nil
}

func applyActionState(hud *CharacterHUDState, state itemactions.State) {
	hud.Equipment = make([]EquipmentItem, len(state.Equipment))
	for index, equipped := range state.Equipment {
		hud.Equipment[index] = EquipmentItem{
			Slot:         EquipmentSlot(equipped.Slot),
			ItemInstance: instanceFromStack(equipped.Item),
		}
	}
	if state.Bag == nil {
		hud.Bag = nil
	} else {
		bag := instanceFromStack(*state.Bag)
		hud.Bag = &bag
	}
	hud.BagLayout = ProductBagLayout{
		LayoutID:   string(state.Layout.ID),
		Partitions: make([]BagPartition, len(state.Layout.Partitions)),
	}
	for index, capacity := range state.Layout.Partitions {
		hud.BagLayout.Partitions[index] = BagPartition{Ordinal: int16(index), Capacity: capacity}
	}
}

func stackFromInstance(slot int32, item ItemInstance) inventory.Stack {
	return inventory.Stack{
		Slot: slot, InstanceID: item.InstanceID, ItemID: item.ItemID, Count: item.Quantity,
		CounterValue: item.CounterValue, Bound: item.Bound, Cursed: item.Cursed,
		QuestOperator: item.QuestOperator, RemoveTime: item.RemoveTime,
		RuneResourceID: item.RuneResourceID, RuneSlotResourceID: item.RuneSlotResourceID,
	}
}

func instanceFromStack(stack inventory.Stack) ItemInstance {
	return ItemInstance{
		InstanceID: stack.InstanceID, ItemID: stack.ItemID, Quantity: stack.Count,
		CounterValue: stack.CounterValue, Bound: stack.Bound, Cursed: stack.Cursed,
		QuestOperator: stack.QuestOperator, RemoveTime: stack.RemoveTime,
		RuneResourceID: stack.RuneResourceID, RuneSlotResourceID: stack.RuneSlotResourceID,
	}
}
