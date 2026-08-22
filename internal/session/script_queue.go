package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/scriptqueue"
	"github.com/google/uuid"
)

const (
	deferredDocumentSchema = 1
	deferredLeaseMS        = int64(30_000)
)

type deferredDocument struct {
	Schema   int             `json:"schema"`
	Deferred script.Deferred `json:"deferred"`
}

// QuestScriptPlan is the immutable result of evaluating one activation against
// a collecting host. No command has touched gameplay state and no queue row has
// been written yet.
type QuestScriptPlan struct {
	Commands []script.Command
	Deferred []scriptqueue.Work
}

func (driver *ScriptDriver) PlanQuestActivation(
	ctx context.Context, entityID uint64, questID string,
) (QuestScriptPlan, error) {
	return driver.planQuestActivation(ctx, entityID, questID, true)
}

func (driver *ScriptDriver) planQuestActivation(
	ctx context.Context, entityID uint64, questID string, strict bool,
) (QuestScriptPlan, error) {
	if driver == nil {
		return QuestScriptPlan{}, nil
	}
	activation, ok := driver.source.QuestActivation(questID)
	if !ok {
		return QuestScriptPlan{}, nil
	}
	var plan QuestScriptPlan
	err := driver.zone.GameCommand(func(tick gametypes.Tick) error {
		batch, err := driver.beginDeferredBatch()
		if err != nil {
			return err
		}
		commands := make([]script.Command, 0)
		driver.plannedCommands = &commands
		driver.tick = tick
		defer func() {
			driver.tick = nil
			driver.plannedCommands = nil
			if driver.activeDeferred == batch {
				driver.endDeferredBatch(batch)
			}
		}()

		driver.evaluations++
		actor := formatEntityID(entityID)
		frame := script.Frame{
			EvaluationID: fmt.Sprintf("%s|%d|%s|%d", questID, entityID, driver.evaluationBoot, driver.evaluations),
			ZoneID:       driver.zone.ID(), SourceID: questID,
			CasterID: actor, TargetID: actor, Addressee: actor,
		}
		for _, node := range activation.StartImpacts {
			if err := driver.evaluator.Evaluate(ctx, node, frame); err != nil {
				if strict {
					return fmt.Errorf("plan quest %s node %s: %w", questID, node.Key, err)
				}
				driver.logger.Warn("quest activation script failed",
					"quest_id", questID, "entity_id", entityID, "error", err)
			}
		}
		for _, agent := range activation.TriggerAgents {
			if err := driver.evaluator.Evaluate(ctx, agent, frame); err != nil {
				if strict {
					return fmt.Errorf("plan quest %s trigger %s: %w", questID, agent.Key, err)
				}
				driver.logger.Warn("quest trigger agent failed",
					"quest_id", questID, "entity_id", entityID, "error", err)
			}
		}
		deferred := driver.endDeferredBatch(batch)
		works := make([]scriptqueue.Work, 0, len(deferred))
		for _, item := range deferred {
			work, err := driver.deferredWork(item, "", "")
			if err != nil {
				return err
			}
			works = append(works, work)
		}
		plan = QuestScriptPlan{Commands: append([]script.Command(nil), commands...), Deferred: works}
		return nil
	})
	return plan, err
}

// ApplyQuestPlan projects a committed plan into live zone state and schedules
// the rows returned by the transaction. It performs no database operation
// while the zone lock is held.
func (driver *ScriptDriver) ApplyQuestPlan(
	ctx context.Context, plan QuestScriptPlan, persisted []scriptqueue.Work,
) error {
	if driver == nil {
		return nil
	}
	var commitErr error
	err := driver.zone.GameCommand(func(tick gametypes.Tick) error {
		batch, err := driver.beginDeferredBatch()
		if err != nil {
			return err
		}
		driver.tick = tick
		defer func() {
			driver.tick = nil
			if driver.activeDeferred == batch {
				driver.endDeferredBatch(batch)
			}
		}()
		for _, command := range plan.Commands {
			if err := (scriptHost{driver: driver}).Apply(ctx, command); err != nil {
				return fmt.Errorf("apply planned script command %d: %w", command.Kind, err)
			}
		}
		for _, row := range persisted {
			driver.scheduleDeferred(tick, row)
		}
		extra := driver.endDeferredBatch(batch)
		return afterUnlock(tick, func() {
			unlock := driver.lockDeferredCommit()
			defer unlock()
			commitErr = driver.persistBatch(ctx, extra)
		})
	})
	return errors.Join(err, commitErr)
}

