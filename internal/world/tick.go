package world

import "time"

// Tick is a zone, locked, at one instant of simulation time.
//
// It is the only handle a module outside this package has on entity state, and
// it is valid only for the duration of the call it was passed to. Retaining
// one, or the *Entity values it hands out, means reading and writing the
// registry with no lock held.
type Tick struct {
	zone   *Zone
	number uint64
}

// Number is the server tick this call is running at. Every duration in the
// simulation is expressed in these, never in wall-clock time.
func (tick *Tick) Number() uint64 { return tick.number }

// Interval is how much simulated time one tick covers.
func (tick *Tick) Interval() time.Duration { return tick.zone.config.TickInterval }

// ZoneID names the zone, for logs and events.
func (tick *Tick) ZoneID() string { return tick.zone.config.ID }

// Entity returns one entity by id, or nil.
func (tick *Tick) Entity(id uint64) *Entity { return tick.zone.registry.get(id) }

// Each visits every entity in ascending id order, stopping when visit returns
// false. Ascending order is load-bearing: mechanics/combat.md rule 5.7.3
// resolves an aggro tie to the lower entity id and says so because this order
// was already there.
func (tick *Tick) Each(visit func(*Entity) bool) { tick.zone.registry.each(visit) }

// Within visits every entity of `kind` inside `radius` of `centre`, in
// ascending id order. EntityKindUnspecified means any kind.
//
// It is the spatial query the aggro and targeting passes use instead of
// walking the whole registry. Naming a kind matters: an aggro scan wants the
// players near a mob, and in a zone of 288 mobs and one player, asking for
// everything and filtering afterwards is the quadratic term back again.
func (tick *Tick) Within(centre Vec3, radius float32, kind EntityKind, visit func(*Entity) bool) {
	tick.zone.registry.within(centre, radius, kind, visit)
}

// MoveTo repositions an entity and keeps the spatial index consistent. A
// non-finite destination is ignored rather than corrupting the index.
func (tick *Tick) MoveTo(entity *Entity, to Vec3) {
	if entity == nil || !to.Finite() {
		return
	}
	grounded, err := tick.zone.groundMove(entity.position, to, false)
	if err != nil {
		return
	}
	tick.zone.registry.moveTo(entity, grounded)
}

// SpawnNPC adds one non-player entity from inside a locked callback, so a
// module can place content-described mobs without reaching back through the
// zone and deadlocking on the mutex it is already holding.
func (tick *Tick) SpawnNPC(spec NPCSpec) *Entity {
	return tick.zone.spawnNPCLocked(spec)
}

// Despawn removes one non-player entity from the registry and the spatial
// index, reporting whether it was there.
//
// It is deliberately not the mirror of [Zone.Leave]: a player entity belongs to
// a session and only the session may retire it, so this refuses one. What it
// serves is an entity that exists for as long as some rule says it does and
// then genuinely goes — a loot corpse container, which mechanics/loot.md rule
// 5.1.2 destroys with its drop at the despawn tick.
//
// A mob whose corpse is waiting to respawn is a different case and does not
// come through here: it stays in the registry with Replicated false, because
// the placement it fills has to keep its identity.
func (tick *Tick) Despawn(entityID uint64) bool {
	entity := tick.zone.registry.get(entityID)
	if entity == nil || entity.Kind == EntityKindPlayer {
		return false
	}
	tick.zone.registry.remove(entityID)
	return true
}

// After files `run` to happen `delay` ticks from now. A delay of zero runs on
// the next tick; work is never dropped for being late.
func (tick *Tick) After(delay uint64, run func(*Tick)) {
	tick.zone.wheel.schedule(tick.number+delay, run)
}

// PendingWork counts the deferred work the zone is holding, so a test can
// assert that a respawn was really scheduled instead of inferring it from the
// absence of a mob.
func (tick *Tick) PendingWork() int { return tick.zone.wheel.pending() }
