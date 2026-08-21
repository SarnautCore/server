package world

import "github.com/SarnautCore/server/internal/gametypes"

// EntitySnapshot is one entity as a replication value.
//
// It is a value, not a pointer into the registry: the zone copies out from
// under its mutex once per publish, and every recipient reads the same
// immutable data afterwards. That is what retires ADR 0026's shared-batch
// hazard, and it is why the boundary type is not a protobuf message
// (ADR 0028).
type EntitySnapshot = gametypes.EntitySnapshot

// Snapshot is the newest view of a zone. It is immutable once published.
type Snapshot = gametypes.Snapshot

// SnapshotSink accepts the newest view of a zone.
//
// Implementations must treat every slice as immutable. PublishSnapshot builds
// a separate Snapshot, including separate backing memory, for each sink.
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
