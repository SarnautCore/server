package session_test

import "github.com/SarnautCore/server/internal/charstore"

func sessionTestHUD(bagInstanceID uint64) charstore.CharacterHUDState {
	return charstore.CharacterHUDState{
		Bag: &charstore.ItemInstance{InstanceID: bagInstanceID, ItemID: "item.bag.fixture", Quantity: 1},
		BagLayout: charstore.ProductBagLayout{
			LayoutID:   "bag.layout.12",
			Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 12}},
		},
		Stats:   charstore.EmptyOrderedStats(),
		Actions: charstore.EmptyOrderedActionSlots(),
	}
}