// BindDeferredQueue replaces the driver's process-local test queue with a
// durable store and schedules every surviving row for this zone. Call it at
// composition time, before the driver can evaluate a script.
func (driver *ScriptDriver) BindDeferredQueue(
	ctx context.Context, store scriptqueue.Store, workerID string,
) error {
	if driver == nil {
		return errors.New("session: script driver is required")
	}
	if store == nil {
		return errors.New("session: deferred script store is required")
	}
	if workerID == "" {
		return errors.New("session: deferred script worker id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	current, err := driver.deferredStore.LoadZone(ctx, driver.zone.ID())
	if err != nil {
		return fmt.Errorf("session: inspect current deferred queue: %w", err)
	}
	if len(current) != 0 {
		return fmt.Errorf("session: cannot replace deferred queue with %d scheduled rows", len(current))
	}
	driver.deferredStore = store
	driver.deferredWorker = workerID
	driver.deferredCtx = ctx
	rows, err := store.LoadZone(ctx, driver.zone.ID())
	if err != nil {
		return fmt.Errorf("session: recover deferred queue for zone %s: %w", driver.zone.ID(), err)
	}
	return driver.zone.GameCommand(func(tick gametypes.Tick) error {
		for _, row := range rows {
			driver.scheduleDeferred(tick, row)
		}
		return nil
	})
}

func (driver *ScriptDriver) beginDeferredBatch() (*[]script.Deferred, error) {
	if driver.activeDeferred != nil {
		return nil, errors.New("session: nested deferred commit batch")
	}
	batch := make([]script.Deferred, 0)
	driver.activeDeferred = &batch
	driver.deferredBatchNow = driver.deferredNow().UTC()
	return &batch, nil
}

func (driver *ScriptDriver) endDeferredBatch(batch *[]script.Deferred) []script.Deferred {
	if driver.activeDeferred == batch {
		driver.activeDeferred = nil
		driver.deferredBatchNow = time.Time{}
	}
	return append([]script.Deferred(nil), (*batch)...)
}

func afterUnlock(tick gametypes.Tick, run func()) error {
	postCommit, ok := tick.(gametypes.PostCommitTick)
	if !ok {
		return errors.New("session: zone tick cannot run a deferred commit after unlock")
	}
	postCommit.AfterUnlock(run)
	return nil
}

func (driver *ScriptDriver) persistBatch(ctx context.Context, deferred []script.Deferred) error {
	if len(deferred) == 0 {
		return nil
	}
	works := make([]scriptqueue.Work, 0, len(deferred))
	for _, item := range deferred {
		work, err := driver.deferredWork(item, "", "")
		if err != nil {
			return err
		}
		works = append(works, work)
	}
	rows, err := scriptqueue.EnqueueBatch(ctx, driver.deferredStore, works)
	if err != nil {
		return fmt.Errorf("session: persist deferred batch: %w", err)
	}
	return driver.zone.GameCommand(func(tick gametypes.Tick) error {
		for _, row := range rows {
			driver.scheduleDeferred(tick, row)
		}
		return nil
	})
}

func (driver *ScriptDriver) deferredWork(
	item script.Deferred, scopeKind, scopeID string,
) (scriptqueue.Work, error) {
	if item.Node == nil || item.Node.Key == "" {
		return scriptqueue.Work{}, errors.New("session: deferred impact has no keyed node")
	}
	if item.Frame.EvaluationID == "" {
		return scriptqueue.Work{}, fmt.Errorf("session: deferred impact %s has no evaluation id", item.Node.Key)
	}
	zoneID := item.Frame.ZoneID
	if zoneID == "" {
		zoneID = driver.zone.ID()
	}
	if zoneID != driver.zone.ID() {
		return scriptqueue.Work{}, fmt.Errorf("session: deferred impact %s belongs to zone %s, not %s",
			item.Node.Key, zoneID, driver.zone.ID())
	}
	if item.DueAtMS > math.MaxInt64 {
		return scriptqueue.Work{}, fmt.Errorf("session: deferred impact %s due time overflows int64", item.Node.Key)
	}
	payload, err := json.Marshal(deferredDocument{Schema: deferredDocumentSchema, Deferred: item})
	if err != nil {
		return scriptqueue.Work{}, fmt.Errorf("session: encode deferred impact %s: %w", item.Node.Key, err)
	}
	if scopeID == "" {
		scopeID = item.Frame.EvaluationID
	}
	if scopeKind == "" {
		scopeKind = scriptqueue.ScopeActivation
	}
	return scriptqueue.Work{
		ID: fmt.Sprintf("%s|%d|%s",
			item.Frame.EvaluationID, item.Frame.ActivationOrdinal, item.Node.Key),
		ZoneID: zoneID, ScopeKind: scopeKind, ScopeID: scopeID,
		DueAtMS: int64(item.DueAtMS), Payload: payload,
	}, nil
}

func decodeDeferred(work scriptqueue.Work) (script.Deferred, error) {
	var document deferredDocument
	if err := json.Unmarshal(work.Payload, &document); err != nil {
		return script.Deferred{}, fmt.Errorf("decode deferred work %s: %w", work.ID, err)
	}
	if document.Schema != deferredDocumentSchema {
		return script.Deferred{}, fmt.Errorf("deferred work %s has schema %d, want %d",
			work.ID, document.Schema, deferredDocumentSchema)
	}
	if document.Deferred.Node == nil || document.Deferred.Node.Key == "" {
		return script.Deferred{}, fmt.Errorf("deferred work %s has no keyed node", work.ID)
	}
	return document.Deferred, nil
}

func (driver *ScriptDriver) scheduleDeferred(tick gametypes.Tick, work scriptqueue.Work) {
	if driver.deferredRows[work.ID] {
		return
	}
	driver.deferredRows[work.ID] = true
	intervalMS := tick.Interval().Milliseconds()
	if intervalMS <= 0 {
		driver.logger.Error("cannot schedule deferred script work",
			"work_id", work.ID, "interval", tick.Interval())
		return
	}
	nowMS := driver.deferredNow().UnixMilli()
	dueMS := work.DueAtMS
	if work.AvailableAtMS > dueMS {
		dueMS = work.AvailableAtMS
	}
	var delayTicks uint64
	if dueMS > nowMS {
		delayTicks = uint64((dueMS - nowMS + intervalMS - 1) / intervalMS)
	}
	tick.After(delayTicks, func(later gametypes.Tick) {
		delete(driver.deferredRows, work.ID)
		now := driver.deferredNow().UnixMilli()
		if err := afterUnlock(later, func() { driver.claimDeferred(work.ID, now) }); err != nil {
			driver.logger.Error("deferred script work cannot leave zone lock",
				"work_id", work.ID, "error", err)
		}
	})
}

func (driver *ScriptDriver) claimDeferred(id string, nowMS int64) {
	leaseToken := uuid.NewString()
	claim, err := driver.deferredStore.Claim(
		driver.deferredCtx, id, driver.deferredWorker, leaseToken, nowMS, nowMS+deferredLeaseMS,
	)
	if err != nil {
		driver.logger.Warn("deferred script claim failed", "work_id", id, "error", err)
		driver.rescheduleDeferred(scriptqueue.Work{ID: id, DueAtMS: nowMS + 1, AvailableAtMS: nowMS + 1})
		return
	}
	switch claim.State {
	case scriptqueue.ClaimMissing:
		return
	case scriptqueue.ClaimBlocked:
		driver.rescheduleDeferred(claim.Work)
		return
	case scriptqueue.ClaimAcquired:
		driver.evaluateDeferred(claim.Work, nowMS)
	default:
		driver.logger.Error("deferred script claim returned unknown state", "work_id", id)
	}
}

func (driver *ScriptDriver) rescheduleDeferred(work scriptqueue.Work) {
	_ = driver.zone.GameCommand(func(tick gametypes.Tick) error {
		driver.scheduleDeferred(tick, work)
		return nil
	})
}

func (driver *ScriptDriver) evaluateDeferred(work scriptqueue.Work, nowMS int64) {
	item, decodeErr := decodeDeferred(work)
	if decodeErr != nil {
		driver.retryDeferred(work, nowMS, decodeErr)
		return
	}
	// A durable row is one execution. Replacing the activation id with its row
	// id keeps command keys stable across retries while distinguishing another
	// genuine activation of the same authored node.
	item.Frame.EvaluationID = work.ID
	var evaluationErr error
	var nested []script.Deferred
	err := driver.zone.GameCommand(func(tick gametypes.Tick) error {
		batch, err := driver.beginDeferredBatch()
		if err != nil {
			return err
		}
		driver.tick = tick
		evaluationErr = driver.evaluator.Evaluate(driver.deferredCtx, item.Node, item.Frame)
		driver.tick = nil
		nested = driver.endDeferredBatch(batch)
		return afterUnlock(tick, func() {
			if evaluationErr != nil {
				driver.retryDeferred(work, nowMS, evaluationErr)
				return
			}
			driver.completeDeferred(work, nested)
		})
	})
	if err != nil {
		driver.retryDeferred(work, nowMS, err)
	}
}

func (driver *ScriptDriver) retryDeferred(work scriptqueue.Work, nowMS int64, cause error) {
	next := scriptqueue.RetryAtMS(nowMS, work.Attempts, time.Second)
	for {
		err := driver.deferredStore.Retry(
			driver.deferredCtx, work.ID, driver.deferredWorker, work.LeaseToken, next, cause.Error(),
		)
		if err == nil {
			work.AvailableAtMS = next
			work.LeaseOwner = ""
			work.LeaseToken = ""
			work.LeaseUntilMS = 0
			driver.rescheduleDeferred(work)
			return
		}
		if errors.Is(err, scriptqueue.ErrLeaseLost) {
			return
		}
		driver.logger.Warn("deferred script retry state failed",
			"work_id", work.ID, "cause", cause, "error", err)
		if !waitDeferredRetry(driver.deferredCtx) {
			return
		}
	}
}

func (driver *ScriptDriver) completeDeferred(work scriptqueue.Work, nested []script.Deferred) {
	nestedRows := make([]scriptqueue.Work, 0, len(nested))
	for _, item := range nested {
		row, err := driver.deferredWork(item, work.ScopeKind, work.ScopeID)
		if err != nil {
			driver.retryDeferred(work, work.DueAtMS, err)
			return
		}
		nestedRows = append(nestedRows, row)
	}
	for {
		var inserted []scriptqueue.Work
		err := driver.deferredStore.RunInTx(driver.deferredCtx, func(ctx context.Context, tx scriptqueue.Store) error {
			if err := tx.Complete(ctx, work.ID, driver.deferredWorker, work.LeaseToken); err != nil {
				return err
			}
			for _, row := range nestedRows {
				row, err := tx.Enqueue(ctx, row)
				if err != nil {
					return err
				}
				inserted = append(inserted, row)
			}
			return nil
		})
		if err == nil {
			_ = driver.zone.GameCommand(func(tick gametypes.Tick) error {
				for _, row := range inserted {
					driver.scheduleDeferred(tick, row)
				}
				return nil
			})
			return
		}
		if errors.Is(err, scriptqueue.ErrLeaseLost) {
			return
		}
		driver.logger.Warn("complete deferred script work failed", "work_id", work.ID, "error", err)
		if !waitDeferredRetry(driver.deferredCtx) {
			return
		}
	}
}

func waitDeferredRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// deferredMu documents that post-commit storage callbacks may overlap a client
// command. The store itself is safe, while batch completion must remain in
// authored callback order.
func (driver *ScriptDriver) lockDeferredCommit() func() {
	driver.deferredMu.Lock()
	return driver.deferredMu.Unlock
}
