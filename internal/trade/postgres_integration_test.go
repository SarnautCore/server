//go:build integration

package trade_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/trade"
)

const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

// TestPostgresFinalTradeCommitIsAtomicAndSingleUse is intentionally serial.
// Its three cases share the production PostgreSQL adapter and prove successful
// cross-transfer, rollback after a late write failure, and two concurrent
// attempts at the same confirmed offer committing at most once.
func TestPostgresFinalTradeCommitIsAtomicAndSingleUse(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("skipping database-backed trade test: %s is not set", postgresDSNEnvironment)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator() error = %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	defer pool.Close()
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}

	t.Run("success", func(t *testing.T) {
		first, second := seedPostgresPair(t, repository, pool, "success")
		store := postgresTradeStore(t, repository, first, second)
		result, err := store.Transfer(t.Context(), trade.Transfer{
			FirstCharacterID:  first,
			SecondCharacterID: second,
			FirstItems:        []trade.Item{{BagSlot: 0, ItemID: "sword", Count: 1}},
			SecondItems:       []trade.Item{{BagSlot: 0, ItemID: "gem", Count: 3}},
			FirstMoney:        10,
			SecondMoney:       5,
		})
		if err != nil {
			t.Fatalf("Transfer() error = %v", err)
		}
		if result.First.Money != 95 || result.Second.Money != 55 {
			t.Fatalf("committed purses = %d/%d, want 95/55", result.First.Money, result.Second.Money)
		}
		assertPostgresHoldings(t, repository, first, 95, "gem")
		assertPostgresHoldings(t, repository, second, 55, "sword")
	})

	t.Run("late failure rolls back both players", func(t *testing.T) {
		first, second := seedPostgresPair(t, repository, pool, "rollback")
		failing := &failCharacterSave{Repository: repository, characterID: second}
		store := postgresTradeStore(t, failing, first, second)
		_, err := store.Transfer(t.Context(), trade.Transfer{
			FirstCharacterID:  first,
			SecondCharacterID: second,
			FirstItems:        []trade.Item{{BagSlot: 0, ItemID: "sword", Count: 1}},
			SecondItems:       []trade.Item{{BagSlot: 0, ItemID: "gem", Count: 3}},
			FirstMoney:        10,
			SecondMoney:       5,
		})
		if err == nil {
			t.Fatal("Transfer() succeeded despite injected second-purse failure")
		}
		assertPostgresHoldings(t, repository, first, 100, "sword")
		assertPostgresHoldings(t, repository, second, 50, "gem")
	})

	t.Run("concurrent double commit", func(t *testing.T) {
		first, second := seedPostgresPair(t, repository, pool, "concurrent")
		store := postgresTradeStore(t, repository, first, second)
		request := trade.Transfer{
			FirstCharacterID:  first,
			SecondCharacterID: second,
			FirstItems:        []trade.Item{{BagSlot: 0, ItemID: "sword", Count: 1}},
			FirstMoney:        10,
		}
		results := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := store.Transfer(t.Context(), request)
				results <- err
			}()
		}
		wait.Wait()
		close(results)
		successes := 0
		failures := 0
		for err := range results {
			if err == nil {
				successes++
			} else {
				failures++
			}
		}
		if successes != 1 || failures != 1 {
			t.Fatalf("concurrent results success/failure = %d/%d, want 1/1", successes, failures)
		}
		assertPostgresHoldings(t, repository, first, 90, "tonic")
		assertPostgresHoldings(t, repository, second, 60, "sword")
	})
}

func seedPostgresPair(
	t *testing.T,
	repository charstore.Repository,
	pool *pgxpool.Pool,
	label string,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	accountID := uuid.New()
	first := uuid.New()
	second := uuid.New()
	suffix := accountID.String()[:8]
	if err := repository.CreateAccount(t.Context(), charstore.Account{
		AccountID: accountID, Email: fmt.Sprintf("trade-%s-%s@example.invalid", label, suffix), PasswordHash: "test",
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	for index, characterID := range []uuid.UUID{first, second} {
		if err := repository.CreateCharacter(t.Context(), charstore.Character{
			CharacterID: characterID,
			AccountID:   accountID,
			Name:        fmt.Sprintf("Tr%s%d", suffix, index),
		}); err != nil {
			t.Fatalf("create character %d: %v", index, err)
		}
	}
	if err := charstore.SaveCharacter(t.Context(), repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: first, ZoneID: "league", Level: 1, Health: 100, Currency: 100, SaveSeq: 1,
		},
		Inventory: []charstore.InventoryItem{
			{Slot: 0, ItemID: "sword", Quantity: 1},
			{Slot: 1, ItemID: "tonic", Quantity: 2},
		},
	}); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	if err := charstore.SaveCharacter(t.Context(), repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: second, ZoneID: "league", Level: 1, Health: 100, Currency: 50, SaveSeq: 1,
		},
		Inventory: []charstore.InventoryItem{{Slot: 0, ItemID: "gem", Quantity: 3}},
	}); err != nil {
		t.Fatalf("seed second: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM auth.accounts WHERE account_id = $1`, accountID)
	})
	return first, second
}

func postgresTradeStore(
	t *testing.T,
	repository charstore.Repository,
	first uuid.UUID,
	second uuid.UUID,
) *trade.RepositoryStore {
	t.Helper()
	store, err := trade.NewRepositoryStore(
		repository,
		limits{"sword": 1, "gem": 20, "tonic": 20},
		capacity{first: 8, second: 8},
		trade.UnboundItems{},
	)
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	return store
}

func assertPostgresHoldings(
	t *testing.T,
	repository charstore.Repository,
	characterID uuid.UUID,
	wantMoney int64,
	wantItem string,
) {
	t.Helper()
	state, err := repository.LoadCharacterState(t.Context(), characterID)
	if err != nil {
		t.Fatalf("load character state: %v", err)
	}
	if state.Currency != wantMoney {
		t.Fatalf("currency = %d, want %d", state.Currency, wantMoney)
	}
	items, err := repository.LoadInventory(t.Context(), characterID)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if _, found := storedItem(items, wantItem); !found {
		t.Fatalf("inventory = %+v, missing %q", items, wantItem)
	}
}
