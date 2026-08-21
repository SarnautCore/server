// Package interest owns spatial lookup and per-observer visibility sets.
package interest

import (
	"fmt"
	"math"
	"sort"

	"github.com/SarnautCore/server/internal/gametypes"
)

const gridCellMetres = 16

// ReplicationRadiusMetres is the radius of a subscriber's replicated view.
const ReplicationRadiusMetres float32 = 48

type cell struct {
	x int32
	y int32
}

type bucket struct {
	at   cell
	kind gametypes.EntityKind
}

var indexedKinds = [...]gametypes.EntityKind{
	gametypes.EntityKindUnspecified,
	gametypes.EntityKindPlayer,
	gametypes.EntityKindNPC,
}

// Entity is the spatial part of one world entity.
type Entity struct {
	ID       gametypes.EntityID
	Kind     gametypes.EntityKind
	Position gametypes.Vec3
}

// Delta is one observer's visible membership after a query.
// Every slice is sorted by entity id.
type Delta struct {
	Current []gametypes.EntityID
	Entered []gametypes.EntityID
	Left    []gametypes.EntityID
}

// Manager indexes entity positions and remembers observer membership.
// Callers provide synchronization; Manager holds no lock of its own.
type Manager struct {
	radius    float32
	entities  map[gametypes.EntityID]Entity
	cells     map[bucket][]gametypes.EntityID
	observers map[gametypes.EntityID][]gametypes.EntityID
}

// New constructs an empty interest manager.
func New(radius float32) (*Manager, error) {
	if radius <= 0 || !gametypes.Finite(radius) {
		return nil, fmt.Errorf("interest radius must be positive and finite")
	}
	return &Manager{
		radius:    radius,
		entities:  make(map[gametypes.EntityID]Entity),
		cells:     make(map[bucket][]gametypes.EntityID),
		observers: make(map[gametypes.EntityID][]gametypes.EntityID),
	}, nil
}

// Upsert adds or moves one entity in the spatial index.
func (manager *Manager) Upsert(entity Entity) {
	if manager == nil || entity.ID == 0 || !entity.Position.Finite() {
		return
	}
	if previous, ok := manager.entities[entity.ID]; ok {
		oldBucket := bucketOf(previous)
		newBucket := bucketOf(entity)
		manager.entities[entity.ID] = entity
		if oldBucket == newBucket {
			return
		}
		manager.unindex(entity.ID, oldBucket)
		manager.index(entity.ID, newBucket)
		return
	}
	manager.entities[entity.ID] = entity
	manager.index(entity.ID, bucketOf(entity))
}

// Remove deletes one entity from the index.
func (manager *Manager) Remove(id gametypes.EntityID) {
	if manager == nil {
		return
	}
	entity, ok := manager.entities[id]
	if !ok {
		return
	}
	manager.unindex(id, bucketOf(entity))
	delete(manager.entities, id)
}

// Within visits matching ids in ascending order. EntityKindUnspecified means
// every kind. Returning false stops the visit.
func (manager *Manager) Within(
	centre gametypes.Vec3,
	radius float32,
	kind gametypes.EntityKind,
	visit func(gametypes.EntityID) bool,
) {
	if manager == nil || visit == nil || radius <= 0 || !gametypes.Finite(radius) || !centre.Finite() {
		return
	}
	low := cellOf(centre.Sub(gametypes.Vec3{X: radius, Y: radius}))
	high := cellOf(centre.Add(gametypes.Vec3{X: radius, Y: radius}))
	kinds := indexedKinds[:]
	if kind != gametypes.EntityKindUnspecified {
		kinds = []gametypes.EntityKind{kind}
	}

	var candidates []gametypes.EntityID
	for x := low.x; x <= high.x; x++ {
		for y := low.y; y <= high.y; y++ {
			for _, wanted := range kinds {
				candidates = append(candidates, manager.cells[bucket{at: cell{x: x, y: y}, kind: wanted}]...)
			}
		}
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(left, right int) bool { return candidates[left] < candidates[right] })
	}
	for _, id := range candidates {
		entity, ok := manager.entities[id]
		if !ok || gametypes.Distance(entity.Position, centre) > radius {
			continue
		}
		if !visit(id) {
			return
		}
	}
}

// Observe queries the configured replication radius and records the visible
// set for one observer. The predicate can hide an indexed entity without
// copying mutable world state into this package.
func (manager *Manager) Observe(
	observer gametypes.EntityID,
	centre gametypes.Vec3,
	visible func(gametypes.EntityID) bool,
) Delta {
	if manager == nil {
		return Delta{}
	}
	current := make([]gametypes.EntityID, 0)
	manager.Within(centre, manager.radius, gametypes.EntityKindUnspecified, func(id gametypes.EntityID) bool {
		if visible == nil || visible(id) {
			current = append(current, id)
		}
		return true
	})
	entered, left := difference(manager.observers[observer], current)
	manager.observers[observer] = append(manager.observers[observer][:0], current...)
	return Delta{Current: current, Entered: entered, Left: left}
}

// Forget removes an observer's remembered set. Its next Observe call reports
// every visible entity as entered.
func (manager *Manager) Forget(observer gametypes.EntityID) {
	if manager != nil {
		delete(manager.observers, observer)
	}
}

func difference(previous, current []gametypes.EntityID) (entered, left []gametypes.EntityID) {
	oldIndex, newIndex := 0, 0
	for oldIndex < len(previous) && newIndex < len(current) {
		switch {
		case previous[oldIndex] < current[newIndex]:
			left = append(left, previous[oldIndex])
			oldIndex++
		case previous[oldIndex] > current[newIndex]:
			entered = append(entered, current[newIndex])
			newIndex++
		default:
			oldIndex++
			newIndex++
		}
	}
	left = append(left, previous[oldIndex:]...)
	entered = append(entered, current[newIndex:]...)
	return entered, left
}

func (manager *Manager) index(id gametypes.EntityID, key bucket) {
	manager.cells[key] = append(manager.cells[key], id)
}

func (manager *Manager) unindex(id gametypes.EntityID, key bucket) {
	ids := manager.cells[key]
	for index, current := range ids {
		if current == id {
			ids = append(ids[:index], ids[index+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(manager.cells, key)
		return
	}
	manager.cells[key] = ids
}

func bucketOf(entity Entity) bucket {
	return bucket{at: cellOf(entity.Position), kind: entity.Kind}
}

func cellOf(at gametypes.Vec3) cell {
	return cell{x: axisCell(at.X), y: axisCell(at.Y)}
}

func axisCell(value float32) int32 {
	if math.IsNaN(float64(value)) {
		return 0
	}
	scaled := math.Floor(float64(value) / gridCellMetres)
	if scaled < math.MinInt32 {
		return math.MinInt32
	}
	if scaled > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(scaled)
}
