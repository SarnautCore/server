// Package charstore owns every SQL statement the server issues and the repository
// interfaces the rest of the process talks to.
//
// The split it enforces is the one ADR 0031 exists to protect: gameplay code
// hands the store a plain-Go snapshot and takes one back, so nothing on the
// 30 Hz tick path can reach a database even by accident. `world` never imports
// this package, and this package never imports `world`.
//
// Two implementations satisfy [Repository]:
//
//   - [NewPostgres] over the `*pgxpool.Pool` that `internal/infra` already opens.
//   - [NewMemory], used by every test that has no DSN, and by `go test ./...`.
//
// Both are held to the same behaviour by the shared suite in
// `conformance_test.go`, so a test that passes against memory is evidence about
// Postgres rather than a separate universe. That extends to the schema's own
// CHECK and FOREIGN KEY constraints: `constraints.go` restates them for the
// in-memory store and the Postgres one maps their SQLSTATEs onto the same
// [ErrConstraintViolated], because an in-memory store that accepts rows the
// database refuses is worse than no in-memory store at all.
package charstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
)

// Sentinel errors. Callers match with [errors.Is]; the Postgres implementation
// maps SQLSTATE values onto these so no caller ever inspects a driver error.
var (
	// ErrNotFound reports that a row the caller named does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrNameTaken reports that the database's unique index on
	// auth.characters.name_normalized rejected an insert. It is raised by
	// SQLSTATE 23505, never by a check-then-insert (ADR 0032 §3).
	ErrNameTaken = errors.New("store: character name taken")

	// ErrEmailTaken reports that auth.accounts.email is already registered.
	ErrEmailTaken = errors.New("store: account email taken")

	// ErrStaleSave reports a save whose save_seq did not advance past the stored
	// value: a slow write from a dying session losing to a newer reconnect
	// (ADR 0031 §6). It is an expected outcome, not a failure to retry.
	ErrStaleSave = errors.New("store: save sequence did not advance")

	// ErrSlotOccupied reports an inventory write aimed at a slot that already
	// holds a different item.
	ErrSlotOccupied = errors.New("store: inventory slot occupied")

	// ErrConstraintViolated reports a write the schema itself refuses: a
	// character name outside 3–16 characters, an account_id with no account, a
	// level below 1, a negative experience, save_seq or slot. Postgres raises it
	// from SQLSTATE 23514 or 23503; the in-memory store raises it from the same
	// rules restated in `constraints.go`, which is what keeps the two honest
	// about each other.
	ErrConstraintViolated = errors.New("store: constraint violated")
)

// Account is a row of auth.accounts. The password hash is a PHC string
// (ADR 0030 §1); this package stores and returns it without interpreting it.
type Account struct {
	AccountID    uuid.UUID
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	DisabledAt   *time.Time
}

// Character is a row of auth.characters.
type Character struct {
	CharacterID     uuid.UUID
	AccountID       uuid.UUID
	Name            string
	NameNormalized  string
	ChargenOptionID string
	CreatedAt       time.Time
	DeletedAt       *time.Time
}

// Vec3 mirrors world.Vec3 without importing it. The dependency must not exist in
// either direction: `world` stays free of persistence (ADR 0031 §7), and this
// package stays constructible in a test with no zone.
type Vec3 struct {
	X float32
	Y float32
	Z float32
}

// CharacterState is a row of shard.character_state: the position, progression
// and save sequence of one character.
type CharacterState struct {
	CharacterID uuid.UUID
	ZoneID      string
	Position    Vec3
	Heading     float32
	Level       int32
	Experience  int64
	Health      int32
	// Currency is the character's purse (mechanics/loot.md rule 5.6.1). Money
	// occupies no bag slot and has no stack limit, so it is a scalar here
	// rather than a row in shard.character_inventory.
	Currency int64
	// Honor is the third thing a quest turn-in credits (mechanics/quests.md
	// rule 5.7.4). Every M2 quest awards zero; the column exists so that a
	// grant which does award some cannot lose it silently.
	Honor int64
	// SaveSeq must strictly increase per character. A save at or below the
	// stored value is rejected with [ErrStaleSave].
	SaveSeq int64
	SavedAt time.Time
}

// InventoryItem is a row of shard.character_inventory. Quantity is always
// positive; an emptied slot is deleted rather than zeroed.
type InventoryItem = inventory.InventoryItem

// QuestState is a row of shard.character_quests. Objectives is raw JSON so the
// quest module owns its own counter shape without a migration per change.
type QuestState = quests.QuestState

// Snapshot is everything one character save writes. ADR 0031 §5 requires all
// three parts in a single transaction: never three transactions, never a partial
// write, so inventory and quest state can never disagree.
type Snapshot struct {
	State     CharacterState
	Inventory []InventoryItem
	Quests    []QuestState
}

// Accounts owns auth.accounts.
type Accounts interface {
	CreateAccount(ctx context.Context, account Account) error
	AccountByID(ctx context.Context, accountID uuid.UUID) (Account, error)
	AccountByEmail(ctx context.Context, email string) (Account, error)
}

