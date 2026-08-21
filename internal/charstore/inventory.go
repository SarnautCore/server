package charstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
)

// InventoryService owns the transaction adapter that persists inventory and
// quest changes. Bag arithmetic remains in inventory; shard.* writes remain
// here, where ADR 0031 assigns them.
type InventoryService struct {
	repository Repository
	limits     inventory.Limits
}

var _ inventory.MoveRepository = (*InventoryService)(nil)

func NewInventoryService(repository Repository, limits inventory.Limits) (*InventoryService, error) {
	if repository == nil {
		return nil, errors.New("charstore: a repository is required")
	}
	if limits == nil {
		return nil, errors.New("charstore: a stack-limit source is required")
	}
	return &InventoryService{repository: repository, limits: limits}, nil
}

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
		hud, layout, err := loadPersistedInventoryHUD(ctx, tx, characterID)
		if err != nil {
			return err
		}
		placed, err := inventory.Insert(inventory.FromStore(stored), award.Grants, service.limits, layout.Capacity())
		if err != nil {
			return err
		}
		if err := assignMissingInstanceIDs(placed, hud); err != nil {
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
		hud, layout, err := loadPersistedInventoryHUD(ctx, tx, grant.CharacterID)
		if err != nil {
			return err
		}
		emptied, err := inventory.Remove(inventory.FromStore(stored), inventoryGrants(grant.Consume))
		if err != nil {
			return err
		}
		placed, err := inventory.Insert(emptied, inventoryGrants(grant.Grants), service.limits, layout.Capacity())
		if errors.Is(err, inventory.ErrBagFull) {
			return fmt.Errorf("%w: %w", quests.ErrGrantWouldNotFit, err)
		}
		if err != nil {
			return err
		}
		if err := assignMissingInstanceIDs(placed, hud); err != nil {
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
		result = quests.GrantResult{
			Inventory: inventory.ToStore(placed), Currency: state.Currency,
			Experience: state.Experience, Honor: state.Honor, SaveSeq: state.SaveSeq,
		}
		return nil
	})
	if err != nil {
		return quests.GrantResult{}, err
	}
	return result, nil
}

// UpdateInventory lets [inventory.MoveService] use the same authoritative
// repository transaction as awards and quest grants.
func (service *InventoryService) UpdateInventory(
	ctx context.Context,
	characterID uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	return service.repository.UpdateInventory(ctx, characterID, update)
}

func (store *memoryStore) UpdateInventory(
	ctx context.Context,
	characterID uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	return updateInventory(ctx, store, characterID, update)
}

func (store *postgresStore) UpdateInventory(
	ctx context.Context,
	characterID uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	return updateInventory(ctx, store, characterID, update)
}

func updateInventory(
	ctx context.Context,
	repository Repository,
	characterID uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	if update == nil {
		return inventory.MoveState{}, errors.New("charstore: inventory update callback is required")
	}
	var committed inventory.MoveState
	err := repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		state, err := loadCharacterStateForInventoryUpdate(ctx, tx, characterID)
		if err != nil {
			return fmt.Errorf("load character state: %w", err)
		}
		items, err := tx.LoadInventory(ctx, characterID)
		if err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}
		layout, err := loadPersistedInventoryLayout(ctx, tx, characterID)
		if err != nil {
			return err
		}
		current := inventory.MoveState{
			Items: cloneMoveItems(items), SaveSeq: state.SaveSeq, Layout: cloneInventoryLayout(layout),
		}
		replacement, err := update(current)
		if err != nil {
			return err
		}
		if replacement.SaveSeq != state.SaveSeq+1 || replacement.SaveSeq <= state.SaveSeq {
			return fmt.Errorf("%w: inventory update sequence %d does not advance %d by one",
				ErrStaleSave, replacement.SaveSeq, state.SaveSeq)
		}
		if !sameBagLayout(replacement.Layout, layout) {
			return fmt.Errorf("%w: inventory update changed persisted bag layout", ErrConstraintViolated)
		}
		if err := tx.ReplaceInventory(ctx, characterID, replacement.Items); err != nil {
			return fmt.Errorf("write inventory: %w", err)
		}
		state.SaveSeq = replacement.SaveSeq
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("advance inventory save sequence: %w", err)
		}
		committed = inventory.MoveState{
			Items: cloneMoveItems(replacement.Items), SaveSeq: replacement.SaveSeq, Layout: cloneInventoryLayout(layout),
		}
		return nil
	})
	if err != nil {
		return inventory.MoveState{}, err
	}
	return committed, nil
}

