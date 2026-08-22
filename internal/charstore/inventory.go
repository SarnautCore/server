package charstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
)

// InventoryService owns the transaction adapter that persists inventory and
// quest changes. Bag arithmetic remains in inventory; shard.* writes remain
// here, where ADR 0031 assigns them.
type InventoryService struct {
	repository Repository
	limits     inventory.Limits
	slots      int32
	experience ExperienceResolver
}

const irreversibleSaveAttempts = 8

// ExperienceResolver applies the private cumulative level curve. It is kept
// primitive here to avoid a dependency cycle: progression owns the rules and
// already depends on charstore for its durable transaction.
type ExperienceResolver interface {
	ResolveExperience(level int32, cumulative, gain int64) (nextLevel int32, nextCumulative int64, err error)
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

// BindExperienceResolver installs the authored curve before quest rewards are
// served. A positive-XP reward fails closed when this is absent.
func (service *InventoryService) BindExperienceResolver(resolver ExperienceResolver) {
	service.experience = resolver
}

func (service *InventoryService) Award(ctx context.Context, characterID uuid.UUID, award inventory.Award) (inventory.Result, error) {
	for attempt := 0; attempt < irreversibleSaveAttempts; attempt++ {
		result, err := service.awardOnce(ctx, characterID, award)
		if !errors.Is(err, ErrStaleSave) {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return inventory.Result{}, err
		}
	}
	return inventory.Result{}, fmt.Errorf(
		"award lost %d save-sequence races: %w", irreversibleSaveAttempts, ErrStaleSave,
	)
}

func (service *InventoryService) awardOnce(
	ctx context.Context,
	characterID uuid.UUID,
	award inventory.Award,
) (inventory.Result, error) {
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
	for attempt := 0; attempt < irreversibleSaveAttempts; attempt++ {
		result, err := service.grantQuestRewardOnce(ctx, grant)
		if !errors.Is(err, ErrStaleSave) {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return quests.GrantResult{}, err
		}
	}
	return quests.GrantResult{}, fmt.Errorf(
		"quest reward lost %d save-sequence races: %w", irreversibleSaveAttempts, ErrStaleSave,
	)
}

func (service *InventoryService) grantQuestRewardOnce(
	ctx context.Context,
	grant quests.Grant,
) (quests.GrantResult, error) {
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
		if grant.Experience > 0 {
			if service.experience == nil {
				return errors.New("charstore: quest experience has no authored progression resolver")
			}
			level, cumulative, err := service.experience.ResolveExperience(
				state.Level, state.Experience, grant.Experience,
			)
			if err != nil {
				return fmt.Errorf("resolve quest experience: %w", err)
			}
			state.Level, state.Experience = level, cumulative
		}
		state.Honor += grant.Honor
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("credit quest rewards: %w", err)
		}
		if err := tx.UpsertQuestState(ctx, grant.CharacterID, grant.Quest); err != nil {
			return fmt.Errorf("write quest state: %w", err)
		}
		result = quests.GrantResult{
			Inventory: inventory.ToStore(placed), Currency: state.Currency,
			Level: state.Level, Experience: state.Experience,
			Honor: state.Honor, SaveSeq: state.SaveSeq,
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
