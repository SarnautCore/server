// Package loot turns a corpse into a drop and decides who may take it.
//
// Everything that varies per table, per entry or per item is read from the
// content pack: the tree shape, the chances, the count bounds and the stack
// limit. What is in Go is the evaluation policy of mechanics/loot.md section 5
// — draw order, draw budget, seed derivation, count rounding and the ownership
// window — which the spec marks as curated rather than derived. Adding a second
// loot table or a second item is a content change and nothing else.
//
// The module holds no lock. Every mutation runs inside the zone's, through
// Zone.GameCommand, except the storage write of rule 5.6, which deliberately
// happens outside it: a database round trip must never run under the tick lock.
package loot

import (
	"errors"
	"fmt"
	"math"

	"github.com/SarnautCore/server/internal/pack"
)

// ErrDrawBudget reports a roll that hit MAX_DRAWS_PER_ROLL. A tree inside
// MaxLootTreeDepth cannot reach it with any content the compiler accepts, so it
// is a guard against a future evaluator bug rather than against content.
var ErrDrawBudget = errors.New("loot: roll exceeded the draw budget")

// ItemGrant is one item and the count rolled for it. Grants are in draw order
// and duplicates are deliberately not merged: merging happens at inventory
// insertion, where stack limits are known (rule 5.5.5).
type ItemGrant struct {
	ItemID string
	Count  int32
}

// Drop is the result of one roll: a money amount and a list of grants.
type Drop struct {
	Money int64
	Items []ItemGrant
}

// Empty reports whether there is nothing on the corpse.
func (drop Drop) Empty() bool { return drop.Money == 0 && len(drop.Items) == 0 }

// Clone copies a drop so a caller cannot mutate the corpse's copy through the
// slice it was handed.
func (drop Drop) Clone() Drop {
	return Drop{Money: drop.Money, Items: append([]ItemGrant(nil), drop.Items...)}
}

// Evaluate rolls one tree against one stream, per rules 5.3 to 5.5.
//
// It is a pure function of the tree and the stream. It does not know what a
// corpse is, does not log, and does not look at an item's stack limit: a roll
// produces counts, and turning counts into stacks is rule 5.7's job in
// `internal/inventory`.
func Evaluate(table pack.LootTable, stream Stream) (Drop, error) {
	var drop Drop
	if err := evaluateNode(table.Root, stream, &drop); err != nil {
		return Drop{}, fmt.Errorf("loot table %q: %w", table.ID, err)
	}
	return drop, nil
}

func evaluateNode(node pack.LootNode, stream Stream, drop *Drop) error {
	if stream.Draws() > MaxDrawsPerRoll {
		return ErrDrawBudget
	}
	switch node.Kind {
	case pack.LootNodeAnd:
		return evaluateAnd(node, stream, drop)
	case pack.LootNodeOr:
		return evaluateOr(node, stream, drop)
	case pack.LootNodeSingleItem:
		// A leaf whose bounds are both zero draws, and grants nothing. The
		// draw is what rule 5.5.3 requires; the grant is dropped because
		// section 4 defines a Stack as holding at least one unit, and an
		// ItemGrant of zero would reach inventory with nowhere to go.
		if count := drawCount(node, stream); count > 0 {
			drop.Items = append(drop.Items, ItemGrant{ItemID: node.ItemID, Count: count})
		}
		return nil
	case pack.LootNodeMoney:
		drop.Money += int64(drawCount(node, stream))
		return nil
	case pack.LootNodeUnspecified:
		return fmt.Errorf("loot: node has no kind")
	default:
		return fmt.Errorf("loot: node kind %s has no evaluation rule", node.Kind)
	}
}

// evaluateAnd is rule 5.3. The chances are independent probabilities, one per
// entry, so zero, some or all of them may contribute.
//
// A draw is taken for every entry, including the ones whose chance is 0 or 1.
// That is what makes the draw budget a function of the tree's shape alone: a
// content edit that tightens a chance does not shift every subsequent draw and
// silently change every other drop in the table.
func evaluateAnd(node pack.LootNode, stream Stream, drop *Drop) error {
	for index, entry := range node.Entries {
		if stream.Draws() >= MaxDrawsPerRoll {
			return ErrDrawBudget
		}
		if stream.NextFloat64() >= node.Chances[index] {
			// Rule 5.3.1.3: a skipped subtree consumes no draws.
			continue
		}
		if err := evaluateNode(entry, stream, drop); err != nil {
			return err
		}
	}
	return nil
}

// evaluateOr is rule 5.4. The chances are a distribution and at most one entry
// is selected, on exactly one draw.
//
// A walk that completes without selecting is not an error. It is how a table
// says "nothing dropped" without a null entry, and rule 5.4.3 makes it the
// meaning of a distribution that sums to less than one.
func evaluateOr(node pack.LootNode, stream Stream, drop *Drop) error {
	if stream.Draws() >= MaxDrawsPerRoll {
		return ErrDrawBudget
	}
	value := stream.NextFloat64()
	var cumulative float64
	for index, entry := range node.Entries {
		cumulative += node.Chances[index]
		if value < cumulative {
			return evaluateNode(entry, stream, drop)
		}
	}
	return nil
}

// drawCount is the count formula of rule 5.5.1, shared by both leaf kinds.
//
// The draw happens even when min equals max (rule 5.5.3). The clamp is
// defensive: u < 1.0 makes it unreachable for a correct stream, and it is there
// so a stream implementation that can return 1.0 produces a wrong count rather
// than an out-of-range one.
func drawCount(node pack.LootNode, stream Stream) int32 {
	value := stream.NextFloat64()
	span := int64(node.MaxNumber) - int64(node.MinNumber) + 1
	if span < 1 {
		// The loader rejects max below min, so this is unreachable through a
		// loaded pack. A hand-built node in a test is the only way here.
		return node.MinNumber
	}
	count := int64(node.MinNumber) + int64(math.Floor(value*float64(span)))
	if count > int64(node.MaxNumber) {
		count = int64(node.MaxNumber)
	}
	return int32(count)
}
