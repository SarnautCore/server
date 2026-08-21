package charstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// ChargenTemplates hands out the state a character of one chargen option is
// created with: spawn, level, starting loadout and starting quests.
//
// It is an interface here rather than a pack reader because this package must
// stay free of content types: the shard builds the templates from its loaded
// pack (ADR 0032) and hands them over as plain snapshots, so a persistence test
// needs no pack and the pack reader needs no database.
type ChargenTemplates interface {
	// Template returns the fresh-character snapshot for one option id. Its
	// State.CharacterID and State.ZoneID are ignored: the caller stamps both.
	Template(chargenOptionID string) (Snapshot, bool)
}

// Characters is the load and save seam the session layer uses. Every checkpoint
// in the cadence of ADR 0031 §5 goes through one of its three methods.
type CharacterService struct {
	repository Repository
	templates  ChargenTemplates
	worker     *SaveWorker
	logger     *slog.Logger
	timeout    time.Duration
}

// NewCharacterService binds a repository, the chargen templates a first login
// materializes from, and the bounded save worker every checkpoint is enqueued
// on.
func NewCharacterService(
	repository Repository,
	templates ChargenTemplates,
	worker *SaveWorker,
	logger *slog.Logger,
	timeout time.Duration,
) *CharacterService {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = DefaultSaveTimeout
	}
	return &CharacterService{
		repository: repository,
		templates:  templates,
		worker:     worker,
		logger:     logger,
		timeout:    timeout,
	}
}

// ErrNoTemplate reports a character whose chargen option the pack does not
// carry. It is fatal for that login: materializing a character from a guess is
// how a player ends up naked in the void.
var ErrNoTemplate = errors.New("store: no chargen template for this option")

// Load is checkpoint L1 (protocol/session.md rule 5.7.1) and, for a character
// that has never logged in, the materialization of ADR 0032 §5.4.
//
// Both happen in one transaction, so a redeem that races or retries produces
// one character with one set of starting gear rather than two. The returned
// snapshot is what the caller places the world entity from.
func (service *CharacterService) Load(
	ctx context.Context,
	characterID uuid.UUID,
	chargenOptionID string,
	zoneID string,
) (Snapshot, error) {
	var loaded Snapshot
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		state, err := tx.LoadCharacterState(ctx, characterID)
		switch {
		case err == nil:
			inventory, err := tx.LoadInventory(ctx, characterID)
			if err != nil {
				return err
			}
			quests, err := tx.LoadQuestStates(ctx, characterID)
			if err != nil {
				return err
			}
			loaded = Snapshot{State: state, Inventory: inventory, Quests: quests}
			return nil
		case errors.Is(err, ErrNotFound):
			fresh, ok := service.templates.Template(chargenOptionID)
			if !ok {
				return fmt.Errorf("%w: %q", ErrNoTemplate, chargenOptionID)
			}
			fresh.State.CharacterID = characterID
			fresh.State.ZoneID = zoneID
			fresh.State.SaveSeq = 1
			if err := saveWithin(ctx, tx, fresh); err != nil {
				return err
			}
			loaded = fresh
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return Snapshot{}, err
	}
	return loaded, nil
}

// Checkpoint enqueues a save on the bounded worker and returns immediately. It
// is what S0, S1 and S2 use: a checkpoint must never block the session, and a
// full queue drops rather than waits (ADR 0031 §7).
func (service *CharacterService) Checkpoint(snapshot Snapshot) bool {
	if service.worker == nil {
		return false
	}
	return service.worker.Enqueue(snapshot)
}

// CheckpointNow writes synchronously, for the irreversible gains of ADR 0031
// §5.4 that must be committed before the client is told they happened. It is
// not on any path the tick loop can reach.
func (service *CharacterService) CheckpointNow(ctx context.Context, snapshot Snapshot) error {
	return SaveNow(ctx, service.repository, snapshot, service.timeout)
}

// saveWithin writes the three tables of a snapshot inside an already-open
// transaction. [SaveCharacter] would open its own, which nests correctly but
// reads as though the materialization were two units of work.
func saveWithin(ctx context.Context, tx Repository, snapshot Snapshot) error {
	if err := tx.SaveCharacterState(ctx, snapshot.State); err != nil {
		return err
	}
	if err := tx.ReplaceInventory(ctx, snapshot.State.CharacterID, snapshot.Inventory); err != nil {
		return err
	}
	for _, quest := range snapshot.Quests {
		if err := tx.UpsertQuestState(ctx, snapshot.State.CharacterID, quest); err != nil {
			return err
		}
	}
	return nil
}
