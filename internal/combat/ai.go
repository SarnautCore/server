package combat

import (
	"fmt"
	"math"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
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
	anchor      gametypes.Vec3
	anchorYaw   float32

	phase       phase
	aggroTarget uint64
	threat      map[uint64]int64
	// Guard is a transient effect rather than authored mob content. The mark
	// mirrors retail's first-attach/last-detach lifetime and the observer owns
	// its refresh cadence.
	guardAggroMarks int
	guard           *guardObserver

	levelMin     uint32
	levelMax     uint32
	hpMod        float64
	walkSpeed    float32
	aggroRadius  float32
	leashRadius  float32
	stopDistance float32
	respawnMin   time.Duration
	respawnMax   time.Duration
	summoned     bool
}

type playerPhase uint8

const (
	playerAlive playerPhase = iota
	playerDead
)

type playerState struct {
	anchor                    gametypes.Vec3
	anchorYaw                 float32
	phase                     playerPhase
	resurrectionSicknessUntil uint64
}

func (module *Module) newMobState(mob gametypes.Mob, spawn gametypes.NPCSpawn) *mobState {
	return &mobState{
		contentID:   mob.ID,
		placementID: spawn.PlacementID,
		anchor:      gametypes.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z},
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
func (module *Module) Step(tick gametypes.Tick) {
	if len(module.mobs) == 0 {
		return
	}
	tick.Each(func(entity *gametypes.EntityData) bool {
		state, ok := module.mobs[entity.ID]
		if !ok {
			return true
		}
		switch state.phase {
		case phaseIdle:
			if state.guard == nil {
				module.scanForAggro(tick, entity, state, state.aggroRadius)
			} else if tick.Number() >= state.guard.nextRecheckTick {
				module.scanForAggro(tick, entity, state, state.guard.radius)
				state.guard.nextRecheckTick = tick.Number() + state.guard.recheckTicks
			}
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
func (module *Module) scanForAggro(
	tick gametypes.Tick,
	mob *gametypes.EntityData,
	state *mobState,
	radius float32,
) {
	if radius <= 0 {
		return
	}
	tick.Within(tick.Position(mob), radius, gametypes.EntityKindPlayer, func(candidate *gametypes.EntityData) bool {
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
func (module *Module) chase(tick gametypes.Tick, mob *gametypes.EntityData, state *mobState) {
	target := tick.Entity(state.aggroTarget)
	if target == nil || !target.Alive || !target.Replicated {
		// Rule 5.8.1: the player left the zone.
		module.breakLeash(mob, state)
		return
	}
	// Rule 5.8.2, evaluated before the step, so a mob that is already outside
	// its leash does not get one more stride first.
	if state.leashRadius > 0 && gametypes.Distance(tick.Position(mob), state.anchor) > state.leashRadius {
		module.breakLeash(mob, state)
		return
	}
	module.walkToward(tick, mob, state, tick.Position(target), state.stopDistance)
}

// breakLeash is rule 5.8.5.
func (module *Module) breakLeash(mob *gametypes.EntityData, state *mobState) {
	state.phase = phaseReturning
	state.aggroTarget = 0
	state.threat = make(map[uint64]int64)
	mob.Velocity = gametypes.Vec3{}
	mob.Animation = gametypes.AnimationStateMoving
}

// walkHome is rules 5.8.6 and 5.8.7.
func (module *Module) walkHome(tick gametypes.Tick, mob *gametypes.EntityData, state *mobState) {
	if gametypes.Distance(tick.Position(mob), state.anchor) <= rangeTolerance {
		tick.MoveTo(mob, state.anchor)
		mob.Heading = state.anchorYaw
		mob.Velocity = gametypes.Vec3{}
		mob.Animation = gametypes.AnimationStateIdle
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
	tick gametypes.Tick,
	mob *gametypes.EntityData,
	state *mobState,
	destination gametypes.Vec3,
	stopAt float32,
) {
	if state.walkSpeed <= 0 {
		return
	}
	from := tick.Position(mob)
	offset := gametypes.Vec3{X: destination.X - from.X, Y: destination.Y - from.Y}
	distance := offset.Length()
	if distance <= stopAt || distance == 0 {
		mob.Velocity = gametypes.Vec3{}
		mob.Animation = gametypes.AnimationStateIdle
		return
	}

	speed := state.walkSpeed * chaseSpeedMultiplier
	step := speed * float32(tick.Interval().Seconds())
	if remaining := distance - stopAt; step > remaining {
		step = remaining
	}
	direction := offset.Scale(1 / distance)
	tick.MoveTo(mob, gametypes.Vec3{
		X: from.X + direction.X*step,
		Y: from.Y + direction.Y*step,
		Z: from.Z,
	})
	mob.Heading = headingOf(direction)
	mob.Velocity = direction.Scale(speed)
	mob.Animation = gametypes.AnimationStateMoving
}

// kill is rule 5.9: death, the corpse, and the respawn behind it.
func (module *Module) kill(tick gametypes.Tick, victim *gametypes.EntityData, killerID uint64) {
	victim.Alive = false
	module.clearSelectedTarget(victim.ID)
	victim.Velocity = gametypes.Vec3{}
	victim.Animation = gametypes.AnimationStateIdle

	state, ok := module.mobs[victim.ID]
	if !ok {
		if player, playerOK := module.players[victim.ID]; playerOK {
			module.killPlayer(tick, victim, player, killerID)
		}
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
			Position:        tick.Position(victim),
			Heading:         victim.Heading,
			DeathTick:       tick.Number(),
			DespawnTick:     despawnAt,
		})
	}

	victimID := victim.ID
	tick.After(despawnAt-tick.Number(), func(later gametypes.Tick) {
		module.despawnCorpse(later, victimID)
	})
}

func (module *Module) killPlayer(
	tick gametypes.Tick,
	victim *gametypes.EntityData,
	state *playerState,
	killerID uint64,
) {
	if state.phase == playerDead {
		return
	}
	state.phase = playerDead
	state.resurrectionSicknessUntil = 0
	victim.ResurrectionSicknessUntilTick = 0
	if module.lifecycle == nil || !module.lifecycle.valid() {
		module.logger.Error("player death has no authored lifecycle",
			"zone_id", tick.ZoneID(), "entity_id", victim.ID)
		return
	}
	module.schedulePlayerRespawn(tick, victim)
	respawnTick := tick.Number() + ticksIn(module.lifecycle.RespawnDelay, tick.Interval())
	module.publish(Event{
		Kind:        EventKindPlayerDeath,
		ServerTick:  tick.Number(),
		ZoneID:      tick.ZoneID(),
		CasterID:    killerID,
		TargetID:    victim.ID,
		RespawnTick: respawnTick,
	})
}

func (module *Module) schedulePlayerRespawn(tick gametypes.Tick, victim *gametypes.EntityData) {
	delay := ticksIn(module.lifecycle.RespawnDelay, tick.Interval())
	victimID := victim.ID
	tick.After(delay, func(later gametypes.Tick) {
		module.respawnPlayer(later, victimID)
	})
}

func (module *Module) respawnPlayer(tick gametypes.Tick, victimID uint64) {
	entity := tick.Entity(victimID)
	state, ok := module.players[victimID]
	if entity == nil || !ok || state.phase != playerDead {
		return
	}
	entity.Health = entity.MaxHealth
	entity.Alive = true
	entity.Heading = state.anchorYaw
	entity.Velocity = gametypes.Vec3{}
	entity.Animation = gametypes.AnimationStateIdle
	tick.MoveTo(entity, state.anchor)
	state.phase = playerAlive
	state.resurrectionSicknessUntil = tick.Number() +
		ticksIn(module.lifecycle.ResurrectionSickness, tick.Interval())
	entity.ResurrectionSicknessUntilTick = state.resurrectionSicknessUntil
	module.publish(Event{
		Kind:                          EventKindPlayerRespawn,
		ServerTick:                    tick.Number(),
		ZoneID:                        tick.ZoneID(),
		TargetID:                      victimID,
		ResurrectionSicknessUntilTick: state.resurrectionSicknessUntil,
	})
	until := state.resurrectionSicknessUntil
	tick.After(until-tick.Number(), func(later gametypes.Tick) {
		module.expirePlayerSickness(later, victimID, until)
	})
}

func (module *Module) resumePlayerSickness(
	tick gametypes.Tick,
	entity *gametypes.EntityData,
	remaining time.Duration,
) {
	state := module.players[entity.ID]
	state.resurrectionSicknessUntil = tick.Number() + ticksIn(remaining, tick.Interval())
	entity.ResurrectionSicknessUntilTick = state.resurrectionSicknessUntil
	until := state.resurrectionSicknessUntil
	tick.After(until-tick.Number(), func(later gametypes.Tick) {
		module.expirePlayerSickness(later, entity.ID, until)
	})
}

func (module *Module) expirePlayerSickness(tick gametypes.Tick, entityID, until uint64) {
	state, ok := module.players[entityID]
	if !ok || state.resurrectionSicknessUntil != until || tick.Number() < until {
		return
	}
	state.resurrectionSicknessUntil = 0
	if entity := tick.Entity(entityID); entity != nil {
		entity.ResurrectionSicknessUntilTick = 0
	}
	module.publish(Event{
		Kind:       EventKindResurrectionSicknessExpired,
		ServerTick: tick.Number(),
		ZoneID:     tick.ZoneID(),
		TargetID:   entityID,
	})
}

// PlayerResurrectionSickness returns the active authored revive-buff window.
func (module *Module) PlayerResurrectionSickness(entityID uint64) (bool, uint64, error) {
	var active bool
	var until uint64
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		state, ok := module.players[entityID]
		if !ok {
			return fmt.Errorf("player lifecycle entity %d: %w", entityID, gametypes.ErrUnknownEntity)
		}
		until = state.resurrectionSicknessUntil
		active = until > tick.Number()
		return nil
	})
	return active, until, err
}

// despawnCorpse is rule 5.9.5, and files the respawn of rule 5.9.6.
func (module *Module) despawnCorpse(tick gametypes.Tick, victimID uint64) {
	entity := tick.Entity(victimID)
	state, ok := module.mobs[victimID]
	if entity == nil || !ok || state.phase != phaseDead {
		return
	}
	if state.summoned {
		tick.Despawn(victimID)
		delete(module.mobs, victimID)
		module.retireScriptReplays(victimID)
		return
	}
	entity.Replicated = false
	state.phase = phaseDespawned
	module.retireScriptReplays(victimID)

	delay := module.stream.respawnDelay(state.respawnMin, state.respawnMax)
	tick.After(ticksIn(delay, tick.Interval()), func(later gametypes.Tick) {
		module.respawn(later, victimID)
	})
}

// respawn is rule 5.9.7: back at the anchor, at full health, with a freshly
// drawn level.
func (module *Module) respawn(tick gametypes.Tick, victimID uint64) {
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
	entity.Velocity = gametypes.Vec3{}
	entity.Animation = gametypes.AnimationStateIdle
	tick.MoveTo(entity, state.anchor)

	state.phase = phaseIdle
	state.aggroTarget = 0
	state.threat = make(map[uint64]int64)
}

// shortestRange is how close a mob has to get to use any ability it carries.
// Zero means it carries none and walks all the way in.
func (module *Module) shortestRange(mob gametypes.Mob) float32 {
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
func headingOf(direction gametypes.Vec3) float32 {
	return float32(math.Atan2(float64(direction.Y), float64(direction.X)))
}
