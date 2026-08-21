package inventory_test

import (
	"testing"

	"github.com/SarnautCore/server/internal/inventory"
)

func TestCursedAndUncursedGrantsDoNotShareAStack(t *testing.T) {
	before := []inventory.Stack{{
		Slot:       0,
		InstanceID: 11,
		ItemID:     tonic,
		Count:      5,
		Cursed:     false,
	}}
	placed, err := inventory.Insert(before, []inventory.Grant{{
		ItemID: tonic,
		Count:  2,
		Cursed: true,
	}}, fixtureLimits(), 2)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if len(placed) != 2 {
		t.Fatalf("Insert() stacks = %+v, want separate cursed and uncursed stacks", placed)
	}
	if placed[0].Count != 5 || placed[0].Cursed {
		t.Errorf("original stack = %+v, want unchanged and uncursed", placed[0])
	}
	if placed[1].Count != 2 || !placed[1].Cursed {
		t.Errorf("new stack = %+v, want two cursed units", placed[1])
	}
}
