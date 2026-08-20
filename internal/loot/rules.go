package loot

import (
	"fmt"

	"github.com/SarnautCore/server/internal/pack"
)

// Rules is every loot input the shard rolls against, read from one content
// pack.
//
// It is a value with no behaviour beyond lookup, so a test can build one from a
// hand-made table and assert the worked example of mechanics/loot.md section
// 6.1 without standing up a zone. Adding a second loot table is a row in the
// pack and nothing here.
type Rules struct {
	tables map[string]pack.LootTable
	items  ItemSource
}

// ItemSource is the pack's lazy item lookup, kept behind an interface so a unit
// test can supply three stack limits instead of a content pack. `*pack.Pack`
// implements it.
type ItemSource interface {
	Item(id string) (pack.Item, bool)
}

// RulesFromPack reads the loot tables of a loaded pack and keeps its item
// lookup.
//
// It validates that every grant in every tree names an item the pack carries.
// The pack compiler checks the same thing, and this is not redundant: the shard
// is what has to refuse a pack built by an older writer, and finding out at
// boot is much better than finding out when a player kills something.
func RulesFromPack(content *pack.Pack) (Rules, error) {
	rules := Rules{tables: make(map[string]pack.LootTable), items: content}
	for _, id := range content.LootTableIDs() {
		table, ok := content.LootTable(id)
		if !ok {
			continue
		}
		if err := checkGrants(content, table.ID, table.Root); err != nil {
			return Rules{}, fmt.Errorf("content pack %s: %w", content.ID(), err)
		}
		rules.tables[table.ID] = table
	}
	return rules, nil
}

// NewRules builds a rule set directly, for a test that has a tree and a stack
// limit table but no pack.
func NewRules(tables []pack.LootTable, items ItemSource) Rules {
	rules := Rules{tables: make(map[string]pack.LootTable, len(tables)), items: items}
	for _, table := range tables {
		rules.tables[table.ID] = table
	}
	return rules
}

// Table returns one loot tree by canonical id.
func (rules Rules) Table(id string) (pack.LootTable, bool) {
	table, ok := rules.tables[id]
	return table, ok
}

// TableCount is how many trees the rules carry.
func (rules Rules) TableCount() int { return len(rules.tables) }

// StackLimit implements `inventory.Limits` against the pack's item table.
func (rules Rules) StackLimit(itemID string) (int32, bool) {
	if rules.items == nil {
		return 0, false
	}
	item, ok := rules.items.Item(itemID)
	if !ok {
		return 0, false
	}
	return item.Stack(), true
}

func checkGrants(content *pack.Pack, tableID string, node pack.LootNode) error {
	if node.Kind == pack.LootNodeSingleItem {
		if _, ok := content.Item(node.ItemID); !ok {
			return fmt.Errorf(
				"loot table %q grants %q, which the pack does not carry as an item",
				tableID, node.ItemID,
			)
		}
		return nil
	}
	for _, entry := range node.Entries {
		if err := checkGrants(content, tableID, entry); err != nil {
			return err
		}
	}
	return nil
}
