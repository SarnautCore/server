package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/SarnautCore/server/migrations"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

// Migrator applies the embedded goose migrations.
//
// goose runs over `database/sql` rather than pgx's native interface, which is
// why this opens its own short-lived connection instead of borrowing the pool:
// `pgx/v5/stdlib` is a package of a module already required, so the adapter
// costs a driver registration and no new dependency (ADR 0031 §2).
//
// Each migration runs in its own transaction under a session-level advisory
// lock, so two shard replicas starting at once do not race and a failure leaves
// nothing half-applied.
type Migrator struct {
	database *sql.DB
}

// NewMigrator opens a `database/sql` handle for dsn. The caller closes it.
func NewMigrator(dsn string) (*Migrator, error) {
	if dsn == "" {
		return nil, fmt.Errorf("store: a PostgreSQL DSN is required to migrate")
	}

	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open migration connection: %w", err)
	}
	if err := goose.SetDialect(migrations.Dialect); err != nil {
		return nil, fmt.Errorf("select goose dialect: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	return &Migrator{database: database}, nil
}

// Close releases the migration connection.
func (migrator *Migrator) Close() error {
	if migrator == nil || migrator.database == nil {
		return nil
	}
	if err := migrator.database.Close(); err != nil {
		return fmt.Errorf("close migration connection: %w", err)
	}
	return nil
}

// Up applies every pending migration.
func (migrator *Migrator) Up(ctx context.Context) error {
	if err := goose.UpContext(ctx, migrator.database, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Down rolls back the most recently applied migration.
func (migrator *Migrator) Down(ctx context.Context) error {
	if err := goose.DownContext(ctx, migrator.database, "."); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// DownTo rolls back to version, where 0 means an empty schema. CI runs
// `up`, `down-to 0`, `up` on every change, which is the only way the ADR 0031
// promise that every migration is reversible stays true rather than assumed.
func (migrator *Migrator) DownTo(ctx context.Context, version int64) error {
	if err := goose.DownToContext(ctx, migrator.database, ".", version); err != nil {
		return fmt.Errorf("roll back migrations to version %d: %w", version, err)
	}
	return nil
}

// Version reports the highest applied migration, or 0 on an empty database.
func (migrator *Migrator) Version(ctx context.Context) (int64, error) {
	version, err := goose.GetDBVersionContext(ctx, migrator.database)
	if err != nil {
		return 0, fmt.Errorf("read migration version: %w", err)
	}
	return version, nil
}

// Status prints the applied and pending migrations to goose's logger.
func (migrator *Migrator) Status(ctx context.Context) error {
	if err := goose.StatusContext(ctx, migrator.database, "."); err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	return nil
}

// Ping verifies the DSN reaches a live database, so a mistyped host fails with
// a connection error rather than a migration error.
func (migrator *Migrator) Ping(ctx context.Context) error {
	if err := migrator.database.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}
