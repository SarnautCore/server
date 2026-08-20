package store_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/store"
	"github.com/google/uuid"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func snapshotFor(characterID uuid.UUID, saveSeq int64) store.Snapshot {
	return store.Snapshot{
		State: store.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Level:       1,
			Health:      100,
			SaveSeq:     saveSeq,
		},
	}
}

// A full queue is the case this whole design exists for: the tick must keep
// running, so the snapshot is dropped and counted rather than waited on.
func TestSaveWorkerDropsWhenTheQueueIsFullInsteadOfBlocking(t *testing.T) {
	t.Parallel()

	const capacity = 2
	worker := store.NewSaveWorker(store.NewMemory(), discardLogger(), capacity, time.Second)
	characterID := uuid.New()

	for index := range capacity {
		if !worker.Enqueue(snapshotFor(characterID, int64(index+1))) {
			t.Fatalf("Enqueue %d was refused while the queue had room", index)
		}
	}
	if worker.Depth() != capacity {
		t.Fatalf("queue depth = %d, want %d", worker.Depth(), capacity)
	}

	// Nothing is draining the queue, so this offer has nowhere to go. It must
	// return promptly rather than park the caller.
	returned := make(chan bool, 1)
	go func() { returned <- worker.Enqueue(snapshotFor(characterID, 99)) }()

	select {
	case accepted := <-returned:
		if accepted {
			t.Fatal("Enqueue accepted a snapshot with a full queue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked on a full queue; the tick would have stalled")
	}

	if dropped := worker.Dropped(); dropped != 1 {
		t.Errorf("Dropped() = %d, want 1", dropped)
	}
	if worker.Depth() != capacity {
		t.Errorf("queue depth = %d after the drop, want %d", worker.Depth(), capacity)
	}

	// Further drops keep counting, so the metric shows the size of the incident
	// rather than just its existence.
	for range 3 {
		worker.Enqueue(snapshotFor(characterID, 100))
	}
	if dropped := worker.Dropped(); dropped != 4 {
		t.Errorf("Dropped() = %d after four refused offers, want 4", dropped)
	}
}

func TestSaveWorkerPersistsQueuedSnapshots(t *testing.T) {
	t.Parallel()

	repository := store.NewMemory()
	worker := store.NewSaveWorker(repository, discardLogger(), 8, time.Second)
	characterID := uuid.New()

	snapshot := snapshotFor(characterID, 1)
	snapshot.Inventory = []store.InventoryItem{{Slot: 0, ItemID: "item.sword-rusty", Quantity: 1}}
	snapshot.Quests = []store.QuestState{{QuestID: "quest.league.first-blood", State: "accepted"}}
	if !worker.Enqueue(snapshot) {
		t.Fatal("Enqueue was refused with an empty queue")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	deadline := time.After(5 * time.Second)
	for worker.Persisted() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not persist the queued snapshot")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	state, err := repository.LoadCharacterState(t.Context(), characterID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.SaveSeq != 1 {
		t.Errorf("save_seq = %d, want 1", state.SaveSeq)
	}
	items, err := repository.LoadInventory(t.Context(), characterID)
	if err != nil || len(items) != 1 {
		t.Errorf("inventory = %+v (err %v), want one item", items, err)
	}
}

// Shutdown must not throw away checkpoints that were already accepted
// (ADR 0031 §5.6).
func TestSaveWorkerDrainsOnShutdown(t *testing.T) {
	t.Parallel()

	repository := store.NewMemory()
	worker := store.NewSaveWorker(repository, discardLogger(), 8, time.Second)

	characters := make([]uuid.UUID, 3)
	for index := range characters {
		characters[index] = uuid.New()
		if !worker.Enqueue(snapshotFor(characters[index], 1)) {
			t.Fatalf("Enqueue %d was refused", index)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	worker.Run(ctx)

	if worker.Persisted() != uint64(len(characters)) {
		t.Fatalf("Persisted() = %d, want %d", worker.Persisted(), len(characters))
	}
	for _, characterID := range characters {
		if _, err := repository.LoadCharacterState(t.Context(), characterID); err != nil {
			t.Errorf("load state for %v: %v", characterID, err)
		}
	}
}

// A superseded save is an expected outcome on reconnect, not an error to alert
// on, so it is counted separately from failures.
func TestSaveWorkerCountsSupersededSavesSeparately(t *testing.T) {
	t.Parallel()

	repository := store.NewMemory()
	worker := store.NewSaveWorker(repository, discardLogger(), 8, time.Second)
	characterID := uuid.New()

	if err := store.SaveNow(t.Context(), repository, snapshotFor(characterID, 5), time.Second); err != nil {
		t.Fatalf("save now: %v", err)
	}
	worker.Enqueue(snapshotFor(characterID, 2))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	worker.Run(ctx)

	if worker.Stale() != 1 {
		t.Errorf("Stale() = %d, want 1", worker.Stale())
	}
	if worker.Failed() != 0 {
		t.Errorf("Failed() = %d, want 0", worker.Failed())
	}
}
