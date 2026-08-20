package store_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/store"
)

// templates is a [store.ChargenTemplates] over a map, standing in for what the
// shard builds from the pack's chargen table.
type templates map[string]store.Snapshot

func (set templates) Template(chargenOptionID string) (store.Snapshot, bool) {
	snapshot, ok := set[chargenOptionID]
	return snapshot, ok
}

const testOption = "chargen.league.warrior"

func testTemplates() templates {
	return templates{testOption: {
		State: store.CharacterState{
			Position: store.Vec3{X: 12, Y: 4.5},
			Heading:  1.5,
			Level:    1,
			Health:   100,
		},
		Inventory: []store.InventoryItem{{Slot: 0, ItemID: "item.consumable.harbor-tonic", Quantity: 3}},
		Quests:    []store.QuestState{{QuestID: "quest.paper-harbor.mossy-gate", State: "offered"}},
	}}
}

func newCharacterService(t *testing.T) (*store.CharacterService, store.Repository) {
	t.Helper()
	repository := store.NewMemory()
	worker := store.NewSaveWorker(repository, slog.New(slog.DiscardHandler), 8, 0)
	return store.NewCharacterService(repository, testTemplates(), worker, slog.New(slog.DiscardHandler), 0), repository
}

// A first login has identity but no state, so the load materializes one from
// the chargen template — spawn, level, starting loadout and starting quests, in
// one transaction, before any spawn is granted (ADR 0032 §5.4).
func TestLoadMaterializesAFirstLoginFromTheChargenTemplate(t *testing.T) {
	t.Parallel()

	service, repository := newCharacterService(t)
	characterID := uuid.New()

	snapshot, err := service.Load(t.Context(), characterID, testOption, "InstLeague1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.State.CharacterID != characterID {
		t.Errorf("materialized character = %s, want %s", snapshot.State.CharacterID, characterID)
	}
	if snapshot.State.ZoneID != "InstLeague1" {
		t.Errorf("zone_id = %q, want the zone the caller stamped", snapshot.State.ZoneID)
	}
	if snapshot.State.Position != (store.Vec3{X: 12, Y: 4.5}) {
		t.Errorf("position = %+v, want the template's spawn", snapshot.State.Position)
	}
	if len(snapshot.Inventory) != 1 || snapshot.Inventory[0].Quantity != 3 {
		t.Errorf("inventory = %+v, want the starting loadout", snapshot.Inventory)
	}
	if len(snapshot.Quests) != 1 {
		t.Errorf("quests = %+v, want the starting quest", snapshot.Quests)
	}

	// It is committed, not merely returned: the spawn is granted after the
	// write, so a crash in between replays as "you already have it".
	stored, err := repository.LoadCharacterState(t.Context(), characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if stored.Position != snapshot.State.Position {
		t.Errorf("stored position = %+v, want %+v", stored.Position, snapshot.State.Position)
	}
	items, err := repository.LoadInventory(t.Context(), characterID)
	if err != nil || len(items) != 1 {
		t.Errorf("stored inventory = %+v, %v; want the starting loadout", items, err)
	}
}

// Materialization is idempotent on character_id: a redeem that races or retries
// produces one character with one set of starting gear, not two.
func TestLoadIsIdempotentAndDoesNotRegrantStartingGear(t *testing.T) {
	t.Parallel()

	service, repository := newCharacterService(t)
	characterID := uuid.New()

	if _, err := service.Load(t.Context(), characterID, testOption, "InstLeague1"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Spend the starting gear, as a session would.
	if err := repository.ReplaceInventory(t.Context(), characterID, nil); err != nil {
		t.Fatalf("ReplaceInventory() error = %v", err)
	}

	second, err := service.Load(t.Context(), characterID, testOption, "InstLeague1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(second.Inventory) != 0 {
		t.Errorf("the second load re-granted the starting loadout: %+v", second.Inventory)
	}
	if second.State.SaveSeq != 1 {
		t.Errorf("save_seq = %d after a reload, want the stored 1", second.State.SaveSeq)
	}
}

// A returning character loads what was saved, which is what puts a reconnect
// back where it logged out rather than at the chargen spawn.
func TestLoadReturnsTheSavedStateRatherThanTheTemplate(t *testing.T) {
	t.Parallel()

	service, repository := newCharacterService(t)
	characterID := uuid.New()

	first, err := service.Load(t.Context(), characterID, testOption, "InstLeague1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	moved := first
	moved.State.Position = store.Vec3{X: 51.9, Y: 4.5}
	moved.State.SaveSeq = first.State.SaveSeq + 1
	if err := store.SaveCharacter(t.Context(), repository, moved); err != nil {
		t.Fatalf("SaveCharacter() error = %v", err)
	}

	reloaded, err := service.Load(t.Context(), characterID, testOption, "InstLeague1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if reloaded.State.Position != moved.State.Position {
		t.Errorf("position = %+v, want the saved %+v", reloaded.State.Position, moved.State.Position)
	}
	if reloaded.State.SaveSeq != moved.State.SaveSeq {
		t.Errorf("save_seq = %d, want the saved %d", reloaded.State.SaveSeq, moved.State.SaveSeq)
	}
}

// An option the pack does not carry is fatal for that login. Materializing from
// a guess is how a player ends up naked in the void.
func TestLoadRefusesAnUnknownChargenOption(t *testing.T) {
	t.Parallel()

	service, _ := newCharacterService(t)
	_, err := service.Load(t.Context(), uuid.New(), "chargen.league.mage", "InstLeague1")
	if !errors.Is(err, store.ErrNoTemplate) {
		t.Errorf("Load() error = %v, want ErrNoTemplate", err)
	}
}

// Checkpoint hands the snapshot to the bounded worker and returns immediately.
// A caller that waited would be a caller that could stall a session.
func TestCheckpointEnqueuesWithoutWaiting(t *testing.T) {
	t.Parallel()

	repository := store.NewMemory()
	worker := store.NewSaveWorker(repository, slog.New(slog.DiscardHandler), 1, 0)
	service := store.NewCharacterService(repository, testTemplates(), worker, slog.New(slog.DiscardHandler), 0)
	characterID := uuid.New()

	snapshot, err := service.Load(t.Context(), characterID, testOption, "InstLeague1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	snapshot.State.SaveSeq++
	if !service.Checkpoint(snapshot) {
		t.Fatal("Checkpoint() reported a drop with an empty queue")
	}
	// The worker is not running, so the queue of one is now full and the next
	// checkpoint drops rather than blocking.
	snapshot.State.SaveSeq++
	if service.Checkpoint(snapshot) {
		t.Error("Checkpoint() blocked or grew the queue instead of dropping")
	}
	if worker.Dropped() != 1 {
		t.Errorf("Dropped() = %d, want 1", worker.Dropped())
	}
}
