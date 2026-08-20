package combat

import (
	"math"
	"time"

	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

// phase is the mob AI state of mechanics/combat.md section 4.
type phase uint8

const (
	phaseIdle phase = iota
	phaseAggro
	phaseReturning
	phaseDead
	// phaseDespawned is a corpse that has gone and whose respawn is filed on
	// the tick wheel. The entity stays in the registry so the placement keeps
	// its identity; it is simply not replicated.
	phaseDespawned
)

// mobState is everything about one mob that is not world state.
//
// Every field that is a number came out of the content pack. The struct exists
// so that nothing in this package has to ask "what is the aggro radius" and
// get a Go constant back.
type mobState struct {
	contentID   string
	placementID string
	anchor      world.Vec3
	anchorYaw   float32

	phase       phase
	aggroTarget uint64
	threat      map[uint64]int64

	levelMin     uint32
	levelMax     uint32
	hpMod        float64
	walkSpeed    float32
	aggroRadius  float32
	leashRadius  float32
	stopDistance float32
	respawnMin   time.Duration
	respawnMax   time.Duration
}

func (module *Module) newMobState(mob pack.Mob, spawn pack.NPCSpawn) *mobState {
	return &mobState{
		contentID:   mob.ID,
		placementID: spawn.PlacementID,
		anchor:      world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z},
		anchorYaw:   spawn.Heading,
		phase:       phaseIdle,
		threat:      make(map[uint64]int64),
		levelMin:    mob.LevelMin,
		levelMax:    mob.LevelMax,
		hpMod:       mob.HPMod,
		walkSpeed:   mob.WalkSpeed,
		aggroRadius: mob.AggroRadiusM,
		leashRadius: mob.LeashRadiusM,
		// Rule 5.8.3 says a chasing mob stops at its ability range so it does
		// not stand on top of its target. Which range is per-mob data: the
		// shortest of the abilities the pack gave it. A mob with no abilities
		// walks all the way in, which is visible and wrong-looking, and is the
		// content's statement rather than this package's.
		stopDistance: module.shortestRange(mob),
		respawnMin:   spawn.RespawnMin,
		respawnMax:   spawn.RespawnMax,
	}
}

func (state *mobState) addThreat(entityID uint64, amount int64) {
	if state.threat == nil {
		state.threat = make(map[uint64]int64)
	}
	state.threat[entityID] += amount
}

// Step runs the per-tick mob rules: aggro, chase and leash.
//
// Mobs are visited in ascending entity id, which is what makes an aggro tie
// resolve to the lower id (rule 5.7.3) without a tie-break of its own.
func (module *Module) Step(tick *world.Tick) {
	if len(module.mobs) == 0 {
		return
	}
	tick.Each(func(entity *world.Entity) bool {
		state, ok := module.mobs[entity.ID]
		if !ok {
			return true
		}
		switch state.phase {
		case phaseIdle:
			module.scanForAggro(tick, entity, state)
		case phaseAggro:
			module.chase(tick, entity, state)
		case phaseReturning:
			module.walkHome(tick, entity, state)
		case phaseDead, phaseDespawned:
			// The tick wheel owns these. Nothing to poll.
		}
		return true
	})
}

// scanForAggro is rule 5.7. The neighbour query is what makes it cost the
// number of players near this mob rather than the number of entities in the
// zone.
func (module *Module) scanForAggro(tick *world.Tick, mob *world.Entity, state *mobState) {
	if state.aggroRadius <= 0 {
		return
	}
	tick.Within(mob.Position(), state.aggroRadius, world.EntityKindPlayer, func(candidate *world.Entity) bool {
		if !candidate.Alive || !candidate.Replicated {
			return true
		}
		if !module.hostile(candidate.Faction, mob.Faction) {
			return true
		}
		state.phase = phaseAggro
		state.aggroTarget = candidate.ID
		return false
	})
}

// chase is rule 5.8, points 1 to 4.
func (module *Module) chase(tick *world.Tick, mob *world.Entity, state *mobState) {
	target := tick.Entity(state.aggroTarget)
	if target == nil || !target.Alive || !target.Replicated {
		// Rule 5.8.1: the player left the zone.
		module.breakLeash(mob, state)
		return
	}
	// Rule 5.8.2, evaluated before the step, so a mob that is already outside
	// its leash does not get one more stride first.
	if state.leashRadius > 0 && world.Distance(mob.Position(), state.anchor) > state.leashRadius {
		module.breakLeash(mob, state)
		return
	}
	module.walkToward(tick, mob, state, target.Position(), state.stopDistance)
}

// breakLeash is rule 5.8.5.
func (module *Module) breakLeash(mob *world.Entity, state *mobState) {
	state.phase = phaseReturning
	state.aggroTarget = 0
	state.threat = make(map[uint64]int64)
	mob.Velocity = world.Vec3{}
	mob.Animation = world.AnimationStateMoving
}

// walkHome is rules 5.8.6 and 5.8.7.
func (module *Module) walkHome(tick *world.Tick, mob *world.Entity, state *mobState) {
	if world.Distance(mob.Position(), state.anchor) <= rangeTolerance {
		tick.MoveTo(mob, state.anchor)
		mob.Heading = state.anchorYaw
		mob.Velocity = world.Vec3{}
		mob.Animation = world.AnimationStateIdle
		mob.Health = mob.MaxHealth
		state.phase = phaseIdle
		return
	}
	module.walkToward(tick, mob, state, state.anchor, 0)
}

