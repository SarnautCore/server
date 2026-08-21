package inventory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
)

// questRow is the row a turn-in writes. Its shape belongs to `internal/quests`;
// what this package promises about it is only that it is written inside the
// same transaction as everything else.
func questRow(state string) charstore.QuestState {
	return charstore.QuestState{
		QuestID:    "quest.paper-harbor.tide-tally",
		State:      state,
		Objectives: []byte(`{"counters":[1]}`),
	}
}

// TestQuestGrantCommitsItemsCurrenciesAndTheRowTogether is
// mechanics/quests.md rule 5.7.4.
func TestQuestGrantCommitsItemsCurrenciesAndTheRowTogether(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	// Two tonics in the bag to be consumed, and two unstackable rewards out.
	if _, err := service.Award(context.Background(), characterID, inventory.Award{
		Grants: []inventory.Grant{{ItemID: tonic, Count: 2}},
	}); err != nil {
		t.Fatalf("Award() error = %v", err)
	}

	result, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: characterID,
		Consume:     []charstore.ItemCount{{ItemID: tonic, Count: 2}},
		Grants:      []charstore.ItemCount{{ItemID: scale, Count: 1}, {ItemID: feather, Count: 1}},
		Experience:  8,
		Money:       2,
		Honor:       3,
		Quest:       questRow("turned-in"),
	})
	if err != nil {
		t.Fatalf("GrantQuestReward() error = %v", err)
	}
	if result.Experience != 8 || result.Currency != 2 || result.Honor != 3 {
		t.Errorf("result = %+v, want experience 8, currency 2, honor 3", result)
	}

	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Experience != 8 || state.Currency != 2 || state.Honor != 3 {
		t.Errorf("stored state = %+v, want the three currencies credited", state)
	}
	items, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	held := map[string]int32{}
	for _, item := range items {
		held[item.ItemID] += item.Quantity
	}
	if held[tonic] != 0 {
		t.Errorf("the bag still holds %d tonics, want them consumed", held[tonic])
	}
	if held[scale] != 1 || held[feather] != 1 {
		t.Errorf("the bag holds %+v, want one of each reward", held)
	}
	rows, err := repository.LoadQuestStates(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadQuestStates() error = %v", err)
	}
	if len(rows) != 1 || rows[0].State != "turned-in" {
		t.Errorf("quest rows = %+v, want the one turned-in row", rows)
	}
}

// TestAQuestGrantThatDoesNotFitWritesNothing is rule 5.7.3 and 5.7.6.
//
// The assertion is not "an error came back". It is that after the error the
// experience is unchanged, the currencies are unchanged, the bag is unchanged
// and no quest row exists — because the failure mode this guards against is a
// grant that credits the experience and then cannot place the item.
func TestAQuestGrantThatDoesNotFitWritesNothing(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := newCharacter(t, repository)
	// Two slots, and both of them full of something unstackable.
	service := newService(t, repository, 2)
	if _, err := service.Award(context.Background(), characterID, inventory.Award{
		Money:  5,
		Grants: []inventory.Grant{{ItemID: scale, Count: 2}},
	}); err != nil {
		t.Fatalf("Award() error = %v", err)
	}

	_, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: characterID,
		Grants:      []charstore.ItemCount{{ItemID: feather, Count: 1}},
		Experience:  8,
		Money:       2,
		Quest:       questRow("turned-in"),
	})
	if !errors.Is(err, charstore.ErrGrantWouldNotFit) {
		t.Fatalf("GrantQuestReward() error = %v, want store.ErrGrantWouldNotFit", err)
	}
	if !errors.Is(err, inventory.ErrBagFull) {
		t.Errorf("error = %v, want it to still wrap inventory.ErrBagFull for a caller in this package", err)
	}

	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Experience != 0 {
		t.Errorf("experience = %d after a refused grant, want 0", state.Experience)
	}
	if state.Currency != 5 {
		t.Errorf("currency = %d, want the 5 that was there before the grant", state.Currency)
	}
	items, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(items) != 2 {
		t.Errorf("the bag holds %d slots, want the two it had", len(items))
	}
	rows, err := repository.LoadQuestStates(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadQuestStates() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("quest rows = %+v, want none: nothing was applied", rows)
	}
}

