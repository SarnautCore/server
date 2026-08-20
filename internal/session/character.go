package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/world"
)

// CharacterStore is the persistence seam of protocol/session.md §5.7: the load
// at zone entry, and the checkpoints that follow it.
//
// The session layer holds an interface rather than a repository so that no
// session code can reach a database directly, and so a session test needs no
// container. `internal/store` is the only implementation.
type CharacterStore interface {
	// Load is checkpoint L1: the character's saved snapshot, materialized from
	// the chargen table on a first login (ADR 0032 §5.4).
	Load(ctx context.Context, characterID uuid.UUID, chargenOptionID, zoneID string) (store.Snapshot, error)

	// Checkpoint enqueues a save on the bounded worker and returns without
	// waiting. It reports false when the queue was full and the snapshot was
	// dropped.
	Checkpoint(snapshot store.Snapshot) bool
}

// characterSession is one connection's view of its character: the state L1
// loaded, and the save sequence every later checkpoint advances.
//
// It exists so that "build a snapshot from what the zone has now" is written
// once. Position, heading, level and health come from the zone under its own
// mutex; inventory and quests come from the load, because in M2 nothing else
// changes them yet.
type characterSession struct {
	characterID uuid.UUID
	zoneID      string
	loaded      store.Snapshot
	// saveSeq is only ever touched by the goroutine that owns the session: the
	// handler before the loops start, the periodic saver while they run, and
	// the handler again in teardown after they have stopped.
	saveSeq int64
}

func newCharacterSession(admission Admission, zoneID string, loaded store.Snapshot) *characterSession {
	return &characterSession{
		characterID: admission.CharacterID,
		zoneID:      zoneID,
		loaded:      loaded,
		saveSeq:     loaded.State.SaveSeq,
	}
}

// spawn is where the world entity is placed: the loaded position, which for a
// character that has never logged in is the chargen option's spawn.
func (character *characterSession) spawn() (world.Vec3, float32) {
	return world.Vec3{
		X: character.loaded.State.Position.X,
		Y: character.loaded.State.Position.Y,
		Z: character.loaded.State.Position.Z,
	}, character.loaded.State.Heading
}

// snapshotFrom folds one zone view into a full character snapshot and advances
// the save sequence. A save whose sequence does not advance past the stored one
// is rejected, which is what stops a slow write from a dying session clobbering
// a newer write from a reconnect (ADR 0031 §6).
func (character *characterSession) snapshotFrom(view world.CharacterSnapshot) store.Snapshot {
	character.saveSeq++
	state := character.loaded.State
	state.CharacterID = character.characterID
	state.ZoneID = character.zoneID
	state.Position = store.Vec3{X: view.Position.X, Y: view.Position.Y, Z: view.Position.Z}
	state.Heading = view.Heading
	if view.Level > 0 {
		state.Level = int32(view.Level)
	}
	state.Health = view.Health
	state.SaveSeq = character.saveSeq
	return store.Snapshot{
		State:     state,
		Inventory: character.loaded.Inventory,
		Quests:    character.loaded.Quests,
	}
}

// checkpoint takes one snapshot of the entity under the zone's mutex and
// enqueues it. It reports false when the entity is already gone, which is the
// ordinary answer once teardown has run.
func (character *characterSession) checkpoint(
	zone *world.Zone,
	entityID uint64,
	characters CharacterStore,
	logger *slog.Logger,
	kind string,
) bool {
	view, ok := zone.SnapshotCharacter(entityID)
	if !ok {
		return false
	}
	snapshot := character.snapshotFrom(view)
	if !characters.Checkpoint(snapshot) {
		logger.Warn("character checkpoint dropped",
			"checkpoint", kind,
			"character_id", character.characterID.String(),
			"save_seq", snapshot.State.SaveSeq,
		)
		return false
	}
	return true
}

// runPeriodicSaves is checkpoint S2. It bounds how much progress an unclean
// shard exit can destroy; without it the disconnect save is the only writer and
// any crash loses the whole session.
//
// It also renews the play lock, because both are periodic work on the same
// session and a second ticker would only add a second thing to shut down.
func (character *characterSession) runPeriodicSaves(
	ctx context.Context,
	zone *world.Zone,
	entityID uint64,
	characters CharacterStore,
	authority Authority,
	interval time.Duration,
	logger *slog.Logger,
) error {
	saves := time.NewTicker(interval)
	defer saves.Stop()
	locks := time.NewTicker(playLockRenewInterval)
	defer locks.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-saves.C:
			character.checkpoint(zone, entityID, characters, logger, "S2")
		case <-locks.C:
			granted, err := authority.RenewPlayLock(ctx, character.characterID)
			if err != nil {
				// A renewal that cannot be delivered is not fatal: the lock
				// outlives one missed renewal, and dropping the player because
				// auth blinked would be worse than the risk.
				logger.Warn("play lock renewal failed",
					"character_id", character.characterID.String(),
					"error", err,
				)
				continue
			}
			if !granted {
				logger.Warn("play lock lost to another holder",
					"character_id", character.characterID.String(),
				)
			}
		}
	}
}

// playLockRenewInterval is ADR 0030 §4's 20 seconds against a 60-second lock:
// two missed renewals still leave a session alive, three do not.
const playLockRenewInterval = 20 * time.Second
