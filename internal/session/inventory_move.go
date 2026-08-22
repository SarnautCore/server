package session

import (
	"context"
	"errors"
	"math"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
)

const inventoryMoveTimeout = 5 * time.Second

func (reader *commandReader) inventoryMove(request *sarnautv1.InventoryMove) error {
	refusal := reader.preflightInventoryMove(request)
	if refusal != sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_NONE {
		return reader.writeInventoryMove(request.GetRequestId(), refusal, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), inventoryMoveTimeout)
	defer cancel()
	result, err := reader.inventoryMoves.Move(
		ctx,
		reader.characterID,
		int64(request.GetExpectedRevision()),
		int32(request.GetFromSlot()),
		int32(request.GetToSlot()),
	)
	if err != nil {
		reader.span.RecordError(err)
		return reader.writeInventoryMove(request.GetRequestId(), inventoryMoveRefusal(err), false)
	}
	current := reader.character.current()
	reader.character.adopt(inventory.ToStore(result.Slots), current.State.Currency, result.SaveSeq)
	reader.actionRevision = uint64(result.SaveSeq)
	return reader.writeInventoryMove(
		request.GetRequestId(),
		sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_NONE,
		true,
	)
}

func (reader *commandReader) preflightInventoryMove(
	request *sarnautv1.InventoryMove,
) sarnautv1.InventoryMoveRefusal {
	if reader.inventoryMoves == nil || reader.character == nil {
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INTERNAL
	}
	if request.GetExpectedRevision() > math.MaxInt64 {
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STALE_REVISION
	}
	current := reader.character.current()
	capacity := int32(0)
	if current.HUD != nil {
		capacity = current.HUD.BagLayout.Capacity()
	}
	if capacity <= 0 || request.GetFromSlot() >= uint32(capacity) {
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_SOURCE
	}
	if request.GetToSlot() >= uint32(capacity) {
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_DESTINATION
	}
	if request.GetFromSlot() == request.GetToSlot() {
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_SAME_SLOT
	}
	return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_NONE
}

func inventoryMoveRefusal(err error) sarnautv1.InventoryMoveRefusal {
	switch {
	case errors.Is(err, inventory.ErrStaleRevision):
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STALE_REVISION
	case errors.Is(err, inventory.ErrEmptySource):
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_EMPTY_SOURCE
	case errors.Is(err, inventory.ErrStackAtLimit):
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_STACK_FULL
	case errors.Is(err, inventory.ErrSlotOutOfRange), errors.Is(err, inventory.ErrInvalidMove):
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INVALID_STATE
	default:
		return sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_INTERNAL
	}
}

func (reader *commandReader) writeInventoryMove(
	requestID uint64,
	refusal sarnautv1.InventoryMoveRefusal,
	committed bool,
) error {
	current := reader.character.current()
	revision, err := hudRevision(current.State.SaveSeq)
	if err != nil {
		return err
	}
	inventoryMessage, err := inventoryStateReplacementMessage(
		revision, current.State.Currency, current.HUD, current.Inventory,
	)
	if err != nil {
		return err
	}
	if err := reader.writer.write(&sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_InventoryMoveResult{
			InventoryMoveResult: &sarnautv1.InventoryMoveResult{
				RequestId:   requestID,
				Refusal:     refusal,
				Replacement: inventoryMessage.GetInventoryStateReplacement(),
			},
		},
	}); err != nil {
		return err
	}
	if err := reader.writer.write(characterStateReplacementMessage(
		revision,
		reader.entityID,
		reader.characterName,
		characterLevel(current.State.Level),
		current.HUD,
	)); err != nil {
		return err
	}
	if committed {
		return reader.writeActionBar(
			0,
			combat.ActionRejectionNone,
			sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_UNSPECIFIED,
		)
	}
	return nil
}
