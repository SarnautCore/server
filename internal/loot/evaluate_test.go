package loot_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
)

func item(id string, low, high int32) pack.LootNode {
	return pack.LootNode{Kind: pack.LootNodeSingleItem, ItemID: id, MinNumber: low, MaxNumber: high}
}

func money(low, high int32) pack.LootNode {
	return pack.LootNode{Kind: pack.LootNodeMoney, MinNumber: low, MaxNumber: high}
}

func table(id string, root pack.LootNode) pack.LootTable {
	return pack.LootTable{ID: id, Root: root}
}

func itemIDs(drop loot.Drop) []string {
	ids := make([]string, 0, len(drop.Items))
	for _, grant := range drop.Items {
		ids = append(ids, grant.ItemID)
	}
	return ids
}

// TestAndYieldsEveryBranchThatPasses is rule 5.3: the chances are independent
// probabilities, so all three entries can contribute to one drop.
func TestAndYieldsEveryBranchThatPasses(t *testing.T) {
	tree := table("and", pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Chances: []float64{1, 1, 1},
		Entries: []pack.LootNode{item("a", 1, 1), item("b", 1, 1), item("c", 1, 1)},
	})

	// Six draws: three gates and three counts. Every value is below every
	// chance, so nothing is skipped.
	stream := loot.NewScriptedStream(0, 0, 0, 0, 0, 0)
	drop, err := loot.Evaluate(tree, stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if got := strings.Join(itemIDs(drop), ","); got != "a,b,c" {
		t.Errorf("drop items = %q, want every branch", got)
	}
	if stream.Draws() != 6 {
		t.Errorf("stream.Draws() = %d, want 6 (rule 5.3.3: one gate plus one leaf draw each)", stream.Draws())
	}
}

// TestOrSelectsExactlyOneBranch is rule 5.4: at most one entry, on exactly one
// draw, whichever cumulative bucket the value lands in.
func TestOrSelectsExactlyOneBranch(t *testing.T) {
	tree := table("or", pack.LootNode{
		Kind:    pack.LootNodeOr,
		Chances: []float64{0.25, 0.25, 0.50},
		Entries: []pack.LootNode{item("a", 1, 1), item("b", 1, 1), item("c", 1, 1)},
	})

	for _, testCase := range []struct {
		selector float64
		want     string
	}{
		{selector: 0.00, want: "a"},
		{selector: 0.24, want: "a"},
		{selector: 0.25, want: "b"},
		{selector: 0.49, want: "b"},
		{selector: 0.50, want: "c"},
		{selector: 0.99, want: "c"},
	} {
		// Two draws: the selection, then the selected leaf's count.
		stream := loot.NewScriptedStream(testCase.selector, 0)
		drop, err := loot.Evaluate(tree, stream)
		if err != nil {
			t.Fatalf("Evaluate(%v) error = %v", testCase.selector, err)
		}
		if got := strings.Join(itemIDs(drop), ","); got != testCase.want {
			t.Errorf("u = %v selected %q, want exactly %q", testCase.selector, got, testCase.want)
		}
		// Rule 5.4.5: the unselected subtrees consume nothing.
		if stream.Draws() != 2 {
			t.Errorf("u = %v took %d draws, want 2", testCase.selector, stream.Draws())
		}
	}
}

