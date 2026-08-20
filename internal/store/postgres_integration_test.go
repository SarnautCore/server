//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// requirePostgresDSN returns the configured DSN or skips with a reason that says
// exactly what to do about it, so an unconfigured machine reports an honest skip
// rather than a red suite.
func requirePostgresDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Logf("%s is not set; %s", postgresDSNEnvironment, dsnHint)
		t.Skipf("skipping database-backed test: %s is not set", postgresDSNEnvironment)
	}
	return dsn
}

var (
	sharedPoolOnce sync.Once
	sharedPool     *pgxpool.Pool
	sharedPoolErr  error
)

// migratedPool brings the database to the current schema once per test binary
// and hands every test the same pool. Migrating per test would serialize on
// goose's advisory lock for no benefit.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := requirePostgresDSN(t)

	sharedPoolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		migrator, err := store.NewMigrator(dsn)
		if err != nil {
			sharedPoolErr = err
			return
		}
		defer func() { _ = migrator.Close() }()

		if err := migrator.Up(ctx); err != nil {
			sharedPoolErr = err
			return
		}
		sharedPool, sharedPoolErr = pgxpool.New(ctx, dsn)
	})

	if sharedPoolErr != nil {
		t.Fatalf("prepare test database: %v", sharedPoolErr)
	}
	return sharedPool
}

// newIntegrationRepository returns a repository over an empty schema. Truncating
// per test keeps the globally unique character names from leaking between cases
// and makes a re-run behave like a first run.
func newIntegrationRepository(t *testing.T) store.Repository {
	t.Helper()
	pool := migratedPool(t)

	const truncate = `
		TRUNCATE shard.character_quests, shard.character_inventory, shard.character_state,
		         auth.name_reservations, auth.characters, auth.accounts
		RESTART IDENTITY CASCADE`
	if _, err := pool.Exec(t.Context(), truncate); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}

	repository, err := store.NewPostgres(pool)
	if err != nil {
		t.Fatalf("create postgres store: %v", err)
	}
	return repository
}

func TestPostgresRepositoryConformance(t *testing.T) {
	runRepositoryConformance(t, newIntegrationRepository)
}

// Every Down section is proven rather than assumed. This is the test the
// reversibility promise in ADR 0031 §3 rests on.
func TestMigrationsUpDownToZeroAndUpAgain(t *testing.T) {
	dsn := requirePostgresDSN(t)
	// Force the shared pool to exist first, so this test's down-to-0 cannot race
	// a lazily migrating sibling.
	migratedPool(t)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	migrator, err := store.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	defer func() {
		if err := migrator.Close(); err != nil {
			t.Errorf("close migrator: %v", err)
		}
	}()

	if err := migrator.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to zero from the applied schema: %v", err)
	}
	if version, err := migrator.Version(ctx); err != nil || version != 0 {
		t.Fatalf("version after down-to 0 = %d (err %v), want 0", version, err)
	}
	assertSchemaAbsent(t, ctx, dsn, "auth")
	assertSchemaAbsent(t, ctx, dsn, "shard")

	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("up from empty: %v", err)
	}
	version, err := migrator.Version(ctx)
	if err != nil || version < 1 {
		t.Fatalf("version after up = %d (err %v), want at least 1", version, err)
	}

	if err := migrator.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to zero again: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	t.Logf("migrations applied, reversed to zero and re-applied; schema version %d", version)
}

func assertSchemaAbsent(t *testing.T, ctx context.Context, dsn, schema string) {
	t.Helper()

	connection, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to check schema %q: %v", schema, err)
	}
	defer connection.Close()

	var exists bool
	const statement = `SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`
	if err := connection.QueryRow(ctx, statement, schema).Scan(&exists); err != nil {
		t.Fatalf("query schema %q: %v", schema, err)
	}
	if exists {
		t.Errorf("schema %q still exists after down-to 0", schema)
	}
}

