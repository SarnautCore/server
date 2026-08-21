package session

import (
	"fmt"
	"slices"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/quests"
)

var retailInventoryLayouts = map[string]sarnautv1.InventoryLayoutId{
	"12":          sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_12,
	"16":          sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_16,
	"12,6":        sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_18,
	"16,8":        sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_24,
	"30":          sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_30,
	"8,8,8,6,6":   sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_36,
	"30,12":       sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_42,
	"12,12,12,12": sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_48,
	"30,12,12":    sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_54,
	"30,30":       sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_60,
}

func characterStateReplacementMessage(
	revision uint64,
	entityID uint64,
	name string,
	level uint32,
	hud *charstore.CharacterHUDState,
) *sarnautv1.ServerMessage {
	replacement := &sarnautv1.CharacterStateReplacement{
		Revision:          revision,
		CharacterEntityId: entityID,
		Name:              name,
		Level:             level,
		Equipment:         make([]*sarnautv1.EquipmentSlotState, 0, len(charstore.RegularEquipmentSlots)),
		Stats:             make([]*sarnautv1.CharacterStatState, 0, int(charstore.StatCount)),
	}
	equipped := make(map[charstore.EquipmentSlot]charstore.ItemInstance)
	if hud != nil {
		for _, item := range hud.Equipment {
			equipped[item.Slot] = item.ItemInstance
		}
		if hud.Bag != nil {
			replacement.Bag = itemStackStateToProto(*hud.Bag)
		}
	}
	for _, slot := range charstore.RegularEquipmentSlots {
		row := &sarnautv1.EquipmentSlotState{Slot: sarnautv1.EquipmentSlotId(slot)}
		if item, ok := equipped[slot]; ok {
			row.Item = itemStackStateToProto(item)
		}
		replacement.Equipment = append(replacement.Equipment, row)
	}
	for ordinal := charstore.StatOrdinal(0); ordinal < charstore.StatCount; ordinal++ {
		row := &sarnautv1.CharacterStatState{Stat: sarnautv1.CharacterStatId(ordinal)}
		if hud != nil {
			stat := hud.Stats[ordinal]
			row.Base = cloneFloat32(stat.Base)
			row.Effective = cloneFloat32(stat.Result)
			row.LongTerm = cloneFloat32(stat.ResultLongTerm)
		}
		replacement.Stats = append(replacement.Stats, row)
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_CharacterStateReplacement{
			CharacterStateReplacement: replacement,
		},
	}
}

func actionBindings(hud *charstore.CharacterHUDState) []combat.ActionBinding {
	if hud == nil {
		return nil
	}
	bindings := make([]combat.ActionBinding, 0, len(hud.Actions))
	for _, slot := range hud.Actions {
		if slot.AbilityID == nil {
			continue
		}
		bindings = append(bindings, combat.ActionBinding{
			SlotIndex: uint32(slot.Ordinal),
			AbilityID: *slot.AbilityID,
		})
	}
	return bindings
}

func inventoryStateReplacementMessage(
	revision uint64,
	currency int64,
	hud *charstore.CharacterHUDState,
	items []charstore.InventoryItem,
) (*sarnautv1.ServerMessage, error) {
	replacement := &sarnautv1.InventoryStateReplacement{
		Revision: revision,
		Currency: currency,
		Slots:    make([]*sarnautv1.InventorySlotState, 0, len(items)),
	}
	if hud != nil {
		layoutID, partitions, err := inventoryLayoutToProto(hud.BagLayout)
		if err != nil {
			return nil, err
		}
		replacement.LayoutId = layoutID
		replacement.PartitionSizes = partitions
		for _, size := range partitions {
			replacement.Capacity += size
		}
		if hud.Bag != nil {
			replacement.EquippedBagItemId = hud.Bag.InstanceID
		}
	}
	for _, item := range items {
		if item.Slot < 0 {
			return nil, fmt.Errorf("inventory slot %d is negative", item.Slot)
		}
		replacement.Slots = append(replacement.Slots, &sarnautv1.InventorySlotState{
			SlotIndex: uint32(item.Slot),
			Item: itemStackStateToProto(charstore.ItemInstance{
				InstanceID:         item.InstanceID,
				ItemID:             item.ItemID,
				Quantity:           item.Quantity,
				CounterValue:       item.CounterValue,
				Bound:              item.Bound,
				Cursed:             item.Cursed,
				QuestOperator:      item.QuestOperator,
				RemoveTime:         item.RemoveTime,
				RuneResourceID:     item.RuneResourceID,
				RuneSlotResourceID: item.RuneSlotResourceID,
			}),
		})
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_InventoryStateReplacement{
			InventoryStateReplacement: replacement,
		},
	}, nil
}

