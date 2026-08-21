package world

import (
	"sort"

	"github.com/SarnautCore/server/internal/interest"
)

// registry owns the entity set and the spatial index over it.
//
// It holds no lock of its own. Every method is called with the zone mutex
// held, which is the only place *Entity values are handed out.
type registry struct {
	entities map[uint64]*Entity
	// ordered is every entity id, ascending. Snapshot order and the aggro
	// tie-break of mechanics/combat.md rule 5.7.3 both depend on it, so it is
	// maintained on insert rather than sorted per publish.
	ordered  []uint64
	interest *interest.Manager
	nextID   uint64
}

func newRegistry(manager *interest.Manager) *registry {
	return &registry{
		entities: make(map[uint64]*Entity),
		interest: manager,
	}
}

func (r *registry) add(entity *Entity) *Entity {
	r.nextID++
	entity.ID = r.nextID
	r.entities[entity.ID] = entity
	r.ordered = append(r.ordered, entity.ID)
	r.interest.Upsert(interest.Entity{ID: entity.ID, Kind: entity.Kind, Position: entity.position})
	return entity
}

func (r *registry) get(id uint64) *Entity { return r.entities[id] }

func (r *registry) count() int { return len(r.entities) }

func (r *registry) remove(id uint64) {
	if _, ok := r.entities[id]; !ok {
		return
	}
	r.interest.Remove(id)
	delete(r.entities, id)
	at := sort.Search(len(r.ordered), func(index int) bool { return r.ordered[index] >= id })
	if at < len(r.ordered) && r.ordered[at] == id {
		r.ordered = append(r.ordered[:at], r.ordered[at+1:]...)
	}
}

func (r *registry) moveTo(entity *Entity, to Vec3) {
	entity.position = to
	r.interest.Upsert(interest.Entity{ID: entity.ID, Kind: entity.Kind, Position: to})
}

// each visits every entity in ascending id order, stopping early when visit
// returns false.
func (r *registry) each(visit func(*Entity) bool) {
	for _, id := range r.ordered {
		entity, ok := r.entities[id]
		if !ok {
			continue
		}
		if !visit(entity) {
			return
		}
	}
}

// within visits every entity of `kind` whose centre is inside `radius` of
// `centre`, in ascending id order. EntityKindUnspecified means any kind.
//
// It replaces the full-registry scan the aggro pass used to need. The cell
// sweep is a superset filter; the exact Euclidean test still decides, so the
// answer does not depend on the cell size.
func (r *registry) within(centre Vec3, radius float32, kind EntityKind, visit func(*Entity) bool) {
	r.interest.Within(centre, radius, kind, func(id uint64) bool {
		entity, ok := r.entities[id]
		if !ok {
			return true
		}
		return visit(entity)
	})
}
