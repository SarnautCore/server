package charstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/scriptqueue"
)

// InventoryService owns the transaction adapter that persists inventory and
// quest changes. Bag arithmetic remains in inventory; shard.* writes remain
// here, where ADR 0031 assigns them.
type InventoryService struct {
	repository Repository
	limits     inventory.Limits
	slots      int32
}

func NewInventoryService(repository Repository, limits inventory.Limits, slots int32) (*InventoryService, error) {
	if repository == nil {
		return nil, errors.New("charstore: a repository is required")
	}
	if limits == nil {
		return nil, errors.New("charstore: a stack-limit source is required")
	}
	if slots <= 0 {
		slots = inventory.DefaultSlots
	}
	return &InventoryService{repository: repository, limits: limits, slots: slots}, nil
}

func (service *InventoryService) Slots() int32 { return service.slots }

func (service *InventoryService) Award(ctx context.Context, characterID uuid.UUID, award inventory.Award) (inventory.Result, error) {
	var result inventory.Result
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		state, err := tx.LoadCharacterState(ctx, characterID)
		if err != nil {
			return fmt.Errorf("load character state: %w", err)
		}
		stored, err := tx.LoadInventory(ctx, characterID)
		if err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}
		placed, err := inventory.Insert(inventory.FromStore(stored), award.Grants, service.limits, service.slots)
		if err != nil {
			return err
		}
		if err := tx.ReplaceInventory(ctx, characterID, inventory.ToStore(placed)); err != nil {
			return fmt.Errorf("write inventory: %w", err)
		}
		state.Currency += award.Money
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("credit purse: %w", err)
		}
		result = inventory.Result{Slots: placed, Currency: state.Currency, SaveSeq: state.SaveSeq}
		return nil
	})
	if err != nil {
		return inventory.Result{}, err
	}
	return result, nil
}

func (service *InventoryService) GrantQuestReward(ctx context.Context, grant quests.Grant) (quests.GrantResult, error) {
	var result quests.GrantResult
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		state, err := tx.LoadCharacterState(ctx, grant.CharacterID)
		if err != nil {
			return fmt.Errorf("load character state: %w", err)
		}
		stored, err := tx.LoadInventory(ctx, grant.CharacterID)
		if err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}
		emptied, err := inventory.Remove(inventory.FromStore(stored), inventoryGrants(grant.Consume))
		if err != nil {
			return err
		}
		placed, err := inventory.Insert(emptied, inventoryGrants(grant.Grants), service.limits, service.slots)
		if errors.Is(err, inventory.ErrBagFull) {
			return fmt.Errorf("%w: %w", quests.ErrGrantWouldNotFit, err)
		}
		if err != nil {
			return err
		}
		if err := tx.ReplaceInventory(ctx, grant.CharacterID, inventory.ToStore(placed)); err != nil {
			return fmt.Errorf("write inventory: %w", err)
		}
		state.Currency += grant.Money
		state.Experience += grant.Experience
		state.Honor += grant.Honor
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("credit quest rewards: %w", err)
		}
		if err := tx.UpsertQuestState(ctx, grant.CharacterID, grant.Quest); err != nil {
			return fmt.Errorf("write quest state: %w", err)
		}
		deferred := make([]scriptqueue.Work, 0, len(grant.Deferred))
		for _, work := range grant.Deferred {
			row, err := tx.EnqueueDeferredScript(ctx, work)
			if err != nil {
				return fmt.Errorf("write quest script outbox: %w", err)
			}
			deferred = append(deferred, row)
		}
		result = quests.GrantResult{
			Inventory: inventory.ToStore(placed), Currency: state.Currency,
			Experience: state.Experience, Honor: state.Honor, SaveSeq: state.SaveSeq,
			Deferred: deferred,
		}
		return nil
	})
	if err != nil {
		return quests.GrantResult{}, err
	}
	return result, nil
}

func inventoryGrants(counts []quests.ItemCount) []inventory.Grant {
	grants := make([]inventory.Grant, 0, len(counts))
	for _, count := range counts {
		grants = append(grants, inventory.Grant{ItemID: count.ItemID, Count: count.Count})
	}
	return grants
}
