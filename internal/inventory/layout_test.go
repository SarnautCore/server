package inventory_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/inventory"
)

func TestBagLayoutCatalogMatchesRetailPartitions(t *testing.T) {
	want := []inventory.BagLayout{
		{ID: inventory.BagLayout12ID, Partitions: []int32{12}},
		{ID: inventory.BagLayout16ID, Partitions: []int32{16}},
		{ID: inventory.BagLayout18ID, Partitions: []int32{12, 6}},
		{ID: inventory.BagLayout24ID, Partitions: []int32{16, 8}},
		{ID: inventory.BagLayout30ID, Partitions: []int32{30}},
		{ID: inventory.BagLayout36ID, Partitions: []int32{8, 8, 8, 6, 6}},
		{ID: inventory.BagLayout42ID, Partitions: []int32{30, 12}},
		{ID: inventory.BagLayout48ID, Partitions: []int32{12, 12, 12, 12}},
		{ID: inventory.BagLayout54ID, Partitions: []int32{30, 12, 12}},
		{ID: inventory.BagLayout60ID, Partitions: []int32{30, 30}},
	}
	got := inventory.BagLayouts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BagLayouts() = %+v, want %+v", got, want)
	}
	for _, layout := range got {
		if err := layout.Validate(); err != nil {
			t.Errorf("layout %q rejected: %v", layout.ID, err)
		}
		byID, ok := inventory.BagLayoutByID(layout.ID)
		if !ok || !reflect.DeepEqual(byID, layout) {
			t.Errorf("BagLayoutByID(%q) = %+v, %v", layout.ID, byID, ok)
		}
		byCapacity, ok := inventory.BagLayoutForCapacity(layout.Capacity())
		if !ok || !reflect.DeepEqual(byCapacity, layout) {
			t.Errorf("BagLayoutForCapacity(%d) = %+v, %v", layout.Capacity(), byCapacity, ok)
		}
	}
}

func TestBagLayoutIDsAreProductNative(t *testing.T) {
	for _, layout := range inventory.BagLayouts() {
		id := string(layout.ID)
		if !strings.HasPrefix(id, "bag.layout.") {
			t.Errorf("layout id %q lacks product namespace", id)
		}
		for _, sourceToken := range []string{"Size_", "Type_", "Bags_", "/", "\\"} {
			if strings.Contains(id, sourceToken) {
				t.Errorf("layout id %q leaks source token %q", id, sourceToken)
			}
		}
	}
}

func TestBagLayoutCatalogReturnsIndependentValues(t *testing.T) {
	first := inventory.BagLayouts()
	first[0].Partitions[0] = 99
	second := inventory.BagLayouts()
	if second[0].Partitions[0] != 12 {
		t.Fatalf("catalog partition mutated through caller: %+v", second[0])
	}

	byID, _ := inventory.BagLayoutByID(inventory.BagLayout18ID)
	byID.Partitions[0] = 99
	again, _ := inventory.BagLayoutByID(inventory.BagLayout18ID)
	if !reflect.DeepEqual(again.Partitions, []int32{12, 6}) {
		t.Fatalf("lookup partition mutated through caller: %v", again.Partitions)
	}
}

func TestBagLayoutValidationEnforcesRetailLimits(t *testing.T) {
	tests := []inventory.BagLayout{
		{ID: "", Partitions: []int32{12}},
		{ID: "source.Size_12", Partitions: []int32{12}},
		{ID: "bag.layout.none"},
		{ID: "bag.layout.six", Partitions: []int32{1, 1, 1, 1, 1, 1}},
		{ID: "bag.layout.zero", Partitions: []int32{12, 0}},
		{ID: "bag.layout.too-large", Partitions: []int32{30, 31}},
		{ID: "bag.layout.unknown", Partitions: []int32{12}},
		{ID: inventory.BagLayout18ID, Partitions: []int32{18}},
	}
	for _, layout := range tests {
		if err := layout.Validate(); !errors.Is(err, inventory.ErrInvalidBagLayout) {
			t.Errorf("layout %+v error = %v, want ErrInvalidBagLayout", layout, err)
		}
	}
}

func TestDefaultBagLayoutIsCatalogBacked(t *testing.T) {
	layout := inventory.DefaultBagLayout()
	if layout.ID != inventory.DefaultBagLayoutID || layout.Capacity() != 16 {
		t.Fatalf("DefaultBagLayout() = %+v", layout)
	}
}
