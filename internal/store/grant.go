package store

import (
	"errors"

	"github.com/google/uuid"
)

// ErrGrantWouldNotFit reports that a [QuestGrant]'s reward items do not fit in
// the character's bag, so nothing at all was written (mechanics/quests.md rule
// 5.7.3).
//
// It lives here for the same reason the values below do: the implementation
// raises it and the caller matches it, and the two packages must not import
// each other. It is a distinct sentinel rather than the inventory layer's own
// full-bag error because that one belongs to a package the quest module cannot
// name.
var ErrGrantWouldNotFit = errors.New("store: the quest grant does not fit in the bag")

// The types in this file describe one quest turn-in as a unit of work, and
// they live here rather than in `internal/inventory` for a structural reason
// worth stating.
//
// `internal/quests` declares the interface the grant crosses; `internal/inventory`
// implements it, because the bag arithmetic of mechanics/loot.md rule 5.7 is
// inventory's and the transaction has to contain it. Neither package may import
// the other: quests reaching into inventory would make the seam decorative, and
// inventory reaching into quests would put a gameplay module underneath a
// storage one. A Go interface can only be satisfied structurally, so the values
// on the seam have to be nameable from both sides — which means here, in the
// package they both already depend on.

// ItemCount is one item id and a number of units. It is not a bag slot: stack
// limits have not been applied and duplicates have not been merged.
type ItemCount struct {
	ItemID string
	Count  int32
}

// QuestGrant is everything one turn-in changes about a character's persisted
// state (mechanics/quests.md rule 5.7.4), applied together or not at all.
//
// An abandon that destroys tracked items (rule 5.5.5) is the same shape with
// empty rewards, which is why this is a grant and not a reward: what makes it
// one value is the transaction, not the direction the items move.
type QuestGrant struct {
	CharacterID uuid.UUID

	// Consume is removed from the bag before Grants is inserted. Doing it in
	// that order is what makes rule 6.2's arithmetic net rather than gross: a
	// quest that takes five of something and gives one thing back frees a slot
	// instead of needing one.
	Consume []ItemCount
	Grants  []ItemCount

	Experience int64
	Money      int64
	Honor      int64

	// Quest is the row that says the turn-in happened. It is written inside the
	// same transaction as the items and the currencies, so there is no state in
	// which experience was granted and the quest is still open.
	Quest QuestState
}

// QuestGrantResult is the character's holdings as the grant committed them.
//
// It is returned rather than left for the caller to re-read because the session
// keeps its own view for checkpointing, and a view refreshed by a second query
// is stale between the two.
type QuestGrantResult struct {
	Inventory  []InventoryItem
	Currency   int64
	Experience int64
	Honor      int64
	// SaveSeq is the sequence the grant committed at. The session adopts it so
	// its next checkpoint advances past this write instead of being rejected as
	// stale (ADR 0031 §6).
	SaveSeq int64
}