// Characters owns auth.characters.
type Characters interface {
	// CreateCharacter inserts unconditionally and returns [ErrNameTaken] when
	// the database's unique index refuses the row.
	CreateCharacter(ctx context.Context, character Character) error
	CharacterByID(ctx context.Context, characterID uuid.UUID) (Character, error)
	CharactersByAccount(ctx context.Context, accountID uuid.UUID) ([]Character, error)

	// CharacterByNormalizedName resolves a name that is already normalized.
	// It exists for the creation form's courtesy check and for diagnostics;
	// it is never a precondition of an insert, because check-then-insert is a
	// race and the unique index is the only authority (ADR 0032 §3).
	// Soft-deleted characters are still returned: their names stay taken.
	CharacterByNormalizedName(ctx context.Context, nameNormalized string) (Character, error)

	// DeleteCharacter soft-deletes one character of one account, stamping
	// deleted_at. It returns [ErrNotFound] when the character does not exist,
	// is already deleted, or belongs to another account — the three answers a
	// caller must not be able to tell apart, because distinguishing them
	// discloses somebody else's roster.
	DeleteCharacter(ctx context.Context, accountID, characterID uuid.UUID) error
}

// NameReservation is a row of auth.name_reservations: a courtesy for the
// creation form, not an authority. If the reservation table and the unique
// index ever disagree, the index wins (ADR 0032 §3).
type NameReservation struct {
	NameNormalized string
	AccountID      uuid.UUID
	ReservedUntil  time.Time
}

// NameReservations owns auth.name_reservations.
type NameReservations interface {
	// ReserveName holds a name for one account until ReservedUntil. An expired
	// row is ignored and overwritten; a live row held by another account is
	// refused with [ErrNameTaken].
	ReserveName(ctx context.Context, reservation NameReservation) error

	// ReleaseNameReservation drops a reservation the same account holds.
	// Releasing one that is absent or held by somebody else is not an error:
	// the caller is tidying up, not asserting anything.
	ReleaseNameReservation(ctx context.Context, nameNormalized string, accountID uuid.UUID) error
}

// CharacterStates owns shard.character_state.
type CharacterStates interface {
	// SaveCharacterState upserts, rejecting a state whose SaveSeq does not
	// advance past the stored one with [ErrStaleSave].
	SaveCharacterState(ctx context.Context, state CharacterState) error
	LoadCharacterState(ctx context.Context, characterID uuid.UUID) (CharacterState, error)
}

// Inventory owns shard.character_inventory.
type Inventory interface {
	// PutItem places quantity of itemID in slot. If the slot already holds the
	// same item the quantities stack; if it holds a different one the write is
	// refused with [ErrSlotOccupied].
	PutItem(ctx context.Context, characterID uuid.UUID, item InventoryItem) error

	// MoveItem relocates the contents of fromSlot to toSlot. An empty
	// destination takes the stack, a matching item absorbs it, and any other
	// item is swapped into the vacated slot.
	MoveItem(ctx context.Context, characterID uuid.UUID, fromSlot, toSlot int32) error

	// ReplaceInventory makes the stored inventory exactly items.
	ReplaceInventory(ctx context.Context, characterID uuid.UUID, items []InventoryItem) error

	// LoadInventory returns every occupied slot in ascending slot order.
	LoadInventory(ctx context.Context, characterID uuid.UUID) ([]InventoryItem, error)
}

// Quests owns shard.character_quests.
type Quests interface {
	UpsertQuestState(ctx context.Context, characterID uuid.UUID, quest QuestState) error
	LoadQuestStates(ctx context.Context, characterID uuid.UUID) ([]QuestState, error)
}

// Repository is the whole persistence surface. It is one interface rather than
// five so that [Repository.RunInTx] can hand a caller a transactional view of
// every table at once — which is what a character save needs.
type Repository interface {
	Accounts
	Characters
	NameReservations
	CharacterStates
	Inventory
	Quests

	// RunInTx runs fn inside one transaction, committing when fn returns nil and
	// rolling back on any error or panic. The Repository handed to fn is scoped
	// to that transaction; the outer one must not be used inside it.
	//
	// Nesting reuses the open transaction rather than opening a second one, so a
	// helper that saves may be called either standalone or inside a larger unit
	// of work without knowing which.
	RunInTx(ctx context.Context, fn func(ctx context.Context, tx Repository) error) error
}

// SaveCharacter writes state, inventory and quests as one transaction. Every
// caller in the save cadence of ADR 0031 §5 goes through here, so "one
// transaction per save" is a property of this function rather than a rule each
// call site has to remember.
func SaveCharacter(ctx context.Context, repository Repository, snapshot Snapshot) error {
	return repository.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
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
	})
}

// NormalizeEmail matches what the citext column on auth.accounts does, so the
// in-memory store rejects the same duplicates the database would.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func errQuantity(quantity int32) error {
	return fmt.Errorf("store: item quantity must be positive, got %d", quantity)
}

// objectivesOrEmpty keeps the jsonb column non-null for a quest that has no
// counters yet, so a reader never has to distinguish "no objectives" from
// "objectives unknown".
func objectivesOrEmpty(objectives []byte) []byte {
	if len(objectives) == 0 {
		return []byte("{}")
	}
	return objectives
}

// NormalizeCharacterName produces the auth.characters.name_normalized value:
// apostrophes and hyphens stripped, then lowercased (ADR 0032 §3). "O'brien",
// "Obrien" and "Ob-rien" therefore collide, which is the intent — impersonation
// by punctuation is the cheapest attack on a name system.
//
// It normalizes only. Shape validation belongs to the auth service, which owns
// the accept/reject table.
func NormalizeCharacterName(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))
	for _, character := range name {
		if character == '\'' || character == '-' {
			continue
		}
		builder.WriteRune(character)
	}
	return strings.ToLower(builder.String())
}
