package charstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/google/uuid"
)

var errDeliberateInventoryUpdate = errors.New("deliberate inventory update failure")

// runRepositoryConformance is the behaviour every [charstore.Repository] owes its
// callers, run once against the in-memory implementation and once against
// PostgreSQL. Keeping one suite is what makes a passing memory-backed test
// evidence about production rather than about a second, kinder database.
func runRepositoryConformance(t *testing.T, newRepository func(t *testing.T) charstore.Repository) {
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

		if _, err := repository.AccountByID(ctx, uuid.New()); !errors.Is(err, charstore.ErrNotFound) {
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
		if err := repository.CreateAccount(ctx, second); !errors.Is(err, charstore.ErrEmailTaken) {
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
			if !errors.Is(err, charstore.ErrNameTaken) {
				t.Errorf("create character %q error = %v, want ErrNameTaken", name, err)
			}
		}
	})

	// The schema's own CHECK and FOREIGN KEY constraints, exercised through the
	// same suite as everything else. Without these the in-memory store is not
	// evidence about Postgres in the direction that matters: a wave-3 chargen
	// test writing a two-character name or an orphan account_id goes green here
	// and fails in production as an unclassified insert error.
	t.Run("a character name outside 3-16 characters is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		account := newAccount()
		if err := repository.CreateAccount(ctx, account); err != nil {
			t.Fatalf("create account: %v", err)
		}
		for _, name := range []string{"", "Ab", strings.Repeat("A", 17)} {
			err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, name))
			if !errors.Is(err, charstore.ErrConstraintViolated) {
				t.Errorf("create character %q (%d chars) error = %v, want ErrConstraintViolated",
					name, len(name), err)
			}
		}
		// The bounds themselves are inclusive.
		for _, name := range []string{"Abc", strings.Repeat("A", 16)} {
			if err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, name)); err != nil {
				t.Errorf("create character %q error = %v, want it accepted", name, err)
			}
		}
	})

	t.Run("a character with no account is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		err := repository.CreateCharacter(ctx, newCharacter(uuid.New(), "Orphan"))
		if !errors.Is(err, charstore.ErrConstraintViolated) {
			t.Errorf("create orphan character error = %v, want ErrConstraintViolated", err)
		}
	})

	t.Run("character state below its column checks is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		valid := charstore.CharacterState{
			CharacterID: uuid.New(),
			ZoneID:      "InstLeague1",
			Level:       1,
			Health:      100,
			SaveSeq:     1,
		}
		for name, corrupt := range map[string]func(*charstore.CharacterState){
			"level below one":     func(state *charstore.CharacterState) { state.Level = 0 },
			"negative experience": func(state *charstore.CharacterState) { state.Experience = -1 },
			"negative save_seq":   func(state *charstore.CharacterState) { state.SaveSeq = -1 },
		} {
			state := valid
			state.CharacterID = uuid.New()
			corrupt(&state)
			if err := repository.SaveCharacterState(ctx, state); !errors.Is(err, charstore.ErrConstraintViolated) {
				t.Errorf("save with %s error = %v, want ErrConstraintViolated", name, err)
			}
		}
	})

	t.Run("a negative inventory slot is rejected", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()

		characterID := uuid.New()
		err := repository.PutItem(ctx, characterID, charstore.InventoryItem{
			Slot: -1, InstanceID: 1, ItemID: "item.fixture", Quantity: 1,
		})
		if !errors.Is(err, charstore.ErrConstraintViolated) {
			t.Errorf("put item in slot -1 error = %v, want ErrConstraintViolated", err)
		}
	})

	t.Run("character state save rejects a non-advancing sequence", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		state := charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Position:    charstore.Vec3{X: 1, Y: 2, Z: 3},
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
		if err := repository.SaveCharacterState(ctx, state); !errors.Is(err, charstore.ErrStaleSave) {
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

	t.Run("character currency round-trips", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		state := charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Level:       1,
			Health:      100,
			SaveSeq:     1,
		}
		if err := repository.SaveCharacterState(ctx, state); err != nil {
			t.Fatalf("first save: %v", err)
		}
		loaded, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		if loaded.Currency != 0 {
			t.Errorf("a fresh character's purse holds %d, want 0", loaded.Currency)
		}

		// mechanics/loot.md rule 5.6.1: money credits the purse. It is written
		// by the same statement as the position, so a save that carried a
		// currency and dropped it would show up here and nowhere else.
		state.SaveSeq = 2
		state.Currency = 4_294_967_297 // past 32 bits, because the column is a bigint
		if err := repository.SaveCharacterState(ctx, state); err != nil {
			t.Fatalf("credit save: %v", err)
		}
		credited, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil {
			t.Fatalf("reload state: %v", err)
		}
		if credited.Currency != state.Currency {
			t.Errorf("purse = %d, want %d", credited.Currency, state.Currency)
		}
	})

	t.Run("inventory insert stack and move", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		put := func(slot int32, itemID string, quantity int32) {
			t.Helper()
			item := charstore.InventoryItem{Slot: slot, InstanceID: uint64(slot) + 1, ItemID: itemID, Quantity: quantity}
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
		mismatched := charstore.InventoryItem{Slot: 1, InstanceID: 2, ItemID: "item.potion-minor", Quantity: 1}
		if err := repository.PutItem(ctx, characterID, mismatched); !errors.Is(err, charstore.ErrSlotOccupied) {
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

		if err := repository.MoveItem(ctx, characterID, 9, 10); !errors.Is(err, charstore.ErrNotFound) {
			t.Errorf("move from an empty slot error = %v, want ErrNotFound", err)
		}
	})

	t.Run("inventory item instance state round-trips losslessly", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		removeTime := int64(-9_223_372_036_854_775_000)
		rune := "resource.rune.fixture"
		runeSlot := "resource.rune-slot.fixture"
		want := charstore.InventoryItem{
			Slot: 3, InstanceID: ^uint64(0), ItemID: "item.fixture.native", Quantity: 7,
			CounterValue: -4, Bound: true, Cursed: true, QuestOperator: true,
			RemoveTime: &removeTime, RuneResourceID: &rune, RuneSlotResourceID: &runeSlot,
		}
		if err := repository.ReplaceInventory(ctx, characterID, []charstore.InventoryItem{want}); err != nil {
			t.Fatalf("replace inventory: %v", err)
		}
		got := loadInventory(t, ctx, repository, characterID)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("inventory instance = %#v, want %#v", got, want)
		}
	})

	t.Run("HUD state round-trips exact authored capacities and optional values", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		want := hudFixture("roundtrip", 1)
		if err := repository.SaveCharacterHUD(ctx, characterID, want); err != nil {
			t.Fatalf("save HUD: %v", err)
		}
		got, err := repository.LoadCharacterHUD(ctx, characterID)
		if err != nil {
			t.Fatalf("load HUD: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("HUD state = %#v, want %#v", got, want)
		}
		if len(got.Stats) != 14 || len(got.Actions) != 36 || len(charstore.RegularEquipmentSlots) != 20 {
			t.Fatalf("closed capacities stats/actions/equipment = %d/%d/%d, want 14/36/20",
				len(got.Stats), len(got.Actions), len(charstore.RegularEquipmentSlots))
		}
		if got.Stats[charstore.StatStrength].Base == nil ||
			got.Stats[charstore.StatStrength].Result != nil ||
			got.Stats[charstore.StatStrength].ResultLongTerm != nil {
			t.Fatalf("strength components = %+v, want authored base and absent computed values", got.Stats[charstore.StatStrength])
		}
	})

	t.Run("HUD constraints reject invented or structurally invalid state", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		for name, corrupt := range map[string]func(*charstore.CharacterHUDState){
			"zero instance id": func(hud *charstore.CharacterHUDState) { hud.Equipment[0].InstanceID = 0 },
			"ammo slot 17":     func(hud *charstore.CharacterHUDState) { hud.Equipment[0].Slot = 17 },
			"six bag partitions": func(hud *charstore.CharacterHUDState) {
				hud.BagLayout.Partitions = append(hud.BagLayout.Partitions,
					charstore.BagPartition{Ordinal: 2, Capacity: 1},
					charstore.BagPartition{Ordinal: 3, Capacity: 1},
					charstore.BagPartition{Ordinal: 4, Capacity: 1},
					charstore.BagPartition{Ordinal: 5, Capacity: 1})
			},
			"bag capacity 61": func(hud *charstore.CharacterHUDState) {
				hud.BagLayout.Partitions = []charstore.BagPartition{{Ordinal: 0, Capacity: 60}, {Ordinal: 1, Capacity: 1}}
			},
			"stat order":   func(hud *charstore.CharacterHUDState) { hud.Stats[4].Ordinal = 5 },
			"action order": func(hud *charstore.CharacterHUDState) { hud.Actions[0].Ordinal = 1 },
		} {
			t.Run(name, func(t *testing.T) {
				hud := hudFixture(name, 1)
				corrupt(&hud)
				if err := repository.SaveCharacterHUD(ctx, uuid.New(), hud); !errors.Is(err, charstore.ErrConstraintViolated) {
					t.Fatalf("SaveCharacterHUD() error = %v, want ErrConstraintViolated", err)
				}
			})
		}
	})

	t.Run("an invalid HUD rolls back state and inventory", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		first := snapshotWithHUD(characterID, 1, "first")
		if err := charstore.SaveCharacter(ctx, repository, first); err != nil {
			t.Fatalf("seed snapshot: %v", err)
		}
		second := snapshotWithHUD(characterID, 2, "second")
		second.HUD.Equipment[0].InstanceID = 0
		if err := charstore.SaveCharacter(ctx, repository, second); !errors.Is(err, charstore.ErrConstraintViolated) {
			t.Fatalf("invalid snapshot error = %v, want ErrConstraintViolated", err)
		}
		state, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil || state.SaveSeq != 1 {
			t.Fatalf("state after rollback = %+v, %v; want sequence 1", state, err)
		}
		items := loadInventory(t, ctx, repository, characterID)
		if len(items) != 1 || items[0].ItemID != "item.first" {
			t.Fatalf("inventory after rollback = %+v, want first snapshot", items)
		}
		hud, err := repository.LoadCharacterHUD(ctx, characterID)
		if err != nil || len(hud.Equipment) != 1 || hud.Equipment[0].ItemID != "item.weapon.first" {
			t.Fatalf("HUD after rollback = %+v, %v; want first snapshot", hud, err)
		}
	})

	t.Run("an item instance cannot exist in inventory and equipment", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		snapshot := snapshotWithHUD(characterID, 1, "duplicate")
		snapshot.Inventory[0].InstanceID = snapshot.HUD.Equipment[0].InstanceID
		if err := charstore.SaveCharacter(ctx, repository, snapshot); !errors.Is(err, charstore.ErrConstraintViolated) {
			t.Fatalf("duplicate cross-container identity error = %v, want ErrConstraintViolated", err)
		}
		if _, err := repository.LoadCharacterState(ctx, characterID); !errors.Is(err, charstore.ErrNotFound) {
			t.Fatalf("duplicate snapshot partially committed state: %v", err)
		}
	})

	t.Run("a checkpoint atomically moves an instance from equipment to inventory", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		first := snapshotWithHUD(characterID, 1, "equipped")
		if err := charstore.SaveCharacter(ctx, repository, first); err != nil {
			t.Fatalf("seed equipped snapshot: %v", err)
		}

		second := snapshotWithHUD(characterID, 2, "unequipped")
		second.Inventory[0].InstanceID = first.HUD.Equipment[0].InstanceID
		second.Inventory[0].ItemID = first.HUD.Equipment[0].ItemID
		if err := charstore.SaveCharacter(ctx, repository, second); err != nil {
			t.Fatalf("save atomic unequip snapshot: %v", err)
		}
		items := loadInventory(t, ctx, repository, characterID)
		if len(items) != 1 || items[0].InstanceID != first.HUD.Equipment[0].InstanceID {
			t.Fatalf("inventory after unequip = %+v, want preserved instance %d", items, first.HUD.Equipment[0].InstanceID)
		}
	})

	t.Run("UpdateInventory commits items and sequence under the persisted layout", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		seed := snapshotWithHUD(characterID, 1, "inventory-update")
		seed.HUD.BagLayout = charstore.ProductBagLayout{
			LayoutID:   "bag.layout.18",
			Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 12}, {Ordinal: 1, Capacity: 6}},
		}
		seed.Inventory[0].Slot = 17
		if err := charstore.SaveCharacter(ctx, repository, seed); err != nil {
			t.Fatalf("seed inventory update: %v", err)
		}

		committed, err := repository.UpdateInventory(ctx, characterID, func(state inventory.MoveState) (inventory.MoveState, error) {
			if state.Layout.ID != inventory.BagLayout18ID || !reflect.DeepEqual(state.Layout.Partitions, []int32{12, 6}) {
				t.Fatalf("callback layout = %+v, want authored bag.layout.18 [12,6]", state.Layout)
			}
			state.Items[0].Slot = 0
			state.SaveSeq++
			return state, nil
		})
		if err != nil {
			t.Fatalf("UpdateInventory() error = %v", err)
		}
		if committed.SaveSeq != 2 || len(committed.Items) != 1 || committed.Items[0].Slot != 0 {
			t.Fatalf("committed inventory state = %+v, want slot 0 at sequence 2", committed)
		}
		stored := loadInventory(t, ctx, repository, characterID)
		state, stateErr := repository.LoadCharacterState(ctx, characterID)
		hud, hudErr := repository.LoadCharacterHUD(ctx, characterID)
		if len(stored) != 1 || stored[0].Slot != 0 || stateErr != nil || state.SaveSeq != 2 ||
			hudErr != nil || hud.BagLayout.LayoutID != "bag.layout.18" {
			t.Fatalf("stored inventory/state/HUD = %+v / %+v (%v) / %+v (%v)", stored, state, stateErr, hud, hudErr)
		}
	})

	t.Run("UpdateInventory rolls back callback errors and stale replacements", func(t *testing.T) {
		for name, update := range map[string]func(inventory.MoveState) (inventory.MoveState, error){
			"callback error": func(state inventory.MoveState) (inventory.MoveState, error) {
				state.Items[0].Slot = 5
				return state, errDeliberateInventoryUpdate
			},
			"stale sequence": func(state inventory.MoveState) (inventory.MoveState, error) {
				state.Items[0].Slot = 5
				return state, nil
			},
			"layout mutation": func(state inventory.MoveState) (inventory.MoveState, error) {
				state.Items[0].Slot = 5
				state.SaveSeq++
				state.Layout.Partitions[0] = 11
				return state, nil
			},
		} {
			t.Run(name, func(t *testing.T) {
				repository := newRepository(t)
				ctx := t.Context()
				characterID := uuid.New()
				if err := charstore.SaveCharacter(ctx, repository, snapshotWithHUD(characterID, 1, name)); err != nil {
					t.Fatalf("seed inventory update: %v", err)
				}
				_, err := repository.UpdateInventory(ctx, characterID, update)
				switch name {
				case "callback error":
					if !errors.Is(err, errDeliberateInventoryUpdate) {
						t.Fatalf("UpdateInventory() error = %v, want callback error", err)
					}
				case "stale sequence":
					if !errors.Is(err, charstore.ErrStaleSave) {
						t.Fatalf("UpdateInventory() error = %v, want ErrStaleSave", err)
					}
				default:
					if !errors.Is(err, charstore.ErrConstraintViolated) {
						t.Fatalf("UpdateInventory() error = %v, want ErrConstraintViolated", err)
					}
				}
				stored := loadInventory(t, ctx, repository, characterID)
				state, stateErr := repository.LoadCharacterState(ctx, characterID)
				if len(stored) != 1 || stored[0].Slot != 0 || stateErr != nil || state.SaveSeq != 1 {
					t.Fatalf("failed update changed stored state: %+v / %+v (%v)", stored, state, stateErr)
				}
			})
		}
	})

	t.Run("UpdateInventory rejects a non-catalog persisted layout", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		seed := snapshotWithHUD(characterID, 1, "bad-layout")
		seed.HUD.BagLayout = charstore.ProductBagLayout{
			LayoutID:   "bag.layout.18",
			Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 9}, {Ordinal: 1, Capacity: 9}},
		}
		if err := charstore.SaveCharacter(ctx, repository, seed); err != nil {
			t.Fatalf("seed structurally valid layout: %v", err)
		}
		called := false
		_, err := repository.UpdateInventory(ctx, characterID, func(state inventory.MoveState) (inventory.MoveState, error) {
			called = true
			return state, nil
		})
		if !errors.Is(err, charstore.ErrConstraintViolated) || called {
			t.Fatalf("UpdateInventory() = %v, callback called %v; want exact-layout refusal before callback", err, called)
		}
	})

	t.Run("concurrent UpdateInventory calls serialize without lost writes", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		if err := charstore.SaveCharacter(ctx, repository, snapshotWithHUD(characterID, 1, "update-race")); err != nil {
			t.Fatalf("seed inventory update race: %v", err)
		}
		start := make(chan struct{})
		errorsSeen := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, err := repository.UpdateInventory(ctx, characterID, func(state inventory.MoveState) (inventory.MoveState, error) {
					state.Items[0].Quantity++
					state.SaveSeq++
					return state, nil
				})
				errorsSeen <- err
			}()
		}
		close(start)
		wait.Wait()
		close(errorsSeen)
		for err := range errorsSeen {
			if err != nil {
				t.Fatalf("concurrent UpdateInventory() error = %v", err)
			}
		}
		stored := loadInventory(t, ctx, repository, characterID)
		state, err := repository.LoadCharacterState(ctx, characterID)
		if len(stored) != 1 || stored[0].Quantity != 3 || err != nil || state.SaveSeq != 3 {
			t.Fatalf("concurrent inventory/state = %+v / %+v (%v), want quantity 3 sequence 3", stored, state, err)
		}
	})

	t.Run("concurrent snapshots cannot tear HUD from the winning sequence", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()
		if err := charstore.SaveCharacter(ctx, repository, snapshotWithHUD(characterID, 1, "seed")); err != nil {
			t.Fatalf("seed snapshot: %v", err)
		}
		start := make(chan struct{})
		errorsBySequence := make(chan error, 2)
		var wait sync.WaitGroup
		for sequence, marker := range map[int64]string{2: "second", 3: "third"} {
			sequence, marker := sequence, marker
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				errorsBySequence <- charstore.SaveCharacter(ctx, repository,
					snapshotWithHUD(characterID, sequence, marker))
			}()
		}
		close(start)
		wait.Wait()
		close(errorsBySequence)
		for err := range errorsBySequence {
			if err != nil && !errors.Is(err, charstore.ErrStaleSave) {
				t.Fatalf("concurrent save error = %v", err)
			}
		}
		state, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil || state.SaveSeq != 3 {
			t.Fatalf("winning state = %+v, %v; want sequence 3", state, err)
		}
		hud, err := repository.LoadCharacterHUD(ctx, characterID)
		if err != nil || len(hud.Equipment) != 1 || hud.Equipment[0].ItemID != "item.weapon.third" {
			t.Fatalf("winning HUD = %+v, %v; want third", hud, err)
		}
		items := loadInventory(t, ctx, repository, characterID)
		if len(items) != 1 || items[0].ItemID != "item.third" {
			t.Fatalf("winning inventory = %+v, want third", items)
		}
	})

	t.Run("quest state upsert", func(t *testing.T) {
		repository := newRepository(t)
		ctx := t.Context()
		characterID := uuid.New()

		quest := charstore.QuestState{
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

		quest := charstore.QuestState{QuestID: "quest.league.first-blood", State: "accepted"}
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

		err := charstore.SaveCharacter(ctx, repository, charstore.Snapshot{
			State: charstore.CharacterState{
				CharacterID: characterID,
				ZoneID:      "InstLeague1",
				Level:       1,
				Health:      100,
				SaveSeq:     1,
			},
			Inventory: []charstore.InventoryItem{{Slot: 0, InstanceID: 1, ItemID: "item.sword-rusty", Quantity: 1}},
			Quests:    []charstore.QuestState{{QuestID: "quest.league.first-blood", State: "accepted"}},
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
		err := repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
			if err := tx.CreateCharacter(ctx, newCharacter(account.AccountID, "Rollback")); err != nil {
				return err
			}
			if err := tx.SaveCharacterState(ctx, charstore.CharacterState{
				CharacterID: characterID,
				ZoneID:      "InstLeague1",
				Level:       1,
				Health:      100,
				SaveSeq:     1,
			}); err != nil {
				return err
			}
			if err := tx.PutItem(ctx, characterID, charstore.InventoryItem{
				Slot: 0, InstanceID: 1, ItemID: "item.sword-rusty", Quantity: 1,
			}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunInTx error = %v, want the sentinel", err)
		}

		if _, err := repository.LoadCharacterState(ctx, characterID); !errors.Is(err, charstore.ErrNotFound) {
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

func newAccount() charstore.Account {
	id := uuid.New()
	return charstore.Account{
		AccountID:    id,
		Email:        "player-" + id.String() + "@example.invalid",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA",
	}
}

func newCharacter(accountID uuid.UUID, name string) charstore.Character {
	return charstore.Character{
		CharacterID:     uuid.New(),
		AccountID:       accountID,
		Name:            name,
		ChargenOptionID: "chargen.league.warrior",
	}
}

func loadInventory(
	t *testing.T,
	ctx context.Context,
	repository charstore.Repository,
	characterID uuid.UUID,
) []charstore.InventoryItem {
	t.Helper()
	items, err := repository.LoadInventory(ctx, characterID)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	return items
}

func hudFixture(marker string, firstInstanceID uint64) charstore.CharacterHUDState {
	base := float32(12.5)
	result := float32(14.25)
	longTerm := float32(13.75)
	abilityID := "ability." + marker
	removeTime := int64(-7_654_321)
	runeID := "resource.rune." + marker
	runeSlotID := "resource.rune-slot." + marker
	stats := charstore.EmptyOrderedStats()
	stats[charstore.StatStrength].Base = &base
	stats[charstore.StatMight].Result = &result
	stats[charstore.StatLethality].ResultLongTerm = &longTerm
	actions := charstore.EmptyOrderedActionSlots()
	actions[0].AbilityID = &abilityID
	return charstore.CharacterHUDState{
		Equipment: []charstore.EquipmentItem{{
			Slot: charstore.EquipmentMainhand,
			ItemInstance: charstore.ItemInstance{
				InstanceID: firstInstanceID, ItemID: "item.weapon." + marker, Quantity: 1,
				CounterValue: -9, Bound: true, Cursed: true, QuestOperator: true,
				RemoveTime: &removeTime, RuneResourceID: &runeID, RuneSlotResourceID: &runeSlotID,
			},
		}},
		Bag: &charstore.ItemInstance{
			InstanceID: firstInstanceID + 1, ItemID: "item.bag." + marker, Quantity: 1,
		},
		BagLayout: charstore.ProductBagLayout{
			LayoutID: "bag.layout.36",
			Partitions: []charstore.BagPartition{
				{Ordinal: 0, Capacity: 8},
				{Ordinal: 1, Capacity: 8},
				{Ordinal: 2, Capacity: 8},
				{Ordinal: 3, Capacity: 6},
				{Ordinal: 4, Capacity: 6},
			},
		},
		Stats:   stats,
		Actions: actions,
	}
}

func snapshotWithHUD(characterID uuid.UUID, saveSequence int64, marker string) charstore.Snapshot {
	hud := hudFixture(marker, uint64(saveSequence)*10)
	return charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Level:       1,
			Health:      100,
			SaveSeq:     saveSequence,
		},
		Inventory: []charstore.InventoryItem{{
			Slot: 0, InstanceID: uint64(saveSequence) * 100, ItemID: "item." + marker, Quantity: 1,
		}},
		HUD: &hud,
	}
}
