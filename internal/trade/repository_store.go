package trade

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
)

// Capacity reports the currently equipped bag's addressable slot count.
type Capacity interface {
	Slots(characterID uuid.UUID) (int32, bool)
}

// Binding answers the instance-specific bound flag that the current inventory
// schema does not own yet. A production adapter reads the authoritative item
// instance; tests use a map. The trade module refuses to guess.
type Binding interface {
	Bound(ctx context.Context, characterID uuid.UUID, item charstore.InventoryItem) (bool, error)
}

// RepositoryStore adapts charstore's real transaction to the trade Store seam.
type RepositoryStore struct {
	repository charstore.Repository
	limits     inventory.Limits
	capacity   Capacity
	binding    Binding
}

func NewRepositoryStore(
	repository charstore.Repository,
	limits inventory.Limits,
	capacity Capacity,
	binding Binding,
) (*RepositoryStore, error) {
	switch {
	case repository == nil:
		return nil, errors.New("trade: a repository is required")
	case limits == nil:
		return nil, errors.New("trade: stack limits are required")
	case capacity == nil:
		return nil, errors.New("trade: bag capacity is required")
	case binding == nil:
		return nil, errors.New("trade: item binding authority is required")
	default:
		return &RepositoryStore{
			repository: repository,
			limits:     limits,
			capacity:   capacity,
			binding:    binding,
		}, nil
	}
}

func (store *RepositoryStore) Load(
	ctx context.Context,
	characterID uuid.UUID,
) (Holdings, error) {
	state, err := store.repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		return Holdings{}, fmt.Errorf("load purse: %w", err)
	}
	items, err := store.repository.LoadInventory(ctx, characterID)
	if err != nil {
		return Holdings{}, fmt.Errorf("load inventory: %w", err)
	}
	capacity, ok := store.capacity.Slots(characterID)
	if !ok || capacity < 0 {
		return Holdings{}, fmt.Errorf("trade: no valid bag capacity for %s", characterID)
	}
	return store.holdings(ctx, characterID, items, state.Currency, capacity, state.SaveSeq)
}

func (store *RepositoryStore) Transfer(
	ctx context.Context,
	transfer Transfer,
) (TransferResult, error) {
	if transfer.FirstCharacterID == uuid.Nil ||
		transfer.SecondCharacterID == uuid.Nil ||
		transfer.FirstCharacterID == transfer.SecondCharacterID {
		return TransferResult{}, ErrHoldingsChanged
	}
	if transfer.FirstMoney < 0 || transfer.SecondMoney < 0 ||
		len(transfer.FirstItems) > OfferSlots || len(transfer.SecondItems) > OfferSlots {
		return TransferResult{}, ErrHoldingsChanged
	}

	var result TransferResult
	err := store.repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		firstState, firstStored, firstCapacity, err := store.loadForTransfer(
			ctx, tx, transfer.FirstCharacterID,
		)
		if err != nil {
			return err
		}
		secondState, secondStored, secondCapacity, err := store.loadForTransfer(
			ctx, tx, transfer.SecondCharacterID,
		)
		if err != nil {
			return err
		}
		if firstState.Currency < transfer.FirstMoney || secondState.Currency < transfer.SecondMoney {
			return ErrHoldingsChanged
		}
		if err := store.validateOffer(ctx, transfer.FirstCharacterID, firstStored, transfer.FirstItems); err != nil {
			return err
		}
		if err := store.validateOffer(ctx, transfer.SecondCharacterID, secondStored, transfer.SecondItems); err != nil {
			return err
		}

		firstRemaining := removeExact(firstStored, transfer.FirstItems)
		secondRemaining := removeExact(secondStored, transfer.SecondItems)
		firstPlaced, err := inventory.Insert(
			inventory.FromStore(firstRemaining), grants(transfer.SecondItems), store.limits, firstCapacity,
		)
		if errors.Is(err, inventory.ErrBagFull) {
			return ErrNoBagSpace
		}
		if err != nil {
			return fmt.Errorf("place second offer in first bag: %w", err)
		}
		secondPlaced, err := inventory.Insert(
			inventory.FromStore(secondRemaining), grants(transfer.FirstItems), store.limits, secondCapacity,
		)
		if errors.Is(err, inventory.ErrBagFull) {
			return ErrNoBagSpace
		}
		if err != nil {
			return fmt.Errorf("place first offer in second bag: %w", err)
		}

		firstMoney, ok := exchangedMoney(
			firstState.Currency, transfer.FirstMoney, transfer.SecondMoney,
		)
		if !ok {
			return ErrHoldingsChanged
		}
		secondMoney, ok := exchangedMoney(
			secondState.Currency, transfer.SecondMoney, transfer.FirstMoney,
		)
		if !ok {
			return ErrHoldingsChanged
		}
		firstItems := inventory.ToStore(firstPlaced)
		secondItems := inventory.ToStore(secondPlaced)
		firstState.Currency = firstMoney
		secondState.Currency = secondMoney
		firstState.SaveSeq++
		secondState.SaveSeq++

		if err := tx.ReplaceInventory(ctx, transfer.FirstCharacterID, firstItems); err != nil {
			return fmt.Errorf("write first inventory: %w", err)
		}
		if err := tx.ReplaceInventory(ctx, transfer.SecondCharacterID, secondItems); err != nil {
			return fmt.Errorf("write second inventory: %w", err)
		}
		if err := tx.SaveCharacterState(ctx, firstState); err != nil {
			return fmt.Errorf("write first purse: %w", err)
		}
		if err := tx.SaveCharacterState(ctx, secondState); err != nil {
			return fmt.Errorf("write second purse: %w", err)
		}

		first, err := store.holdings(
			ctx, transfer.FirstCharacterID, firstItems, firstMoney, firstCapacity, firstState.SaveSeq,
		)
		if err != nil {
			return err
		}
		second, err := store.holdings(
			ctx, transfer.SecondCharacterID, secondItems, secondMoney, secondCapacity, secondState.SaveSeq,
		)
		if err != nil {
			return err
		}
		result = TransferResult{First: first, Second: second}
		return nil
	})
	if err != nil {
		return TransferResult{}, err
	}
	return result, nil
}

