package charstore

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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
		INSERT INTO shard.character_inventory (
			character_id, slot, instance_id, product_item_id, quantity, counter_value,
			is_bound, is_cursed, is_quest_operator, remove_time_raw,
			rune_resource_id, rune_slot_resource_id
		)
		VALUES ($1, $2, $3::numeric, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (character_id, slot) DO UPDATE
		SET quantity = character_inventory.quantity + EXCLUDED.quantity
		WHERE character_inventory.product_item_id = EXCLUDED.product_item_id`

	tag, err := store.db.Exec(ctx, statement, characterID, item.Slot, strconv.FormatUint(item.InstanceID, 10), item.ItemID, item.Quantity,
		item.CounterValue, item.Bound, item.Cursed, item.QuestOperator, item.RemoveTime,
		item.RuneResourceID, item.RuneSlotResourceID)
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
		SELECT slot, instance_id::text, product_item_id, quantity, counter_value,
		       is_bound, is_cursed, is_quest_operator, remove_time_raw,
		       rune_resource_id, rune_slot_resource_id
		FROM shard.character_inventory
		WHERE character_id = $1 AND slot = $2 FOR UPDATE`

	item, err := scanInventoryItem(store.db.QueryRow(ctx, statement, characterID, slot))
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
		hud, err := scoped.LoadCharacterHUD(ctx, characterID)
		switch {
		case err == nil:
			if err := validateSnapshotItemIdentities(items, &hud); err != nil {
				return err
			}
		case errors.Is(err, ErrNotFound):
			if err := validateSnapshotItemIdentities(items, nil); err != nil {
				return err
			}
		default:
			return err
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
		INSERT INTO shard.character_inventory (
			character_id, slot, instance_id, product_item_id, quantity, counter_value,
			is_bound, is_cursed, is_quest_operator, remove_time_raw,
			rune_resource_id, rune_slot_resource_id
		)
		VALUES ($1, $2, $3::numeric, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	for _, item := range items {
		if err := validateInventoryItem(item); err != nil {
			return err
		}
		_, err := store.db.Exec(ctx, statement, characterID, item.Slot, strconv.FormatUint(item.InstanceID, 10), item.ItemID, item.Quantity,
			item.CounterValue, item.Bound, item.Cursed, item.QuestOperator, item.RemoveTime,
			item.RuneResourceID, item.RuneSlotResourceID)
		if err != nil {
			return classifyConstraint(err, fmt.Sprintf("insert inventory item in slot %d", item.Slot))
		}
	}
	return nil
}

func (store *postgresStore) ReplaceInventoryAndHUD(
	ctx context.Context,
	characterID uuid.UUID,
	items []InventoryItem,
	hud CharacterHUDState,
) error {
	if err := validateHUDState(hud); err != nil {
		return err
	}
	if err := validateSnapshotItemIdentities(items, &hud); err != nil {
		return err
	}
	const clear = `DELETE FROM shard.character_inventory WHERE character_id = $1`
	if _, err := store.db.Exec(ctx, clear, characterID); err != nil {
		return fmt.Errorf("clear inventory: %w", err)
	}
	if err := store.insertItems(ctx, characterID, items); err != nil {
		return err
	}
	return store.replaceHUDLocked(ctx, characterID, hud)
}

func (store *postgresStore) LoadInventory(ctx context.Context, characterID uuid.UUID) ([]InventoryItem, error) {
	const statement = `
		SELECT slot, instance_id::text, product_item_id, quantity, counter_value,
		       is_bound, is_cursed, is_quest_operator, remove_time_raw,
		       rune_resource_id, rune_slot_resource_id
		FROM shard.character_inventory
		WHERE character_id = $1 ORDER BY slot`

	rows, err := store.db.Query(ctx, statement, characterID)
	if err != nil {
		return nil, fmt.Errorf("query inventory: %w", err)
	}
	defer rows.Close()

	items := make([]InventoryItem, 0)
	for rows.Next() {
		item, err := scanInventoryItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan inventory item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	return items, nil
}

type rowScanner interface {
	Scan(destinations ...any) error
}

func scanInventoryItem(row rowScanner) (InventoryItem, error) {
	var item InventoryItem
	var instanceID string
	var removeTime pgtype.Int8
	var runeResource, runeSlotResource pgtype.Text
	err := row.Scan(
		&item.Slot,
		&instanceID,
		&item.ItemID,
		&item.Quantity,
		&item.CounterValue,
		&item.Bound,
		&item.Cursed,
		&item.QuestOperator,
		&removeTime,
		&runeResource,
		&runeSlotResource,
	)
	if err != nil {
		return InventoryItem{}, err
	}
	item.InstanceID, err = strconv.ParseUint(instanceID, 10, 64)
	if err != nil {
		return InventoryItem{}, fmt.Errorf("parse inventory instance id %q: %w", instanceID, err)
	}
	applyNullableItemFields(&item.RemoveTime, &item.RuneResourceID, &item.RuneSlotResourceID,
		removeTime, runeResource, runeSlotResource)
	return item, nil
}

func scanEquipmentItem(row rowScanner) (EquipmentSlot, ItemInstance, error) {
	var slot int16
	var item ItemInstance
	var instanceID string
	var removeTime pgtype.Int8
	var runeResource, runeSlotResource pgtype.Text
	err := row.Scan(
		&slot,
		&instanceID,
		&item.ItemID,
		&item.Quantity,
		&item.CounterValue,
		&item.Bound,
		&item.Cursed,
		&item.QuestOperator,
		&removeTime,
		&runeResource,
		&runeSlotResource,
	)
	if err != nil {
		return 0, ItemInstance{}, err
	}
	item.InstanceID, err = strconv.ParseUint(instanceID, 10, 64)
	if err != nil {
		return 0, ItemInstance{}, fmt.Errorf("parse equipment instance id %q: %w", instanceID, err)
	}
	applyNullableItemFields(&item.RemoveTime, &item.RuneResourceID, &item.RuneSlotResourceID,
		removeTime, runeResource, runeSlotResource)
	return EquipmentSlot(slot), item, nil
}

func applyNullableItemFields(
	removeTime **int64,
	runeResource **string,
	runeSlotResource **string,
	storedRemoveTime pgtype.Int8,
	storedRuneResource pgtype.Text,
	storedRuneSlotResource pgtype.Text,
) {
	if storedRemoveTime.Valid {
		value := storedRemoveTime.Int64
		*removeTime = &value
	}
	if storedRuneResource.Valid {
		value := storedRuneResource.String
		*runeResource = &value
	}
	if storedRuneSlotResource.Valid {
		value := storedRuneSlotResource.String
		*runeSlotResource = &value
	}
}

func optionalFloat4(value pgtype.Float4) *float32 {
	if !value.Valid {
		return nil
	}
	authored := value.Float32
	return &authored
}

func (store *postgresStore) SaveCharacterHUD(
	ctx context.Context,
	characterID uuid.UUID,
	hud CharacterHUDState,
) error {
	if characterID == uuid.Nil {
		return fmt.Errorf("%w: HUD state has no character_id", ErrConstraintViolated)
	}
	if err := validateHUDState(hud); err != nil {
		return err
	}
	return store.RunInTx(ctx, func(ctx context.Context, tx Repository) error {
		scoped, ok := tx.(*postgresStore)
		if !ok {
			return errors.New("store: transactional repository is not a postgres store")
		}
		inventory, err := scoped.LoadInventory(ctx, characterID)
		if err != nil {
			return err
		}
		if err := validateSnapshotItemIdentities(inventory, &hud); err != nil {
			return err
		}
		return scoped.replaceHUDLocked(ctx, characterID, hud)
	})
}

func (store *postgresStore) replaceHUDLocked(
	ctx context.Context,
	characterID uuid.UUID,
	hud CharacterHUDState,
) error {
	if _, err := store.db.Exec(ctx,
		`DELETE FROM shard.character_equipment WHERE character_id = $1`, characterID); err != nil {
		return fmt.Errorf("clear character equipment: %w", err)
	}
	for _, equipped := range hud.Equipment {
		if err := store.insertEquipmentItem(ctx, characterID, equipped.Slot, equipped.ItemInstance); err != nil {
			return err
		}
	}
	if hud.Bag != nil {
		if err := store.insertEquipmentItem(ctx, characterID, EquipmentBag, *hud.Bag); err != nil {
			return err
		}
	}

	var partitions [MaxBagPartitions]*int16
	for index, partition := range hud.BagLayout.Partitions {
		capacity := int16(partition.Capacity)
		partitions[index] = &capacity
	}
	const saveLayout = `
		INSERT INTO shard.character_bag_layout (
			character_id, layout_id, partition_0, partition_1, partition_2, partition_3, partition_4
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (character_id) DO UPDATE SET
			layout_id = EXCLUDED.layout_id,
			partition_0 = EXCLUDED.partition_0,
			partition_1 = EXCLUDED.partition_1,
			partition_2 = EXCLUDED.partition_2,
			partition_3 = EXCLUDED.partition_3,
			partition_4 = EXCLUDED.partition_4`
	if _, err := store.db.Exec(ctx, saveLayout, characterID, hud.BagLayout.LayoutID,
		partitions[0], partitions[1], partitions[2], partitions[3], partitions[4]); err != nil {
		return classifyConstraint(err, "save character bag layout")
	}

	if _, err := store.db.Exec(ctx,
		`DELETE FROM shard.character_stats WHERE character_id = $1`, characterID); err != nil {
		return fmt.Errorf("clear character stats: %w", err)
	}
	const insertStat = `
		INSERT INTO shard.character_stats (
			character_id, ordinal, base, result, result_long_term
		) VALUES ($1, $2, $3, $4, $5)`
	for _, stat := range hud.Stats {
		if _, err := store.db.Exec(ctx, insertStat, characterID, int16(stat.Ordinal),
			stat.Base, stat.Result, stat.ResultLongTerm); err != nil {
			return classifyConstraint(err, fmt.Sprintf("insert character stat %d", stat.Ordinal))
		}
	}

	if _, err := store.db.Exec(ctx,
		`DELETE FROM shard.character_actions WHERE character_id = $1`, characterID); err != nil {
		return fmt.Errorf("clear character action slots: %w", err)
	}
	const insertAction = `
		INSERT INTO shard.character_actions (character_id, ordinal, ability_id) VALUES ($1, $2, $3)`
	for _, action := range hud.Actions {
		if _, err := store.db.Exec(ctx, insertAction, characterID, action.Ordinal, action.AbilityID); err != nil {
			return classifyConstraint(err, fmt.Sprintf("insert character action slot %d", action.Ordinal))
		}
	}
	return nil
}

func (store *postgresStore) insertEquipmentItem(
	ctx context.Context,
	characterID uuid.UUID,
	slot EquipmentSlot,
	item ItemInstance,
) error {
	const statement = `
		INSERT INTO shard.character_equipment (
			character_id, slot, instance_id, product_item_id, quantity, counter_value,
			is_bound, is_cursed, is_quest_operator, remove_time_raw,
			rune_resource_id, rune_slot_resource_id
		) VALUES ($1, $2, $3::numeric, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err := store.db.Exec(ctx, statement, characterID, int16(slot), strconv.FormatUint(item.InstanceID, 10), item.ItemID, item.Quantity,
		item.CounterValue, item.Bound, item.Cursed, item.QuestOperator, item.RemoveTime,
		item.RuneResourceID, item.RuneSlotResourceID)
	if err != nil {
		return classifyConstraint(err, fmt.Sprintf("insert equipment item in slot %d", slot))
	}
	return nil
}

func (store *postgresStore) LoadCharacterHUD(
	ctx context.Context,
	characterID uuid.UUID,
) (CharacterHUDState, error) {
	const loadLayout = `
		SELECT layout_id, partition_0, partition_1, partition_2, partition_3, partition_4
		FROM shard.character_bag_layout WHERE character_id = $1`
	var hud CharacterHUDState
	var capacities [MaxBagPartitions]pgtype.Int2
	err := store.db.QueryRow(ctx, loadLayout, characterID).Scan(
		&hud.BagLayout.LayoutID,
		&capacities[0],
		&capacities[1],
		&capacities[2],
		&capacities[3],
		&capacities[4],
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CharacterHUDState{}, ErrNotFound
	}
	if err != nil {
		return CharacterHUDState{}, fmt.Errorf("scan character bag layout: %w", err)
	}
	for ordinal, capacity := range capacities {
		if capacity.Valid {
			hud.BagLayout.Partitions = append(hud.BagLayout.Partitions, BagPartition{
				Ordinal: int16(ordinal), Capacity: int32(capacity.Int16),
			})
		}
	}

	const loadEquipment = `
		SELECT slot, instance_id::text, product_item_id, quantity, counter_value,
		       is_bound, is_cursed, is_quest_operator, remove_time_raw,
		       rune_resource_id, rune_slot_resource_id
		FROM shard.character_equipment WHERE character_id = $1 ORDER BY slot`
	rows, err := store.db.Query(ctx, loadEquipment, characterID)
	if err != nil {
		return CharacterHUDState{}, fmt.Errorf("query character equipment: %w", err)
	}
	for rows.Next() {
		slot, item, err := scanEquipmentItem(rows)
		if err != nil {
			rows.Close()
			return CharacterHUDState{}, fmt.Errorf("scan character equipment: %w", err)
		}
		if slot == EquipmentBag {
			bag := item
			hud.Bag = &bag
		} else {
			hud.Equipment = append(hud.Equipment, EquipmentItem{Slot: slot, ItemInstance: item})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CharacterHUDState{}, fmt.Errorf("read character equipment: %w", err)
	}
	rows.Close()

	hud.Stats = EmptyOrderedStats()
	const loadStats = `
		SELECT ordinal, base, result, result_long_term
		FROM shard.character_stats WHERE character_id = $1 ORDER BY ordinal`
	rows, err = store.db.Query(ctx, loadStats, characterID)
	if err != nil {
		return CharacterHUDState{}, fmt.Errorf("query character stats: %w", err)
	}
	statRows := 0
	for rows.Next() {
		var ordinal int16
		var base, result, resultLongTerm pgtype.Float4
		if err := rows.Scan(&ordinal, &base, &result, &resultLongTerm); err != nil {
			rows.Close()
			return CharacterHUDState{}, fmt.Errorf("scan character stat: %w", err)
		}
		if ordinal < 0 || ordinal >= int16(StatCount) {
			rows.Close()
			return CharacterHUDState{}, fmt.Errorf("store: character stat ordinal %d is outside 0..13", ordinal)
		}
		hud.Stats[ordinal].Base = optionalFloat4(base)
		hud.Stats[ordinal].Result = optionalFloat4(result)
		hud.Stats[ordinal].ResultLongTerm = optionalFloat4(resultLongTerm)
		statRows++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CharacterHUDState{}, fmt.Errorf("read character stats: %w", err)
	}
	rows.Close()
	if statRows != int(StatCount) {
		return CharacterHUDState{}, fmt.Errorf("store: character has %d stat rows, want exactly %d", statRows, StatCount)
	}

	hud.Actions = EmptyOrderedActionSlots()
	const loadActions = `
		SELECT ordinal, ability_id FROM shard.character_actions WHERE character_id = $1 ORDER BY ordinal`
	rows, err = store.db.Query(ctx, loadActions, characterID)
	if err != nil {
		return CharacterHUDState{}, fmt.Errorf("query character action slots: %w", err)
	}
	actionRows := 0
	for rows.Next() {
		var ordinal int16
		var ability pgtype.Text
		if err := rows.Scan(&ordinal, &ability); err != nil {
			rows.Close()
			return CharacterHUDState{}, fmt.Errorf("scan character action slot: %w", err)
		}
		if ordinal < 0 || ordinal >= ActionSlotCount {
			rows.Close()
			return CharacterHUDState{}, fmt.Errorf("store: character action ordinal %d is outside 0..35", ordinal)
		}
		if ability.Valid {
			value := ability.String
			hud.Actions[ordinal].AbilityID = &value
		}
		actionRows++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CharacterHUDState{}, fmt.Errorf("read character action slots: %w", err)
	}
	rows.Close()
	if actionRows != ActionSlotCount {
		return CharacterHUDState{}, fmt.Errorf("store: character has %d action rows, want exactly %d", actionRows, ActionSlotCount)
	}
	if err := validateHUDState(hud); err != nil {
		return CharacterHUDState{}, fmt.Errorf("load character HUD: %w", err)
	}
	return hud, nil
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
