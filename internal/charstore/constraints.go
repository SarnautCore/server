package charstore

import (
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

// This file restates, in Go, the CHECK and FOREIGN KEY constraints
// `migrations/00001_initial_schema.sql` puts on the schema.
//
// They are restated because the package doc promises that a test passing
// against [NewMemory] is evidence about Postgres. An in-memory store that
// accepts a two-character name, or an orphan account_id, or a level of zero,
// while the schema refuses all three, breaks that promise in the one direction
// that matters: green in test, SQLSTATE 23514 in production, surfacing as an
// unclassified "insert character: ...".
//
// Postgres remains the authority for its own rows — these functions do not run
// on that path, and a check-then-insert would be a race in any case. What the
// Postgres implementation does instead is map SQLSTATE 23514 (check violation)
// and 23503 (foreign key violation) onto the same [ErrConstraintViolated] the
// memory implementation returns, so both answer the same question the same way.

// Character name bounds, mirroring `characters_name_length_check`. The column
// check counts characters, not bytes, so this counts runes.
const (
	MinCharacterNameLength = 3
	MaxCharacterNameLength = 16
	// MaxResurrectionSicknessMS is the largest millisecond count that can be
	// converted to time.Duration without wrapping.
	MaxResurrectionSicknessMS int64 = 9_223_372_036_854
)

// validateCharacter applies auth.characters' own constraints.
func validateCharacter(character Character) error {
	length := utf8.RuneCountInString(character.Name)
	if length < MinCharacterNameLength || length > MaxCharacterNameLength {
		return fmt.Errorf(
			"%w: character name is %d characters, must be between %d and %d",
			ErrConstraintViolated, length, MinCharacterNameLength, MaxCharacterNameLength,
		)
	}
	if character.AccountID == uuid.Nil {
		return fmt.Errorf("%w: character has no account_id", ErrConstraintViolated)
	}
	return nil
}

// validateCharacterState applies shard.character_state's own constraints.
func validateCharacterState(state CharacterState) error {
	if err := validateResurrectionSickness(state.ResurrectionSicknessMS); err != nil {
		return err
	}
	switch {
	case state.Level < 1:
		return fmt.Errorf("%w: level is %d, must be at least 1", ErrConstraintViolated, state.Level)
	case state.Experience < 0:
		return fmt.Errorf(
			"%w: experience is %d, must not be negative", ErrConstraintViolated, state.Experience)
	case state.Currency < 0:
		return fmt.Errorf(
			"%w: currency is %d, must not be negative", ErrConstraintViolated, state.Currency)
	case state.Honor < 0:
		return fmt.Errorf(
			"%w: honor is %d, must not be negative", ErrConstraintViolated, state.Honor)
	case state.SaveSeq < 0:
		return fmt.Errorf(
			"%w: save_seq is %d, must not be negative", ErrConstraintViolated, state.SaveSeq)
	}
	return nil
}

func validateResurrectionSickness(milliseconds int64) error {
	switch {
	case milliseconds < 0:
		return fmt.Errorf(
			"%w: resurrection sickness is %dms, must not be negative",
			ErrConstraintViolated, milliseconds)
	case milliseconds > MaxResurrectionSicknessMS:
		return fmt.Errorf(
			"%w: resurrection sickness is %dms, exceeds duration capacity",
			ErrConstraintViolated, milliseconds)
	default:
		return nil
	}
}

// validateInventoryItem applies shard.character_inventory's own constraints.
// Quantity keeps its existing dedicated error, which callers already read.
func validateInventoryItem(item InventoryItem) error {
	if item.Quantity <= 0 {
		return errQuantity(item.Quantity)
	}
	if item.Slot < 0 {
		return fmt.Errorf("%w: slot is %d, must not be negative", ErrConstraintViolated, item.Slot)
	}
	return nil
}
