package gametypes

import (
	"errors"
	"time"
)

// ErrUnknownEntity reports a command naming an entity the zone does not have.
var ErrUnknownEntity = errors.New("unknown world entity")

// EntityData is the mutable, non-spatial state gameplay modules share while a
// zone command holds the world lock. Position remains behind Tick.Position and
// Tick.MoveTo so no caller can desynchronize the spatial index.
type EntityData struct {
	ID   EntityID
	Kind EntityKind

	ContentID   string
	NameKey     string
	PlacementID string
	Faction     string
	Level       uint32
	Health      int32
	MaxHealth   int32
	Alive       bool
	Heading     float32
	Velocity    Vec3
	Animation   AnimationState
	// ResurrectionSicknessUntilTick is zero when the authored revive buff is
	// inactive. It is simulation state only and is not a wire field.
	ResurrectionSicknessUntilTick uint64

	Origin        Vec3
	OriginHeading float32
	Replicated    bool
}

// NPCSpec is everything needed to place one non-player entity.
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

// Tick is a locked view of one simulation instant. Implementations and values
// handed out through it are valid only until the current callback returns.
type Tick interface {
	Number() uint64
	Interval() time.Duration
	ZoneID() string
	Entity(EntityID) *EntityData
	Each(func(*EntityData) bool)
	Within(Vec3, float32, EntityKind, func(*EntityData) bool)
	Position(*EntityData) Vec3
	MoveTo(*EntityData, Vec3)
	SpawnNPC(NPCSpec) *EntityData
	Despawn(EntityID) bool
	After(uint64, func(Tick))
	PendingWork() int
}

// System is per-tick work registered with a zone.
type System interface {
	Step(Tick)
}

// Zone is the simulation seam used by gameplay modules. The Game prefix keeps
// this interface alongside world's existing concrete command methods without
// weakening their more specific return types.
type Zone interface {
	ID() string
	GameCommand(func(Tick) error) error
	GameAddSystem(System)
}
