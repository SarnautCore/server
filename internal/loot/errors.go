package loot

import "errors"

// Refusal names why a loot verb resolved to nothing.
//
// It is a typed value and never a bare string, and it is deliberately not a
// protocol ErrorCode: a protocol error is a violation and closes the
// connection, while opening somebody else's corpse or arriving with a full bag
// is ordinary play that must leave the session running (mechanics/loot.md rules
// 5.6.3 and 5.8.2).
type Refusal uint8

const (
	// RefusalNone means the verb resolved.
	RefusalNone Refusal = iota
	// RefusalNoCorpse means the entity named is not a corpse container in this
	// zone, or has already despawned.
	RefusalNoCorpse
	// RefusalNotYourLoot is rule 5.8.2: the corpse belongs to another
	// character.
	RefusalNotYourLoot
	// RefusalAlreadyLooted means the drop was taken. Rule 5.6.4 leaves the
	// corpse standing until its despawn tick, so this is the ordinary answer to
	// opening it twice, not an error.
	RefusalAlreadyLooted
	// RefusalBagFull is rule 5.6.3. Nothing was inserted, no money was
	// credited, and the corpse still holds the whole drop.
	RefusalBagFull
	// RefusalInProgress means another take on this corpse is mid-transaction.
	// One character cannot race itself in M2, but a retransmit can, and a
	// second concurrent take would be a second chance to insert the same drop.
	RefusalInProgress
	// RefusalInternal means the award could not be committed. The corpse is
	// left intact: an item that cannot be persisted has not been taken.
	RefusalInternal
	// RefusalInvalidItemIndex means a per-item take did not name an entry in
	// the corpse's current ordered item list. Nothing is reserved or awarded.
	RefusalInvalidItemIndex
)

func (refusal Refusal) String() string {
	switch refusal {
	case RefusalNone:
		return "none"
	case RefusalNoCorpse:
		return "no_corpse"
	case RefusalNotYourLoot:
		return "not_your_loot"
	case RefusalAlreadyLooted:
		return "already_looted"
	case RefusalBagFull:
		return "bag_full"
	case RefusalInProgress:
		return "in_progress"
	case RefusalInternal:
		return "internal"
	case RefusalInvalidItemIndex:
		return "invalid_item_index"
	default:
		return "unknown"
	}
}

// Errors a caller can match with [errors.Is]. Every one of them is also
// reported to the client as its [Refusal]; these exist so a Go caller — the
// slice driver, a test — can branch without re-deriving the reason.
var (
	ErrNoCorpse         = errors.New("loot: no such corpse")
	ErrNotYourLoot      = errors.New("loot: this corpse belongs to another character")
	ErrAlreadyLooted    = errors.New("loot: this corpse has already been looted")
	ErrBagFull          = errors.New("loot: the bag cannot hold this drop")
	ErrTakeInProgress   = errors.New("loot: a take on this corpse is already running")
	ErrInvalidItemIndex = errors.New("loot: item index is outside the current corpse drop")
)

func (refusal Refusal) err() error {
	switch refusal {
	case RefusalNoCorpse:
		return ErrNoCorpse
	case RefusalNotYourLoot:
		return ErrNotYourLoot
	case RefusalAlreadyLooted:
		return ErrAlreadyLooted
	case RefusalBagFull:
		return ErrBagFull
	case RefusalInProgress:
		return ErrTakeInProgress
	case RefusalInvalidItemIndex:
		return ErrInvalidItemIndex
	case RefusalNone, RefusalInternal:
		return nil
	default:
		return nil
	}
}
