package combat

import (
	"fmt"
	"math"

	"github.com/SarnautCore/server/internal/gametypes"
)

// Summon places one content-described mob into the active combat simulation.
// The caller already holds the zone lock through tick.
func (module *Module) Summon(
	tick gametypes.Tick,
	mob gametypes.Mob,
	placementID string,
	position gametypes.Vec3,
	heading float32,
) (*gametypes.EntityData, error) {
	if module == nil || tick == nil {
		return nil, fmt.Errorf("combat: summon has no active simulation tick")
	}
	if mob.ID == "" || mob.LevelMax < mob.LevelMin {
		return nil, fmt.Errorf("combat: summon mob definition is malformed")
	}
	if mob.FactionID == "" || !module.rules.HasFaction(mob.FactionID) {
		return nil, fmt.Errorf("combat: summon mob %s has unknown faction %q", mob.ID, mob.FactionID)
	}
	if !position.Finite() || math.IsNaN(float64(heading)) || math.IsInf(float64(heading), 0) {
		return nil, fmt.Errorf("combat: summon mob %s has a non-finite transform", mob.ID)
	}
	level := module.stream.level(mob.LevelMin, mob.LevelMax)
	spawn := gametypes.NPCSpawn{
		PlacementID: placementID,
		MobID:       mob.ID,
		Position:    position,
		Heading:     heading,
	}
	entity := tick.SpawnNPC(gametypes.NPCSpec{
		ContentID:   mob.ID,
		NameKey:     mob.NameKey,
		PlacementID: placementID,
		Faction:     mob.FactionID,
		Level:       level,
		MaxHealth:   MaxHealth(level, mob.HPMod),
		Position:    position,
		Heading:     heading,
	})
	state := module.newMobState(mob, spawn)
	state.summoned = true
	module.retireScriptReplays(entity.ID)
	module.mobs[entity.ID] = state
	// Later combat and effect paths resolve the entity's content id through the
	// rules map. A summon row need not have an authored placement, so Populate
	// may not have admitted it earlier.
	module.rules.mobs[mob.ID] = mob
	return entity, nil
}

// DismissSummon compensates a failed ImpactSummon activation. It refuses to
// remove an authored placement, even if a caller passes the wrong entity id.
func (module *Module) DismissSummon(tick gametypes.Tick, entityID uint64) error {
	if module == nil || tick == nil {
		return fmt.Errorf("combat: dismiss summon has no active simulation tick")
	}
	state, ok := module.mobs[entityID]
	if !ok || !state.summoned {
		return fmt.Errorf("combat: entity %d is not a live summon", entityID)
	}
	if !tick.Despawn(entityID) {
		return fmt.Errorf("combat: summon entity %d is absent or cannot despawn", entityID)
	}
	delete(module.mobs, entityID)
	module.retireScriptReplays(entityID)
	return nil
}

// TurnMob faces a stationary, idle mob toward destination. The impact is not
// allowed to interrupt movement or combat.
func (module *Module) TurnMob(
	tick gametypes.Tick,
	entityID uint64,
	destination gametypes.Vec3,
) error {
	if module == nil || tick == nil {
		return fmt.Errorf("combat: turn mob has no active simulation tick")
	}
	if !destination.Finite() {
		return fmt.Errorf("combat: turn destination is non-finite")
	}
	entity := tick.Entity(entityID)
	state, ok := module.mobs[entityID]
	if entity == nil || entity.Kind != gametypes.EntityKindNPC || !ok {
		return fmt.Errorf("combat: turn entity %d is not a combat mob", entityID)
	}
	if state.phase != phaseIdle || entity.Velocity != (gametypes.Vec3{}) {
		return fmt.Errorf("combat: turn entity %d is moving or in combat", entityID)
	}
	position := tick.Position(entity)
	deltaX := destination.X - position.X
	deltaY := destination.Y - position.Y
	if deltaX == 0 && deltaY == 0 {
		return nil
	}
	entity.Heading = float32(math.Atan2(float64(deltaY), float64(deltaX)))
	state.anchorYaw = entity.Heading
	return nil
}
