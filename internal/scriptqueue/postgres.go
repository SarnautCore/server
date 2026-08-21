package scriptqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type postgresStore struct {
	pool *pgxpool.Pool
	db   querier
	inTx bool
}

func NewPostgres(pool *pgxpool.Pool) (Store, error) {
	if pool == nil {
		return nil, errors.New("script queue: postgres pool is required")
	}
	return &postgresStore{pool: pool, db: pool}, nil
}

func (store *postgresStore) RunInTx(ctx context.Context, run func(context.Context, Store) error) error {
	if store.inTx {
		return run(ctx, store)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("script queue: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	view := &postgresStore{pool: store.pool, db: tx, inTx: true}
	if err := run(ctx, view); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("script queue: commit transaction: %w", err)
	}
	return nil
}

const rowColumns = `id, zone_id, scope_id, due_at_ms, available_at_ms, sequence,
       payload, attempts, COALESCE(lease_owner, ''), COALESCE(lease_until_ms, 0),
       COALESCE(last_error, '')`

func scanWork(row pgx.Row) (Work, error) {
	var work Work
	err := row.Scan(&work.ID, &work.ZoneID, &work.ScopeID, &work.DueAtMS,
		&work.AvailableAtMS, &work.Sequence, &work.Payload, &work.Attempts,
		&work.LeaseOwner, &work.LeaseUntilMS, &work.LastError)
	return work, err
}

func (store *postgresStore) Enqueue(ctx context.Context, work Work) (Work, error) {
	if work.AvailableAtMS == 0 {
		work.AvailableAtMS = work.DueAtMS
	}
	if err := Validate(work); err != nil {
		return Work{}, err
	}
	const insert = `INSERT INTO shard.deferred_script_impacts
        (id, zone_id, scope_id, due_at_ms, available_at_ms, payload)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (id) DO NOTHING
        RETURNING ` + rowColumns
	inserted, err := scanWork(store.db.QueryRow(ctx, insert, work.ID, work.ZoneID,
		work.ScopeID, work.DueAtMS, work.AvailableAtMS, work.Payload))
	if err == nil {
		return inserted, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Work{}, fmt.Errorf("script queue: enqueue %q: %w", work.ID, err)
	}
	existing, err := scanWork(store.db.QueryRow(ctx,
		`SELECT `+rowColumns+` FROM shard.deferred_script_impacts WHERE id = $1`, work.ID))
	if err != nil {
		return Work{}, fmt.Errorf("script queue: load conflicting work %q: %w", work.ID, err)
	}
	if !sameWork(existing, work) {
		return Work{}, ErrConflict
	}
	return existing, nil
}

func (store *postgresStore) LoadZone(ctx context.Context, zoneID string) ([]Work, error) {
	rows, err := store.db.Query(ctx, `SELECT `+rowColumns+`
        FROM shard.deferred_script_impacts WHERE zone_id = $1
        ORDER BY due_at_ms, sequence`, zoneID)
	if err != nil {
		return nil, fmt.Errorf("script queue: load zone %q: %w", zoneID, err)
	}
	defer rows.Close()
	var result []Work
	for rows.Next() {
		work, err := scanWork(rows)
		if err != nil {
			return nil, fmt.Errorf("script queue: scan zone %q: %w", zoneID, err)
		}
		result = append(result, work)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("script queue: read zone %q: %w", zoneID, err)
	}
	return result, nil
}

func (store *postgresStore) Claim(
	ctx context.Context, id, owner string, nowMS, leaseUntilMS int64,
) (Claim, error) {
	if owner == "" || leaseUntilMS <= nowMS {
		return Claim{}, fmt.Errorf("script queue: invalid lease for %q", id)
	}
	var result Claim
	err := store.RunInTx(ctx, func(ctx context.Context, tx Store) error {
		view := tx.(*postgresStore)
		target, err := scanWork(view.db.QueryRow(ctx,
			`SELECT `+rowColumns+` FROM shard.deferred_script_impacts WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			result.State = ClaimMissing
			return nil
		}
		if err != nil {
			return err
		}
		var firstID string
		err = view.db.QueryRow(ctx, `SELECT id FROM shard.deferred_script_impacts
            WHERE zone_id = $1 AND scope_id = $2
            ORDER BY due_at_ms, sequence LIMIT 1 FOR UPDATE`, target.ZoneID, target.ScopeID).Scan(&firstID)
		if err != nil {
			return err
		}
		if firstID != id || target.AvailableAtMS > nowMS ||
			(target.LeaseOwner != "" && target.LeaseOwner != owner && target.LeaseUntilMS > nowMS) {
			result = Claim{State: ClaimBlocked, Work: target}
			return nil
		}
		const update = `UPDATE shard.deferred_script_impacts SET
            attempts = attempts + CASE WHEN lease_owner = $2 AND lease_until_ms > $3 THEN 0 ELSE 1 END,
            lease_owner = $2, lease_until_ms = $4
            WHERE id = $1 RETURNING ` + rowColumns
		claimed, err := scanWork(view.db.QueryRow(ctx, update, id, owner, nowMS, leaseUntilMS))
		if err != nil {
			return err
		}
		result = Claim{State: ClaimAcquired, Work: claimed}
		return nil
	})
	if err != nil {
		return Claim{}, fmt.Errorf("script queue: claim %q: %w", id, err)
	}
	return result, nil
}

func (store *postgresStore) Complete(ctx context.Context, id, owner string) error {
	tag, err := store.db.Exec(ctx,
		`DELETE FROM shard.deferred_script_impacts WHERE id = $1 AND lease_owner = $2`, id, owner)
	if err != nil {
		return fmt.Errorf("script queue: complete %q: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (store *postgresStore) Retry(
	ctx context.Context, id, owner string, availableAtMS int64, lastError string,
) error {
	tag, err := store.db.Exec(ctx, `UPDATE shard.deferred_script_impacts SET
        available_at_ms = $3, lease_owner = NULL, lease_until_ms = NULL, last_error = $4
        WHERE id = $1 AND lease_owner = $2`, id, owner, availableAtMS, lastError)
	if err != nil {
		return fmt.Errorf("script queue: retry %q: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}
