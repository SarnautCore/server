// Package world owns authoritative zone simulation and replication state.
package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
)

const maxIntentDuration = 250 * time.Millisecond

var ErrUnknownEntity = errors.New("unknown world entity")

// Vec3 is a world-space vector. Z is the vertical axis.
type Vec3 struct {
	X float32
	Y float32
	Z float32
}

// ZoneConfig sets fixed simulation and replication rates.
type ZoneConfig struct {
	ID               string
	TickInterval     time.Duration
	SnapshotInterval time.Duration
	MaxMoveSpeed     float32
	PlayerSpawn      Vec3
}

// SnapshotSink accepts the newest immutable view of a zone.
type SnapshotSink interface {
	OfferSnapshot(*sarnautv1.SnapshotBatch)
}

type entity struct {
	id              uint64
	kind            sarnautv1.EntityKind
	position        Vec3
	heading         float32
	velocity        Vec3
	animation       sarnautv1.AnimationState
	hasIntent       bool
	lastIntentSeq   uint64
	intentRemaining time.Duration
}

// Zone owns one entity registry and its fixed-rate simulation.
type Zone struct {
	config ZoneConfig

	mu         sync.Mutex
	entities   map[uint64]*entity
	sessions   map[uint64]SnapshotSink
	nextID     uint64
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
		entities: make(map[uint64]*entity),
		sessions: make(map[uint64]SnapshotSink),
	}, nil
}

func (zone *Zone) ID() string {
	return zone.config.ID
}

// SpawnNPC adds a stationary NPC to the registry.
func (zone *Zone) SpawnNPC(position Vec3, heading float32) uint64 {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.addEntityLocked(sarnautv1.EntityKind_ENTITY_KIND_NPC, position, heading)
}

// Join adds a player. Subscribe must follow the reliable EnterZoneResponse.
func (zone *Zone) Join() (uint64, Vec3) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	id := zone.addEntityLocked(
		sarnautv1.EntityKind_ENTITY_KIND_PLAYER,
		zone.config.PlayerSpawn,
		0,
	)
	return id, zone.config.PlayerSpawn
}

// Subscribe starts snapshot delivery for an admitted player.
func (zone *Zone) Subscribe(entityID uint64, sink SnapshotSink) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	current, ok := zone.entities[entityID]
	if !ok || current.kind != sarnautv1.EntityKind_ENTITY_KIND_PLAYER {
		return fmt.Errorf("subscribe entity %d: %w", entityID, ErrUnknownEntity)
	}
	zone.sessions[entityID] = sink
	return nil
}

// Leave removes a player and its replication subscription.
func (zone *Zone) Leave(entityID uint64) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	delete(zone.sessions, entityID)
	if current := zone.entities[entityID]; current != nil && current.kind == sarnautv1.EntityKind_ENTITY_KIND_PLAYER {
		delete(zone.entities, entityID)
	}
}

// ApplyMoveIntent records the newest valid input for fixed-tick integration.
func (zone *Zone) ApplyMoveIntent(entityID uint64, intent *sarnautv1.ClientMoveIntent) error {
	if intent == nil || intent.GetInput() == nil {
		return fmt.Errorf("move intent has no input")
	}
	input := intent.GetInput()
	if !finite(input.GetX()) || !finite(input.GetY()) || !finite(intent.GetHeading()) || !finite(intent.GetDtSeconds()) {
		return fmt.Errorf("move intent contains a non-finite value")
	}

	zone.mu.Lock()
	defer zone.mu.Unlock()
	current, ok := zone.entities[entityID]
	if !ok || current.kind != sarnautv1.EntityKind_ENTITY_KIND_PLAYER {
		return fmt.Errorf("move entity %d: %w", entityID, ErrUnknownEntity)
	}
	if current.hasIntent && intent.GetSeq() <= current.lastIntentSeq {
		return nil
	}
	current.hasIntent = true
	current.lastIntentSeq = intent.GetSeq()
	current.heading = intent.GetHeading()

	x, y := normalized(input.GetX(), input.GetY())
	current.velocity = Vec3{X: x * zone.config.MaxMoveSpeed, Y: y * zone.config.MaxMoveSpeed}
	duration := time.Duration(float64(intent.GetDtSeconds()) * float64(time.Second))
	if duration <= 0 {
		duration = zone.config.TickInterval
	}
	if duration > maxIntentDuration {
		duration = maxIntentDuration
	}
	current.intentRemaining = duration
	if x == 0 && y == 0 {
		current.animation = sarnautv1.AnimationState_ANIMATION_STATE_IDLE
	} else {
		current.animation = sarnautv1.AnimationState_ANIMATION_STATE_MOVING
	}
	return nil
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
			zone.step()
		case <-snapshot.C:
			zone.publishSnapshot()
		}
	}
}

