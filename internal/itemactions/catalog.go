package itemactions

import "github.com/SarnautCore/server/internal/inventory"

// Definition is one private native gameplay row. It contains rules only. UI
// names, icons and quality remain in the presentation catalog.
type Definition struct {
	ItemID      string
	StackLimit  int32
	Droppable   bool
	EquipSlots  []EquipmentSlot
	BagLayout   *inventory.BagLayout
	BindOnEquip bool
	TriggerSlot string
	Use         *UseDefinition
	Box         *BoxDefinition
}

type UseDefinition struct {
	ProductActionID  string
	AllowedLocations []LocationKind
}

type BoxDefinition struct {
	// Empty means the box needs no key and KeySlot must be -1.
	KeyItemID  string
	ConsumeKey bool
	Rewards    []inventory.Grant
}

// Catalog resolves private gameplay rules by canonical product item ID.
type Catalog interface {
	ItemDefinition(itemID string) (Definition, bool)
}

type limits struct{ catalog Catalog }

func (source limits) StackLimit(itemID string) (int32, bool) {
	definition, ok := source.catalog.ItemDefinition(itemID)
	if !ok {
		return 0, false
	}
	if definition.StackLimit < 1 {
		return 1, true
	}
	return definition.StackLimit, true
}
