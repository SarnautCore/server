package gametypes

// EntitySnapshot is one entity copied out for replication.
type EntitySnapshot struct {
	EntityID  EntityID
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

// Snapshot is the newest visible view of a zone.
type Snapshot struct {
	ServerTick EntityID
	Entities   []EntitySnapshot
	Spawns     []EntitySnapshot
	Despawns   []EntityID
}