// TestAQuestGrantIsNetNotGross is worked example 6.2.
//
// A completely full bag, a grant that consumes one whole stack and gives one
// unstackable item back. Gross the requirement is one slot and there are none;
// net it is zero, and the turn-in must succeed. Computing the gross requirement
// rejects turn-ins that should work, which is the failure this pins.
func TestAQuestGrantIsNetNotGross(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 2)
	if _, err := service.Award(context.Background(), characterID, inventory.Award{
		Grants: []inventory.Grant{{ItemID: scale, Count: 1}, {ItemID: tonic, Count: 1}},
	}); err != nil {
		t.Fatalf("Award() error = %v", err)
	}

	if _, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: characterID,
		Consume:     []charstore.ItemCount{{ItemID: tonic, Count: 1}},
		Grants:      []charstore.ItemCount{{ItemID: feather, Count: 1}},
		Quest:       questRow("turned-in"),
	}); err != nil {
		t.Fatalf("GrantQuestReward() error = %v; the consumed stack frees the slot the reward needs", err)
	}
	items, err := repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("the bag holds %d slots, want 2", len(items))
	}
}

// TestAQuestGrantConsumingWhatIsNotThereWritesNothing keeps the removal
// all-or-nothing. Taking "as many as there are" would be a partial turn-in.
func TestAQuestGrantConsumingWhatIsNotThereWritesNothing(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	_, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: characterID,
		Consume:     []charstore.ItemCount{{ItemID: tonic, Count: 2}},
		Experience:  8,
		Quest:       questRow("turned-in"),
	})
	if !errors.Is(err, inventory.ErrNotEnough) {
		t.Fatalf("GrantQuestReward() error = %v, want inventory.ErrNotEnough", err)
	}
	state, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if state.Experience != 0 {
		t.Errorf("experience = %d, want 0", state.Experience)
	}
}

// TestRemoveDrainsInAscendingSlotOrderAndDeletesEmptyStacks is the arithmetic
// the grant above rests on, on its own.
func TestRemoveDrainsInAscendingSlotOrderAndDeletesEmptyStacks(t *testing.T) {
	t.Parallel()

	remaining, err := inventory.Remove([]inventory.Stack{
		{Slot: 0, ItemID: tonic, Count: 3},
		{Slot: 1, ItemID: scale, Count: 1},
		{Slot: 2, ItemID: tonic, Count: 5},
	}, []inventory.Grant{{ItemID: tonic, Count: 4}})
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining = %+v, want two slots: the first tonic stack is emptied", remaining)
	}
	if remaining[0].Slot != 1 || remaining[0].ItemID != scale {
		t.Errorf("remaining[0] = %+v, want the untouched scale in slot 1", remaining[0])
	}
	if remaining[1].Slot != 2 || remaining[1].Count != 4 {
		t.Errorf("remaining[1] = %+v, want 4 tonics left in slot 2", remaining[1])
	}

	if _, err := inventory.Remove(nil, []inventory.Grant{{ItemID: tonic, Count: 1}}); !errors.Is(
		err, inventory.ErrNotEnough,
	) {
		t.Errorf("Remove() from an empty bag error = %v, want ErrNotEnough", err)
	}
}

// TestAQuestGrantAdvancesTheSaveSequence keeps the anti-clobber rule of
// ADR 0031 §6 true across the new writer.
func TestAQuestGrantAdvancesTheSaveSequence(t *testing.T) {
	repository := charstore.NewMemory()
	characterID := newCharacter(t, repository)
	service := newService(t, repository, 8)

	before, err := repository.LoadCharacterState(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	result, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: characterID,
		Experience:  1,
		Quest:       questRow("accepted"),
	})
	if err != nil {
		t.Fatalf("GrantQuestReward() error = %v", err)
	}
	if result.SaveSeq != before.SaveSeq+1 {
		t.Errorf("save_seq = %d, want %d", result.SaveSeq, before.SaveSeq+1)
	}
}

// TestAQuestGrantForAnUnknownCharacterFails keeps a grant from materializing a
// character out of nothing.
func TestAQuestGrantForAnUnknownCharacterFails(t *testing.T) {
	repository := charstore.NewMemory()
	service := newService(t, repository, 8)

	_, err := service.GrantQuestReward(context.Background(), charstore.QuestGrant{
		CharacterID: uuid.New(),
		Experience:  1,
		Quest:       questRow("accepted"),
	})
	if !errors.Is(err, charstore.ErrNotFound) {
		t.Errorf("GrantQuestReward() error = %v, want store.ErrNotFound", err)
	}
}
