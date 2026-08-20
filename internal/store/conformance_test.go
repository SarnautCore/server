package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/store"
	"github.com/google/uuid"
)

// runRepositoryConformance is the behaviour every [store.Repository] owes its
// callers, run once against the in-memory implementation and once against
// PostgreSQL. Keeping one suite is what makes a passing memory-backed test
// evidence about production rather than about a second, kinder database.
func runRepositoryConformance(t *testing.T, newRepository func(t *testing.T) store.Repository) {
	t.Helper()

	t.Run("account create and read back", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		account := newAccount()
		if err := repository.CreateAccount(ctx, account); err != nil {
			t.Fatalf("create account: %v", err)
		}

		byID, err := repository.AccountByID(ctx, account.AccountID)
		if err != nil {
			t.Fatalf("account by id: %v", err)
		}
		if byID.Email != account.Email || byID.PasswordHash != account.PasswordHash {
			t.Fatalf("account by id = %+v, want email %q", byID, account.Email)
		}
		if byID.CreatedAt.IsZero() {
			t.Error("created_at was not stamped")
		}

		byEmail, err := repository.AccountByEmail(ctx, account.Email)
		if err != nil {
			t.Fatalf("account by email: %v", err)
		}
		if byEmail.AccountID != account.AccountID {
			t.Errorf("account by email = %v, want %v", byEmail.AccountID, account.AccountID)
		}

		if _, err := repository.AccountByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("account by unknown id error = %v, want ErrNotFound", err)
		}
	})

	t.Run("duplicate email is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		first := newAccount()
		if err := repository.CreateAccount(ctx, first); err != nil {
			t.Fatalf("create first account: %v", err)
		}

		second := newAccount()
		second.Email = first.Email
		if err := repository.CreateAccount(ctx, second); !errors.Is(err, store.ErrEmailTaken) {
			t.Fatalf("create duplicate email error = %v, want ErrEmailTaken", err)
		}
	})

	t.Run("character create and list by account", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		account := newAccount()
		if err := repository.CreateAccount(ctx, account); err != nil {
			t.Fatalf("create account: %v", err)
		}

		character := newCharacter(account.AccountID, "Anne")
		if err := repository.CreateCharacter(ctx, character); err != nil {
			t.Fatalf("create character: %v", err)
		}

		stored, err := repository.CharacterByID(ctx, character.CharacterID)
		if err != nil {
			t.Fatalf("character by id: %v", err)
		}
		if stored.Name != "Anne" || stored.NameNormalized != "anne" {
			t.Errorf("character = %+v, want name Anne normalized anne", stored)
		}

		characters, err := repository.CharactersByAccount(ctx, account.AccountID)
		if err != nil {
			t.Fatalf("characters by account: %v", err)
		}
		if len(characters) != 1 || characters[0].CharacterID != character.CharacterID {
			t.Fatalf("characters by account = %+v, want the one created", characters)
		}
	})

	t.Run("duplicate name is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		account := newAccount()
		if err := repository.CreateAccount(ctx, account); err != nil {
			t.Fatalf("create account: %v", err)
		}
		if err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, "O'brien")); err != nil {
			t.Fatalf("create first character: %v", err)
		}

		// Punctuation is stripped before the unique index sees the name, so
		// these three are the same name (ADR 0032 §3).
		for _, name := range []string{"O'brien", "Obrien", "Ob-rien"} {
			err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, name))
			if !errors.Is(err, store.ErrNameTaken) {
				t.Errorf("create character %q error = %v, want ErrNameTaken", name, err)
			}
		}
	})

	t.Run("character state save rejects a non-advancing sequence", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		state := store.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Position:    store.Vec3{X: 1, Y: 2, Z: 3},
			Heading:     0.5,
			Level:       1,
			Experience:  0,
			Health:      100,
			SaveSeq:     1,
		}
		if err := repository.SaveCharacterState(ctx, state); err != nil {
			t.Fatalf("first save: %v", err)
		}

		state.SaveSeq = 2
		state.Position.X = 9
		if err := repository.SaveCharacterState(ctx, state); err != nil {
			t.Fatalf("second save: %v", err)
		}

		loaded, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		if loaded.SaveSeq != 2 || loaded.Position.X != 9 {
			t.Fatalf("loaded state = %+v, want save_seq 2 at x=9", loaded)
		}
		if loaded.SavedAt.IsZero() {
			t.Error("saved_at was not stamped")
		}

		// A slow write from a dying session must not clobber the newer one.
		state.SaveSeq = 2
		state.Position.X = -1
		if err := repository.SaveCharacterState(ctx, state); !errors.Is(err, store.ErrStaleSave) {
			t.Fatalf("replayed save error = %v, want ErrStaleSave", err)
		}
		reloaded, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil {
			t.Fatalf("reload state: %v", err)
		}
		if reloaded.Position.X != 9 {
			t.Errorf("stale save overwrote position: x = %v, want 9", reloaded.Position.X)
		}
	})

	t.Run("inventory insert stack and move", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		put := func(slot int32, itemID string, quantity int32) {
			t.Helper()
			item := store.InventoryItem{Slot: slot, ItemID: itemID, Quantity: quantity}
			if err := repository.PutItem(ctx, characterID, item); err != nil {
				t.Fatalf("put %s x%d in slot %d: %v", itemID, quantity, slot, err)
			}
		}

		put(0, "item.potion-minor", 3)
		put(0, "item.potion-minor", 5) // stacks onto the same slot
		put(1, "item.sword-rusty", 1)

		items := loadInventory(t, ctx, repository, characterID)
		if len(items) != 2 || items[0].Quantity != 8 {
			t.Fatalf("inventory after stacking = %+v, want 8 potions in slot 0", items)
		}

		// A different item must not be overwritten by a misaddressed write.
		mismatched := store.InventoryItem{Slot: 1, ItemID: "item.potion-minor", Quantity: 1}
		if err := repository.PutItem(ctx, characterID, mismatched); !errors.Is(err, store.ErrSlotOccupied) {
			t.Fatalf("put onto a different item error = %v, want ErrSlotOccupied", err)
		}

		// Move into an empty slot.
		if err := repository.MoveItem(ctx, characterID, 1, 5); err != nil {
			t.Fatalf("move to empty slot: %v", err)
		}
		items = loadInventory(t, ctx, repository, characterID)
		if len(items) != 2 || items[1].Slot != 5 || items[1].ItemID != "item.sword-rusty" {
			t.Fatalf("inventory after move = %+v, want the sword in slot 5", items)
		}

		// Move onto a matching stack merges.
		put(2, "item.potion-minor", 4)
		if err := repository.MoveItem(ctx, characterID, 2, 0); err != nil {
			t.Fatalf("move onto a matching stack: %v", err)
		}
		items = loadInventory(t, ctx, repository, characterID)
		if len(items) != 2 || items[0].Slot != 0 || items[0].Quantity != 12 {
			t.Fatalf("inventory after merge = %+v, want 12 potions in slot 0", items)
		}

		// Move onto a different item swaps.
		if err := repository.MoveItem(ctx, characterID, 0, 5); err != nil {
			t.Fatalf("move onto a different item: %v", err)
		}
		items = loadInventory(t, ctx, repository, characterID)
		if len(items) != 2 {
			t.Fatalf("inventory after swap = %+v, want two slots", items)
		}
		if items[0].Slot != 0 || items[0].ItemID != "item.sword-rusty" {
			t.Errorf("slot 0 after swap = %+v, want the sword", items[0])
		}
		if items[1].Slot != 5 || items[1].ItemID != "item.potion-minor" || items[1].Quantity != 12 {
			t.Errorf("slot 5 after swap = %+v, want 12 potions", items[1])
		}

		if err := repository.MoveItem(ctx, characterID, 9, 10); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("move from an empty slot error = %v, want ErrNotFound", err)
		}
	})

	t.Run("quest state upsert", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		quest := store.QuestState{
			QuestID:    "quest.league.first-blood",
			State:      "accepted",
			Objectives: []byte(`{"kill_boars": 0}`),
		}
		if err := repository.UpsertQuestState(ctx, characterID, quest); err != nil {
			t.Fatalf("insert quest state: %v", err)
		}

		quest.State = "completed"
		quest.Objectives = []byte(`{"kill_boars": 5}`)
		if err := repository.UpsertQuestState(ctx, characterID, quest); err != nil {
			t.Fatalf("update quest state: %v", err)
		}

		quests, err := repository.LoadQuestStates(ctx, characterID)
		if err != nil {
			t.Fatalf("load quest states: %v", err)
		}
		if len(quests) != 1 {
			t.Fatalf("quest states = %+v, want exactly one row", quests)
		}
		if quests[0].State != "completed" {
			t.Errorf("quest state = %q, want completed", quests[0].State)
		}
		if quests[0].UpdatedAt.IsZero() {
			t.Error("updated_at was not stamped")
		}
		if !strings.Contains(string(quests[0].Objectives), `"kill_boars"`) ||
			!strings.Contains(string(quests[0].Objectives), "5") {
			t.Errorf("objectives = %s, want the updated counter", quests[0].Objectives)
		}
	})

	t.Run("quest state defaults to an empty objectives document", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		quest := store.QuestState{QuestID: "quest.league.first-blood", State: "accepted"}
		if err := repository.UpsertQuestState(ctx, characterID, quest); err != nil {
			t.Fatalf("insert quest state: %v", err)
		}

		quests, err := repository.LoadQuestStates(ctx, characterID)
		if err != nil {
			t.Fatalf("load quest states: %v", err)
		}
		if len(quests) != 1 || len(quests[0].Objectives) == 0 {
			t.Fatalf("quest states = %+v, want a non-empty objectives document", quests)
		}
	})

	t.Run("RunInTx commits everything at once", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		err := store.SaveCharacter(ctx, repository, store.Snapshot{
			State: store.CharacterState{
				CharacterID: characterID,
				ZoneID:      "InstLeague1",
				Level:       1,
				Health:      100,
				SaveSeq:     1,
			},
			Inventory: []store.InventoryItem{{Slot: 0, ItemID: "item.sword-rusty", Quantity: 1}},
			Quests:    []store.QuestState{{QuestID: "quest.league.first-blood", State: "accepted"}},
		})
		if err != nil {
			t.Fatalf("save character: %v", err)
		}

		if _, err := repository.LoadCharacterState(ctx, characterID); err != nil {
			t.Errorf("load state after save: %v", err)
		}
		if items := loadInventory(t, ctx, repository, characterID); len(items) != 1 {
			t.Errorf("inventory after save = %+v, want one item", items)
		}
		quests, err := repository.LoadQuestStates(ctx, characterID)
		if err != nil || len(quests) != 1 {
			t.Errorf("quests after save = %+v (err %v), want one row", quests, err)
		}
	})

	t.Run("RunInTx rolls back every write on error", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		account := newAccount()
		if err := repository.CreateAccount(ctx, account); err != nil {
			t.Fatalf("create account: %v", err)
		}

		characterID := uuid.New()
		sentinel := errors.New("deliberate failure")
		err := repository.RunInTx(ctx, func(ctx context.Context, tx store.Repository) error {
			if err := tx.CreateCharacter(ctx, newCharacter(account.AccountID, "Rollback")); err != nil {
				return err
			}
			if err := tx.SaveCharacterState(ctx, store.CharacterState{
				CharacterID: characterID,
				ZoneID:      "InstLeague1",
				Level:       1,
				Health:      100,
				SaveSeq:     1,
			}); err != nil {
				return err
			}
			if err := tx.PutItem(ctx, characterID, store.InventoryItem{
				Slot: 0, ItemID: "item.sword-rusty", Quantity: 1,
			}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunInTx error = %v, want the sentinel", err)
		}

		if _, err := repository.LoadCharacterState(ctx, characterID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("character state survived rollback: err = %v, want ErrNotFound", err)
		}
		if items := loadInventory(t, ctx, repository, characterID); len(items) != 0 {
			t.Errorf("inventory survived rollback: %+v", items)
		}
		characters, err := repository.CharactersByAccount(ctx, account.AccountID)
		if err != nil {
			t.Fatalf("characters by account: %v", err)
		}
		if len(characters) != 0 {
			t.Errorf("character survived rollback: %+v", characters)
		}

		// The name must be free again, which is the proof the rollback reached
		// the unique index and not just the row.
		if err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, "Rollback")); err != nil {
			t.Errorf("create character with the rolled-back name: %v", err)
		}
	})
}

func newAccount() store.Account {
	id := uuid.New()
	return store.Account{
		AccountID:    id,
		Email:        "player-" + id.String() + "@example.invalid",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA",
	}
}

func newCharacter(accountID uuid.UUID, name string) store.Character {
	return store.Character{
		CharacterID:     uuid.New(),
		AccountID:       accountID,
		Name:            name,
		ChargenOptionID: "chargen.league.warrior",
	}
}

func loadInventory(
	t *testing.T,
	ctx context.Context,
	repository store.Repository,
	characterID uuid.UUID,
) []store.InventoryItem {
	t.Helper()
	items, err := repository.LoadInventory(ctx, characterID)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	return items
}