// The name is unique because the database says so. This asserts the constraint
// itself, not the Go code that maps its error, so deleting the mapping cannot
// make the test pass by accident.
func TestDuplicateCharacterNameIsRejectedByTheDatabaseConstraint(t *testing.T) {
	repository := newIntegrationRepository(t)
	pool := migratedPool(t)
	ctx := t.Context()

	account := newAccount()
	if err := repository.CreateAccount(ctx, account); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, "Anne")); err != nil {
		t.Fatalf("create character: %v", err)
	}

	const rawInsert = `
		INSERT INTO auth.characters (character_id, account_id, name, name_normalized, chargen_option_id)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := pool.Exec(ctx, rawInsert, uuid.New(), account.AccountID, "Anne", "anne", "chargen.league.warrior")

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("raw duplicate insert error = %v, want a PostgreSQL error", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("SQLSTATE = %s, want 23505 (unique_violation)", pgErr.Code)
	}
	if pgErr.ConstraintName != "characters_name_normalized_key" {
		t.Errorf("constraint = %q, want characters_name_normalized_key", pgErr.ConstraintName)
	}
	t.Logf("database refused the duplicate: SQLSTATE %s on %s", pgErr.Code, pgErr.ConstraintName)

	// And the store maps exactly that error, rather than pre-checking.
	if err := repository.CreateCharacter(ctx, newCharacter(account.AccountID, "Anne")); !errors.Is(err, store.ErrNameTaken) {
		t.Errorf("CreateCharacter error = %v, want ErrNameTaken", err)
	}
}

// citext is what makes "anne" and "Anne" the same normalized name, so a missing
// extension would silently weaken uniqueness rather than fail loudly.
func TestNormalizedNameUniquenessIsCaseInsensitive(t *testing.T) {
	repository := newIntegrationRepository(t)
	ctx := t.Context()

	account := newAccount()
	if err := repository.CreateAccount(ctx, account); err != nil {
		t.Fatalf("create account: %v", err)
	}

	first := newCharacter(account.AccountID, "Anne")
	first.NameNormalized = "anne"
	if err := repository.CreateCharacter(ctx, first); err != nil {
		t.Fatalf("create first character: %v", err)
	}

	second := newCharacter(account.AccountID, "Anne")
	second.NameNormalized = "ANNE"
	if err := repository.CreateCharacter(ctx, second); !errors.Is(err, store.ErrNameTaken) {
		t.Errorf("create character with ANNE error = %v, want ErrNameTaken", err)
	}
}

// A rollback has to be observable from a second connection, which is the part an
// in-memory double cannot prove.
func TestRunInTxRollbackIsInvisibleToOtherConnections(t *testing.T) {
	repository := newIntegrationRepository(t)
	pool := migratedPool(t)
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
		if err := tx.UpsertQuestState(ctx, characterID, store.QuestState{
			QuestID: "quest.league.first-blood",
			State:   "accepted",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx error = %v, want the sentinel", err)
	}

	assertRowCount(t, ctx, pool, `SELECT count(*) FROM auth.characters WHERE account_id = $1`, 0, account.AccountID)
	assertRowCount(t, ctx, pool, `SELECT count(*) FROM shard.character_state WHERE character_id = $1`, 0, characterID)
	assertRowCount(t, ctx, pool, `SELECT count(*) FROM shard.character_quests WHERE character_id = $1`, 0, characterID)
}

// Committing must be equally visible, otherwise the test above would also pass
// against a RunInTx that never wrote anything.
func TestRunInTxCommitIsVisibleToOtherConnections(t *testing.T) {
	repository := newIntegrationRepository(t)
	pool := migratedPool(t)
	ctx := t.Context()

	account := newAccount()
	if err := repository.CreateAccount(ctx, account); err != nil {
		t.Fatalf("create account: %v", err)
	}

	characterID := uuid.New()
	err := store.SaveCharacter(ctx, repository, store.Snapshot{
		State: store.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Level:       1,
			Health:      100,
			SaveSeq:     1,
		},
		Inventory: []store.InventoryItem{
			{Slot: 0, ItemID: "item.sword-rusty", Quantity: 1},
			{Slot: 1, ItemID: "item.potion-minor", Quantity: 5},
		},
		Quests: []store.QuestState{{QuestID: "quest.league.first-blood", State: "accepted"}},
	})
	if err != nil {
		t.Fatalf("save character: %v", err)
	}

	assertRowCount(t, ctx, pool, `SELECT count(*) FROM shard.character_state WHERE character_id = $1`, 1, characterID)
	assertRowCount(t, ctx, pool, `SELECT count(*) FROM shard.character_inventory WHERE character_id = $1`, 2, characterID)
	assertRowCount(t, ctx, pool, `SELECT count(*) FROM shard.character_quests WHERE character_id = $1`, 1, characterID)
}

// The save worker under a real database: the checkpoint path end to end.
func TestSaveWorkerPersistsAgainstPostgres(t *testing.T) {
	repository := newIntegrationRepository(t)
	worker := store.NewSaveWorker(repository, discardLogger(), 8, 5*time.Second)
	characterID := uuid.New()

	snapshot := snapshotFor(characterID, 1)
	snapshot.Inventory = []store.InventoryItem{{Slot: 0, ItemID: "item.sword-rusty", Quantity: 1}}
	if !worker.Enqueue(snapshot) {
		t.Fatal("Enqueue was refused with an empty queue")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	worker.Run(ctx)

	if worker.Persisted() != 1 || worker.Failed() != 0 || worker.Dropped() != 0 {
		t.Fatalf("worker counters: persisted %d failed %d dropped %d, want 1/0/0",
			worker.Persisted(), worker.Failed(), worker.Dropped())
	}
	if _, err := repository.LoadCharacterState(t.Context(), characterID); err != nil {
		t.Errorf("load state after the worker ran: %v", err)
	}
}

// The migration creates the roles that make table ownership a database fact
// rather than a convention (ADR 0031 §4).
func TestOwnershipRolesExist(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()

	for _, role := range []string{"sarnaut_auth", "sarnaut_shard"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).
			Scan(&exists); err != nil {
			t.Fatalf("query role %q: %v", role, err)
		}
		if !exists {
			t.Errorf("role %q was not created by the migration", role)
		}
	}
}

func assertRowCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	statement string,
	want int,
	arguments ...any,
) {
	t.Helper()

	var count int
	if err := pool.QueryRow(ctx, statement, arguments...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != want {
		t.Errorf("row count = %d, want %d (%s)", count, want, statement)
	}
}
