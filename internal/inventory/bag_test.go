package inventory_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SarnautCore/server/internal/inventory"
)

// limits is the content half of every test here: a stack limit per item and
// nothing else. Every number a test asserts is derived from one of these, so a
// reader that hard-coded a limit fails on the item that does not share it.
type limits map[string]int32

func (table limits) StackLimit(itemID string) (int32, bool) {
	limit, ok := table[itemID]
	return limit, ok
}

const (
	tonic   = "item.consumable.harbor-tonic" // stack limit 20 in the fixture
	feather = "item.junk.clockwork-feather"  // stack limit 10
	scale   = "item.junk.brine-scale"        // stack limit 1
)

func fixtureLimits() limits {
	return limits{tonic: 20, feather: 10, scale: 1}
}

func counts(stacks []inventory.Stack) []int32 {
	result := make([]int32, 0, len(stacks))
	for _, stack := range stacks {
		result = append(result, stack.Count)
	}
	return result
}

func equal(left, right []int32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestWorkedExampleSection62 is the split of mechanics/loot.md section 6.2: 45
// units of a 20-limit item become two full stacks and a remainder of five,
// requiring three free slots.
func TestWorkedExampleSection62(t *testing.T) {
	if got := inventory.Split(45, 20); !equal(got, []int32{20, 20, 5}) {
		t.Errorf("Split(45, 20) = %v, want [20 20 5]", got)
	}

	placed, err := inventory.Insert(nil, []inventory.Grant{{ItemID: tonic, Count: 45}}, fixtureLimits(), 3)
	if err != nil {
		t.Fatalf("Insert() into three free slots error = %v", err)
	}
	if got := counts(placed); !equal(got, []int32{20, 20, 5}) {
		t.Errorf("placed counts = %v, want [20 20 5]", got)
	}
	for index, stack := range placed {
		if stack.Slot != int32(index) {
			t.Errorf("stack %d landed in slot %d, want %d", index, stack.Slot, index)
		}
	}
}

// TestACountOverTheStackLimitSplitsIntoASecondStack is the same rule at its
// smallest: one unit past the limit is two stacks, not one oversized one.
func TestACountOverTheStackLimitSplitsIntoASecondStack(t *testing.T) {
	table := fixtureLimits()
	limit := table[feather]

	placed, err := inventory.Insert(nil, []inventory.Grant{{ItemID: feather, Count: limit + 1}}, table, 4)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if len(placed) != 2 {
		t.Fatalf("placed = %+v, want two stacks", placed)
	}
	if placed[0].Count != limit || placed[1].Count != 1 {
		t.Errorf("placed counts = %v, want [%d 1]", counts(placed), limit)
	}
}

// TestBagFullLeavesEverythingAlone is rule 5.6.3. Nothing is inserted, and the
// caller's slice is untouched: this is the path a corpse survives.
func TestBagFullLeavesEverythingAlone(t *testing.T) {
	before := []inventory.Stack{{Slot: 0, ItemID: feather, Count: 3}}

	placed, err := inventory.Insert(before, []inventory.Grant{{ItemID: tonic, Count: 45}}, fixtureLimits(), 3)
	if !errors.Is(err, inventory.ErrBagFull) {
		t.Fatalf("Insert() error = %v, want ErrBagFull", err)
	}
	if placed != nil {
		t.Errorf("Insert() returned %+v on a refusal, want nothing", placed)
	}
	if len(before) != 1 || before[0].Count != 3 {
		t.Errorf("the caller's slots became %+v; a refusal must change nothing", before)
	}
}

// TestMergingIntoAPartialStackReducesTheSlotRequirement is rule 5.7.5, and the
// second half of worked example 6.2.
//
// With two free slots and an existing stack of twelve, eight units top the
// stack up and the remaining thirty-seven need two slots. An implementation
// that computed the requirement before merging would refuse this insertion and
// tell a player with room that their bag is full.
func TestMergingIntoAPartialStackReducesTheSlotRequirement(t *testing.T) {
	before := []inventory.Stack{{Slot: 0, ItemID: tonic, Count: 12}}

	placed, err := inventory.Insert(before, []inventory.Grant{{ItemID: tonic, Count: 45}}, fixtureLimits(), 3)
	if err != nil {
		t.Fatalf("Insert() error = %v, want the merge to make room", err)
	}
	if got := counts(placed); !equal(got, []int32{20, 20, 17}) {
		t.Errorf("placed counts = %v, want [20 20 17]", got)
	}

	// The same insertion with no partial stack to merge into needs three slots
	// and there are only two left, so it is refused. That contrast is what
	// makes the test above about merging rather than about arithmetic.
	if _, err := inventory.Insert(nil, []inventory.Grant{{ItemID: tonic, Count: 45}}, fixtureLimits(), 2); !errors.Is(err, inventory.ErrBagFull) {
		t.Errorf("Insert() into two empty slots error = %v, want ErrBagFull", err)
	}
}

// TestAStackLimitOfOneDegeneratesCorrectly is rule 5.7.6: one slot per unit and
// no remainder stack.
func TestAStackLimitOfOneDegeneratesCorrectly(t *testing.T) {
	if got := inventory.Split(4, 1); !equal(got, []int32{1, 1, 1, 1}) {
		t.Errorf("Split(4, 1) = %v, want four stacks of one", got)
	}

	placed, err := inventory.Insert(nil, []inventory.Grant{{ItemID: scale, Count: 4}}, fixtureLimits(), 4)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if len(placed) != 4 {
		t.Fatalf("placed = %+v, want four stacks", placed)
	}
	for _, stack := range placed {
		if stack.Count != 1 {
			t.Errorf("slot %d holds %d, want 1", stack.Slot, stack.Count)
		}
	}
	if _, err := inventory.Insert(nil, []inventory.Grant{{ItemID: scale, Count: 5}}, fixtureLimits(), 4); !errors.Is(err, inventory.ErrBagFull) {
		t.Errorf("five unstackable items into four slots error = %v, want ErrBagFull", err)
	}
}

// TestDuplicateGrantsShareStacks is rule 5.5.5's other end. The evaluator
// leaves duplicates alone because it does not know stack limits; here they are
// known, and two grants of the same item must not each open a slot of their
// own.
func TestDuplicateGrantsShareStacks(t *testing.T) {
	grants := []inventory.Grant{
		{ItemID: feather, Count: 6},
		{ItemID: feather, Count: 6},
	}

	placed, err := inventory.Insert(nil, grants, fixtureLimits(), 2)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if got := counts(placed); !equal(got, []int32{10, 2}) {
		t.Errorf("placed counts = %v, want [10 2]: twelve units into a limit of ten", got)
	}
}

// TestFreeSlotsAreFilledInAscendingOrder pins the placement order. Without it
// the result would depend on map iteration and no two runs would agree.
func TestFreeSlotsAreFilledInAscendingOrder(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 0, ItemID: scale, Count: 1},
		{Slot: 2, ItemID: scale, Count: 1},
	}

	placed, err := inventory.Insert(before, []inventory.Grant{{ItemID: feather, Count: 5}}, fixtureLimits(), 5)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	var landed int32 = -1
	for _, stack := range placed {
		if stack.ItemID == feather {
			landed = stack.Slot
		}
	}
	if landed != 1 {
		t.Errorf("the new stack landed in slot %d, want the lowest free slot 1", landed)
	}
}

