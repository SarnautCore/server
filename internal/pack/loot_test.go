package pack

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

func TestLootTablesCarryTheTreeTheSpecReads(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ids := loaded.LootTableIDs()
	if len(ids) != 2 {
		t.Fatalf("LootTableIDs() = %v, want the fixture's two trees", ids)
	}
	if ids[0] >= ids[1] {
		t.Errorf("LootTableIDs() = %v, want canonical-id order", ids)
	}

	nested, ok := loaded.LootTable("loot.fixture.m2-nested")
	if !ok {
		t.Fatal("the fixture pack carries no loot.fixture.m2-nested")
	}
	// The curated tree of mechanics/loot.md section 6.1: an And root of three
	// entries, whose middle entry is an Or whose second entry is another And.
	if nested.MaxDepth != 3 {
		t.Errorf("MaxDepth = %d, want the section 6.1 container depth of 3", nested.MaxDepth)
	}
	if nested.Root.Kind != LootNodeAnd || len(nested.Root.Entries) != 3 {
		t.Fatalf("root = %+v, want an And of three entries", nested.Root)
	}
	if len(nested.Root.Chances) != len(nested.Root.Entries) {
		t.Fatalf("root pairs %d entries with %d chances", len(nested.Root.Entries), len(nested.Root.Chances))
	}
	if nested.Root.Entries[0].Kind != LootNodeMoney {
		t.Errorf("entries[0] = %s, want money", nested.Root.Entries[0].Kind)
	}
	if nested.Root.Entries[0].ItemID != "" {
		t.Errorf("the money leaf carries item %q; money credits the purse", nested.Root.Entries[0].ItemID)
	}
	inner := nested.Root.Entries[1]
	if inner.Kind != LootNodeOr || len(inner.Entries) != 2 {
		t.Fatalf("entries[1] = %+v, want an Or of two entries", inner)
	}
	if inner.Entries[1].Kind != LootNodeAnd {
		t.Errorf("entries[1]/entries[1] = %s, want the nested And", inner.Entries[1].Kind)
	}

	flat, ok := loaded.LootTable("loot.fixture.m2-flat")
	if !ok {
		t.Fatal("the fixture pack carries no loot.fixture.m2-flat")
	}
	if flat.MaxDepth != 1 {
		t.Errorf("the flat tree's MaxDepth = %d, want 1", flat.MaxDepth)
	}
	// The chance literal survives as a float64, bit for bit.
	if flat.Root.Chances[1] != 0.00618751 {
		t.Errorf("chances[1] = %v, want exactly 0.00618751", flat.Root.Chances[1])
	}

	if _, ok := loaded.LootTable("loot.fixture.absent"); ok {
		t.Error("LootTable() resolved a tree the pack does not carry")
	}
}

func TestItemsAreResolvedByKeyWithoutReadingTheTable(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.ItemCount() != 4 {
		t.Fatalf("ItemCount() = %d, want the fixture's four items", loaded.ItemCount())
	}

	tonic, ok := loaded.Item("item.consumable.harbor-tonic")
	if !ok {
		t.Fatal("Item() did not resolve the harbor tonic")
	}
	if tonic.StackLimit != 20 || tonic.Stack() != 20 {
		t.Errorf("harbor tonic stack limit = %d, want the authored 20", tonic.StackLimit)
	}
	if tonic.NameKey == "" {
		t.Error("the item carries no name key; the client has nothing to display")
	}

	scale, ok := loaded.Item("item.junk.brine-scale")
	if !ok {
		t.Fatal("Item() did not resolve the brine scale")
	}
	if scale.Stack() != 1 {
		t.Errorf("brine scale stack = %d, want the authored 1", scale.Stack())
	}

	if _, ok := loaded.Item("item.junk.absent"); ok {
		t.Error("Item() resolved an id the pack does not carry")
	}
	if _, ok := loaded.Item(""); ok {
		t.Error("Item(\"\") resolved something")
	}
}

// TestAnItemWithNoStackLimitIsUnstackable pins the one default this reader
// applies, and pins it here rather than leaving it to whichever caller divides
// by it first.
func TestAnItemWithNoStackLimitIsUnstackable(t *testing.T) {
	t.Parallel()

	for _, limit := range []int32{-5, 0} {
		if got := (Item{StackLimit: limit}).Stack(); got != 1 {
			t.Errorf("Item{StackLimit: %d}.Stack() = %d, want 1", limit, got)
		}
	}
}

