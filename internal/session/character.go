package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/world"
)

// CharacterStore is the persistence seam of protocol/session.md §5.7: the load
// at zone entry, and the checkpoints that follow it.
//
// The session layer holds an interface rather than a repository so that no
// session code can reach a database directly, and so a session test needs no
// container. `internal/charstore` is the only implementation.
type CharacterStore interface {
	// Load is checkpoint L1: the character's saved snapshot, materialized from
	// the chargen table on a first login (ADR 0032 §5.4).
	Load(ctx context.Context, characterID uuid.UUID, chargenOptionID, zoneID string) (charstore.Snapshot, error)

	// Checkpoint enqueues a save on the bounded worker and returns without
	// waiting. It reports false when the queue was full and the snapshot was
	// dropped.
	Checkpoint(snapshot charstore.Snapshot) bool
}

// characterSession is one connection's view of its character: the state L1
// loaded, and the save sequence every later checkpoint advances.
//
// It exists so that "build a snapshot from what the zone has now" is written
// once. Position, heading, level and health come from the zone under its own
// mutex; inventory, purse and quests come from the load, and from whatever has
// committed a change to them since.
//
// That last clause is why there is a mutex here. A loot take commits the bag
// and the purse from the reliable reader's goroutine, and the periodic saver
// builds a snapshot on its own; without the lock the saver would race the take
// and could write the pre-loot bag back over it.
type characterSession struct {
	characterID uuid.UUID
	zoneID      string

	mu     sync.Mutex
	loaded charstore.Snapshot
	// saveSeq is the sequence this session's next checkpoint will use. It is
	// advanced by every checkpoint and re-synchronised by [adopt] when some
	// other unit of work commits at a higher one.
	saveSeq int64
	// quests is where the live quest log lives, when the zone hosts one. The
	// counters move on the tick goroutine as kills land, so a checkpoint has to
	// ask for them rather than keep a copy that is stale by one kill.
	quests questRows
}

func newCharacterSession(admission Admission, zoneID string, loaded charstore.Snapshot) *characterSession {
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
	character.mu.Lock()
	defer character.mu.Unlock()
	return world.Vec3{
		X: character.loaded.State.Position.X,
		Y: character.loaded.State.Position.Y,
		Z: character.loaded.State.Position.Z,
	}, character.loaded.State.Heading
}

// bindQuests points the checkpoint at the live quest log. It is called once, at
// zone entry, before the first checkpoint runs.
func (character *characterSession) bindQuests(rows questRows) {
	character.mu.Lock()
	defer character.mu.Unlock()
	character.quests = rows
}

// questSnapshotLocked is the quest half of a checkpoint. The caller holds the
// mutex.
//
// A nil answer from the module is not "no quests": it is the ordinary answer
// once the session has been released, and writing it would erase the log. The
// rows the session loaded are the fallback, which is exactly what a session
// with no quest module saves.
func (character *characterSession) questSnapshotLocked() []charstore.QuestState {
	if character.quests == nil {
		return character.loaded.Quests
	}
	if rows := character.quests.Rows(character.characterID); rows != nil {
		return rows
	}
	return character.loaded.Quests
}

// snapshotFrom folds one zone view into a full character snapshot and advances
// the save sequence. A save whose sequence does not advance past the stored one
// is rejected, which is what stops a slow write from a dying session clobbering
// a newer write from a reconnect (ADR 0031 §6).
func (character *characterSession) snapshotFrom(view world.CharacterSnapshot) charstore.Snapshot {
	character.mu.Lock()
	defer character.mu.Unlock()
	character.saveSeq++
	state := character.loaded.State
	state.CharacterID = character.characterID
	state.ZoneID = character.zoneID
	state.Position = charstore.Vec3{X: view.Position.X, Y: view.Position.Y, Z: view.Position.Z}
	state.Heading = view.Heading
	if view.Level > 0 {
		state.Level = int32(view.Level)
	}
	state.Health = view.Health
	state.SaveSeq = character.saveSeq
	return charstore.Snapshot{
		State:     state,
		Inventory: character.loaded.Inventory,
		Quests:    character.questSnapshotLocked(),
	}
}

// adopt re-synchronises this view with a write some other unit of work already
// committed: the bag and purse a loot take wrote, and the sequence it wrote
// them at (mechanics/loot.md rule 5.6).
//
// Without it the session would keep checkpointing the inventory it loaded at
// zone entry, and the first periodic save after a loot would either be rejected
// as stale or, worse, overwrite the looted bag with the empty one. The sequence
// only ever moves forward: two writers racing both try to advance to the same
// number, and the anti-clobber rule of ADR 0031 §6 decides which one wins.
func (character *characterSession) adopt(inventory []charstore.InventoryItem, currency, saveSeq int64) {
	character.mu.Lock()
	defer character.mu.Unlock()
	character.loaded.Inventory = inventory
	character.loaded.State.Currency = currency
	if saveSeq > character.saveSeq {
		character.saveSeq = saveSeq
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
