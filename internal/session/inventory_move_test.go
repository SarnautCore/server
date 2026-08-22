package session

import (
	"errors"
	"fmt"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/inventory"
)

func TestInventoryMoveCommitsAgainstTheSessionRevision(t *testing.T) {
	harness := startSession(t, true)
	harness.drainInitialHUD(t)

	harness.writeInventoryMove(t, 41, 2, 0, 7)
	result := harness.readInventoryMoveResult(t, 41)
	if result.GetRefusal() != sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_NONE {
		t.Fatalf("move refusal = %s, want NONE", result.GetRefusal())
	}
	replacement := result.GetReplacement()
	if replacement.GetRevision() != 3 || replacement.GetCapacity() != 16 {
		t.Fatalf("replacement revision/capacity = %d/%d, want 3/16",
			replacement.GetRevision(), replacement.GetCapacity())
	}
	if len(replacement.GetSlots()) != 1 || replacement.GetSlots()[0].GetSlotIndex() != 7 ||
		replacement.GetSlots()[0].GetItem().GetInstanceId() != 1 {
		t.Fatalf("replacement slots = %+v, want instance 1 at slot 7", replacement.GetSlots())
	}
	character := harness.readCharacterReplacementAtRevision(t, 3)
	if character.GetCharacterEntityId() != harness.entityID || character.GetName() != "Anne" {
		t.Fatalf("character replacement = %+v, want the admitted character", character)
	}
	actions := harness.readActionReplacementAtRevision(t, 3)
	if len(actions.GetSlots()) != 36 || actions.GetSlots()[0].GetAbilityId() != "ability.melee.harbor-cleave" {
		t.Fatalf("action replacement = %+v, want authored bindings at revision 3", actions)
	}
	harness.writeReliable(t, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_ActivateAction{
			ActivateAction: &sarnautv1.ActivateAction{
				RequestId:        42,
				SlotIndex:        0,
				ExpectedRevision: 3,
			},
		},
	})
	activation := readActionReplacement(t, harness, 42)
	if activation.GetActivationRefusal() == sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_STALE_REVISION {
		t.Fatalf("action activation still treated committed inventory revision 3 as stale: %+v", activation)
	}

	stored, ok := harness.characters.saved(testAdmission().CharacterID)
	if !ok || stored.State.SaveSeq != 3 || len(stored.Inventory) != 1 || stored.Inventory[0].Slot != 7 {
		t.Fatalf("stored snapshot = %+v, want the move and revision committed together", stored)
	}

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}

func TestInventoryMoveRefusalsReturnTheCurrentFullReplacement(t *testing.T) {
	harness := startSession(t, true)
	harness.drainInitialHUD(t)

	cases := []struct {
		name     string
		request  uint64
		revision uint64
		from     uint32
		to       uint32
		want     sarnautv1.InventoryMoveRefusal
	}{
		{
			name: "same slot", request: 51, revision: 2, from: 0, to: 0,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_SAME_SLOT,
		},
		{
			name: "invalid source", request: 52, revision: 2, from: 16, to: 0,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_SOURCE,
		},
		{
			name: "invalid destination", request: 53, revision: 2, from: 0, to: 16,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_DESTINATION,
		},
		{
			name: "empty source", request: 54, revision: 2, from: 4, to: 5,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_EMPTY_SOURCE,
		},
		{
			name: "stale revision", request: 55, revision: 1, from: 0, to: 7,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STALE_REVISION,
		},
		{
			name: "unrepresentable revision", request: 56, revision: ^uint64(0), from: 0, to: 7,
			want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STALE_REVISION,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			harness.writeInventoryMove(t, test.request, test.revision, test.from, test.to)
			result := harness.readInventoryMoveResult(t, test.request)
			if result.GetRefusal() != test.want {
				t.Fatalf("move refusal = %s, want %s", result.GetRefusal(), test.want)
			}
			replacement := result.GetReplacement()
			if replacement.GetRevision() != 2 || len(replacement.GetSlots()) != 1 ||
				replacement.GetSlots()[0].GetSlotIndex() != 0 {
				t.Fatalf("refusal replacement = %+v, want unchanged full revision 2", replacement)
			}
			harness.readCharacterReplacementAtRevision(t, 2)
		})
	}

	stored, ok := harness.characters.saved(testAdmission().CharacterID)
	if !ok || stored.State.SaveSeq != 2 || len(stored.Inventory) != 1 || stored.Inventory[0].Slot != 0 {
		t.Fatalf("refused moves changed stored snapshot to %+v", stored)
	}

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}

func TestInventoryMoveRefusalMapsDomainFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want sarnautv1.InventoryMoveRefusal
	}{
		{name: "stale", err: inventory.ErrStaleRevision, want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STALE_REVISION},
		{name: "empty", err: inventory.ErrEmptySource, want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_EMPTY_SOURCE},
		{name: "full", err: inventory.ErrStackAtLimit, want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STACK_FULL},
		{name: "range", err: inventory.ErrSlotOutOfRange, want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_STATE},
		{name: "invalid", err: inventory.ErrInvalidMove, want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_STATE},
		{name: "wrapped", err: fmt.Errorf("commit: %w", inventory.ErrEmptySource), want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_EMPTY_SOURCE},
		{name: "internal", err: errors.New("storage unavailable"), want: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INTERNAL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := inventoryMoveRefusal(test.err); got != test.want {
				t.Fatalf("inventoryMoveRefusal(%v) = %s, want %s", test.err, got, test.want)
			}
		})
	}
}

func (harness *sessionHarness) writeInventoryMove(
	t *testing.T,
	requestID,
	expectedRevision uint64,
	from,
	to uint32,
) {
	t.Helper()
	harness.writeReliable(t, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_InventoryMove{
			InventoryMove: &sarnautv1.InventoryMove{
				RequestId:        requestID,
				ExpectedRevision: expectedRevision,
				FromSlot:         from,
				ToSlot:           to,
			},
		},
	})
}

func (harness *sessionHarness) drainInitialHUD(t *testing.T) {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		target := harness.readReliable(t).GetTargetStateReplacement()
		if target != nil && target.GetRequestId() == 0 && target.GetRevision() == 1 {
			return
		}
	}
	t.Fatal("initial HUD replacement sequence did not finish")
}

func (harness *sessionHarness) readInventoryMoveResult(
	t *testing.T,
	requestID uint64,
) *sarnautv1.InventoryMoveResult {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		result := harness.readReliable(t).GetInventoryMoveResult()
		if result != nil && result.GetRequestId() == requestID {
			return result
		}
	}
	t.Fatalf("no inventory move result answered request %d", requestID)
	return nil
}

func (harness *sessionHarness) readCharacterReplacementAtRevision(
	t *testing.T,
	revision uint64,
) *sarnautv1.CharacterStateReplacement {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		replacement := harness.readReliable(t).GetCharacterStateReplacement()
		if replacement != nil && replacement.GetRevision() == revision {
			return replacement
		}
	}
	t.Fatalf("no character replacement arrived at revision %d", revision)
	return nil
}

func (harness *sessionHarness) readActionReplacementAtRevision(
	t *testing.T,
	revision uint64,
) *sarnautv1.ActionBarReplacement {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		replacement := harness.readReliable(t).GetActionBarReplacement()
		if replacement != nil && replacement.GetRevision() == revision {
			return replacement
		}
	}
	t.Fatalf("no action replacement arrived at revision %d", revision)
	return nil
}
