package charstore

import "github.com/SarnautCore/server/internal/quests"

// ErrGrantWouldNotFit reports that a [QuestGrant]'s reward items do not fit in
// the character's bag, so nothing at all was written (mechanics/quests.md rule
// 5.7.3).
//
// It lives here for the same reason the values below do: the implementation
// raises it and the caller matches it, and the two packages must not import
// each other. It is a distinct sentinel rather than the inventory layer's own
// full-bag error because that one belongs to a package the quest module cannot
// name.
var ErrGrantWouldNotFit = quests.ErrGrantWouldNotFit

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
type ItemCount = quests.ItemCount

// QuestGrant is everything one turn-in changes about a character's persisted
// state (mechanics/quests.md rule 5.7.4), applied together or not at all.
//
// An abandon that destroys tracked items (rule 5.5.5) is the same shape with
// empty rewards, which is why this is a grant and not a reward: what makes it
// one value is the transaction, not the direction the items move.
type QuestGrant = quests.Grant

// QuestGrantResult is the character's holdings as the grant committed them.
//
// It is returned rather than left for the caller to re-read because the session
// keeps its own view for checkpointing, and a view refreshed by a second query
// is stale between the two.
type QuestGrantResult = quests.GrantResult
