package world

// EntitySnapshot is one entity as a replication value.
//
// It is a value, not a pointer into the registry: the zone copies out from
// under its mutex once per publish, and every recipient reads the same
// immutable data afterwards. That is what retires ADR 0026's shared-batch
// hazard, and it is why the boundary type is not a protobuf message
// (ADR 0028).
type EntitySnapshot struct {
	EntityID  uint64
	Kind      EntityKind
	Position  Vec3
	Heading   float32
	Velocity  Vec3
	Animation AnimationState

	ContentID string
	NameKey   string
	Faction   string
	Level     uint32
	Health    int32
	MaxHealth int32
	Alive     bool
}

// Snapshot is the newest view of a zone. It is immutable once published.
type Snapshot struct {
	ServerTick uint64
	Entities   []EntitySnapshot
}

// SnapshotSink accepts the newest view of a zone.
//
// Implementations must not retain or mutate the slice: it is shared across
// every sink of one publish, which is safe precisely because nothing writes to
// it.
type SnapshotSink interface {
	OfferSnapshot(Snapshot)
}

func viewOf(entity *Entity) EntitySnapshot {
	return EntitySnapshot{
		EntityID:  entity.ID,
		Kind:      entity.Kind,
		Position:  entity.position,
		Heading:   entity.Heading,
		Velocity:  entity.Velocity,
		Animation: entity.Animation,
		ContentID: entity.ContentID,
		NameKey:   entity.NameKey,
		Faction:   entity.Faction,
		Level:     entity.Level,
		Health:    entity.Health,
		MaxHealth: entity.MaxHealth,
		Alive:     entity.Alive,
	}
}
