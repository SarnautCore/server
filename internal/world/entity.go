package world

import "time"

// EntityKind is what a world entity is, in simulation terms. It is deliberately
// a domain type: the wire enum of the same shape belongs to the session layer
// and this one must be free to diverge from it (ADR 0028).
type EntityKind uint8

const (
	EntityKindUnspecified EntityKind = iota
	EntityKindPlayer
	EntityKindNPC
)

// AnimationState is the coarse pose the client should play.
type AnimationState uint8

const (
	AnimationStateUnspecified AnimationState = iota
	AnimationStateIdle
	AnimationStateMoving
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
	ID   uint64
	Kind EntityKind

	// Content identity, resolved by the reader against the runtime pack.
	ContentID string
	NameKey   string
	// PlacementID names the authored spawn slot this entity came from. It is
	// the respawn key of mechanics/combat.md rule 5.9.4.
	PlacementID string

	// Combatant state, mechanics/combat.md section 4.
	Faction   string
	Level     uint32
	Health    int32
	MaxHealth int32
	Alive     bool

	Heading   float32
	Velocity  Vec3
	Animation AnimationState

	// Origin is where the entity was first placed: a mob's anchor for the leash
	// radius, and where a respawn puts it back.
	Origin        Vec3
	OriginHeading float32

	// Replicated is false while the entity exists in the registry but must not
	// appear in a snapshot: a player between joining and subscribing, and a
	// corpse that has despawned and is waiting to respawn.
	Replicated bool

	position Vec3

	hasIntent       bool
	lastIntentSeq   uint64
	intentRemaining time.Duration
}

// Position is where the entity is. Write it with [Tick.MoveTo].
func (entity *Entity) Position() Vec3 { return entity.position }

// NPCSpec is everything the registry needs to place one non-player entity. The
// combat values come from the content pack; the zone invents none of them.
type NPCSpec struct {
	ContentID   string
	NameKey     string
	PlacementID string
	Faction     string
	Level       uint32
	MaxHealth   int32
	Position    Vec3
	Heading     float32
}
