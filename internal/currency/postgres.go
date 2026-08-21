package currency

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgres constructs a ledger over an already-open shared pool. The caller
// owns the pool lifetime.
func NewPostgres(pool *pgxpool.Pool) (*Ledger, error) {
	if pool == nil {
		return nil, errors.New("currency: postgres pool is required")
	}
	return newLedger(&postgresStore{pool: pool}), nil
}

func (store *postgresStore) Balance(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
) (uint64, error) {
	const statement = `
		SELECT balance
		FROM shard.character_alternative_currency
		WHERE character_id = $1 AND resource_id = $2`

	var balance int64
	err := store.pool.QueryRow(ctx, statement, characterID, int64(resourceID)).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read alternative currency balance: %w", err)
	}
	return uint64(balance), nil
}

func (store *postgresStore) Credit(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
	amount uint64,
) (uint64, error) {
	// One statement is one PostgreSQL transaction. The conflict predicate also
	// keeps arithmetic inside bigint's range instead of relying on overflow to
	// abort the statement.
	const statement = `
		INSERT INTO shard.character_alternative_currency (
			character_id, resource_id, balance, updated_at
		) VALUES ($1, $2, $3, now())
		ON CONFLICT (character_id, resource_id) DO UPDATE SET
			balance = character_alternative_currency.balance + EXCLUDED.balance,
			updated_at = now()
		WHERE character_alternative_currency.balance <= $4 - EXCLUDED.balance
		RETURNING balance`

	var balance int64
	err := store.pool.QueryRow(
		ctx,
		statement,
		characterID,
		int64(resourceID),
		int64(amount),
		int64(MaxBalance),
	).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBalanceOverflow
	}
	if err != nil {
		return 0, fmt.Errorf("credit alternative currency: %w", err)
	}
	return uint64(balance), nil
}

func (store *postgresStore) DebitOne(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
) (bool, error) {
	// The balance predicate and decrement happen under one row lock in one SQL
	// statement. Exactly one of concurrent spenders can claim the last unit.
	const statement = `
		UPDATE shard.character_alternative_currency
		SET balance = balance - 1, updated_at = now()
		WHERE character_id = $1 AND resource_id = $2 AND balance >= 1`

	tag, err := store.pool.Exec(ctx, statement, characterID, int64(resourceID))
	if err != nil {
		return false, fmt.Errorf("debit alternative currency: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
