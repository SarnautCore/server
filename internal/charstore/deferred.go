package charstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SarnautCore/server/internal/scriptqueue"
)

func (store *postgresStore) EnqueueDeferredScript(
	ctx context.Context, work scriptqueue.Work,
) (scriptqueue.Work, error) {
	if work.AvailableAtMS == 0 {
		work.AvailableAtMS = work.DueAtMS
	}
	if err := scriptqueue.Validate(work); err != nil {
		return scriptqueue.Work{}, err
	}
	const insert = `INSERT INTO shard.deferred_script_impacts
		(id, zone_id, scope_kind, scope_id, due_at_ms, available_at_ms, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (id) DO NOTHING
        RETURNING sequence`
	err := store.db.QueryRow(ctx, insert, work.ID, work.ZoneID, work.ScopeKind, work.ScopeID,
		work.DueAtMS, work.AvailableAtMS, work.Payload).Scan(&work.Sequence)
	if err == nil {
		return work, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return scriptqueue.Work{}, fmt.Errorf("store: enqueue deferred script %q: %w", work.ID, err)
	}
	var existing scriptqueue.Work
	const load = `SELECT id, zone_id, scope_kind, scope_id, due_at_ms, available_at_ms,
        sequence, payload FROM shard.deferred_script_impacts WHERE id = $1`
	if err := store.db.QueryRow(ctx, load, work.ID).Scan(
		&existing.ID, &existing.ZoneID, &existing.ScopeKind, &existing.ScopeID, &existing.DueAtMS,
		&existing.AvailableAtMS, &existing.Sequence, &existing.Payload,
	); err != nil {
		return scriptqueue.Work{}, fmt.Errorf("store: load deferred script %q: %w", work.ID, err)
	}
	if !scriptqueue.Equivalent(existing, work) {
		return scriptqueue.Work{}, scriptqueue.ErrConflict
	}
	return existing, nil
}