// TestOrThatFallsShortSelectsNothing is rule 5.4.3, the branch section 7.1
// flags as inferred rather than observed. It is the guard, not an error: it is
// how a table says "nothing dropped" without a null entry.
func TestOrThatFallsShortSelectsNothing(t *testing.T) {
	tree := table("or-short", pack.LootNode{
		Kind:    pack.LootNodeOr,
		Chances: []float64{0.10, 0.20},
		Entries: []pack.LootNode{item("a", 1, 1), item("b", 1, 1)},
	})

	stream := loot.NewScriptedStream(0.95)
	drop, err := loot.Evaluate(tree, stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !drop.Empty() {
		t.Errorf("drop = %+v, want nothing", drop)
	}
	if stream.Draws() != 1 {
		t.Errorf("stream.Draws() = %d, want 1: the selection draw and no leaf", stream.Draws())
	}
}

// TestChancesOfZeroAndOne is rule 5.3.2. Both extremes still cost a draw, which
// is what keeps the draw budget a function of the tree's shape: tightening a
// chance in content must not shift every subsequent draw.
func TestChancesOfZeroAndOne(t *testing.T) {
	tree := table("extremes", pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Chances: []float64{1, 0, 1},
		Entries: []pack.LootNode{item("always", 1, 1), item("never", 1, 1), money(5, 5)},
	})

	// The highest value a correct stream can return is just under one, and a
	// chance of one still has to pass at it.
	const almostOne = 0.999999999
	stream := loot.NewScriptedStream(almostOne, 0, almostOne, 0, almostOne, 0)
	drop, err := loot.Evaluate(tree, stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if got := strings.Join(itemIDs(drop), ","); got != "always" {
		t.Errorf("drop items = %q, want only the chance-of-one branch", got)
	}
	if drop.Money != 5 {
		t.Errorf("drop.Money = %d, want 5", drop.Money)
	}
	// Three gates, plus a count for each of the two branches that passed. The
	// zero-chance branch cost its gate draw and nothing else.
	if stream.Draws() != 5 {
		t.Errorf("stream.Draws() = %d, want 5", stream.Draws())
	}
}

// TestALeafDrawsEvenWhenItsBoundsAreEqual is rule 5.5.3. The wasted draw is the
// point: the draw budget depends on the tree's shape alone, so tightening a
// count range in content does not silently change every other drop in the
// table.
func TestALeafDrawsEvenWhenItsBoundsAreEqual(t *testing.T) {
	tree := table("degenerate", pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Chances: []float64{1, 1},
		Entries: []pack.LootNode{item("fixed", 3, 3), money(7, 7)},
	})

	stream := loot.NewScriptedStream(0, 0.99, 0, 0.99)
	drop, err := loot.Evaluate(tree, stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(drop.Items) != 1 || drop.Items[0].Count != 3 {
		t.Errorf("drop.Items = %+v, want one grant of 3", drop.Items)
	}
	if drop.Money != 7 {
		t.Errorf("drop.Money = %d, want 7", drop.Money)
	}
	if stream.Draws() != 4 {
		t.Errorf("stream.Draws() = %d, want 4", stream.Draws())
	}
}

// TestCountFormulaSpansTheWholeRange is rule 5.5.1, at both ends and in the
// middle. The clamp is why the top of the range is reachable at all without
// overshooting it.
func TestCountFormulaSpansTheWholeRange(t *testing.T) {
	tree := table("counts", pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Chances: []float64{1},
		Entries: []pack.LootNode{item("spread", 2, 4)},
	})

	for _, testCase := range []struct {
		draw float64
		want int32
	}{
		{draw: 0.00, want: 2},
		{draw: 0.33, want: 2},
		{draw: 0.34, want: 3},
		{draw: 0.66, want: 3},
		{draw: 0.67, want: 4},
		{draw: 0.999999, want: 4},
	} {
		stream := loot.NewScriptedStream(0, testCase.draw)
		drop, err := loot.Evaluate(tree, stream)
		if err != nil {
			t.Fatalf("Evaluate(%v) error = %v", testCase.draw, err)
		}
		if len(drop.Items) != 1 || drop.Items[0].Count != testCase.want {
			t.Errorf("u = %v gave %+v, want a count of %d", testCase.draw, drop.Items, testCase.want)
		}
	}
}

