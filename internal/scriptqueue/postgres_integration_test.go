//go:build integration

package scriptqueue_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/scriptqueue"
)

func TestPostgresQueueFencesLeasesAndPreservesScopeOrder(t *testing.T) {
	dsn := os.Getenv("SARNAUT_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SARNAUT_POSTGRES_DSN is not set")
	}
	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator() error = %v", err)
	}
	if err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("migrate queue schema: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store, err := scriptqueue.NewPostgres(pool)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}

	prefix := "integration|" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx,
			`DELETE FROM shard.deferred_script_impacts WHERE id LIKE $1`, prefix+"%",
		); err != nil {
			t.Errorf("clean queue fixture: %v", err)
		}
	})
	work := func(suffix string) scriptqueue.Work {
		return scriptqueue.Work{
			ID: prefix + "|" + suffix, ZoneID: "zone.integration",
			ScopeKind: scriptqueue.ScopeActivation, ScopeID: prefix,
			DueAtMS: 10, Payload: []byte(`{"schema":1}`),
		}
	}
	first, second := work("first"), work("second")
	for _, row := range []scriptqueue.Work{first, second} {
		if _, err := store.Enqueue(t.Context(), row); err != nil {
			t.Fatalf("Enqueue(%q) error = %v", row.ID, err)
		}
	}
	claim, err := store.Claim(t.Context(), second.ID, "worker", "second-token", 10, 100)
	if err != nil || claim.State != scriptqueue.ClaimBlocked {
		t.Fatalf("Claim(second) = %#v, %v, want blocked", claim, err)
	}
	claim, err = store.Claim(t.Context(), first.ID, "worker", "old-token", 10, 20)
	if err != nil || claim.State != scriptqueue.ClaimAcquired {
		t.Fatalf("first Claim(first) = %#v, %v", claim, err)
	}
	claim, err = store.Claim(t.Context(), first.ID, "worker", "new-token", 20, 30)
	if err != nil || claim.State != scriptqueue.ClaimAcquired || claim.Work.Attempts != 2 {
		t.Fatalf("recovered Claim(first) = %#v, %v", claim, err)
	}
	if err := store.Complete(t.Context(), first.ID, "worker", "old-token"); !errors.Is(err, scriptqueue.ErrLeaseLost) {
		t.Fatalf("stale Complete() error = %v, want ErrLeaseLost", err)
	}
	if err := store.Complete(t.Context(), first.ID, "worker", "new-token"); err != nil {
		t.Fatalf("current Complete() error = %v", err)
	}
	claim, err = store.Claim(t.Context(), second.ID, "worker", "second-token-2", 20, 30)
	if err != nil || claim.State != scriptqueue.ClaimAcquired {
		t.Fatalf("Claim(second after first) = %#v, %v, want acquired", claim, err)
	}
}
