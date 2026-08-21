package charstore

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// DefaultSaveQueueSize is the checkpoint backlog one shard tolerates before
	// it starts dropping. At the 60-second cadence of ADR 0031 §5 this is far
	// more than a healthy shard ever holds; it is sized for a database stall,
	// not for steady state.
	DefaultSaveQueueSize = 256

	// DefaultSaveTimeout bounds one save transaction. ADR 0031 §8 fixes it at
	// five seconds for the disconnect path, and there is no reason for a
	// checkpoint to be more patient.
	DefaultSaveTimeout = 5 * time.Second
)

// SaveWorker turns a character save into an operation the caller never waits
// for.
//
// The rule it enforces is the one ADR 0031 §7 cares about: nothing on the tick
// path may block on I/O. [SaveWorker.Enqueue] therefore never blocks and never
// returns an error a caller might be tempted to retry inline — a full queue
// drops the snapshot and increments [SaveWorker.Dropped]. Dropping is a
// deliberate choice over blocking: the 60-second checkpoint will produce
// another snapshot shortly, and the ceiling on damage is a minute of reversible
// progress, whereas a blocked tick stalls movement for every player in the zone.
//
// Irreversible gains must not be routed through here. Those are committed
// synchronously, off the tick, before the client is told they happened.
type SaveWorker struct {
	repository Repository
	logger     *slog.Logger
	queue      chan Snapshot
	timeout    time.Duration

	dropped   atomic.Uint64
	persisted atomic.Uint64
	failed    atomic.Uint64
	stale     atomic.Uint64
}

// NewSaveWorker builds a worker over repository. A non-positive queueSize or
// timeout falls back to [DefaultSaveQueueSize] and [DefaultSaveTimeout].
func NewSaveWorker(repository Repository, logger *slog.Logger, queueSize int, timeout time.Duration) *SaveWorker {
	if queueSize <= 0 {
		queueSize = DefaultSaveQueueSize
	}
	if timeout <= 0 {
		timeout = DefaultSaveTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SaveWorker{
		repository: repository,
		logger:     logger,
		queue:      make(chan Snapshot, queueSize),
		timeout:    timeout,
	}
}

// Enqueue offers a snapshot for persistence and returns immediately.
//
// It reports false when the queue is full, having incremented the drop counter
// and logged the character that lost a checkpoint. Callers must not retry on
// false: the next checkpoint supersedes this one, and retrying in a loop is how
// a bounded queue becomes an unbounded one.
func (worker *SaveWorker) Enqueue(snapshot Snapshot) bool {
	select {
	case worker.queue <- snapshot:
		return true
	default:
		dropped := worker.dropped.Add(1)
		worker.logger.Warn(
			"character save dropped: save queue full",
			"character_id", snapshot.State.CharacterID.String(),
			"save_seq", snapshot.State.SaveSeq,
			"queue_capacity", cap(worker.queue),
			"dropped_total", dropped,
		)
		return false
	}
}

// Run drains the queue until ctx ends, then persists whatever is still queued
// so a clean shutdown does not throw away checkpoints (ADR 0031 §5.6).
func (worker *SaveWorker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			worker.drain(ctx)
			return
		case snapshot := <-worker.queue:
			worker.persist(ctx, snapshot)
		}
	}
}

func (worker *SaveWorker) drain(ctx context.Context) {
	for {
		select {
		case snapshot := <-worker.queue:
			worker.persist(ctx, snapshot)
		default:
			return
		}
	}
}

// persist writes one snapshot as a single transaction. The context is derived
// with [context.WithoutCancel] on purpose: the shard shutting down, or a
// connection dying, must not cancel the save of the state it was holding
// (ADR 0031 §8). The timeout is what bounds it instead.
func (worker *SaveWorker) persist(ctx context.Context, snapshot Snapshot) {
	saveContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), worker.timeout)
	defer cancel()

	err := SaveCharacter(saveContext, worker.repository, snapshot)
	switch {
	case err == nil:
		worker.persisted.Add(1)
	case errors.Is(err, ErrStaleSave):
		// A newer save already landed. Expected on reconnect, not a failure.
		worker.stale.Add(1)
		worker.logger.Debug(
			"character save superseded by a newer one",
			"character_id", snapshot.State.CharacterID.String(),
			"save_seq", snapshot.State.SaveSeq,
		)
	default:
		failed := worker.failed.Add(1)
		worker.logger.Error(
			"character save failed",
			"character_id", snapshot.State.CharacterID.String(),
			"save_seq", snapshot.State.SaveSeq,
			"failed_total", failed,
			"error", err,
		)
	}
}

// Dropped counts snapshots refused because the queue was full.
func (worker *SaveWorker) Dropped() uint64 { return worker.dropped.Load() }

// Persisted counts snapshots committed.
func (worker *SaveWorker) Persisted() uint64 { return worker.persisted.Load() }

// Failed counts snapshots whose transaction returned an error.
func (worker *SaveWorker) Failed() uint64 { return worker.failed.Load() }

// Stale counts snapshots a newer save had already superseded.
func (worker *SaveWorker) Stale() uint64 { return worker.stale.Load() }

// Depth reports the current queue depth, for a gauge or a shutdown check.
func (worker *SaveWorker) Depth() int { return len(worker.queue) }

// SaveNow persists a snapshot synchronously, for the paths ADR 0031 §5.4 says
// must commit before the client is told anything happened: loot picked up, item
// consumed, quest reward granted, level-up. It must never be called from the
// tick loop.
func SaveNow(ctx context.Context, repository Repository, snapshot Snapshot, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultSaveTimeout
	}
	saveContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return SaveCharacter(saveContext, repository, snapshot)
}
