package store

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uniqueViolation is SQLSTATE 23505. Character-name uniqueness is enforced by
// the index and by nothing else (ADR 0032 §3), so this is the only place that
// answers "was the name taken".
const uniqueViolation = "23505"

// The other two SQLSTATEs the schema can raise on a write: a CHECK the row
// fails, and a foreign key with nothing to point at. Both are mapped onto
// [ErrConstraintViolated] so a caller reads the same answer from either
// implementation (see `constraints.go`).
const (
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// querier is the intersection of *pgxpool.Pool and pgx.Tx, which is what lets
// every statement below be written once and run either directly or inside a
// transaction.
type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

type postgresStore struct {
	pool *pgxpool.Pool
	db   querier
	// inTx records whether db is a pgx.Tx rather than the pool, so RunInTx can
	// join an open transaction instead of opening a second one against a
	// different connection.
	inTx bool
}

// NewPostgres returns a [Repository] backed by an already-open pool — the one
// `internal/infra` builds from SARNAUT_POSTGRES_DSN. It does not open, ping or
// close the pool: connection lifetime stays with infra, so a store is cheap to
// construct and never owns something it did not create.
func NewPostgres(pool *pgxpool.Pool) (Repository, error) {
	if pool == nil {
		return nil, errors.New("store: postgres pool is required")
	}
	return &postgresStore{pool: pool, db: pool}, nil
}

func (store *postgresStore) RunInTx(ctx context.Context, fn func(ctx context.Context, tx Repository) error) error {
	if store.inTx {
		// Already inside a transaction: reuse it. Opening a second one here
		// would take a different pooled connection and block on the rows this
		// one already holds.
		return fn(ctx, store)
	}

	transaction, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	scoped := &postgresStore{pool: store.pool, db: transaction, inTx: true}
	if err := runAndRecover(ctx, scoped, fn); err != nil {
		return errors.Join(err, ignoreTxClosed(transaction.Rollback(ctx)))
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// runAndRecover converts a panic inside fn into an error so that the caller's
// rollback still runs. Without it a panic leaves the transaction open until the
// connection is reaped.
//
// The stack goes into the error text. A nil-map write or an index out of range
// inside a transaction is a bug, not a database outcome, and turning it into a
// bare "panic in transaction: ..." erases the only evidence of where it
// happened — the error surfaces in a log line that names the caller and nothing
// else, and the bug reads as an ordinary failed save.
func runAndRecover(
	ctx context.Context,
	scoped Repository,
	fn func(ctx context.Context, tx Repository) error,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic in transaction: %v\n%s", recovered, debug.Stack())
		}
	}()
	return fn(ctx, scoped)
}

func ignoreTxClosed(err error) error {
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return fmt.Errorf("rollback transaction: %w", err)
}

func (store *postgresStore) CreateAccount(ctx context.Context, account Account) error {
	const statement = `
		INSERT INTO auth.accounts (account_id, email, password_hash, created_at, disabled_at)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()), $5::timestamptz)`

	_, err := store.db.Exec(
		ctx,
		statement,
		account.AccountID,
		account.Email,
		account.PasswordHash,
		nullTime(account.CreatedAt),
		account.DisabledAt,
	)
	if isUniqueViolation(err, "accounts_email_key") {
		return ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("insert account: %w", err)
	}
	return nil
}

func (store *postgresStore) AccountByID(ctx context.Context, accountID uuid.UUID) (Account, error) {
	const statement = `
		SELECT account_id, email, password_hash, created_at, disabled_at
		FROM auth.accounts WHERE account_id = $1`
	return scanAccount(store.db.QueryRow(ctx, statement, accountID))
}

func (store *postgresStore) AccountByEmail(ctx context.Context, email string) (Account, error) {
	const statement = `
		SELECT account_id, email, password_hash, created_at, disabled_at
		FROM auth.accounts WHERE email = $1`
	return scanAccount(store.db.QueryRow(ctx, statement, email))
}

func scanAccount(row pgx.Row) (Account, error) {
	var account Account
	err := row.Scan(
		&account.AccountID,
		&account.Email,
		&account.PasswordHash,
		&account.CreatedAt,
		&account.DisabledAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("scan account: %w", err)
	}
	return account, nil
}

func (store *postgresStore) CreateCharacter(ctx context.Context, character Character) error {
	const statement = `
		INSERT INTO auth.characters (
			character_id, account_id, name, name_normalized, chargen_option_id, created_at, deleted_at
		) VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), $7::timestamptz)`

	normalized := character.NameNormalized
	if normalized == "" {
		normalized = NormalizeCharacterName(character.Name)
	}

	_, err := store.db.Exec(
		ctx,
		statement,
		character.CharacterID,
		character.AccountID,
		character.Name,
		normalized,
		character.ChargenOptionID,
		nullTime(character.CreatedAt),
		character.DeletedAt,
	)
	if isUniqueViolation(err, "characters_name_normalized_key") {
		return ErrNameTaken
	}
	return classifyConstraint(err, "insert character")
}

func (store *postgresStore) CharacterByID(ctx context.Context, characterID uuid.UUID) (Character, error) {
	const statement = `
		SELECT character_id, account_id, name, name_normalized, chargen_option_id, created_at, deleted_at
		FROM auth.characters WHERE character_id = $1`

	var character Character
	err := store.db.QueryRow(ctx, statement, characterID).Scan(
		&character.CharacterID,
		&character.AccountID,
		&character.Name,
		&character.NameNormalized,
		&character.ChargenOptionID,
		&character.CreatedAt,
		&character.DeletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Character{}, ErrNotFound
	}
	if err != nil {
		return Character{}, fmt.Errorf("scan character: %w", err)
	}
	return character, nil
}

func (store *postgresStore) CharactersByAccount(ctx context.Context, accountID uuid.UUID) ([]Character, error) {
	const statement = `
		SELECT character_id, account_id, name, name_normalized, chargen_option_id, created_at, deleted_at
		FROM auth.characters
		WHERE account_id = $1 AND deleted_at IS NULL
		ORDER BY created_at, character_id`

	rows, err := store.db.Query(ctx, statement, accountID)
	if err != nil {
		return nil, fmt.Errorf("query characters: %w", err)
	}
	defer rows.Close()

	characters := make([]Character, 0)
	for rows.Next() {
		var character Character
		if err := rows.Scan(
			&character.CharacterID,
			&character.AccountID,
			&character.Name,
			&character.NameNormalized,
			&character.ChargenOptionID,
			&character.CreatedAt,
			&character.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan character: %w", err)
		}
		characters = append(characters, character)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read characters: %w", err)
	}
	return characters, nil
}

func (store *postgresStore) CharacterByNormalizedName(ctx context.Context, nameNormalized string) (Character, error) {
	const statement = `
		SELECT character_id, account_id, name, name_normalized, chargen_option_id, created_at, deleted_at
		FROM auth.characters WHERE name_normalized = $1`

	var character Character
	err := store.db.QueryRow(ctx, statement, nameNormalized).Scan(
		&character.CharacterID,
		&character.AccountID,
		&character.Name,
		&character.NameNormalized,
		&character.ChargenOptionID,
		&character.CreatedAt,
		&character.DeletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Character{}, ErrNotFound
	}
	if err != nil {
		return Character{}, fmt.Errorf("scan character by name: %w", err)
	}
	return character, nil
}

func (store *postgresStore) DeleteCharacter(ctx context.Context, accountID, characterID uuid.UUID) error {
	// The account_id predicate is the authorization: a character of another
	// account matches nothing, which is the same answer as one that does not
	// exist. The name stays taken indefinitely (ADR 0032 §3).
	const statement = `
		UPDATE auth.characters SET deleted_at = now()
		WHERE character_id = $1 AND account_id = $2 AND deleted_at IS NULL`

	tag, err := store.db.Exec(ctx, statement, characterID, accountID)
	if err != nil {
		return fmt.Errorf("delete character: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *postgresStore) ReserveName(ctx context.Context, reservation NameReservation) error {
	// The conflict branch takes the row only when the existing reservation has
	// expired or already belongs to this account, so a live reservation held by
	// somebody else updates nothing and zero affected rows is the refusal.
	const statement = `
		INSERT INTO auth.name_reservations (name_normalized, account_id, reserved_until)
		VALUES ($1, $2, $3)
		ON CONFLICT (name_normalized) DO UPDATE SET
			account_id     = EXCLUDED.account_id,
			reserved_until = EXCLUDED.reserved_until
		WHERE name_reservations.reserved_until <= now()
		   OR name_reservations.account_id = EXCLUDED.account_id`

	tag, err := store.db.Exec(
		ctx,
		statement,
		reservation.NameNormalized,
		reservation.AccountID,
		reservation.ReservedUntil,
	)
	if err != nil {
		return fmt.Errorf("reserve character name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNameTaken
	}
	return nil
}

func (store *postgresStore) ReleaseNameReservation(ctx context.Context, nameNormalized string, accountID uuid.UUID) error {
	const statement = `
		DELETE FROM auth.name_reservations WHERE name_normalized = $1 AND account_id = $2`
	if _, err := store.db.Exec(ctx, statement, nameNormalized, accountID); err != nil {
		return fmt.Errorf("release name reservation: %w", err)
	}
	return nil
}

func (store *postgresStore) SaveCharacterState(ctx context.Context, state CharacterState) error {
	// The WHERE clause on the conflict branch is the anti-clobber rule of
	// ADR 0031 §6 expressed in one place: a save that does not advance save_seq
	// updates nothing, and zero affected rows is how the caller learns.
	const statement = `
		INSERT INTO shard.character_state (
			character_id, zone_id, position_x, position_y, position_z,
			heading, level, experience, health, currency, honor, save_seq, saved_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
		ON CONFLICT (character_id) DO UPDATE SET
			zone_id    = EXCLUDED.zone_id,
			position_x = EXCLUDED.position_x,
			position_y = EXCLUDED.position_y,
			position_z = EXCLUDED.position_z,
			heading    = EXCLUDED.heading,
			level      = EXCLUDED.level,
			experience = EXCLUDED.experience,
			health     = EXCLUDED.health,
			currency   = EXCLUDED.currency,
			honor      = EXCLUDED.honor,
			save_seq   = EXCLUDED.save_seq,
			saved_at   = now()
		WHERE character_state.save_seq < EXCLUDED.save_seq`

	tag, err := store.db.Exec(
		ctx,
		statement,
		state.CharacterID,
		state.ZoneID,
		state.Position.X,
		state.Position.Y,
		state.Position.Z,
		state.Heading,
		state.Level,
		state.Experience,
		state.Health,
		state.Currency,
		state.Honor,
		state.SaveSeq,
	)
	if err != nil {
		return classifyConstraint(err, "save character state")
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSave
	}
	return nil
}

func (store *postgresStore) LoadCharacterState(ctx context.Context, characterID uuid.UUID) (CharacterState, error) {
	const statement = `
		SELECT character_id, zone_id, position_x, position_y, position_z,
		       heading, level, experience, health, currency, honor, save_seq, saved_at
		FROM shard.character_state WHERE character_id = $1`

	var state CharacterState
	err := store.db.QueryRow(ctx, statement, characterID).Scan(
		&state.CharacterID,
		&state.ZoneID,
		&state.Position.X,
		&state.Position.Y,
		&state.Position.Z,
		&state.Heading,
		&state.Level,
		&state.Experience,
		&state.Health,
		&state.Currency,
		&state.Honor,
		&state.SaveSeq,
		&state.SavedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CharacterState{}, ErrNotFound
	}
	if err != nil {
		return CharacterState{}, fmt.Errorf("scan character state: %w", err)
	}
	return state, nil
}

func (store *postgresStore) PutItem(ctx context.Context, characterID uuid.UUID, item InventoryItem) error {
	if err := validateInventoryItem(item); err != nil {
		return err
	}

	// The conflict branch stacks only when the slot already holds the same item.
	// A different item leaves zero affected rows rather than being overwritten,
	// which is how an item is never silently destroyed by a misaddressed write.
	const statement = `
		INSERT INTO shard.character_inventory (character_id, slot, item_id, quantity)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (character_id, slot) DO UPDATE
		SET quantity = character_inventory.quantity + EXCLUDED.quantity
		WHERE character_inventory.item_id = EXCLUDED.item_id`

	tag, err := store.db.Exec(ctx, statement, characterID, item.Slot, item.ItemID, item.Quantity)
	if err != nil {
		return fmt.Errorf("put inventory item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSlotOccupied
	}
	return nil
}

func (store *postgresStore) MoveItem(ctx context.Context, characterID uuid.UUID, fromSlot, toSlot int32) error {
	if fromSlot == toSlot {
		return nil
	}

	return store.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		scoped, ok := tx.(*postgresStore)
		if !ok {
			return errors.New("store: transactional repository is not a postgres store")
		}
		return scoped.moveItemLocked(ctx, characterID, fromSlot, toSlot)
	})
}

func (store *postgresStore) moveItemLocked(ctx context.Context, characterID uuid.UUID, fromSlot, toSlot int32) error {
	source, err := store.itemAt(ctx, characterID, fromSlot)
	if err != nil {
		return err
	}

	destination, err := store.itemAt(ctx, characterID, toSlot)
	switch {
	case errors.Is(err, ErrNotFound):
		return store.relocate(ctx, characterID, fromSlot, toSlot)
	case err != nil:
		return err
	case destination.ItemID == source.ItemID:
		return store.stackInto(ctx, characterID, source, destination)
	default:
		return store.swap(ctx, characterID, source, destination)
	}
}

func (store *postgresStore) itemAt(ctx context.Context, characterID uuid.UUID, slot int32) (InventoryItem, error) {
	const statement = `
		SELECT slot, item_id, quantity FROM shard.character_inventory
		WHERE character_id = $1 AND slot = $2 FOR UPDATE`

	var item InventoryItem
	err := store.db.QueryRow(ctx, statement, characterID, slot).
		Scan(&item.Slot, &item.ItemID, &item.Quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return InventoryItem{}, ErrNotFound
	}
	if err != nil {
		return InventoryItem{}, fmt.Errorf("read inventory slot %d: %w", slot, err)
	}
	return item, nil
}

func (store *postgresStore) relocate(ctx context.Context, characterID uuid.UUID, fromSlot, toSlot int32) error {
	const statement = `
		UPDATE shard.character_inventory SET slot = $3
		WHERE character_id = $1 AND slot = $2`
	if _, err := store.db.Exec(ctx, statement, characterID, fromSlot, toSlot); err != nil {
		return fmt.Errorf("relocate inventory item: %w", err)
	}
	return nil
}

func (store *postgresStore) stackInto(ctx context.Context, characterID uuid.UUID, source, destination InventoryItem) error {
	const clear = `DELETE FROM shard.character_inventory WHERE character_id = $1 AND slot = $2`
	if _, err := store.db.Exec(ctx, clear, characterID, source.Slot); err != nil {
		return fmt.Errorf("clear source slot: %w", err)
	}

	const merge = `
		UPDATE shard.character_inventory SET quantity = quantity + $3
		WHERE character_id = $1 AND slot = $2`
	if _, err := store.db.Exec(ctx, merge, characterID, destination.Slot, source.Quantity); err != nil {
		return fmt.Errorf("stack inventory item: %w", err)
	}
	return nil
}

// swap exchanges two occupied slots. Both rows are deleted before either is
// re-inserted because the primary key on (character_id, slot) is checked per
// row: updating the slots in place would collide halfway through.
func (store *postgresStore) swap(ctx context.Context, characterID uuid.UUID, source, destination InventoryItem) error {
	const clear = `DELETE FROM shard.character_inventory WHERE character_id = $1 AND slot = ANY($2)`
	if _, err := store.db.Exec(ctx, clear, characterID, []int32{source.Slot, destination.Slot}); err != nil {
		return fmt.Errorf("clear swapped slots: %w", err)
	}

	source.Slot, destination.Slot = destination.Slot, source.Slot
	return store.insertItems(ctx, characterID, []InventoryItem{source, destination})
}

func (store *postgresStore) ReplaceInventory(ctx context.Context, characterID uuid.UUID, items []InventoryItem) error {
	return store.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		scoped, ok := tx.(*postgresStore)
		if !ok {
			return errors.New("store: transactional repository is not a postgres store")
		}

		const clear = `DELETE FROM shard.character_inventory WHERE character_id = $1`
		if _, err := scoped.db.Exec(ctx, clear, characterID); err != nil {
			return fmt.Errorf("clear inventory: %w", err)
		}
		return scoped.insertItems(ctx, characterID, items)
	})
}

func (store *postgresStore) insertItems(ctx context.Context, characterID uuid.UUID, items []InventoryItem) error {
	const statement = `
		INSERT INTO shard.character_inventory (character_id, slot, item_id, quantity)
		VALUES ($1, $2, $3, $4)`

	for _, item := range items {
		if err := validateInventoryItem(item); err != nil {
			return err
		}
		_, err := store.db.Exec(ctx, statement, characterID, item.Slot, item.ItemID, item.Quantity)
		if err != nil {
			return classifyConstraint(err, fmt.Sprintf("insert inventory item in slot %d", item.Slot))
		}
	}
	return nil
}

func (store *postgresStore) LoadInventory(ctx context.Context, characterID uuid.UUID) ([]InventoryItem, error) {
	const statement = `
		SELECT slot, item_id, quantity FROM shard.character_inventory
		WHERE character_id = $1 ORDER BY slot`

	rows, err := store.db.Query(ctx, statement, characterID)
	if err != nil {
		return nil, fmt.Errorf("query inventory: %w", err)
	}
	defer rows.Close()

	items := make([]InventoryItem, 0)
	for rows.Next() {
		var item InventoryItem
		if err := rows.Scan(&item.Slot, &item.ItemID, &item.Quantity); err != nil {
			return nil, fmt.Errorf("scan inventory item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	return items, nil
}

func (store *postgresStore) UpsertQuestState(ctx context.Context, characterID uuid.UUID, quest QuestState) error {
	const statement = `
		INSERT INTO shard.character_quests (character_id, quest_id, state, objectives, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (character_id, quest_id) DO UPDATE SET
			state      = EXCLUDED.state,
			objectives = EXCLUDED.objectives,
			updated_at = now()`

	if _, err := store.db.Exec(
		ctx,
		statement,
		characterID,
		quest.QuestID,
		quest.State,
		objectivesOrEmpty(quest.Objectives),
	); err != nil {
		return fmt.Errorf("upsert quest state: %w", err)
	}
	return nil
}

func (store *postgresStore) LoadQuestStates(ctx context.Context, characterID uuid.UUID) ([]QuestState, error) {
	const statement = `
		SELECT quest_id, state, objectives, updated_at FROM shard.character_quests
		WHERE character_id = $1 ORDER BY quest_id`

	rows, err := store.db.Query(ctx, statement, characterID)
	if err != nil {
		return nil, fmt.Errorf("query quest states: %w", err)
	}
	defer rows.Close()

	quests := make([]QuestState, 0)
	for rows.Next() {
		var quest QuestState
		if err := rows.Scan(&quest.QuestID, &quest.State, &quest.Objectives, &quest.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan quest state: %w", err)
		}
		quests = append(quests, quest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read quest states: %w", err)
	}
	return quests, nil
}

// nullTime lets a caller leave CreatedAt zero and have the database stamp it,
// which is what every production call site does; tests that need a fixed clock
// pass a value instead.
func nullTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// classifyConstraint turns a CHECK or FOREIGN KEY violation into
// [ErrConstraintViolated], naming the constraint the database named.
//
// Without it, a level of zero or an orphan account_id arrives at the caller as
// an opaque wrapped driver error while the in-memory store returns a sentinel
// for the same input — the two implementations disagreeing about the same row is
// exactly what the shared conformance suite exists to prevent. Any other error
// is returned unchanged, wrapped by `context`.
func classifyConstraint(err error, context string) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == checkViolation || pgErr.Code == foreignKeyViolation) {
		return fmt.Errorf("%w: %s rejected by %s", ErrConstraintViolated, context, pgErr.ConstraintName)
	}
	return fmt.Errorf("%s: %w", context, err)
}
