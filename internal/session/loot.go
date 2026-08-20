package session

import (
	"context"
	"fmt"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
)

// This file is the loot half of the translation layer `mapping.go` describes:
// the wire schema on one side, `internal/loot` and `internal/inventory` domain
// values on the other, and nothing else (ADR 0028). It is a file of its own
// rather than four more functions in mapping.go because the loot verbs are the
// only client commands that answer synchronously — the award has to be
// committed to storage before either answer is written (protocol/session.md
// rule 5.7.4) — and that control flow deserves to be readable in one place.

// lootTakeTimeout bounds the storage transaction one take may spend.
//
// The reader goroutine is blocked for its duration, which is the deliberate
// choice: rule 5.6 makes the take all-or-nothing and answers with what was
// committed, so there is nothing useful to do concurrently, and a client that
// sent a second command has it waiting in the QUIC receive buffer. The bound
// exists so a wedged database ends the session instead of the session.
const lootTakeTimeout = 5 * time.Second

// interact answers ClientMessage.interact against a corpse.
//
// A target that is not a corpse is not an error. `Interact` is the generic "use
// the thing I am looking at" verb and quest starters land on it too; a corpse
// refusal is reported as a LootResult so the client learns why, and anything
// else is ignored until the verb that owns it exists.
func (reader *commandReader) interact(request *sarnautv1.Interact) error {
	if reader.loot == nil {
		return nil
	}
	offer, refusal := reader.loot.Look(reader.entityID, request.GetTargetEntityId())
	if refusal == loot.RefusalNoCorpse {
		return nil
	}
	if refusal != loot.RefusalNone {
		return reader.writeLootResult(request.GetTargetEntityId(), refusal, loot.Result{})
	}
	return reader.writer.write(&sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_LootOffer{
			LootOffer: &sarnautv1.LootOffer{
				CorpseEntityId: offer.CorpseEntityID,
				Money:          offer.Money,
				Items:          lootItemsToProto(offer.Items),
			},
		},
	})
}

// lootTake answers ClientMessage.loot_take.
//
// A refusal is not a protocol violation and does not end the session: a full
// bag or somebody else's corpse is ordinary play, and the LootResult carrying
// the reason is the answer the client gets. Only the absence of a loot module
// is worth an error frame, because then the verb can never be served here.
func (reader *commandReader) lootTake(request *sarnautv1.LootTake) error {
	if reader.loot == nil {
		return reader.refuse(
			sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE,
			"this zone hosts no loot module",
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lootTakeTimeout)
	defer cancel()

	result, err := reader.loot.Take(ctx, reader.entityID, request.GetCorpseEntityId())
	if err != nil {
		// Every refusal already carries its reason in result.Refusal, so the
		// error is recorded for an operator and not turned into a frame of its
		// own.
		reader.span.RecordError(err)
	}
	if result.Refusal != loot.RefusalNone {
		return reader.writeLootResult(request.GetCorpseEntityId(), result.Refusal, loot.Result{})
	}

	// The session's own view of the inventory is refreshed before the client is
	// told anything, so the next periodic checkpoint saves the bag the award
	// committed rather than the one the session loaded at zone entry.
	if reader.character != nil {
		reader.character.adopt(inventory.ToStore(result.Slots), result.Currency, result.SaveSeq)
	}
	if err := reader.writeLootResult(request.GetCorpseEntityId(), loot.RefusalNone, result); err != nil {
		return err
	}
	return reader.writer.write(&sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_InventoryUpdate{
			InventoryUpdate: &sarnautv1.InventoryUpdate{
				Slots:    inventorySlotsToProto(result.Slots),
				Currency: result.Currency,
			},
		},
	})
}

func (reader *commandReader) writeLootResult(
	corpseEntityID uint64,
	refusal loot.Refusal,
	result loot.Result,
) error {
	wire, err := lootRefusalToProto(refusal)
	if err != nil {
		// A domain refusal with no wire value is a mapping bug, not a peer
		// problem. It is recorded and reported as the generic internal case so
		// the client still learns the take did not happen.
		reader.span.RecordError(err)
		wire = sarnautv1.LootRefusal_LOOT_REFUSAL_INTERNAL
	}
	return reader.writer.write(&sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_LootResult{
			LootResult: &sarnautv1.LootResult{
				CorpseEntityId: corpseEntityID,
				Refusal:        wire,
				Money:          result.Money,
				Items:          lootItemsToProto(result.Items),
			},
		},
	})
}

func lootItemsToProto(grants []loot.ItemGrant) []*sarnautv1.LootItem {
	items := make([]*sarnautv1.LootItem, 0, len(grants))
	for _, grant := range grants {
		items = append(items, &sarnautv1.LootItem{ItemId: grant.ItemID, Count: grant.Count})
	}
	return items
}

func inventorySlotsToProto(stacks []inventory.Stack) []*sarnautv1.InventorySlot {
	slots := make([]*sarnautv1.InventorySlot, 0, len(stacks))
	for _, stack := range stacks {
		slots = append(slots, &sarnautv1.InventorySlot{
			Slot:   stack.Slot,
			ItemId: stack.ItemID,
			Count:  stack.Count,
		})
	}
	return slots
}

func lootRefusalToProto(refusal loot.Refusal) (sarnautv1.LootRefusal, error) {
	switch refusal {
	case loot.RefusalNone:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_NONE, nil
	case loot.RefusalNoCorpse:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_NO_CORPSE, nil
	case loot.RefusalNotYourLoot:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_NOT_YOUR_LOOT, nil
	case loot.RefusalAlreadyLooted:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_ALREADY_LOOTED, nil
	case loot.RefusalBagFull:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_BAG_FULL, nil
	case loot.RefusalInProgress:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_IN_PROGRESS, nil
	case loot.RefusalInternal:
		return sarnautv1.LootRefusal_LOOT_REFUSAL_INTERNAL, nil
	default:
		return 0, fmt.Errorf("loot refusal %d has no wire value", uint8(refusal))
	}
}