func (zone *Zone) addEntityLocked(kind sarnautv1.EntityKind, position Vec3, heading float32) uint64 {
	zone.nextID++
	zone.entities[zone.nextID] = &entity{
		id:        zone.nextID,
		kind:      kind,
		position:  position,
		heading:   heading,
		animation: sarnautv1.AnimationState_ANIMATION_STATE_IDLE,
	}
	return zone.nextID
}

func (zone *Zone) step() {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	zone.serverTick++
	for _, current := range zone.entities {
		if current.kind != sarnautv1.EntityKind_ENTITY_KIND_PLAYER || current.intentRemaining <= 0 {
			continue
		}
		delta := min(zone.config.TickInterval, current.intentRemaining)
		seconds := float32(delta.Seconds())
		current.position.X += current.velocity.X * seconds
		current.position.Y += current.velocity.Y * seconds
		// TODO(SAR-19): resolve terrain height and collisions before assigning Z movement.
		current.intentRemaining -= delta
		if current.intentRemaining <= 0 {
			current.velocity = Vec3{}
			current.animation = sarnautv1.AnimationState_ANIMATION_STATE_IDLE
		}
	}
}

func (zone *Zone) publishSnapshot() {
	zone.mu.Lock()
	batch := &sarnautv1.SnapshotBatch{
		ServerTick: zone.serverTick,
		Entities:   zone.interestedEntitiesLocked(),
	}
	sinks := make([]SnapshotSink, 0, len(zone.sessions))
	for _, sink := range zone.sessions {
		sinks = append(sinks, sink)
	}
	zone.mu.Unlock()

	for _, sink := range sinks {
		sink.OfferSnapshot(batch)
	}
}

func (zone *Zone) interestedEntitiesLocked() []*sarnautv1.EntitySnapshot {
	// TODO(SAR-19): replace all-entities interest with a spatial query.
	ids := make([]uint64, 0, len(zone.entities))
	for id := range zone.entities {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	result := make([]*sarnautv1.EntitySnapshot, 0, len(ids))
	for _, id := range ids {
		current := zone.entities[id]
		result = append(result, &sarnautv1.EntitySnapshot{
			EntityId:       current.id,
			Kind:           current.kind,
			Position:       protobufVec(current.position),
			Heading:        current.heading,
			Velocity:       protobufVec(current.velocity),
			AnimationState: current.animation,
		})
	}
	return result
}

func normalized(x, y float32) (float32, float32) {
	magnitude := float32(math.Hypot(float64(x), float64(y)))
	if magnitude <= 1 {
		return x, y
	}
	return x / magnitude, y / magnitude
}

func finite(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}

func protobufVec(value Vec3) *sarnautv1.Vec3 {
	return &sarnautv1.Vec3{X: value.X, Y: value.Y, Z: value.Z}
}

// Module runs the shard's configured zones.
type Module struct {
	zones  []*Zone
	logger *slog.Logger
}

func New(logger *slog.Logger, zones ...*Zone) *Module {
	return &Module{zones: zones, logger: logger}
}

// Run starts every zone and waits for shutdown.
func (module *Module) Run(ctx context.Context) {
	var group sync.WaitGroup
	for _, zone := range module.zones {
		group.Add(1)
		go func(current *Zone) {
			defer group.Done()
			module.logger.DebugContext(ctx, "zone tick loop started", "zone_id", current.ID())
			current.Run(ctx)
		}(zone)
	}
	group.Wait()
}
