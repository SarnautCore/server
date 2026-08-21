package scriptqueue

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func testWork(id, scope string, due int64) Work {
	return Work{
		ID: id, ZoneID: "zone.inst-league1", ScopeID: scope,
		DueAtMS: due, Payload: []byte(`{"node":"` + id + `"}`),
	}
}

func TestMemoryStoreOrdersAndDeduplicatesWork(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	ctx := t.Context()
	for _, work := range []Work{
		testWork("later", "quest-a", 20),
		testWork("first", "quest-a", 10),
		testWork("second", "quest-a", 10),
	} {
		if _, err := store.Enqueue(ctx, work); err != nil {
			t.Fatalf("Enqueue(%q) error = %v", work.ID, err)
		}
	}
	duplicate, err := store.Enqueue(ctx, testWork("first", "quest-a", 10))
	if err != nil {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	}
	rows, err := store.LoadZone(ctx, "zone.inst-league1")
	if err != nil {
		t.Fatalf("LoadZone() error = %v", err)
	}
	if len(rows) != 3 || rows[0].ID != "first" || rows[1].ID != "second" || rows[2].ID != "later" {
		t.Fatalf("ordered ids = %#v, want first, second, later", rowIDs(rows))
	}
	if duplicate.Sequence != rows[0].Sequence {
		t.Fatalf("duplicate sequence = %d, want original %d", duplicate.Sequence, rows[0].Sequence)
	}
	conflict := testWork("first", "quest-a", 11)
	if _, err := store.Enqueue(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Enqueue() error = %v, want ErrConflict", err)
	}
}

func TestEarlierFailureBlocksOnlyItsOwnScope(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	ctx := t.Context()
	for _, work := range []Work{
		testWork("a1", "quest-a", 10), testWork("a2", "quest-a", 10),
		testWork("b1", "quest-b", 10),
	} {
		if _, err := store.Enqueue(ctx, work); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := store.Claim(ctx, "a2", "worker", 10, 100)
	if err != nil || claim.State != ClaimBlocked {
		t.Fatalf("Claim(a2) = %#v, %v, want blocked", claim, err)
	}
	claim, err = store.Claim(ctx, "b1", "worker", 10, 100)
	if err != nil || claim.State != ClaimAcquired {
		t.Fatalf("Claim(b1) = %#v, %v, want acquired", claim, err)
	}
	claim, err = store.Claim(ctx, "a1", "worker", 10, 100)
	if err != nil || claim.State != ClaimAcquired || claim.Work.Attempts != 1 {
		t.Fatalf("Claim(a1) = %#v, %v, want first attempt", claim, err)
	}
	if err := store.Retry(ctx, "a1", "worker", 20, "temporary"); err != nil {
		t.Fatal(err)
	}
	claim, err = store.Claim(ctx, "a2", "worker", 20, 100)
	if err != nil || claim.State != ClaimBlocked {
		t.Fatalf("Claim(a2 after retry) = %#v, %v, want blocked", claim, err)
	}
	claim, err = store.Claim(ctx, "a1", "worker-2", 20, 100)
	if err != nil || claim.State != ClaimAcquired || claim.Work.Attempts != 2 {
		t.Fatalf("retry Claim(a1) = %#v, %v, want second attempt", claim, err)
	}
	if err := store.Complete(ctx, "a1", "worker-2"); err != nil {
		t.Fatal(err)
	}
	claim, err = store.Claim(ctx, "a2", "worker", 20, 100)
	if err != nil || claim.State != ClaimAcquired {
		t.Fatalf("Claim(a2 after complete) = %#v, %v, want acquired", claim, err)
	}
}

func TestTransactionRollbackRestoresQueue(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	sentinel := errors.New("abort")
	err := store.RunInTx(t.Context(), func(ctx context.Context, tx Store) error {
		if _, err := tx.Enqueue(ctx, testWork("rolled-back", "quest", 1)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx() error = %v, want abort", err)
	}
	rows, err := store.LoadZone(t.Context(), "zone.inst-league1")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after rollback = %#v, %v, want none", rows, err)
	}
}

func TestConcurrentClaimHasOneWinner(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	if _, err := store.Enqueue(t.Context(), testWork("one", "quest", 1)); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan Claim, 2)
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claim, err := store.Claim(context.Background(), "one", owner, 1, 100)
			results <- claim
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	acquired := 0
	for result := range results {
		if result.State == ClaimAcquired {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired claims = %d, want 1", acquired)
	}
}

func TestExpiredLeaseCanBeRecoveredButOldOwnerCannotComplete(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	if _, err := store.Enqueue(t.Context(), testWork("one", "quest", 1)); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.Claim(t.Context(), "one", "dead-worker", 1, 10); err != nil || claim.State != ClaimAcquired {
		t.Fatalf("first Claim() = %#v, %v", claim, err)
	}
	claim, err := store.Claim(t.Context(), "one", "restart-worker", 10, 20)
	if err != nil || claim.State != ClaimAcquired || claim.Work.Attempts != 2 {
		t.Fatalf("recovery Claim() = %#v, %v", claim, err)
	}
	if err := store.Complete(t.Context(), "one", "dead-worker"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old Complete() error = %v, want ErrLeaseLost", err)
	}
	if err := store.Complete(t.Context(), "one", "restart-worker"); err != nil {
		t.Fatalf("new Complete() error = %v", err)
	}
}

func rowIDs(rows []Work) []string {
	ids := make([]string, len(rows))
	for index := range rows {
		ids[index] = rows[index].ID
	}
	return ids
}
