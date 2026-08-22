package charstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/itemactions"
)

var errHUDWrite = errors.New("injected HUD write failure")

type failingHUDRepository struct{ charstore.Repository }

func (repository failingHUDRepository) RunInTx(
	ctx context.Context,
	fn func(context.Context, charstore.Repository) error,
) error {
	return repository.Repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		return fn(ctx, failingHUDRepository{Repository: tx})
	})
}

func (repository failingHUDRepository) SaveCharacterHUD(
	context.Context,
	uuid.UUID,
	charstore.CharacterHUDState,
) error {
	return errHUDWrite
}

type actionCatalog map[string]itemactions.Definition

func (catalog actionCatalog) ItemDefinition(id string) (itemactions.Definition, bool) {
	definition, ok := catalog[id]
	return definition, ok
}

func TestItemActionsPersistInventoryEquipmentAndRevisionAtomically(t *testing.T) {
	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := seedItemActionCharacter(t, repository)
	adapter, err := charstore.NewItemActionRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	service, err := itemactions.NewService(adapter, adapter, actionCatalog{
		"item.sword": {
			ItemID: "item.sword", StackLimit: 1, Droppable: true,
			EquipSlots:  []itemactions.EquipmentSlot{itemactions.EquipmentMainhand},
			BindOnEquip: true, TriggerSlot: "MAINHAND",
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Equip(ctx, itemactions.Actor{CharacterID: characterID, EntityID: 99}, itemactions.EquipCommand{
		RequestID: 7, ExpectedRevision: 10, BagSlot: 2, DressSlot: itemactions.EquipmentMainhand,
	})
	if err != nil {
		t.Fatalf("Equip() error = %v", err)
	}
	if result.State.Revision != 11 || len(result.State.Inventory) != 0 || len(result.State.Equipment) != 1 ||
		result.State.Equipment[0].Item.InstanceID != 501 || !result.State.Equipment[0].Item.Bound {
		t.Fatalf("Equip() state = %+v", result.State)
	}

	persisted, err := adapter.Read(ctx, characterID, 11)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(persisted.Inventory) != 0 || len(persisted.Equipment) != 1 ||
		persisted.Equipment[0].Item.InstanceID != 501 || !persisted.Equipment[0].Item.Bound {
		t.Fatalf("persisted action state = %+v", persisted)
	}
	characterState, err := repository.LoadCharacterState(ctx, characterID)
	if err != nil || characterState.SaveSeq != 11 {
		t.Fatalf("character state after equip = %+v, %v", characterState, err)
	}
	hud, err := repository.LoadCharacterHUD(ctx, characterID)
	if err != nil || hud.Stats[3].Base == nil || *hud.Stats[3].Base != 17 {
		t.Fatalf("unrelated HUD state was not preserved: %+v, %v", hud, err)
	}
}

func TestItemActionAdapterReturnsCurrentFullStateOnStaleRevision(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := seedItemActionCharacter(t, repository)
	adapter, err := charstore.NewItemActionRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	service, err := itemactions.NewService(adapter, adapter, actionCatalog{
		"item.sword": {ItemID: "item.sword", StackLimit: 1, Droppable: true},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Drop(context.Background(), itemactions.Actor{CharacterID: characterID, EntityID: 99}, itemactions.DropCommand{
		RequestID: 1, ExpectedRevision: 9, Count: 1, Slot: 2,
	})
	if !errors.Is(err, itemactions.ErrStaleRevision) {
		t.Fatalf("Drop(stale) error = %v", err)
	}
	if result.State.Revision != 10 || len(result.State.Inventory) != 1 || result.State.Inventory[0].InstanceID != 501 {
		t.Fatalf("stale replacement = %+v", result.State)
	}
}

func TestItemActionAdapterRollsBackRevisionAndInventoryWhenHUDWriteFails(t *testing.T) {
	base := charstore.NewMemory()
	characterID := seedItemActionCharacter(t, base)
	adapter, err := charstore.NewItemActionRepository(failingHUDRepository{Repository: base})
	if err != nil {
		t.Fatal(err)
	}
	service, err := itemactions.NewService(adapter, adapter, actionCatalog{
		"item.sword": {
			ItemID: "item.sword", StackLimit: 1, Droppable: true,
			EquipSlots: []itemactions.EquipmentSlot{itemactions.EquipmentMainhand},
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Equip(context.Background(), itemactions.Actor{CharacterID: characterID, EntityID: 99}, itemactions.EquipCommand{
		RequestID: 1, ExpectedRevision: 10, BagSlot: 2, DressSlot: itemactions.EquipmentMainhand,
	})
	if !errors.Is(err, errHUDWrite) {
		t.Fatalf("Equip() error = %v", err)
	}
	plain, err := charstore.NewItemActionRepository(base)
	if err != nil {
		t.Fatal(err)
	}
	state, err := plain.Read(context.Background(), characterID, 10)
	if err != nil {
		t.Fatalf("Read(original) error = %v", err)
	}
	if len(state.Inventory) != 1 || state.Inventory[0].InstanceID != 501 || len(state.Equipment) != 0 {
		t.Fatalf("transaction did not roll back all replacements: %+v", state)
	}
}

func TestItemActionAdapterSerializesExpectedRevisionRace(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := seedItemActionCharacter(t, repository)
	adapter, err := charstore.NewItemActionRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	service, err := itemactions.NewService(adapter, adapter, actionCatalog{
		"item.sword": {ItemID: "item.sword", StackLimit: 1, Droppable: true},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 32
	results := make(chan error, callers)
	var wait sync.WaitGroup
	for request := range callers {
		wait.Add(1)
		go func(requestID uint64) {
			defer wait.Done()
			_, err := service.Drop(context.Background(), itemactions.Actor{CharacterID: characterID, EntityID: 99}, itemactions.DropCommand{
				RequestID: requestID + 1, ExpectedRevision: 10, Count: 1, Slot: 2,
			})
			results <- err
		}(uint64(request))
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, itemactions.ErrStaleRevision) {
			t.Fatalf("race error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("race successes = %d, want 1", successes)
	}
	state, err := adapter.Read(context.Background(), characterID, 11)
	if err != nil || len(state.Inventory) != 0 {
		t.Fatalf("state after race = %+v, %v", state, err)
	}
}

func seedItemActionCharacter(t *testing.T, repository charstore.Repository) uuid.UUID {
	t.Helper()
	characterID := uuid.New()
	base := float32(17)
	hud := charstore.CharacterHUDState{
		BagLayout: charstore.ProductBagLayout{
			LayoutID:   string(inventory.BagLayout16ID),
			Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 16}},
		},
		Stats:   charstore.EmptyOrderedStats(),
		Actions: charstore.EmptyOrderedActionSlots(),
	}
	hud.Stats[3].Base = &base
	err := repository.RunInTx(context.Background(), func(ctx context.Context, tx charstore.Repository) error {
		if err := tx.SaveCharacterState(ctx, charstore.CharacterState{
			CharacterID: characterID, ZoneID: "zone.fixture", Level: 1, Health: 100, SaveSeq: 10,
		}); err != nil {
			return err
		}
		if err := tx.ReplaceInventory(ctx, characterID, []charstore.InventoryItem{{
			Slot: 2, InstanceID: 501, ItemID: "item.sword", Quantity: 1,
		}}); err != nil {
			return err
		}
		return tx.SaveCharacterHUD(ctx, characterID, hud)
	})
	if err != nil {
		t.Fatalf("seed item action character: %v", err)
	}
	return characterID
}