// TestDuplicateGrantsAreNotMerged is rule 5.5.5. Merging waits for inventory
// insertion, where stack limits are known; merging here would throw away the
// information that two separate entries fired.
func TestDuplicateGrantsAreNotMerged(t *testing.T) {
	tree := table("duplicates", pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Chances: []float64{1, 1},
		Entries: []pack.LootNode{item("same", 1, 1), item("same", 2, 2)},
	})

	drop, err := loot.Evaluate(tree, loot.NewScriptedStream(0, 0, 0, 0))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(drop.Items) != 2 {
		t.Fatalf("drop.Items = %+v, want two separate grants", drop.Items)
	}
	if drop.Items[0].Count != 1 || drop.Items[1].Count != 2 {
		t.Errorf("drop.Items = %+v, want counts 1 then 2 in draw order", drop.Items)
	}
}

// TestTheSameSeedRollsTheSameDropEveryTime is the reproducibility claim rule
// 5.2 exists for, asserted over a hundred rolls rather than two.
func TestTheSameSeedRollsTheSameDropEveryTime(t *testing.T) {
	seed := loot.Seed{
		WorldSeed:         "sarnaut-shard",
		ZoneID:            "paper-harbor",
		SpawnSlotID:       "spawn.paper-harbor.placement.tide-steps.1",
		DeathServerTick:   4242,
		KillerCharacterID: uuid.MustParse("019200f0-0000-7000-8000-00000000f001"),
	}
	tree := nestedFixture()

	first, err := loot.Evaluate(tree, loot.NewStream(seed))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	for run := 0; run < 100; run++ {
		stream := loot.NewStream(seed)
		again, err := loot.Evaluate(tree, stream)
		if err != nil {
			t.Fatalf("run %d: Evaluate() error = %v", run, err)
		}
		if again.Money != first.Money || len(again.Items) != len(first.Items) {
			t.Fatalf("run %d rolled %+v, want %+v", run, again, first)
		}
		for index := range again.Items {
			if again.Items[index] != first.Items[index] {
				t.Fatalf("run %d rolled %+v, want %+v", run, again.Items, first.Items)
			}
		}
	}
}

// TestDifferentSeedsRollDifferentDrops is the other half of the claim. One pair
// of seeds could differ by luck, so this walks a range until it finds a
// difference and fails if the whole range collapses onto one drop — which is
// what a stream that ignored its seed would produce.
func TestDifferentSeedsRollDifferentDrops(t *testing.T) {
	tree := nestedFixture()
	base := loot.Seed{WorldSeed: "sarnaut-shard", ZoneID: "paper-harbor", SpawnSlotID: "slot"}

	seen := make(map[string]bool)
	for tick := uint64(0); tick < 64; tick++ {
		seed := base
		seed.DeathServerTick = tick
		drop, err := loot.Evaluate(tree, loot.NewStream(seed))
		if err != nil {
			t.Fatalf("tick %d: Evaluate() error = %v", tick, err)
		}
		var key strings.Builder
		for _, grant := range drop.Items {
			key.WriteString(grant.ItemID)
			key.WriteByte(':')
		}
		seen[key.String()+":"+string(rune('0'+drop.Money%10))] = true
	}
	if len(seen) < 2 {
		t.Fatalf("64 different seeds produced %d distinct drops; the stream is ignoring its seed", len(seen))
	}
}

// TestDrawBudgetIsEnforced guards the runaway limit of rule 5.2. No tree the
// pack compiler accepts can reach it, so the guard is against a future
// evaluator bug and is asserted against a tree built by hand.
func TestDrawBudgetIsEnforced(t *testing.T) {
	wide := pack.LootNode{Kind: pack.LootNodeAnd}
	for index := 0; index < loot.MaxDrawsPerRoll+10; index++ {
		wide.Entries = append(wide.Entries, money(0, 0))
		wide.Chances = append(wide.Chances, 0)
	}

	if _, err := loot.Evaluate(table("wide", wide), loot.NewScriptedStream(1)); err == nil {
		t.Fatal("Evaluate() error = nil, want the draw budget to stop the roll")
	}
}