func inventoryLayoutToProto(layout charstore.ProductBagLayout) (
	sarnautv1.InventoryLayoutId,
	[]uint32,
	error,
) {
	parts := make([]uint32, len(layout.Partitions))
	key := ""
	for index, partition := range layout.Partitions {
		if partition.Capacity <= 0 {
			return 0, nil, fmt.Errorf("bag layout %q has partition %d capacity %d", layout.LayoutID, index, partition.Capacity)
		}
		parts[index] = uint32(partition.Capacity)
		if index > 0 {
			key += ","
		}
		key += fmt.Sprint(partition.Capacity)
	}
	id, ok := retailInventoryLayouts[key]
	if !ok {
		return 0, nil, fmt.Errorf("bag layout %q has unsupported partitions %v", layout.LayoutID, parts)
	}
	return id, parts, nil
}

func itemStackStateToProto(item charstore.ItemInstance) *sarnautv1.ItemStackState {
	state := &sarnautv1.ItemStackState{
		InstanceId:                item.InstanceID,
		ProductItemId:             item.ItemID,
		StackCount:                uint32(item.Quantity),
		CounterValue:              item.CounterValue,
		IsBound:                   item.Bound,
		IsCursed:                  item.Cursed,
		IsQuestOperator:           item.QuestOperator,
		RuneProductResourceId:     cloneOptionalString(item.RuneResourceID),
		RuneSlotProductResourceId: cloneOptionalString(item.RuneSlotResourceID),
	}
	if item.RemoveTime != nil {
		state.RemoveTime = *item.RemoveTime
	}
	return state
}

func cloneFloat32(value *float32) *float32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func writeInitialHUD(
	writer *reliableWriter,
	admission Admission,
	entityID uint64,
	loaded charstore.Snapshot,
	binding ZoneBinding,
) error {
	revision, err := hudRevision(loaded.State.SaveSeq)
	if err != nil {
		return err
	}
	messages := []*sarnautv1.ServerMessage{
		characterStateReplacementMessage(
			revision,
			entityID,
			admission.CharacterName,
			characterLevel(loaded.State.Level),
			loaded.HUD,
		),
	}
	inventoryMessage, err := inventoryStateReplacementMessage(
		revision, loaded.State.Currency, loaded.HUD, loaded.Inventory,
	)
	if err != nil {
		return err
	}
	messages = append(messages, inventoryMessage)

	questMessage, err := initialQuestLog(binding.Quests, admission.CharacterID)
	if err != nil {
		return err
	}
	messages = append(messages, questMessage)

	bar := emptyActionBar()
	if binding.Combat != nil {
		bar, err = binding.Combat.ActionBar(entityID)
		if err != nil {
			return err
		}
	}
	actionMessage, err := actionBarReplacementToProto(
		revision, 0, bar, combat.ActionRejectionNone,
	)
	if err != nil {
		return err
	}
	messages = append(messages, actionMessage)
	targetMessage, err := targetStateReplacementToProto(
		1, 0, binding.Combat != nil, 0, combat.RejectionNone,
	)
	if err != nil {
		return err
	}
	messages = append(messages, targetMessage)
	for _, message := range messages {
		if err := writer.write(message); err != nil {
			return err
		}
	}
	return nil
}

func initialQuestLog(module *quests.Module, characterID uuid.UUID) (*sarnautv1.ServerMessage, error) {
	book := quests.HUDQuestBook{}
	progress := make(map[string]quests.HUDQuestProgress)
	if module != nil {
		projected, found, err := module.HUDBook(characterID, quests.HUDBookState{})
		if err != nil {
			return nil, err
		}
		if found {
			book = projected
		}
		for _, row := range book.Quests {
			item, found, err := module.HUDProgress(characterID, row.ID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("quest %q is visible without progress", row.ID)
			}
			progress[row.ID] = item
		}
	}
	return questLogReplacementToProto(1, book, progress)
}

func emptyActionBar() combat.ActionBar {
	var bar combat.ActionBar
	for index := range bar.Slots {
		bar.Slots[index] = combat.ActionSlotState{
			SlotIndex:         uint32(index),
			UnavailableReason: combat.ActionUnavailableEmptySlot,
		}
	}
	return bar
}

func hudRevision(saveSeq int64) (uint64, error) {
	if saveSeq < 0 {
		return 0, fmt.Errorf("character save sequence %d is negative", saveSeq)
	}
	return uint64(saveSeq), nil
}

func inventoryLayoutMatches(
	replacement *sarnautv1.InventoryStateReplacement,
	want ...uint32,
) bool {
	return replacement != nil && slices.Equal(replacement.GetPartitionSizes(), want)
}
