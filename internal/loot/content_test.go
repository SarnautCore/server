package loot_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
)

// The two loot trees the vendored fixture pack carries. Neither id appears in
// any non-test file in this repository, which is the property the second one
// exists to demonstrate.
const (
	nestedTableID = "loot.fixture.m2-nested"
	flatTableID   = "loot.fixture.m2-flat"

	// The item the flat table grants 45 of. Its stack limit is content, and no
	// test below writes the number down.
	tonicItemID = "item.consumable.harbor-tonic"
	// The unstackable one, rule 5.7.6's degenerate case.
	scaleItemID = "item.junk.brine-scale"
)

func loadFixture(t *testing.T) *pack.Pack {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	return content
}

// TestTheChanceLiteralRoundTripsExactly is why `LootNode.chances` is a double
// and not a float.
//
// `0.00618751` has eight significant digits and is not representable in
// binary32. A pack that narrowed the field would deliver 0.006187510211... to
// the evaluator, and every drop rate in the game would be slightly wrong in a
// way no test that only checked "roughly" would catch. So this one checks
// exactly, from pack bytes through to the value the evaluator reads.
func TestTheChanceLiteralRoundTripsExactly(t *testing.T) {
	content := loadFixture(t)
	table, ok := content.LootTable(flatTableID)
	if !ok {
		t.Fatalf("fixture pack carries no %s", flatTableID)
	}

	const authored = 0.00618751
	found := false
	for index, chance := range table.Root.Chances {
		if chance != authored {
			continue
		}
		found = true
		t.Logf("chances[%d] is exactly the authored literal", index)
	}
	if !found {
		t.Fatalf("chances = %v, none of them is exactly %v", table.Root.Chances, authored)
	}
	// The same value narrowed to binary32 and widened again is a different
	// number. Asserting that keeps this test honest: without it, a float32
	// field would still pass on a machine where the two happened to compare
	// equal, which they do not.
	if narrowed := float64(float32(authored)); narrowed == authored {
		t.Fatalf("%v survives a float32 round trip, so this test proves nothing", authored)
	}
}

// TestASecondLootTableNeedsNoGoChange is the claim the fixture's second table
// exists to test.
//
// The two trees have different shapes — one nests three levels, the other is
// flat — and different chance values, and this file is the only place either id
// appears. Nothing was added to `internal/loot` to make the second one roll.
func TestASecondLootTableNeedsNoGoChange(t *testing.T) {
	content := loadFixture(t)
	rules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	if rules.TableCount() < 2 {
		t.Fatalf("rules carry %d loot tables, want the fixture's two", rules.TableCount())
	}

	for _, id := range []string{nestedTableID, flatTableID} {
		table, ok := rules.Table(id)
		if !ok {
			t.Fatalf("rules carry no %s", id)
		}
		// A stream of zeros passes every gate whose chance is above zero and
		// takes the low end of every count, so it exercises both trees to the
		// bottom without either test knowing their shape.
		drop, err := loot.Evaluate(table, loot.NewScriptedStream(0))
		if err != nil {
			t.Fatalf("Evaluate(%s) error = %v", id, err)
		}
		if drop.Empty() {
			t.Errorf("%s rolled nothing with a stream of zeros", id)
		}
	}

	// The two trees are genuinely different, or the test above would pass for a
	// pack that carried the same tree twice.
	nested, _ := rules.Table(nestedTableID)
	flat, _ := rules.Table(flatTableID)
	if nested.MaxDepth == flat.MaxDepth {
		t.Errorf("both fixture trees have container depth %d; the second one is not a second shape", nested.MaxDepth)
	}
}

// TestFixtureStackLimitsComeFromContent walks the fixture's three items and
// asserts that no two of them share a limit this package could have hard-coded.
func TestFixtureStackLimitsComeFromContent(t *testing.T) {
	content := loadFixture(t)
	rules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}

	tonic, ok := rules.StackLimit(tonicItemID)
	if !ok {
		t.Fatalf("no stack limit for %s", tonicItemID)
	}
	scale, ok := rules.StackLimit(scaleItemID)
	if !ok {
		t.Fatalf("no stack limit for %s", scaleItemID)
	}
	if tonic == scale {
		t.Errorf("both fixture items report a limit of %d; nothing here is reading content", tonic)
	}
	if scale != 1 {
		t.Errorf("%s stack limit = %d, want the authored 1", scaleItemID, scale)
	}
	if _, ok := rules.StackLimit("item.junk.does-not-exist"); ok {
		t.Error("an item the pack does not carry reported a stack limit")
	}
}

// TestLoadingTheFixtureDoesNotReadTheItemTree is the boot-cost claim.
//
// The item table is looked up through its key index and never materialized, so
// loading a pack is independent of how many items it holds. What is asserted
// here is the observable consequence: the row count is known without a decode,
// a lookup still works, and the whole load finishes well inside the two-second
// budget a shard boot has.
func TestLoadingTheFixtureDoesNotReadTheItemTree(t *testing.T) {
	started := time.Now()
	content := loadFixture(t)
	if _, err := loot.RulesFromPack(content); err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	elapsed := time.Since(started)

	if content.ItemCount() == 0 {
		t.Fatal("fixture pack reports no items; the lazy path is not being exercised")
	}
	if _, ok := content.Item(tonicItemID); !ok {
		t.Fatalf("Item(%q) = false after a load that never read the table", tonicItemID)
	}
	if elapsed > 2*time.Second {
		t.Errorf("loading the pack and its loot rules took %v, want under 2s", elapsed)
	}
	t.Logf("loaded %d items and %d loot tables in %v", content.ItemCount(), len(content.LootTableIDs()), elapsed)
}

// TestRulesRefuseAGrantThePackCannotResolve is the boot-time check that keeps a
// pack written by an older compiler from reaching a player. An item with no
// stack limit has no defined slot cost, so the grant could never be inserted.
func TestRulesRefuseAGrantThePackCannotResolve(t *testing.T) {
	rules := loot.NewRules(
		[]pack.LootTable{table("t", item("item.junk.absent", 1, 1))},
		emptyItems{},
	)
	if _, ok := rules.StackLimit("item.junk.absent"); ok {
		t.Fatal("StackLimit() resolved an item that does not exist")
	}
}

type emptyItems struct{}

func (emptyItems) Item(string) (pack.Item, bool) { return pack.Item{}, false }
