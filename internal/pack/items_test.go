package pack

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

func TestItemBagLayoutContentFieldsPreserveTheContractNumbers(t *testing.T) {
	t.Parallel()

	fields := (&contentv1.Item{}).ProtoReflect().Descriptor().Fields()
	want := map[string]int32{
		"id":                  1,
		"name_key":            2,
		"category":            3,
		"level":               4,
		"required_level":      5,
		"stack_limit":         6,
		"vendor_sell":         7,
		"vendor_buy":          8,
		"description_key":     9,
		"bag_layout_id":       10,
		"bag_capacity":        11,
		"bag_partition_sizes": 12,
		"curse_eligible":      13,
		"extra":               15,
	}
	for name, number := range want {
		field := fields.ByName(protoreflect.Name(name))
		if field == nil {
			t.Errorf("Item.%s is absent from the generated content binding", name)
			continue
		}
		if got := int32(field.Number()); got != number {
			t.Errorf("Item.%s field number = %d, want %d", name, got, number)
		}
	}
}

func TestItemReturnsBakedCurseEligibility(t *testing.T) {
	t.Parallel()

	directory := rewriteItem(t, "item.consumable.harbor-tonic", func(row *contentv1.Item) {
		row.CurseEligible = true
	})
	loaded, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	item, ok := loaded.Item("item.consumable.harbor-tonic")
	if !ok {
		t.Fatal("Item() rejected a row with baked curse eligibility")
	}
	if !item.CurseEligible {
		t.Error("Item().CurseEligible = false, want the baked true value")
	}
}

func TestBagLayoutReaderAdmitsOnlyTheProductCatalog(t *testing.T) {
	t.Parallel()

	want := []BagLayout{
		{ID: "bag.layout.12", Capacity: 12, PartitionSizes: []uint32{12}},
		{ID: "bag.layout.16", Capacity: 16, PartitionSizes: []uint32{16}},
		{ID: "bag.layout.18", Capacity: 18, PartitionSizes: []uint32{12, 6}},
		{ID: "bag.layout.24", Capacity: 24, PartitionSizes: []uint32{16, 8}},
		{ID: "bag.layout.30", Capacity: 30, PartitionSizes: []uint32{30}},
		{ID: "bag.layout.36", Capacity: 36, PartitionSizes: []uint32{8, 8, 8, 6, 6}},
		{ID: "bag.layout.42", Capacity: 42, PartitionSizes: []uint32{30, 12}},
		{ID: "bag.layout.48", Capacity: 48, PartitionSizes: []uint32{12, 12, 12, 12}},
		{ID: "bag.layout.54", Capacity: 54, PartitionSizes: []uint32{30, 12, 12}},
		{ID: "bag.layout.60", Capacity: 60, PartitionSizes: []uint32{30, 30}},
	}
	for _, layout := range want {
		row := &contentv1.Item{
			BagLayoutId:       layout.ID,
			BagCapacity:       layout.Capacity,
			BagPartitionSizes: append([]uint32(nil), layout.PartitionSizes...),
		}
		got, ok := readBagLayout(row)
		if !ok || got == nil || !reflect.DeepEqual(*got, layout) {
			t.Errorf("readBagLayout(%+v) = %+v, %v", row, got, ok)
		}
	}
}

func TestBagLayoutReaderRejectsPartialAndInventedLayouts(t *testing.T) {
	t.Parallel()

	tests := map[string]*contentv1.Item{
		"id only": {
			BagLayoutId: "bag.layout.12",
		},
		"capacity only": {
			BagCapacity: 12,
		},
		"partitions only": {
			BagPartitionSizes: []uint32{12},
		},
		"unknown product id": {
			BagLayoutId: "bag.layout.20", BagCapacity: 20, BagPartitionSizes: []uint32{20},
		},
		"non-product id": {
			BagLayoutId: "layout.12", BagCapacity: 12, BagPartitionSizes: []uint32{12},
		},
		"wrong total": {
			BagLayoutId: "bag.layout.18", BagCapacity: 17, BagPartitionSizes: []uint32{12, 6},
		},
		"wrong partition order": {
			BagLayoutId: "bag.layout.18", BagCapacity: 18, BagPartitionSizes: []uint32{6, 12},
		},
		"invented partitioning": {
			BagLayoutId: "bag.layout.36", BagCapacity: 36, BagPartitionSizes: []uint32{30, 6},
		},
		"zero partition": {
			BagLayoutId: "bag.layout.12", BagCapacity: 12, BagPartitionSizes: []uint32{12, 0},
		},
		"six partitions": {
			BagLayoutId: "bag.layout.60", BagCapacity: 60, BagPartitionSizes: []uint32{10, 10, 10, 10, 10, 10},
		},
		"more than sixty slots": {
			BagLayoutId: "bag.layout.60", BagCapacity: 61, BagPartitionSizes: []uint32{30, 31},
		},
	}
	for name, row := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := readBagLayout(row); ok || got != nil {
				t.Errorf("readBagLayout(%+v) = %+v, %v, want rejection", row, got, ok)
			}
		})
	}
}