func (store *RepositoryStore) loadForTransfer(
	ctx context.Context,
	repository charstore.Repository,
	characterID uuid.UUID,
) (charstore.CharacterState, []charstore.InventoryItem, int32, error) {
	state, err := repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		return charstore.CharacterState{}, nil, 0, fmt.Errorf("load character state: %w", err)
	}
	items, err := repository.LoadInventory(ctx, characterID)
	if err != nil {
		return charstore.CharacterState{}, nil, 0, fmt.Errorf("load character inventory: %w", err)
	}
	capacity, ok := store.capacity.Slots(characterID)
	if !ok || capacity < 0 {
		return charstore.CharacterState{}, nil, 0, fmt.Errorf("trade: no valid bag capacity for %s", characterID)
	}
	return state, items, capacity, nil
}

func (store *RepositoryStore) validateOffer(
	ctx context.Context,
	characterID uuid.UUID,
	stored []charstore.InventoryItem,
	offered []Item,
) error {
	seen := make(map[int32]struct{}, len(offered))
	for _, item := range offered {
		if item.BagSlot < 0 || item.ItemID == "" || item.Count <= 0 {
			return ErrHoldingsChanged
		}
		if _, duplicate := seen[item.BagSlot]; duplicate {
			return ErrHoldingsChanged
		}
		seen[item.BagSlot] = struct{}{}
		actual, found := storedAt(stored, item.BagSlot)
		if !found || actual.ItemID != item.ItemID || actual.Quantity != item.Count {
			return ErrHoldingsChanged
		}
		bound, err := store.binding.Bound(ctx, characterID, actual)
		if err != nil {
			return fmt.Errorf("read item binding: %w", err)
		}
		if bound {
			return ErrHoldingsChanged
		}
	}
	return nil
}

func (store *RepositoryStore) holdings(
	ctx context.Context,
	characterID uuid.UUID,
	stored []charstore.InventoryItem,
	money int64,
	capacity int32,
	saveSeq int64,
) (Holdings, error) {
	items := make([]Item, 0, len(stored))
	for _, storedItem := range stored {
		bound, err := store.binding.Bound(ctx, characterID, storedItem)
		if err != nil {
			return Holdings{}, fmt.Errorf("read item binding: %w", err)
		}
		items = append(items, Item{
			BagSlot: storedItem.Slot,
			ItemID:  storedItem.ItemID,
			Count:   storedItem.Quantity,
			Bound:   bound,
		})
	}
	return Holdings{
		Items:    items,
		Money:    money,
		Capacity: capacity,
		SaveSeq:  saveSeq,
	}, nil
}

func removeExact(stored []charstore.InventoryItem, offered []Item) []charstore.InventoryItem {
	removed := make(map[int32]struct{}, len(offered))
	for _, item := range offered {
		removed[item.BagSlot] = struct{}{}
	}
	result := make([]charstore.InventoryItem, 0, len(stored)-len(offered))
	for _, item := range stored {
		if _, remove := removed[item.Slot]; !remove {
			result = append(result, item)
		}
	}
	return result
}

func storedAt(items []charstore.InventoryItem, slot int32) (charstore.InventoryItem, bool) {
	for _, item := range items {
		if item.Slot == slot {
			return item, true
		}
	}
	return charstore.InventoryItem{}, false
}

func grants(items []Item) []inventory.Grant {
	result := make([]inventory.Grant, 0, len(items))
	for _, item := range items {
		result = append(result, inventory.Grant{ItemID: item.ItemID, Count: item.Count})
	}
	return result
}

func exchangedMoney(balance, offered, received int64) (int64, bool) {
	if balance < 0 || offered < 0 || received < 0 || balance < offered {
		return 0, false
	}
	remaining := balance - offered
	if received > math.MaxInt64-remaining {
		return 0, false
	}
	return remaining + received, true
}

// FixedCapacity is the explicit adapter for a shard whose equipped-bag system
// supplies one capacity for every connected character.
type FixedCapacity int32

func (capacity FixedCapacity) Slots(uuid.UUID) (int32, bool) {
	return int32(capacity), capacity >= 0
}

// UnboundItems is the explicit adapter for a ruleset that has no bound item
// instances. It is never selected implicitly by NewRepositoryStore.
type UnboundItems struct{}

func (UnboundItems) Bound(context.Context, uuid.UUID, charstore.InventoryItem) (bool, error) {
	return false, nil
}