// TestAPackWithNoLootTablesStillLoads. A zone-only pack has nothing to drop,
// and refusing to boot over it would make the loot tables a hard dependency
// they are not.
func TestAPackWithNoLootTablesStillLoads(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	kept := document.Tables[:0]
	for _, entry := range document.Tables {
		if entry.Name != tableLootTables && entry.Name != tableItems {
			kept = append(kept, entry)
		}
	}
	document.Tables = kept
	saveManifest(t, directory, document)
	for _, name := range []string{tableLootTables, tableItems} {
		if err := os.Remove(filepath.Join(directory, "tables", name+".sptbl")); err != nil {
			t.Fatalf("remove %s table: %v", name, err)
		}
	}
	reseal(t, directory)

	loaded, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := loaded.LootTableIDs(); len(got) != 0 {
		t.Errorf("LootTableIDs() = %v, want none", got)
	}
	if got := loaded.ItemCount(); got != 0 {
		t.Errorf("ItemCount() = %d, want 0", got)
	}
	if _, ok := loaded.Item("item.consumable.harbor-tonic"); ok {
		t.Error("Item() resolved something from a pack with no item table")
	}
}

// TestLoadRejectsALootTreeWhoseChancesDoNotPair is the load-time check of
// mechanics/loot.md section 4. Nothing in the authored file links entry i to
// chance i other than ordinal position, so an evaluator that indexed one by the
// other without this check would read past the end of a slice.
func TestLoadRejectsALootTreeWhoseChancesDoNotPair(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*contentv1.LootNode){
		"a chances array one short": func(root *contentv1.LootNode) {
			root.Chances = root.GetChances()[:len(root.GetChances())-1]
		},
		"a container with no entries": func(root *contentv1.LootNode) {
			root.Entries, root.Chances = nil, nil
		},
		"a money leaf carrying an item": func(root *contentv1.LootNode) {
			root.Entries[0].Kind = contentv1.LootNodeKind_LOOT_NODE_KIND_MONEY
			root.Entries[0].ItemId = "item.junk.brine-scale"
		},
		"a leaf whose counts are inverted": func(root *contentv1.LootNode) {
			root.Entries[0].MinNumber, root.Entries[0].MaxNumber = 9, 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := rewriteLootTable(t, mutate)
			_, err := Load(directory, Options{})
			if !errors.Is(err, ErrMalformedTable) {
				t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
			}
		})
	}
}

// rewriteLootTable copies the fixture, applies `mutate` to the first loot
// tree's root, and re-seals the pack so only the tree's shape is wrong.
func rewriteLootTable(t *testing.T, mutate func(*contentv1.LootNode)) string {
	t.Helper()

	directory := copyFixture(t)
	path := filepath.Join(directory, "tables", tableLootTables+".sptbl")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read loot table: %v", err)
	}
	loaded, err := openTable(tableLootTables, payload)
	if err != nil {
		t.Fatalf("openTable() error = %v", err)
	}

	rows := loaded.rows()
	decoded := make([]*contentv1.LootTable, 0, len(rows))
	for _, encoded := range rows {
		var row contentv1.LootTable
		if err := proto.Unmarshal(encoded, &row); err != nil {
			t.Fatalf("decode loot table row: %v", err)
		}
		decoded = append(decoded, &row)
	}
	mutate(decoded[0].GetRoot())

	rewritten := make([][]byte, 0, len(decoded))
	for _, row := range decoded {
		bytes, err := proto.Marshal(row)
		if err != nil {
			t.Fatalf("encode loot table row: %v", err)
		}
		rewritten = append(rewritten, bytes)
	}
	writeTable(t, path, loaded, rewritten)
	reseal(t, directory)
	return directory
}

// writeTable rebuilds one `.sptbl` from rewritten rows.
//
// The row count does not change, so the key index and every region offset in
// the header stay exactly as they were and only the row index, the row data and
// the row-data length move. Rebuilding the whole container instead would risk
// the test failing for a reason that has nothing to do with what it mutated.
func writeTable(t *testing.T, path string, loaded *table, rows [][]byte) {
	t.Helper()
	if uint32(len(rows)) != loaded.rowCount {
		t.Fatalf("rewrote %d rows, want %d", len(rows), loaded.rowCount)
	}

	rebuilt := make([]byte, loaded.rowDataOffset)
	copy(rebuilt, loaded.bytes[:loaded.rowDataOffset])

	var offset uint32
	for index, row := range rows {
		binary.LittleEndian.PutUint32(rebuilt[loaded.rowIndexOffset+uint32(index)*4:], offset)
		offset += uint32(len(row))
	}
	binary.LittleEndian.PutUint32(rebuilt[loaded.rowIndexOffset+loaded.rowCount*4:], offset)
	binary.LittleEndian.PutUint32(rebuilt[28:], offset)
	for _, row := range rows {
		rebuilt = append(rebuilt, row...)
	}

	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatalf("write rewritten table: %v", err)
	}
}
