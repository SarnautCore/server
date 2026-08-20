package pack

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

// tableLootTables holds one row per loot tree.
const tableLootTables = "loot-tables"

// MaxLootTreeDepth is mechanics/loot.md section 3's MAX_TREE_DEPTH, counted in
// container levels. The deepest tree in reference data is 2 and the curated
// fixture is 3; the rest is headroom, and the point of the limit is that the
// evaluator's recursion is bounded by something the loader checked rather than
// by the stack.
const MaxLootTreeDepth = 8

// LootNodeKind is one of the four node types a loot tree is made of
// (mechanics/loot.md section 4).
type LootNodeKind uint8

const (
	LootNodeUnspecified LootNodeKind = iota
	// LootNodeAnd's chances are independent per-entry probabilities (rule 5.3).
	LootNodeAnd
	// LootNodeOr's chances are a distribution (rule 5.4).
	LootNodeOr
	// LootNodeSingleItem grants one item in a drawn count (rule 5.5.1).
	LootNodeSingleItem
	// LootNodeMoney credits the purse and occupies no bag slot (rule 5.6.1).
	LootNodeMoney
)

func (kind LootNodeKind) String() string {
	switch kind {
	case LootNodeAnd:
		return "and"
	case LootNodeOr:
		return "or"
	case LootNodeSingleItem:
		return "single-item"
	case LootNodeMoney:
		return "money"
	case LootNodeUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("loot-node-kind(%d)", uint8(kind))
	}
}

// LootNode is one node of a loot tree.
//
// Entries and Chances are positionally paired and the loader guarantees they
// are the same length, so an evaluator may index one by the other without
// checking. Nothing in the authored file links entry i to chance i other than
// ordinal position, which is exactly why the check has to happen somewhere.
type LootNode struct {
	Kind    LootNodeKind
	Entries []LootNode
	Chances []float64

	// ItemID is set on a SingleItem leaf and empty everywhere else.
	ItemID    string
	MinNumber int32
	MaxNumber int32
}

// LootTable is one tree with exactly one root node.
type LootTable struct {
	ID   string
	Root LootNode
	// MaxDepth is the deepest container level in the tree, computed at load and
	// validated against MaxLootTreeDepth.
	MaxDepth uint8
}

// LootTable returns one loot tree by canonical id.
func (p *Pack) LootTable(id string) (LootTable, bool) {
	value, ok := p.lootTables[id]
	return value, ok
}

// LootTableIDs lists every loot tree the pack carries, in canonical-id order.
func (p *Pack) LootTableIDs() []string {
	ids := make([]string, 0, len(p.lootTables))
	for id := range p.lootTables {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// readLootTables decodes and validates every loot tree in the pack.
//
// Unlike items, trees are read eagerly. mechanics/loot.md section 4 marks the
// structural rules as "enforced at load", and a tree that fails one of them is
// a content mistake that should stop a boot rather than surface at the moment a
// player kills something. There are four figures of items and three of trees,
// so the two decisions are not in tension.
func readLootTables(tables map[string]*table) (map[string]LootTable, error) {
	loaded, ok := tables[tableLootTables]
	if !ok {
		// A pack with no loot table is legal. A mob naming one it does not
		// carry is what the loot module refuses, at the point it matters.
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_LOOT_TABLE; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf(
			"%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableLootTables, contentv1.RowType(loaded.rowTypeID), want,
		)
	}

	trees := make(map[string]LootTable, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.LootTable
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode loot table row: %w", ErrMalformedTable, err)
		}
		if row.GetId() == "" {
			return nil, fmt.Errorf("%w: table %q holds a row with no id", ErrMalformedTable, tableLootTables)
		}
		if row.GetRoot() == nil {
			return nil, fmt.Errorf(
				"%w: loot table %q has no root node", ErrMalformedTable, row.GetId(),
			)
		}
		root, depth, err := readLootNode(row.GetId(), "root", row.GetRoot(), 1)
		if err != nil {
			return nil, err
		}
		trees[row.GetId()] = LootTable{ID: row.GetId(), Root: root, MaxDepth: uint8(depth)}
	}
	return trees, nil
}

// readLootNode converts one node and reports the deepest container level under
// it. A leaf occupies no container level, so it reports depth-1: that keeps the
// number comparable with MaxLootTreeDepth, which counts containers only.
func readLootNode(tableID, pointer string, row *contentv1.LootNode, depth int) (LootNode, int, error) {
	fail := func(format string, arguments ...any) (LootNode, int, error) {
		return LootNode{}, 0, fmt.Errorf(
			"%w: loot table %q node %s %s",
			ErrMalformedTable, tableID, pointer, fmt.Sprintf(format, arguments...),
		)
	}

	switch row.GetKind() {
	case contentv1.LootNodeKind_LOOT_NODE_KIND_AND, contentv1.LootNodeKind_LOOT_NODE_KIND_OR:
		if depth > MaxLootTreeDepth {
			return fail("sits at container depth %d, past MAX_TREE_DEPTH %d", depth, MaxLootTreeDepth)
		}
		entries, chances := row.GetEntries(), row.GetChances()
		if len(entries) != len(chances) {
			return fail("has %d entries and %d chances; they are positionally paired", len(entries), len(chances))
		}
		if len(entries) == 0 {
			return fail("has no entries")
		}
		node := LootNode{
			Kind:    lootKind(row.GetKind()),
			Entries: make([]LootNode, 0, len(entries)),
			Chances: append([]float64(nil), chances...),
		}
		deepest := depth
		for index, child := range entries {
			converted, childDepth, err := readLootNode(
				tableID, fmt.Sprintf("%s/entries/%d", pointer, index), child, depth+1,
			)
			if err != nil {
				return LootNode{}, 0, err
			}
			node.Entries = append(node.Entries, converted)
			deepest = max(deepest, childDepth)
		}
		return node, deepest, nil

	case contentv1.LootNodeKind_LOOT_NODE_KIND_SINGLE_ITEM:
		if row.GetItemId() == "" {
			return fail("is a single-item leaf naming no item")
		}
		if row.GetMaxNumber() < row.GetMinNumber() {
			return fail("declares max_number %d below min_number %d", row.GetMaxNumber(), row.GetMinNumber())
		}
		return LootNode{
			Kind:      LootNodeSingleItem,
			ItemID:    row.GetItemId(),
			MinNumber: row.GetMinNumber(),
			MaxNumber: row.GetMaxNumber(),
		}, depth - 1, nil

	case contentv1.LootNodeKind_LOOT_NODE_KIND_MONEY:
		if row.GetItemId() != "" {
			return fail("is a money leaf carrying item %q; money credits the purse", row.GetItemId())
		}
		if row.GetMaxNumber() < row.GetMinNumber() {
			return fail("declares max_number %d below min_number %d", row.GetMaxNumber(), row.GetMinNumber())
		}
		return LootNode{
			Kind:      LootNodeMoney,
			MinNumber: row.GetMinNumber(),
			MaxNumber: row.GetMaxNumber(),
		}, depth - 1, nil

	default:
		return fail("has kind %s, which this reader does not implement", row.GetKind())
	}
}

func lootKind(kind contentv1.LootNodeKind) LootNodeKind {
	if kind == contentv1.LootNodeKind_LOOT_NODE_KIND_OR {
		return LootNodeOr
	}
	return LootNodeAnd
}
