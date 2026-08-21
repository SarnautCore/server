package loot

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/inventory"
)

// Take keeps the original take-all entry point compatible. New callers should
// use the verb-specific methods so their intent is explicit.
func (module *Module) Take(ctx context.Context, actorEntityID, corpseEntityID uint64) (Result, error) {
	return module.TakeAll(ctx, actorEntityID, corpseEntityID)
}

// TakeAll moves the whole remaining drop on one corpse into one character.
func (module *Module) TakeAll(ctx context.Context, actorEntityID, corpseEntityID uint64) (Result, error) {
	return module.take(ctx, actorEntityID, corpseEntityID, takeSelection{kind: takeAll})
}

// TakeItem moves exactly the item grant at itemIndex in the current ordered
// corpse offer. Indices are zero-based; the retail selector -1 delegates to
// TakeAll. Duplicate item ids remain distinct: the position, not the item id,
// identifies the grant being taken.
func (module *Module) TakeItem(
	ctx context.Context,
	actorEntityID, corpseEntityID uint64,
	itemIndex int32,
) (Result, error) {
	if itemIndex == -1 {
		return module.TakeAll(ctx, actorEntityID, corpseEntityID)
	}
	return module.take(ctx, actorEntityID, corpseEntityID, takeSelection{
		kind:      takeOneItem,
		itemIndex: itemIndex,
	})
}

// TakeMoney credits all money currently on the corpse and leaves its item
// entries in their existing order.
func (module *Module) TakeMoney(ctx context.Context, actorEntityID, corpseEntityID uint64) (Result, error) {
	return module.take(ctx, actorEntityID, corpseEntityID, takeSelection{kind: takeAllMoney})
}

type takeKind uint8

const (
	takeAll takeKind = iota
	takeOneItem
	takeAllMoney
)

type takeSelection struct {
	kind      takeKind
	itemIndex int32
}

// take runs every loot verb through the same reserve, durable award, and
// commit shape.
//
// It runs in three phases, and the shape is forced by two constraints that
// pull against each other. The corpse lives under the zone lock, and the award
// is a database transaction that must never run under it. So: reserve under the
// lock, commit outside it, then clear under the lock again.
//
// The reservation is what makes the two-phase shape safe. Between phases the
// corpse still holds the drop and is marked in flight, so a retransmit is
// refused rather than committing the same drop twice, and a crash in phase two
// leaves the drop exactly where it was. The item is in the corpse or in the
// bag, never in both and never in neither. A partial award reserves the whole
// corpse even though it commits only the selected component; a failed award
// therefore releases the corpse with every component exactly as it was.
func (module *Module) take(
	ctx context.Context,
	actorEntityID, corpseEntityID uint64,
	selection takeSelection,
) (Result, error) {
	var (
		reserved Drop
		owner    uuid.UUID
		refusal  Refusal
	)
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		held, actor := module.corpses[corpseEntityID], module.owners[actorEntityID]
		switch {
		case held == nil:
			refusal = RefusalNoCorpse
		case held.owner != actor:
			refusal = RefusalNotYourLoot
		case held.looted:
			refusal = RefusalAlreadyLooted
		case held.inFlight:
			refusal = RefusalInProgress
		case selection.kind == takeOneItem &&
			(selection.itemIndex < 0 || int(selection.itemIndex) >= len(held.drop.Items)):
			refusal = RefusalInvalidItemIndex
		case selection.kind == takeAllMoney && held.drop.Money == 0:
			// The requested component has already gone. The corpse may still
			// contain items, so Look remains available and reports them.
			refusal = RefusalAlreadyLooted
		default:
			held.inFlight = true
			reserved, owner = selectedDrop(held.drop, selection), held.owner
		}
		return nil
	})
	if refusal != RefusalNone {
		return Result{CorpseEntityID: corpseEntityID, Refusal: refusal}, refusal.err()
	}

	awarded, err := module.awarder.Award(ctx, owner, inventory.Award{
		Money:  reserved.Money,
		Grants: grantsFor(reserved),
	})
	if err != nil {
		module.release(corpseEntityID)
		if errors.Is(err, inventory.ErrBagFull) {
			return Result{CorpseEntityID: corpseEntityID, Refusal: RefusalBagFull}, ErrBagFull
		}
		module.logger.Error("loot award failed",
			"corpse_entity_id", corpseEntityID,
			"character_id", owner.String(),
			"error", err,
		)
		return Result{CorpseEntityID: corpseEntityID, Refusal: RefusalInternal}, err
	}

	// Committed. Only now is the selected component removed. The corpse becomes
	// looted only after its money and ordered item list are both empty.
	module.zoneCommit(corpseEntityID, selection)
	return Result{
		CorpseEntityID: corpseEntityID,
		Refusal:        RefusalNone,
		Money:          reserved.Money,
		Items:          reserved.Items,
		Slots:          awarded.Slots,
		Currency:       awarded.Currency,
		SaveSeq:        awarded.SaveSeq,
	}, nil
}

func selectedDrop(drop Drop, selection takeSelection) Drop {
	switch selection.kind {
	case takeOneItem:
		return Drop{Items: []ItemGrant{drop.Items[selection.itemIndex]}}
	case takeAllMoney:
		return Drop{Money: drop.Money}
	case takeAll:
		return drop.Clone()
	default:
		return Drop{}
	}
}

func (module *Module) release(corpseEntityID uint64) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		if held, ok := module.corpses[corpseEntityID]; ok {
			held.inFlight = false
		}
		return nil
	})
}

func (module *Module) zoneCommit(corpseEntityID uint64, selection takeSelection) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		held, ok := module.corpses[corpseEntityID]
		if !ok {
			// Despawned while the award was in flight. The character keeps what
			// was committed; there is nothing left to empty.
			return nil
		}
		switch selection.kind {
		case takeOneItem:
			index := int(selection.itemIndex)
			// The reservation excludes every competing loot mutation. Keep this
			// defensive check so a future non-loot corpse mutation cannot panic
			// after the durable award has committed.
			if index >= 0 && index < len(held.drop.Items) {
				copy(held.drop.Items[index:], held.drop.Items[index+1:])
				held.drop.Items[len(held.drop.Items)-1] = ItemGrant{}
				held.drop.Items = held.drop.Items[:len(held.drop.Items)-1]
			}
		case takeAllMoney:
			held.drop.Money = 0
		case takeAll:
			held.drop = Drop{}
		}
		held.looted = held.drop.Empty()
		held.inFlight = false
		return nil
	})
}