// TestAnItemThePackDoesNotCarryIsRefused. An item with no stack limit has no
// defined slot cost, so guessing one would be inventing content.
func TestAnItemThePackDoesNotCarryIsRefused(t *testing.T) {
	_, err := inventory.Insert(nil, []inventory.Grant{{ItemID: "item.junk.absent", Count: 1}}, fixtureLimits(), 8)
	if !errors.Is(err, inventory.ErrUnknownItem) {
		t.Fatalf("Insert() error = %v, want ErrUnknownItem", err)
	}
}

// TestSplitMatchesInsertOnAnEmptyBag keeps the two statements of rule 5.7 from
// drifting apart: Split says what a count becomes, Insert places it, and on an
// empty bag they have to agree for every count from one to three full stacks.
func TestSplitMatchesInsertOnAnEmptyBag(t *testing.T) {
	table := fixtureLimits()
	for _, itemID := range []string{tonic, feather, scale} {
		limit := table[itemID]
		for count := int32(1); count <= limit*3+1; count++ {
			placed, err := inventory.Insert(nil, []inventory.Grant{{ItemID: itemID, Count: count}}, table, 64)
			if err != nil {
				t.Fatalf("Insert(%s, %d) error = %v", itemID, count, err)
			}
			if got, want := counts(placed), inventory.Split(count, limit); !equal(got, want) {
				t.Errorf("%s x%d placed %v, Split says %v", itemID, count, got, want)
			}
		}
	}
}

func ExampleSplit() {
	fmt.Println(inventory.Split(45, 20))
	// Output: [20 20 5]
}