func loadCharacterStateForInventoryUpdate(
	ctx context.Context,
	repository Repository,
	characterID uuid.UUID,
) (CharacterState, error) {
	store, ok := repository.(*postgresStore)
	if !ok {
		return repository.LoadCharacterState(ctx, characterID)
	}
	if !store.inTx {
		return CharacterState{}, errors.New("charstore: inventory update state lock requires a transaction")
	}
	const statement = `
		SELECT character_id, zone_id, position_x, position_y, position_z,
		       heading, level, experience, health, currency, honor, save_seq, saved_at
		FROM shard.character_state WHERE character_id = $1 FOR UPDATE`
	var state CharacterState
	err := store.db.QueryRow(ctx, statement, characterID).Scan(
		&state.CharacterID,
		&state.ZoneID,
		&state.Position.X,
		&state.Position.Y,
		&state.Position.Z,
		&state.Heading,
		&state.Level,
		&state.Experience,
		&state.Health,
		&state.Currency,
		&state.Honor,
		&state.SaveSeq,
		&state.SavedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CharacterState{}, ErrNotFound
	}
	if err != nil {
		return CharacterState{}, fmt.Errorf("lock character state: %w", err)
	}
	return state, nil
}

func loadPersistedInventoryLayout(
	ctx context.Context,
	repository Repository,
	characterID uuid.UUID,
) (inventory.BagLayout, error) {
	_, layout, err := loadPersistedInventoryHUD(ctx, repository, characterID)
	return layout, err
}

func loadPersistedInventoryHUD(
	ctx context.Context,
	repository Repository,
	characterID uuid.UUID,
) (CharacterHUDState, inventory.BagLayout, error) {
	hud, err := repository.LoadCharacterHUD(ctx, characterID)
	if err != nil {
		return CharacterHUDState{}, inventory.BagLayout{}, fmt.Errorf("load character HUD bag layout: %w", err)
	}
	if hud.Bag == nil {
		return CharacterHUDState{}, inventory.BagLayout{}, fmt.Errorf("%w: character has no equipped bag", ErrConstraintViolated)
	}
	partitions := make([]int32, len(hud.BagLayout.Partitions))
	for index, partition := range hud.BagLayout.Partitions {
		if partition.Ordinal != int16(index) {
			return CharacterHUDState{}, inventory.BagLayout{}, fmt.Errorf("%w: bag partition %d carries ordinal %d",
				ErrConstraintViolated, index, partition.Ordinal)
		}
		partitions[index] = partition.Capacity
	}
	layout := inventory.BagLayout{ID: inventory.BagLayoutID(hud.BagLayout.LayoutID), Partitions: partitions}
	if err := layout.Validate(); err != nil {
		return CharacterHUDState{}, inventory.BagLayout{}, fmt.Errorf("%w: persisted character bag layout: %v", ErrConstraintViolated, err)
	}
	return hud, layout, nil
}

func sameBagLayout(left, right inventory.BagLayout) bool {
	return left.ID == right.ID && slices.Equal(left.Partitions, right.Partitions)
}

func cloneInventoryLayout(layout inventory.BagLayout) inventory.BagLayout {
	return inventory.BagLayout{ID: layout.ID, Partitions: append([]int32(nil), layout.Partitions...)}
}

func cloneMoveItems(items []inventory.InventoryItem) []inventory.InventoryItem {
	cloned := make([]inventory.InventoryItem, len(items))
	for index, item := range items {
		cloned[index] = cloneInventoryItem(item)
	}
	return cloned
}

func inventoryGrants(counts []quests.ItemCount) []inventory.Grant {
	grants := make([]inventory.Grant, 0, len(counts))
	for _, count := range counts {
		grants = append(grants, inventory.Grant{ItemID: count.ItemID, Count: count.Count})
	}
	return grants
}

// assignMissingInstanceIDs allocates within one character while its save
// transaction is open. Existing stack identities win; only stacks created by
// this grant have zero. Save-sequence rejection rolls back a racing allocator,
// so two concurrent grants cannot commit the same next id.
func assignMissingInstanceIDs(stacks []inventory.Stack, hud CharacterHUDState) error {
	var highest uint64
	for _, stack := range stacks {
		if stack.InstanceID > highest {
			highest = stack.InstanceID
		}
	}
	for _, item := range hud.Equipment {
		if item.InstanceID > highest {
			highest = item.InstanceID
		}
	}
	if hud.Bag != nil && hud.Bag.InstanceID > highest {
		highest = hud.Bag.InstanceID
	}
	for index := range stacks {
		if stacks[index].InstanceID != 0 {
			continue
		}
		if highest == ^uint64(0) {
			return errors.New("inventory: item instance id space is exhausted")
		}
		highest++
		stacks[index].InstanceID = highest
	}
	return nil
}
