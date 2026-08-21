package inventory

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	// MaxBagPartitions is the largest number of bags the retail multibag lays
	// out at once.
	MaxBagPartitions = 5
	// MaxBagSlots is the largest authored retail multibag capacity.
	MaxBagSlots int32 = 60
)

// BagLayoutID is a stable product identifier. It deliberately does not expose
// the source UI object's sysName.
type BagLayoutID string

const (
	BagLayout12ID BagLayoutID = "bag.layout.12"
	BagLayout16ID BagLayoutID = "bag.layout.16"
	BagLayout18ID BagLayoutID = "bag.layout.18"
	BagLayout24ID BagLayoutID = "bag.layout.24"
	BagLayout30ID BagLayoutID = "bag.layout.30"
	BagLayout36ID BagLayoutID = "bag.layout.36"
	BagLayout42ID BagLayoutID = "bag.layout.42"
	BagLayout48ID BagLayoutID = "bag.layout.48"
	BagLayout54ID BagLayoutID = "bag.layout.54"
	BagLayout60ID BagLayoutID = "bag.layout.60"
)

// BagLayout describes the ordered retail bag partitions that form one
// addressable inventory. Slot indices are contiguous across the partitions.
type BagLayout struct {
	ID         BagLayoutID
	Partitions []int32
}

// Capacity returns the number of addressable slots in the layout.
func (layout BagLayout) Capacity() int32 {
	var capacity int32
	for _, partition := range layout.Partitions {
		capacity += partition
	}
	return capacity
}

var ErrInvalidBagLayout = errors.New("inventory: invalid bag layout")

// Validate rejects layouts outside the authored catalog or beyond the retail
// multibag's limits.
func (layout BagLayout) Validate() error {
	if layout.ID == "" {
		return fmt.Errorf("%w: id is empty", ErrInvalidBagLayout)
	}
	if !strings.HasPrefix(string(layout.ID), "bag.layout.") {
		return fmt.Errorf("%w: id %q is not product-native", ErrInvalidBagLayout, layout.ID)
	}
	if len(layout.Partitions) == 0 || len(layout.Partitions) > MaxBagPartitions {
		return fmt.Errorf(
			"%w: %d partitions, want 1 through %d",
			ErrInvalidBagLayout,
			len(layout.Partitions),
			MaxBagPartitions,
		)
	}
	var capacity int32
	for index, slots := range layout.Partitions {
		if slots <= 0 {
			return fmt.Errorf("%w: partition %d has %d slots", ErrInvalidBagLayout, index, slots)
		}
		if capacity > MaxBagSlots-slots {
			return fmt.Errorf("%w: capacity exceeds %d slots", ErrInvalidBagLayout, MaxBagSlots)
		}
		capacity += slots
	}
	for _, authored := range authoredBagLayouts {
		if layout.ID != authored.ID {
			continue
		}
		if !slices.Equal(layout.Partitions, authored.Partitions) {
			return fmt.Errorf(
				"%w: id %q has partitions %v, want %v",
				ErrInvalidBagLayout,
				layout.ID,
				layout.Partitions,
				authored.Partitions,
			)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown id %q", ErrInvalidBagLayout, layout.ID)
}

var authoredBagLayouts = []BagLayout{
	{ID: BagLayout12ID, Partitions: []int32{12}},
	{ID: BagLayout16ID, Partitions: []int32{16}},
	{ID: BagLayout18ID, Partitions: []int32{12, 6}},
	{ID: BagLayout24ID, Partitions: []int32{16, 8}},
	{ID: BagLayout30ID, Partitions: []int32{30}},
	{ID: BagLayout36ID, Partitions: []int32{8, 8, 8, 6, 6}},
	{ID: BagLayout42ID, Partitions: []int32{30, 12}},
	{ID: BagLayout48ID, Partitions: []int32{12, 12, 12, 12}},
	{ID: BagLayout54ID, Partitions: []int32{30, 12, 12}},
	{ID: BagLayout60ID, Partitions: []int32{30, 30}},
}

// BagLayouts returns the authored layouts in ascending capacity order. The
// returned layouts own their partition slices.
func BagLayouts() []BagLayout {
	layouts := make([]BagLayout, len(authoredBagLayouts))
	for index, layout := range authoredBagLayouts {
		layouts[index] = cloneBagLayout(layout)
	}
	return layouts
}

// BagLayoutByID resolves one stable product layout identity.
func BagLayoutByID(id BagLayoutID) (BagLayout, bool) {
	for _, layout := range authoredBagLayouts {
		if layout.ID == id {
			return cloneBagLayout(layout), true
		}
	}
	return BagLayout{}, false
}

// BagLayoutForCapacity resolves the authored partitioning for a capacity.
func BagLayoutForCapacity(capacity int32) (BagLayout, bool) {
	for _, layout := range authoredBagLayouts {
		if layout.Capacity() == capacity {
			return cloneBagLayout(layout), true
		}
	}
	return BagLayout{}, false
}

func cloneBagLayout(layout BagLayout) BagLayout {
	return BagLayout{ID: layout.ID, Partitions: append([]int32(nil), layout.Partitions...)}
}
