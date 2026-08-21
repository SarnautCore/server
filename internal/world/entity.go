package world

import (
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
)

// EntityKind is what a world entity is, in simulation terms. It is deliberately
// a domain type: the wire enum of the same shape belongs to the session layer
// and this one must be free to diverge from it (ADR 0028).
type EntityKind = gametypes.EntityKind

const (
	EntityKindUnspecified = gametypes.EntityKindUnspecified
	EntityKindPlayer      = gametypes.EntityKindPlayer
	EntityKindNPC         = gametypes.EntityKindNPC
)

// AnimationState is the coarse pose the client should play.
type AnimationState = gametypes.AnimationState

const (
	AnimationStateUnspecified = gametypes.AnimationStateUnspecified
	AnimationStateIdle        = gametypes.AnimationStateIdle
	AnimationStateMoving      = gametypes.AnimationStateMoving
)

// Entity is one simulated thing in a zone.
//
// Every field except the position is written directly by the module that owns
// it: `internal/combat` sets health, level and faction, and nothing else does.
// The position is behind [Entity.Position] and [Tick.MoveTo] because the
// registry keeps a spatial index of it, and a direct assignment would leave a
// neighbour query answering from a stale cell.
//
// A *Entity is only valid inside the locked callback that handed it out. It
// must not be retained: [Zone.Leave] can remove the entity, and every field is
// read by the snapshot builder under the zone mutex.
type Entity struct {
	gametypes.EntityData

	position Vec3

	hasIntent       bool
	lastIntentSeq   uint64
	intentRemaining time.Duration
}

// Position is where the entity is. Write it with [Tick.MoveTo].
func (entity *Entity) Position() Vec3 { return entity.position }

// NPCSpec is everything the registry needs to place one non-player entity. The
// combat values come from the content pack; the zone invents none of them.
type NPCSpec = gametypes.NPCSpec