func TestItemBagLayoutIsOptionalForExistingFixtureRows(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	item, ok := loaded.Item("item.consumable.harbor-tonic")
	if !ok {
		t.Fatal("Item() rejected a fixture row with no bag-layout fields")
	}
	if item.BagLayout != nil {
		t.Errorf("fixture item BagLayout = %+v, want nil", item.BagLayout)
	}
}

func TestItemReturnsAValidatedNativeBagLayout(t *testing.T) {
	t.Parallel()

	directory := rewriteItem(t, "item.consumable.harbor-tonic", func(row *contentv1.Item) {
		row.BagLayoutId = "bag.layout.36"
		row.BagCapacity = 36
		row.BagPartitionSizes = []uint32{8, 8, 8, 6, 6}
	})
	loaded, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	item, ok := loaded.Item("item.consumable.harbor-tonic")
	if !ok {
		t.Fatal("Item() rejected an admitted bag layout")
	}
	want := BagLayout{ID: "bag.layout.36", Capacity: 36, PartitionSizes: []uint32{8, 8, 8, 6, 6}}
	if item.BagLayout == nil || !reflect.DeepEqual(*item.BagLayout, want) {
		t.Fatalf("Item().BagLayout = %+v, want %+v", item.BagLayout, want)
	}

	item.BagLayout.PartitionSizes[0] = 99
	again, ok := loaded.Item("item.consumable.harbor-tonic")
	if !ok || again.BagLayout == nil || again.BagLayout.PartitionSizes[0] != 8 {
		t.Fatalf("caller mutated a later lookup: %+v, %v", again.BagLayout, ok)
	}
}

func TestMalformedBagLayoutFailsItsLookupWithoutEagerItemDecoding(t *testing.T) {
	t.Parallel()

	directory := rewriteItem(t, "item.consumable.harbor-tonic", func(row *contentv1.Item) {
		row.BagLayoutId = "bag.layout.36"
		row.BagCapacity = 36
		row.BagPartitionSizes = []uint32{30, 6}
	})
	loaded, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() decoded and rejected a lazy item row: %v", err)
	}
	if loaded.ItemCount() != 4 {
		t.Fatalf("ItemCount() = %d, want table-header count 4", loaded.ItemCount())
	}
	if _, ok := loaded.Item("item.consumable.harbor-tonic"); ok {
		t.Error("Item() accepted an invented partitioning")
	}
	if _, ok := loaded.Item("item.junk.brine-scale"); !ok {
		t.Error("a malformed bag row poisoned an unrelated lazy item lookup")
	}
}

func rewriteItem(t *testing.T, id string, mutate func(*contentv1.Item)) string {
	t.Helper()

	directory := copyFixture(t)
	path := filepath.Join(directory, "tables", tableItems+".sptbl")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read item table: %v", err)
	}
	loaded, err := openTable(tableItems, payload)
	if err != nil {
		t.Fatalf("openTable() error = %v", err)
	}

	rows := loaded.rows()
	rewritten := make([][]byte, 0, len(rows))
	found := false
	for _, encoded := range rows {
		var row contentv1.Item
		if err := proto.Unmarshal(encoded, &row); err != nil {
			t.Fatalf("decode item row: %v", err)
		}
		if row.GetId() == id {
			mutate(&row)
			found = true
		}
		bytes, err := proto.Marshal(&row)
		if err != nil {
			t.Fatalf("encode item row: %v", err)
		}
		rewritten = append(rewritten, bytes)
	}
	if !found {
		t.Fatalf("fixture has no item %q", id)
	}
	writeTable(t, path, loaded, rewritten)
	reseal(t, directory)
	return directory
}
