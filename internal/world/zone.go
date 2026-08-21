// Package world owns authoritative zone simulation and replication state.
//
// It exposes domain types only. Nothing here imports the generated protobuf
// package, and nothing may: the wire schema is the session layer's problem, and
// a second front-end has to be addable without touching the simulation
// (ADR 0028, ADR 0010).
package world

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/interest"
)

// ErrUnknownEntity reports a command naming an entity this zone does not have.
var ErrUnknownEntity = gametypes.ErrUnknownEntity

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
	sessions   map[uint64]*snapshotSubscription
	serverTick uint64
}

// ReplicationInterestRadiusMetres is the distance around a subscribed player
// whose replicated entities belong in that player's snapshot. It extends one
// grid cell beyond the combat skeleton's 40 m leash, so an NPC is visible
// before it can enter the farthest server-driven engagement range.
const ReplicationInterestRadiusMetres = interest.ReplicationRadiusMetres

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
	manager, err := interest.New(interest.ReplicationRadiusMetres)
	if err != nil {
		return nil, err
	}
	return &Zone{
		config:   config,
		registry: newRegistry(manager),
		sessions: make(map[uint64]*snapshotSubscription),
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
		EntityData: gametypes.EntityData{
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
		},
		position: spec.Position,
	})
}

// Join adds a player at the zone's configured spawn.
//
// It survives for debug tools and for entities no character owns. A session
// admitting a real player calls [Zone.JoinAt] with the position checkpoint L1
// loaded, because the configured spawn is right for a fresh character and wrong
// for a returning one (protocol/session.md rule 5.4.4.3).
func (zone *Zone) Join() (uint64, Vec3) {
	return zone.JoinAt(zone.config.PlayerSpawn, 0)
}

// JoinAt adds a player at a loaded position and heading, and returns the entity
// id together with the position the zone actually placed it at. That return is
// what the client is told to snap to: it is the server's answer, not a
// confirmation of a request.
//
// The entity is not replicated yet: its combat identity is filled in by the
// combat module and it becomes visible at Subscribe. Publishing a player with
// no level and no health, however briefly, would be a lie the client has to
// correct.
func (zone *Zone) JoinAt(position Vec3, heading float32) (uint64, Vec3) {
	if !position.Finite() || !finite(heading) {
		// A stored position that is not a number would put an entity nowhere
		// the simulation can reason about, so the zone falls back rather than
		// admitting it.
		position, heading = zone.config.PlayerSpawn, 0
	}
	zone.mu.Lock()
	defer zone.mu.Unlock()
	entity := zone.registry.add(&Entity{
		EntityData: gametypes.EntityData{
			Kind:          EntityKindPlayer,
			Alive:         true,
			Heading:       heading,
			Animation:     AnimationStateIdle,
			Origin:        position,
			OriginHeading: heading,
			Replicated:    false,
		},
		position: position,
	})
	return entity.ID, position
}

// CharacterSnapshot is one player entity copied out of the zone. It is what a
// save checkpoint writes about the simulation, and nothing more: inventory and
// quests belong to their own modules.
type CharacterSnapshot struct {
	EntityID  uint64
	Position  Vec3
	Heading   float32
	Level     uint32
	Health    int32
	MaxHealth int32
	Alive     bool
}

// SnapshotCharacter copies one player entity under a single acquisition of the
// zone mutex.
//
// It is a zone method rather than session code reaching into zone state for one
// reason: the tick loop mutates position under this mutex 30 times a second, so
// a field-by-field read from outside can persist a torn mix of two ticks — a
// position the player was never at (protocol/session.md rule 5.7.5.2).
//
// It reports false for an unknown entity, which is the ordinary answer for a
// session whose entity has already been evicted.
func (zone *Zone) SnapshotCharacter(entityID uint64) (CharacterSnapshot, bool) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	current := zone.registry.get(entityID)
	if current == nil || current.Kind != EntityKindPlayer {
		return CharacterSnapshot{}, false
	}
	return CharacterSnapshot{
		EntityID:  current.ID,
		Position:  current.position,
		Heading:   current.Heading,
		Level:     current.Level,
		Health:    current.Health,
		MaxHealth: current.MaxHealth,
		Alive:     current.Alive,
	}, true
}

// EntityCount reports how many entities the zone holds.
//
// It is a diagnostic, not a simulation input: an operator asking how full a
// zone is, and the tests that assert a refused admission created nothing and
// that a replaced session left exactly one entity behind.
func (zone *Zone) EntityCount() int {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.registry.count()
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
	zone.registry.interest.Forget(entityID)
	zone.sessions[entityID] = &snapshotSubscription{sink: sink}
	return nil
}

// Leave removes a player and its replication subscription.
func (zone *Zone) Leave(entityID uint64) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	delete(zone.sessions, entityID)
	zone.registry.interest.Forget(entityID)
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
	deliveries := make([]snapshotDelivery, 0, len(zone.sessions))
	for entityID, subscription := range zone.sessions {
		subscriber := zone.registry.get(entityID)
		if subscriber == nil {
			continue
		}
		delta := zone.registry.interest.Observe(entityID, subscriber.position, func(id uint64) bool {
			entity := zone.registry.get(id)
			return entity != nil && entity.Replicated
		})
		views := make([]EntitySnapshot, 0, len(delta.Current))
		for _, id := range delta.Current {
			if entity := zone.registry.get(id); entity != nil {
				views = append(views, viewOf(entity))
			}
		}
		spawns := make([]EntitySnapshot, 0, len(delta.Entered))
		for _, id := range delta.Entered {
			if entity := zone.registry.get(id); entity != nil {
				spawns = append(spawns, viewOf(entity))
			}
		}
		deliveries = append(deliveries, snapshotDelivery{
			sink: subscription.sink,
			snapshot: Snapshot{
				ServerTick: zone.serverTick,
				Entities:   views,
				Spawns:     spawns,
				Despawns:   delta.Left,
			},
		})
	}
	zone.mu.Unlock()

	for _, delivery := range deliveries {
		delivery.sink.OfferSnapshot(delivery.snapshot)
	}
}

type snapshotDelivery struct {
	sink     SnapshotSink
	snapshot Snapshot
}

type snapshotSubscription struct {
	sink SnapshotSink
}
