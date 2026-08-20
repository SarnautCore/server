// Package world owns authoritative zone simulation and replication state.
//
// It exposes domain types only. Nothing here imports the generated protobuf
// package, and nothing may: the wire schema is the session layer's problem, and
// a second front-end has to be addable without touching the simulation
// (ADR 0028, ADR 0010).
package world

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrUnknownEntity reports a command naming an entity this zone does not have.
var ErrUnknownEntity = errors.New("unknown world entity")

// ZoneConfig sets fixed simulation and replication rates.
type ZoneConfig struct {
	ID               string
	TickInterval     time.Duration
	SnapshotInterval time.Duration
	MaxMoveSpeed     float32
	PlayerSpawn      Vec3
}

// System is per-tick simulation work registered with a zone.
//
// A system runs inside the zone's lock, once per tick, in registration order.
// That is what lets `internal/combat` mutate entity health without owning a
// second mutex and without the zone knowing what health means.
type System interface {
	Step(*Tick)
}

// Zone owns one entity registry and its fixed-rate simulation.
type Zone struct {
	config ZoneConfig

	mu         sync.Mutex
	registry   *registry
	wheel      timerWheel
	systems    []System
	sessions   map[uint64]SnapshotSink
	serverTick uint64
}

// NewZone constructs an empty zone.
func NewZone(config ZoneConfig) (*Zone, error) {
	if config.ID == "" {
		return nil, fmt.Errorf("zone id is required")
	}
	if config.TickInterval <= 0 {
		return nil, fmt.Errorf("zone tick interval must be positive")
	}
	if config.SnapshotInterval <= 0 {
		return nil, fmt.Errorf("zone snapshot interval must be positive")
	}
	if config.MaxMoveSpeed <= 0 || !finite(config.MaxMoveSpeed) {
		return nil, fmt.Errorf("zone maximum move speed must be positive and finite")
	}
	return &Zone{
		config:   config,
		registry: newRegistry(),
		sessions: make(map[uint64]SnapshotSink),
	}, nil
}

// ID is the zone's configured identifier.
func (zone *Zone) ID() string { return zone.config.ID }

// TickInterval is the fixed simulation step.
func (zone *Zone) TickInterval() time.Duration { return zone.config.TickInterval }

// AddSystem registers per-tick work. Systems are added at composition time,
// before Run, and never removed.
func (zone *Zone) AddSystem(system System) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	zone.systems = append(zone.systems, system)
}

// Command runs one validated mutation against the zone under its lock, at the
// current tick, and returns whatever the mutation decided.
//
// It is the entry point every non-movement client verb goes through, and it is
// the only way a module outside this package gets a *Entity. The callback must
// not block, start a goroutine that touches the zone, or retain anything it is
// handed.
func (zone *Zone) Command(mutate func(*Tick) error) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return mutate(&Tick{zone: zone, number: zone.serverTick})
}

// SpawnNPC adds one non-player entity described entirely by content.
func (zone *Zone) SpawnNPC(spec NPCSpec) uint64 {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.spawnNPCLocked(spec).ID
}

func (zone *Zone) spawnNPCLocked(spec NPCSpec) *Entity {
	return zone.registry.add(&Entity{
		Kind:          EntityKindNPC,
		ContentID:     spec.ContentID,
		NameKey:       spec.NameKey,
		PlacementID:   spec.PlacementID,
		Faction:       spec.Faction,
		Level:         spec.Level,
		Health:        spec.MaxHealth,
		MaxHealth:     spec.MaxHealth,
		Alive:         true,
		Heading:       spec.Heading,
		Animation:     AnimationStateIdle,
		Origin:        spec.Position,
		OriginHeading: spec.Heading,
		Replicated:    true,
		position:      spec.Position,
	})
}

// Join adds a player and returns its entity id and authoritative spawn.
//
// The entity is not replicated yet: its combat identity is filled in by the
// combat module and it becomes visible at Subscribe. Publishing a player with
// no level and no health, however briefly, would be a lie the client has to
// correct.
func (zone *Zone) Join() (uint64, Vec3) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	entity := zone.registry.add(&Entity{
		Kind:       EntityKindPlayer,
		Alive:      true,
		Animation:  AnimationStateIdle,
		Origin:     zone.config.PlayerSpawn,
		Replicated: false,
		position:   zone.config.PlayerSpawn,
	})
	return entity.ID, zone.config.PlayerSpawn
}

// Subscribe starts snapshot delivery for an admitted player.
func (zone *Zone) Subscribe(entityID uint64, sink SnapshotSink) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	current := zone.registry.get(entityID)
	if current == nil || current.Kind != EntityKindPlayer {
		return fmt.Errorf("subscribe entity %d: %w", entityID, ErrUnknownEntity)
	}
	current.Replicated = true
	zone.sessions[entityID] = sink
	return nil
}

// Leave removes a player and its replication subscription.
func (zone *Zone) Leave(entityID uint64) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	delete(zone.sessions, entityID)
	if current := zone.registry.get(entityID); current != nil && current.Kind == EntityKindPlayer {
		zone.registry.remove(entityID)
	}
}

// Run advances this zone until the context ends.
func (zone *Zone) Run(ctx context.Context) {
	tick := time.NewTicker(zone.config.TickInterval)
	snapshot := time.NewTicker(zone.config.SnapshotInterval)
	defer tick.Stop()
	defer snapshot.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			zone.Step()
		case <-snapshot.C:
			zone.PublishSnapshot()
		}
	}
}

// Step advances the simulation by exactly one tick.
//
// It is exported so a test can drive the zone without a wall clock. Everything
// in M2 that depends on elapsed time is counted in ticks, so a test that calls
// Step in a loop gets the same answer every run, which is what makes the
// worked example in mechanics/combat.md section 6.1 assertable.
func (zone *Zone) Step() {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	zone.serverTick++
	current := &Tick{zone: zone, number: zone.serverTick}

	zone.integrateLocked()
	// Deferred work runs before the systems, so a mob that respawns on this
	// tick is scanned for aggro on this tick rather than the next one.
	for _, work := range zone.wheel.advance(zone.serverTick) {
		work.run(current)
	}
	for _, system := range zone.systems {
		system.Step(current)
	}
}

// PublishSnapshot sends the newest view to every subscriber.
func (zone *Zone) PublishSnapshot() {
	zone.mu.Lock()
	snapshot := Snapshot{ServerTick: zone.serverTick, Entities: zone.viewsLocked()}
	sinks := make([]SnapshotSink, 0, len(zone.sessions))
	for _, sink := range zone.sessions {
		sinks = append(sinks, sink)
	}
	zone.mu.Unlock()

	for _, sink := range sinks {
		sink.OfferSnapshot(snapshot)
	}
}

func (zone *Zone) viewsLocked() []EntitySnapshot {
	// TODO(SAR-19): narrow all-entities interest to a spatial query per
	// subscriber. The query exists now (registry.within); what is missing is
	// the delta protocol that makes a shrinking interest set expressible, and
	// sending a subscriber a smaller set without it would look like despawns.
	views := make([]EntitySnapshot, 0, len(zone.registry.ordered))
	zone.registry.each(func(entity *Entity) bool {
		if entity.Replicated {
			views = append(views, viewOf(entity))
		}
		return true
	})
	return views
}
