package session

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/progression"
)

const defaultProgressionQueueSize = 256

// ExperienceService is the durable off-tick progression transaction.
type ExperienceService interface {
	ApplyMobExperience(context.Context, progression.MobExperienceGrant) (progression.Change, error)
}

// ProgressionProjection applies a committed change to live session and world
// state. It never runs for a failed transaction.
type ProgressionProjection func(progression.Change) error

type progressionTarget struct {
	characterID uuid.UUID
	generation  uint64
	project     ProgressionProjection
}

type queuedExperienceCommand struct {
	command ExperienceCommand
	target  progressionTarget
}

// ProgressionWorker moves an ImpactAddExperience command from the tick-safe
// ingress to durable character state, then projects the committed result back
// to a live session. The queue is bounded and OfferExperience never blocks.
type ProgressionWorker struct {
	service ExperienceService
	logger  *slog.Logger
	queue   chan queuedExperienceCommand

	mu         sync.Mutex
	targets    map[uint64]progressionTarget
	generation uint64

	accepted  atomic.Uint64
	persisted atomic.Uint64
	failed    atomic.Uint64
	dropped   atomic.Uint64
}

func NewProgressionWorker(
	service ExperienceService,
	logger *slog.Logger,
	queueSize int,
) *ProgressionWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if queueSize <= 0 {
		queueSize = defaultProgressionQueueSize
	}
	return &ProgressionWorker{
		service: service,
		logger:  logger,
		queue:   make(chan queuedExperienceCommand, queueSize),
		targets: make(map[uint64]progressionTarget),
	}
}

// Admit binds one live entity to its durable character and post-commit
// projection. Re-admission gets a new generation so a slow transaction from a
// replaced session cannot project into the replacement.
func (worker *ProgressionWorker) Admit(
	entityID uint64,
	characterID uuid.UUID,
	project ProgressionProjection,
) error {
	if worker == nil || worker.service == nil || entityID == 0 || characterID == uuid.Nil || project == nil {
		return ErrProgressionUnconfigured
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	worker.generation++
	worker.targets[entityID] = progressionTarget{
		characterID: characterID,
		generation:  worker.generation,
		project:     project,
	}
	return nil
}

// Release removes one live projection target. Already committed state remains
// durable and is loaded by the next session.
func (worker *ProgressionWorker) Release(entityID uint64) {
	if worker == nil {
		return
	}
	worker.mu.Lock()
	delete(worker.targets, entityID)
	worker.mu.Unlock()
}

// OfferExperience implements ExperienceCommandSink without blocking the tick.
func (worker *ProgressionWorker) OfferExperience(command ExperienceCommand) bool {
	if worker == nil || worker.service == nil {
		return false
	}
	worker.mu.Lock()
	target, exists := worker.targets[command.EntityID]
	worker.mu.Unlock()
	if !exists {
		worker.dropped.Add(1)
		return false
	}
	queued := queuedExperienceCommand{command: command, target: target}
	select {
	case worker.queue <- queued:
		worker.accepted.Add(1)
		return true
	default:
		worker.dropped.Add(1)
		return false
	}
}

// Run processes accepted commands until ctx ends.
func (worker *ProgressionWorker) Run(ctx context.Context) error {
	if worker == nil || worker.service == nil {
		return ErrProgressionUnconfigured
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case queued := <-worker.queue:
			worker.process(ctx, queued)
		}
	}
}

func (worker *ProgressionWorker) process(ctx context.Context, queued queuedExperienceCommand) {
	command := queued.command
	target := queued.target
	change, err := worker.service.ApplyMobExperience(ctx, progression.MobExperienceGrant{
		CharacterID:  target.characterID,
		ExecutionKey: command.ExecutionKey,
		MobCount:     command.MobCount,
		MobLevel:     command.MobLevel,
	})
	if err != nil {
		worker.failed.Add(1)
		worker.logger.ErrorContext(ctx, "experience transaction failed",
			"character_id", target.characterID.String(),
			"execution_key", command.ExecutionKey, "error", err)
		return
	}
	worker.persisted.Add(1)

	worker.mu.Lock()
	current, stillLive := worker.targets[command.EntityID]
	worker.mu.Unlock()
	if !stillLive || current.generation != target.generation || current.characterID != target.characterID {
		return
	}
	if err := current.project(change); err != nil {
		worker.failed.Add(1)
		worker.logger.ErrorContext(ctx, "committed experience projection failed",
			"character_id", target.characterID.String(),
			"execution_key", command.ExecutionKey, "error", err)
	}
}

func (worker *ProgressionWorker) Accepted() uint64  { return worker.accepted.Load() }
func (worker *ProgressionWorker) Persisted() uint64 { return worker.persisted.Load() }
func (worker *ProgressionWorker) Failed() uint64    { return worker.failed.Load() }
func (worker *ProgressionWorker) Dropped() uint64   { return worker.dropped.Load() }
func (worker *ProgressionWorker) Depth() int        { return len(worker.queue) }
