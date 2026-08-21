package session

import (
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
)

func TestInventoryReplacementUsesTheEquippedProductLayout(t *testing.T) {
	hud := &charstore.CharacterHUDState{
		Bag: &charstore.ItemInstance{InstanceID: 70, ItemID: "item.bag.authored-18", Quantity: 1},
		BagLayout: charstore.ProductBagLayout{
			LayoutID: "bag.layout.18",
			Partitions: []charstore.BagPartition{
				{Ordinal: 0, Capacity: 12},
				{Ordinal: 1, Capacity: 6},
			},
		},
	}
	message, err := inventoryStateReplacementMessage(9, 123, hud, []charstore.InventoryItem{
		{Slot: 17, InstanceID: 71, ItemID: "item.test", Quantity: 2, CounterValue: 4, QuestOperator: true},
	})
	if err != nil {
		t.Fatalf("inventoryStateReplacementMessage() error = %v", err)
	}
	replacement := message.GetInventoryStateReplacement()
	if replacement.GetLayoutId() != sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_18 ||
		replacement.GetCapacity() != 18 ||
		!inventoryLayoutMatches(replacement, 12, 6) {
		t.Fatalf("layout = %s capacity %d partitions %v, want authored 18/[12,6]",
			replacement.GetLayoutId(), replacement.GetCapacity(), replacement.GetPartitionSizes())
	}
	if replacement.GetEquippedBagItemId() != 70 {
		t.Errorf("equipped bag reference = %d, want 70", replacement.GetEquippedBagItemId())
	}
	slot := replacement.GetSlots()[0]
	if slot.GetSlotIndex() != 17 || slot.GetItem().GetInstanceId() != 71 ||
		slot.GetItem().GetCounterValue() != 4 || !slot.GetItem().GetIsQuestOperator() {
		t.Errorf("slot = %+v, want exact persisted item state", slot)
	}
	if slot.GetSpellCooldown() != nil {
		t.Error("inventory replacement invented an item spell cooldown")
	}
}

func TestInventoryReplacementRejectsAnUnknownPartitionShape(t *testing.T) {
	_, err := inventoryStateReplacementMessage(1, 0, &charstore.CharacterHUDState{
		BagLayout: charstore.ProductBagLayout{
			LayoutID:   "bag.layout.not-retail",
			Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 18}},
		},
	}, nil)
	if err == nil {
		t.Fatal("inventoryStateReplacementMessage() accepted invented [18] layout")
	}
}

func TestCharacterReplacementCarriesExactRetailRowsAndAuthoredActions(t *testing.T) {
	base := float32(11)
	stats := charstore.EmptyOrderedStats()
	stats[charstore.StatStrength].Base = &base
	actions := charstore.EmptyOrderedActionSlots()
	ability := "ability.test.authored"
	actions[35].AbilityID = &ability
	hud := &charstore.CharacterHUDState{Stats: stats, Actions: actions}
	replacement := characterStateReplacementMessage(4, 8, "Anne", 3, hud).
		GetCharacterStateReplacement()
	if len(replacement.GetEquipment()) != 20 || len(replacement.GetStats()) != 14 {
		t.Fatalf("equipment/stats counts = %d/%d, want 20/14",
			len(replacement.GetEquipment()), len(replacement.GetStats()))
	}
	if replacement.GetStats()[0].Base == nil || replacement.GetStats()[0].GetBase() != 11 {
		t.Errorf("strength base = %v, want authored 11", replacement.GetStats()[0].Base)
	}
	if replacement.GetStats()[0].Effective != nil || replacement.GetStats()[0].LongTerm != nil {
		t.Error("character replacement manufactured absent computed stat values")
	}
	bindings := actionBindings(hud)
	if len(bindings) != 1 || bindings[0].SlotIndex != 35 || bindings[0].AbilityID != ability {
		t.Fatalf("action bindings = %+v, want only authored slot 35", bindings)
	}
}
