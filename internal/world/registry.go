package world

import (
	"math"
	"sort"
)

// gridCellMetres is the edge length of one spatial-index cell.
//
// It is a few times the largest radius M2 queries with — a 40 m leash, a 25 m
// ability — so a typical query touches a handful of cells rather than one
// enormous one, and a cell holds few enough entities that the exact-distance
// filter is cheap. It is not tuned against a profile; it is sized so that the
// query cost stops scaling with the zone's entity count, which is the property
// that was missing.
const gridCellMetres = 16

// cell is a spatial-index bucket coordinate. Z is deliberately absent: M2
// zones are effectively flat, mob movement never assigns Z, and a third axis
// would multiply the cell count for no selectivity.
type cell struct {
	x int32
	y int32
}

// bucket is one spatial cell of one entity kind.
//
// Splitting the index by kind is what makes the aggro pass cheap. Every mob
// asks "which players are near me" on every tick, and in a zone of 288
// entities with one player in it, an index that does not distinguish them
// hands back every neighbouring mob for the caller to filter, once per mob,
// per tick.
type bucket struct {
	at   cell
	kind EntityKind
}

// indexedKinds is every kind the spatial index buckets by, for a query that
// does not care which.
var indexedKinds = [...]EntityKind{EntityKindUnspecified, EntityKindPlayer, EntityKindNPC}

// registry owns the entity set and the spatial index over it.
//
// It holds no lock of its own. Every method is called with the zone mutex
// held, which is the only place *Entity values are handed out.
type registry struct {
	entities map[uint64]*Entity
	// ordered is every entity id, ascending. Snapshot order and the aggro
	// tie-break of mechanics/combat.md rule 5.7.3 both depend on it, so it is
	// maintained on insert rather than sorted per publish.
	ordered []uint64
	cells   map[bucket][]uint64
	nextID  uint64
}

func newRegistry() *registry {
	return &registry{
		entities: make(map[uint64]*Entity),
		cells:    make(map[bucket][]uint64),
	}
}

func (r *registry) add(entity *Entity) *Entity {
	r.nextID++
	entity.ID = r.nextID
	r.entities[entity.ID] = entity
	r.ordered = append(r.ordered, entity.ID)
	r.index(entity.ID, bucketOf(entity))
	return entity
}

func (r *registry) get(id uint64) *Entity { return r.entities[id] }

func (r *registry) remove(id uint64) {
	entity, ok := r.entities[id]
	if !ok {
		return
	}
	r.unindex(id, bucketOf(entity))
	delete(r.entities, id)
	at := sort.Search(len(r.ordered), func(index int) bool { return r.ordered[index] >= id })
	if at < len(r.ordered) && r.ordered[at] == id {
		r.ordered = append(r.ordered[:at], r.ordered[at+1:]...)
	}
}

func (r *registry) moveTo(entity *Entity, to Vec3) {
	from, at := bucketOf(entity), bucket{at: cellOf(to), kind: entity.Kind}
	entity.position = to
	if from == at {
		return
	}
	r.unindex(entity.ID, from)
	r.index(entity.ID, at)
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
	if radius <= 0 || !finite(radius) || !centre.Finite() {
		return
	}
	low := cellOf(centre.Sub(Vec3{X: radius, Y: radius}))
	high := cellOf(centre.Add(Vec3{X: radius, Y: radius}))
	kinds := indexedKinds[:]
	if kind != EntityKindUnspecified {
		kinds = []EntityKind{kind}
	}

	var candidates []uint64
	for x := low.x; x <= high.x; x++ {
		for y := low.y; y <= high.y; y++ {
			for _, wanted := range kinds {
				candidates = append(candidates, r.cells[bucket{at: cell{x: x, y: y}, kind: wanted}]...)
			}
		}
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(left, right int) bool { return candidates[left] < candidates[right] })
	}
	for _, id := range candidates {
		entity, ok := r.entities[id]
		if !ok {
			continue
		}
		if Distance(entity.position, centre) > radius {
			continue
		}
		if !visit(entity) {
			return
		}
	}
}

func (r *registry) index(id uint64, key bucket) {
	r.cells[key] = append(r.cells[key], id)
}

func (r *registry) unindex(id uint64, key bucket) {
	bucket := r.cells[key]
	for index, current := range bucket {
		if current != id {
			continue
		}
		bucket = append(bucket[:index], bucket[index+1:]...)
		break
	}
	if len(bucket) == 0 {
		delete(r.cells, key)
		return
	}
	r.cells[key] = bucket
}

func bucketOf(entity *Entity) bucket {
	return bucket{at: cellOf(entity.position), kind: entity.Kind}
}

func cellOf(at Vec3) cell {
	return cell{x: axisCell(at.X), y: axisCell(at.Y)}
}

// axisCell maps one coordinate onto a cell index. A non-finite coordinate
// cannot reach here through any validated input path, but it is clamped rather
// than converted, because int32(NaN) is undefined and would corrupt the index.
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