// walkToward steps a mob toward a point at its own walk speed, stopping
// `stopAt` metres short. Movement is on the ground plane: assigning Z waits on
// the same terrain query player movement waits on.
func (module *Module) walkToward(
	tick *world.Tick,
	mob *world.Entity,
	state *mobState,
	destination world.Vec3,
	stopAt float32,
) {
	if state.walkSpeed <= 0 {
		return
	}
	from := mob.Position()
	offset := world.Vec3{X: destination.X - from.X, Y: destination.Y - from.Y}
	distance := offset.Length()
	if distance <= stopAt || distance == 0 {
		mob.Velocity = world.Vec3{}
		mob.Animation = world.AnimationStateIdle
		return
	}

	speed := state.walkSpeed * chaseSpeedMultiplier
	step := speed * float32(tick.Interval().Seconds())
	if remaining := distance - stopAt; step > remaining {
		step = remaining
	}
	direction := offset.Scale(1 / distance)
	tick.MoveTo(mob, world.Vec3{
		X: from.X + direction.X*step,
		Y: from.Y + direction.Y*step,
		Z: from.Z,
	})
	mob.Heading = headingOf(direction)
	mob.Velocity = direction.Scale(speed)
	mob.Animation = world.AnimationStateMoving
}

// kill is rule 5.9: death, the corpse, and the respawn behind it.
func (module *Module) kill(tick *world.Tick, victim *world.Entity, killerID uint64) {
	victim.Alive = false
	victim.Velocity = world.Vec3{}
	victim.Animation = world.AnimationStateIdle

	state, ok := module.mobs[victim.ID]
	if !ok {
		// A player, once players can die. Nothing schedules a respawn for one
		// yet, and mechanics/combat.md section 1 puts player death out of M2.
		return
	}
	state.phase = phaseDead
	state.aggroTarget = 0

	despawnAt := tick.Number() + ticksIn(corpseTimer, tick.Interval())
	module.publish(Event{
		Kind:              EventKindDeath,
		ServerTick:        tick.Number(),
		ZoneID:            tick.ZoneID(),
		CasterID:          killerID,
		TargetID:          victim.ID,
		VictimContentID:   state.contentID,
		VictimLevel:       victim.Level,
		CorpseDespawnTick: despawnAt,
	})

	// The kill sink is told inside this tick, before the despawn is filed, so
	// that mechanics/loot.md rule 5.1.1 holds: the drop is rolled at corpse
	// creation and is fixed before anything can observe it. Rolling it lazily
	// when a player opens the corpse would make a disconnect mid-loot able to
	// change what is there.
	if module.killSink != nil {
		mob, _ := module.rules.Mob(state.contentID)
		module.killSink.MobKilled(tick, Kill{
			VictimEntityID:  victim.ID,
			KillerEntityID:  killerID,
			VictimContentID: state.contentID,
			PlacementID:     state.placementID,
			LootTableID:     mob.LootTableID,
			VictimLevel:     victim.Level,
			Position:        victim.Position(),
			Heading:         victim.Heading,
			DeathTick:       tick.Number(),
			DespawnTick:     despawnAt,
		})
	}

	victimID := victim.ID
	tick.After(despawnAt-tick.Number(), func(later *world.Tick) {
		module.despawnCorpse(later, victimID)
	})
}

// despawnCorpse is rule 5.9.5, and files the respawn of rule 5.9.6.
func (module *Module) despawnCorpse(tick *world.Tick, victimID uint64) {
	entity := tick.Entity(victimID)
	state, ok := module.mobs[victimID]
	if entity == nil || !ok || state.phase != phaseDead {
		return
	}
	entity.Replicated = false
	state.phase = phaseDespawned

	delay := module.stream.respawnDelay(state.respawnMin, state.respawnMax)
	tick.After(ticksIn(delay, tick.Interval()), func(later *world.Tick) {
		module.respawn(later, victimID)
	})
}

// respawn is rule 5.9.7: back at the anchor, at full health, with a freshly
// drawn level.
func (module *Module) respawn(tick *world.Tick, victimID uint64) {
	entity := tick.Entity(victimID)
	state, ok := module.mobs[victimID]
	if entity == nil || !ok || state.phase != phaseDespawned {
		return
	}
	level := module.stream.level(state.levelMin, state.levelMax)
	entity.Level = level
	entity.MaxHealth = MaxHealth(level, state.hpMod)
	entity.Health = entity.MaxHealth
	entity.Alive = true
	entity.Replicated = true
	entity.Heading = state.anchorYaw
	entity.Velocity = world.Vec3{}
	entity.Animation = world.AnimationStateIdle
	tick.MoveTo(entity, state.anchor)

	state.phase = phaseIdle
	state.aggroTarget = 0
	state.threat = make(map[uint64]int64)
}

// shortestRange is how close a mob has to get to use any ability it carries.
// Zero means it carries none and walks all the way in.
func (module *Module) shortestRange(mob pack.Mob) float32 {
	var shortest float32
	for _, id := range mob.AbilityIDs {
		ability, ok := module.rules.Ability(id)
		if !ok || ability.RangeM <= 0 {
			continue
		}
		if shortest == 0 || ability.RangeM < shortest {
			shortest = ability.RangeM
		}
	}
	return shortest
}

// headingOf is the yaw a unit direction faces, in radians.
func headingOf(direction world.Vec3) float32 {
	return float32(math.Atan2(float64(direction.Y), float64(direction.X)))
}
