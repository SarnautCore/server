//go:build integration

package currency

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

func TestPostgresLedgerPersistsAndSerializesTheLastUnit(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("%s is not set", postgresDSNEnvironment)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	defer func() { _ = migrator.Close() }()
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	defer pool.Close()

	characterID := uuid.New()
	ledger, err := NewPostgres(pool)
	if err != nil {
		t.Fatalf("new postgres ledger: %v", err)
	}
	if _, err := ledger.Credit(ctx, characterID, WorldChatResource(), 1); err != nil {
		t.Fatalf("credit world chat: %v", err)
	}
	if _, err := ledger.Credit(ctx, characterID, ZoneChatSpecialResource(), 2); err != nil {
		t.Fatalf("credit zone special: %v", err)
	}

	reloaded, err := NewPostgres(pool)
	if err != nil {
		t.Fatalf("reconstruct postgres ledger: %v", err)
	}
	assertBalance(t, reloaded, characterID, WorldChatResource(), 1)
	assertBalance(t, reloaded, characterID, ZoneChatSpecialResource(), 2)

	const attempts = 64
	results := make(chan bool, attempts)
	errorsSeen := make(chan error, attempts)
	var group sync.WaitGroup
	for attempt := 0; attempt < attempts; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			spent, err := reloaded.Spend(ctx, characterID, WorldChatResource(), 1)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- spent
		}()
	}
	group.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent postgres Spend() error: %v", err)
	}
	accepted := 0
	for spent := range results {
		if spent {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("successful postgres spends = %d, want 1", accepted)
	}
	assertBalance(t, reloaded, characterID, WorldChatResource(), 0)
	assertBalance(t, reloaded, characterID, ZoneChatSpecialResource(), 2)
}

func TestPostgresRejectedCreditLeavesCommittedBalanceUnchanged(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("%s is not set", postgresDSNEnvironment)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	defer func() { _ = migrator.Close() }()
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	defer pool.Close()

	ledger, err := NewPostgres(pool)
	if err != nil {
		t.Fatalf("new postgres ledger: %v", err)
	}
	characterID := uuid.New()
	if _, err := ledger.Credit(ctx, characterID, ZoneChatSpecialResource(), MaxBalance); err != nil {
		t.Fatalf("credit maximum: %v", err)
	}
	if _, err := ledger.Credit(ctx, characterID, ZoneChatSpecialResource(), 1); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("overflow credit error = %v, want ErrBalanceOverflow", err)
	}
	assertBalance(t, ledger, characterID, ZoneChatSpecialResource(), MaxBalance)
}
