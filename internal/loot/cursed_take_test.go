package loot

import (
	"context"
	"testing"

	"github.com/SarnautCore/server/internal/charstore"
)

type cursedItemLimits struct{}

func (cursedItemLimits) StackLimit(itemID string) (int32, bool) {
	return 20, itemID == "item.cursed-relic"
}

func TestCursedItemSurvivesOfferTakeAndPersistence(t *testing.T) {
	repository := charstore.NewMemory()
	err := charstore.SaveCharacter(context.Background(), repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: partialTakeOwner,
			ZoneID:      "PaperHarbor",
			Level:       2,
			Health:      120,
			SaveSeq:     1,
		},
	})
	if err != nil {
		t.Fatalf("seed character: %v", err)
	}

	awarder, err := charstore.NewInventoryService(repository, cursedItemLimits{}, 8)
	if err != nil {
		t.Fatalf("NewInventoryService() error = %v", err)
	}
	module := newPartialTakeModule(Drop{
		Items: []ItemGrant{{
			ItemID:   "item.cursed-relic",
			Count:    2,
			IsCursed: true,
		}},
	}, awarder)

	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone || len(offer.Items) != 1 || !offer.Items[0].IsCursed {
		t.Fatalf("Look() = %+v, %s; want one cursed item", offer, refusal)
	}

	result, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 0)
	if err != nil {
		t.Fatalf("TakeItem() error = %v", err)
	}
	if len(result.Items) != 1 || !result.Items[0].IsCursed {
		t.Fatalf("TakeItem() grants = %+v, want cursed", result.Items)
	}
	if len(result.Slots) != 1 || !result.Slots[0].Cursed {
		t.Fatalf("TakeItem() slots = %+v, want cursed", result.Slots)
	}

	stored, err := repository.LoadInventory(context.Background(), partialTakeOwner)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(stored) != 1 || !stored[0].Cursed {
		t.Fatalf("stored inventory = %+v, want cursed", stored)
	}
}
