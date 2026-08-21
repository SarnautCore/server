package inventory_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/SarnautCore/server/internal/inventory"
)

func moveLayout(t *testing.T) inventory.BagLayout {
	t.Helper()
	layout, ok := inventory.BagLayoutByID(inventory.BagLayout12ID)
	if !ok {
		t.Fatal("12-slot layout absent")
	}
	return layout
}

func TestMoveToEmptySlot(t *testing.T) {
	before := []inventory.Stack{{Slot: 1, InstanceID: 101, ItemID: tonic, Count: 7}}
	got, err := inventory.Move(before, 1, 8, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	want := []inventory.Stack{{Slot: 8, InstanceID: 101, ItemID: tonic, Count: 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Move() = %+v, want %+v", got, want)
	}
	if before[0].Slot != 1 {
		t.Fatalf("Move() mutated input: %+v", before)
	}
}

func TestMoveMergesMatchingStacksToTheirLimit(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 2, InstanceID: 101, ItemID: tonic, Count: 13},
		{Slot: 7, InstanceID: 202, ItemID: tonic, Count: 12},
	}
	got, err := inventory.Move(before, 2, 7, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	want := []inventory.Stack{
		{Slot: 2, InstanceID: 101, ItemID: tonic, Count: 5},
		{Slot: 7, InstanceID: 202, ItemID: tonic, Count: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Move() = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(before, []inventory.Stack{
		{Slot: 2, InstanceID: 101, ItemID: tonic, Count: 13},
		{Slot: 7, InstanceID: 202, ItemID: tonic, Count: 12},
	}) {
		t.Fatalf("Move() mutated input: %+v", before)
	}
}

func TestMoveConsumesSourceWhenMatchingStackHasRoom(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 2, InstanceID: 101, ItemID: tonic, Count: 3},
		{Slot: 7, InstanceID: 202, ItemID: tonic, Count: 12},
	}
	got, err := inventory.Move(before, 2, 7, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	want := []inventory.Stack{{Slot: 7, InstanceID: 202, ItemID: tonic, Count: 15}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Move() = %+v, want %+v", got, want)
	}
}

func TestMoveSwapsDifferentItems(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 2, InstanceID: 101, ItemID: tonic, Count: 3},
		{Slot: 7, InstanceID: 202, ItemID: feather, Count: 9},
	}
	got, err := inventory.Move(before, 2, 7, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	want := []inventory.Stack{
		{Slot: 2, InstanceID: 202, ItemID: feather, Count: 9},
		{Slot: 7, InstanceID: 101, ItemID: tonic, Count: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Move() = %+v, want %+v", got, want)
	}
}

func TestMoveSwapsMatchingProductsWithDifferentCurseState(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 2, InstanceID: 12, ItemID: tonic, Count: 3, Cursed: true},
		{Slot: 7, InstanceID: 19, ItemID: tonic, Count: 9, Cursed: false},
	}
	got, err := inventory.Move(before, 2, 7, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	want := []inventory.Stack{
		{Slot: 2, InstanceID: 19, ItemID: tonic, Count: 9, Cursed: false},
		{Slot: 7, InstanceID: 12, ItemID: tonic, Count: 3, Cursed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Move() = %+v, want curse-preserving swap %+v", got, want)
	}
}

func TestMoveRejectsBadRequestsWithoutMutation(t *testing.T) {
	valid := []inventory.Stack{
		{Slot: 2, ItemID: tonic, Count: 3},
		{Slot: 7, ItemID: feather, Count: 9},
	}
	tests := []struct {
		name   string
		slots  []inventory.Stack
		from   int32
		to     int32
		limits inventory.Limits
		layout inventory.BagLayout
		want   error
	}{
		{name: "empty source", slots: valid, from: 3, to: 4, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrEmptySource},
		{name: "negative source", slots: valid, from: -1, to: 4, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrSlotOutOfRange},
		{name: "large destination", slots: valid, from: 2, to: 12, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrSlotOutOfRange},
		{name: "same slot", slots: valid, from: 2, to: 2, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "nil limits", slots: valid, from: 2, to: 3, layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "bad layout", slots: valid, from: 2, to: 3, limits: fixtureLimits(), layout: inventory.BagLayout{}, want: inventory.ErrInvalidMove},
		{name: "duplicate slot", slots: []inventory.Stack{{Slot: 2, ItemID: tonic, Count: 1}, {Slot: 2, ItemID: feather, Count: 1}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "empty item", slots: []inventory.Stack{{Slot: 2, Count: 1}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "zero count", slots: []inventory.Stack{{Slot: 2, ItemID: tonic}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "over limit", slots: []inventory.Stack{{Slot: 2, ItemID: tonic, Count: 21}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrInvalidMove},
		{name: "unknown item", slots: []inventory.Stack{{Slot: 2, ItemID: "item.absent", Count: 1}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrUnknownItem},
		{name: "occupied slot outside layout", slots: []inventory.Stack{{Slot: 15, ItemID: tonic, Count: 1}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrSlotOutOfRange},
		{name: "full destination", slots: []inventory.Stack{{Slot: 2, ItemID: tonic, Count: 1}, {Slot: 3, ItemID: tonic, Count: 20}}, from: 2, to: 3, limits: fixtureLimits(), layout: moveLayout(t), want: inventory.ErrStackAtLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := append([]inventory.Stack(nil), test.slots...)
			got, err := inventory.Move(test.slots, test.from, test.to, test.limits, test.layout)
			if !errors.Is(err, test.want) {
				t.Fatalf("Move() error = %v, want %v", err, test.want)
			}
			if got != nil {
				t.Errorf("Move() result = %+v on error, want nil", got)
			}
			if !reflect.DeepEqual(test.slots, before) {
				t.Errorf("Move() mutated input from %+v to %+v", before, test.slots)
			}
		})
	}
}

func TestMoveIsDeterministicAndOrdered(t *testing.T) {
	before := []inventory.Stack{
		{Slot: 9, ItemID: feather, Count: 4},
		{Slot: 1, ItemID: tonic, Count: 8},
		{Slot: 5, ItemID: feather, Count: 3},
	}
	want, err := inventory.Move(before, 9, 1, fixtureLimits(), moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	for range 100 {
		got, err := inventory.Move(before, 9, 1, fixtureLimits(), moveLayout(t))
		if err != nil {
			t.Fatalf("Move() error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Move() = %+v, first result %+v", got, want)
		}
		for index := 1; index < len(got); index++ {
			if got[index-1].Slot >= got[index].Slot {
				t.Fatalf("Move() result not ordered: %+v", got)
			}
		}
	}
}

type countingLimits struct {
	limits limits
	calls  map[string]int
}

func (source *countingLimits) StackLimit(itemID string) (int32, bool) {
	source.calls[itemID]++
	limit, ok := source.limits[itemID]
	return limit, ok
}

func TestMoveReadsOneAuthoritativeLimitPerItem(t *testing.T) {
	source := &countingLimits{limits: fixtureLimits(), calls: make(map[string]int)}
	_, err := inventory.Move([]inventory.Stack{
		{Slot: 2, ItemID: tonic, Count: 13},
		{Slot: 7, ItemID: tonic, Count: 12},
	}, 2, 7, source, moveLayout(t))
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	if source.calls[tonic] != 1 {
		t.Fatalf("StackLimit(%q) called %d times, want one authoritative read", tonic, source.calls[tonic])
	}
}
