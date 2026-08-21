package pack

import (
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

// tableItems holds one row per item definition.
const tableItems = "items"

const (
	maxBagPartitions = 5
	maxBagCapacity   = 60
)

// BagLayout is the product-native description of the ordered inventory
// partitions granted by a bag item. Capacity repeats the partition sum on
// purpose: a compiled row with a missing partition cannot silently shrink a
// player's inventory.
type BagLayout struct {
	ID             string
	Capacity       uint32
	PartitionSizes []uint32
}

var admittedBagLayouts = map[string]BagLayout{
	"bag.layout.12": {ID: "bag.layout.12", Capacity: 12, PartitionSizes: []uint32{12}},
	"bag.layout.16": {ID: "bag.layout.16", Capacity: 16, PartitionSizes: []uint32{16}},
	"bag.layout.18": {ID: "bag.layout.18", Capacity: 18, PartitionSizes: []uint32{12, 6}},
	"bag.layout.24": {ID: "bag.layout.24", Capacity: 24, PartitionSizes: []uint32{16, 8}},
	"bag.layout.30": {ID: "bag.layout.30", Capacity: 30, PartitionSizes: []uint32{30}},
	"bag.layout.36": {ID: "bag.layout.36", Capacity: 36, PartitionSizes: []uint32{8, 8, 8, 6, 6}},
	"bag.layout.42": {ID: "bag.layout.42", Capacity: 42, PartitionSizes: []uint32{30, 12}},
	"bag.layout.48": {ID: "bag.layout.48", Capacity: 48, PartitionSizes: []uint32{12, 12, 12, 12}},
	"bag.layout.54": {ID: "bag.layout.54", Capacity: 54, PartitionSizes: []uint32{30, 12, 12}},
	"bag.layout.60": {ID: "bag.layout.60", Capacity: 60, PartitionSizes: []uint32{30, 30}},
}

// Item is one item definition, as far as the shard is concerned.
//
// It is a small struct on purpose. An item record in the source tree carries
// stats, visuals, a quality reference and a vendor block; what a bag needs is
// the id, the display key and the stack limit, and reading the rest into memory
// for 36,980 items would cost megabytes to serve a field nobody asks for.
type Item struct {
	ID            string
	NameKey       string
	Category      string
	Level         uint32
	RequiredLevel int32
	// StackLimit is the maximum number of units in one stack
	// (mechanics/loot.md rule 5.7.1). It is per-item content, never a
	// constant. See [Item.Stack] for what an absent value means.
	StackLimit int32
	VendorSell int64
	VendorBuy  int64
	// CurseEligible is baked by the gameplay-data compiler. Runtime loot does
	// not carry or reinterpret the source fields used to compute it.
	CurseEligible bool
	// BagLayout is nil for every item that does not grant inventory space.
	// When present, it is one exact entry of the product layout catalog.
	BagLayout *BagLayout
}

// Stack is the stack limit to actually pack with: the authored value, or 1 when
// the record carried none.
//
// The default is stated here rather than in the compiler because it is a
// gameplay decision and it should exist in exactly one place. One is the safe
// direction: an item that is wrongly unstackable wastes bag slots, and an item
// that is wrongly stackable merges two things a player expected to keep apart.
func (item Item) Stack() int32 {
	if item.StackLimit < 1 {
		return 1
	}
	return item.StackLimit
}

// Item resolves one item by canonical id.
//
// It reads the key index and decodes only the rows whose key hash matches, so
// loading a pack never walks the item table and a lookup costs a binary search
// plus one small decode. That is the whole reason items are not held in a map
// like abilities and mobs: there are four figures of them in real content and
// three of them in the fixture, and a shard that materialized all of them at
// boot would spend its startup budget on data no session touches.
//
// A pack carrying no item table answers false for every id.
func (p *Pack) Item(id string) (Item, bool) {
	if p.items == nil || id == "" {
		return Item{}, false
	}
	for _, encoded := range p.items.candidates(id) {
		var row contentv1.Item
		if err := proto.Unmarshal(encoded, &row); err != nil {
			// A row that does not decode cannot be the answer, and the table
			// digest was already checked at load, so this is unreachable
			// short of memory corruption. Skipping keeps the lookup total.
			continue
		}
		if row.GetId() != id {
			// A key-hash collision. Legal, so try the next candidate.
			continue
		}
		bagLayout, ok := readBagLayout(&row)
		if !ok {
			// The item exists, but its compiled layout contract is not one the
			// runtime can represent. Refuse the whole lookup rather than hand a
			// caller a bag with guessed capacity or partition boundaries.
			return Item{}, false
		}
		return Item{
			ID:            row.GetId(),
			NameKey:       row.GetNameKey(),
			Category:      row.GetCategory(),
			Level:         row.GetLevel(),
			RequiredLevel: row.GetRequiredLevel(),
			StackLimit:    row.GetStackLimit(),
			VendorSell:    row.GetVendorSell(),
			VendorBuy:     row.GetVendorBuy(),
			CurseEligible: row.GetCurseEligible(),
			BagLayout:     bagLayout,
		}, true
	}
	return Item{}, false
}

// readBagLayout validates only the row Item already had to decode. Load keeps
// the 36,000-row item table lazy, while a malformed bag row cannot cross the
// lookup boundary.
func readBagLayout(row *contentv1.Item) (*BagLayout, bool) {
	id := row.GetBagLayoutId()
	capacity := row.GetBagCapacity()
	partitions := row.GetBagPartitionSizes()
	if id == "" && capacity == 0 && len(partitions) == 0 {
		return nil, true
	}
	if id == "" || capacity == 0 || len(partitions) == 0 || len(partitions) > maxBagPartitions {
		return nil, false
	}

	var sum uint32
	for _, size := range partitions {
		if size == 0 || size > maxBagCapacity || sum > maxBagCapacity-size {
			return nil, false
		}
		sum += size
	}
	if sum != capacity || capacity > maxBagCapacity {
		return nil, false
	}

	want, ok := admittedBagLayouts[id]
	if !ok || capacity != want.Capacity || !slices.Equal(partitions, want.PartitionSizes) {
		return nil, false
	}
	return &BagLayout{
		ID:             want.ID,
		Capacity:       want.Capacity,
		PartitionSizes: append([]uint32(nil), want.PartitionSizes...),
	}, true
}

// ItemCount is how many item rows the pack carries, read from the table header
// rather than by decoding anything.
//
// It exists so a boot log can say how much content is behind the lazy lookup,
// and so a test can assert that the count is known without the tree having been
// read.
func (p *Pack) ItemCount() int {
	if p.items == nil {
		return 0
	}
	return int(p.items.rowCount)
}

// readItems keeps the table handle without decoding a single row. Every other
// reader in this package materializes its table; this one deliberately does
// not, and the absence of a loop here is the property [Pack.Item] documents.
func readItems(tables map[string]*table) (*table, error) {
	loaded, ok := tables[tableItems]
	if !ok {
		// A pack with no items is legal: a zone-only pack has nothing to drop.
		// The loot reader is what refuses a grant it cannot resolve.
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_ITEM; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf(
			"%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableItems, contentv1.RowType(loaded.rowTypeID), want,
		)
	}
	return loaded, nil
}
