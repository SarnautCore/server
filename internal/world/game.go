package world

import (
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
)

// GameCommand adapts the concrete locked tick to the shared gameplay seam.
func (zone *Zone) GameCommand(run func(gametypes.Tick) error) error {
	return zone.Command(func(tick *Tick) error { return run(gameTick{tick: tick}) })
}

// GameAddSystem registers work written against the shared gameplay seam.
func (zone *Zone) GameAddSystem(system gametypes.System) {
	zone.AddSystem(gameSystem{system: system})
}

type gameSystem struct{ system gametypes.System }

func (system gameSystem) Step(tick *Tick) { system.system.Step(gameTick{tick: tick}) }

type gameTick struct{ tick *Tick }

func (tick gameTick) Number() uint64          { return tick.tick.Number() }
func (tick gameTick) Interval() time.Duration { return tick.tick.Interval() }
func (tick gameTick) ZoneID() string          { return tick.tick.ZoneID() }
func (tick gameTick) PendingWork() int        { return tick.tick.PendingWork() }
func (tick gameTick) Despawn(id uint64) bool  { return tick.tick.Despawn(id) }
func (tick gameTick) Entity(id uint64) *gametypes.EntityData {
	entity := tick.tick.Entity(id)
	if entity == nil {
		return nil
	}
	return &entity.EntityData
}

func (tick gameTick) Each(visit func(*gametypes.EntityData) bool) {
	tick.tick.Each(func(entity *Entity) bool { return visit(&entity.EntityData) })
}

func (tick gameTick) Within(
	centre gametypes.Vec3,
	radius float32,
	kind gametypes.EntityKind,
	visit func(*gametypes.EntityData) bool,
) {
	tick.tick.Within(centre, radius, kind, func(entity *Entity) bool {
		return visit(&entity.EntityData)
	})
}

func (tick gameTick) Position(data *gametypes.EntityData) gametypes.Vec3 {
	if entity := tick.entity(data); entity != nil {
		return entity.Position()
	}
	return gametypes.Vec3{}
}

func (tick gameTick) MoveTo(data *gametypes.EntityData, to gametypes.Vec3) {
	tick.tick.MoveTo(tick.entity(data), to)
}

func (tick gameTick) SpawnNPC(spec gametypes.NPCSpec) *gametypes.EntityData {
	entity := tick.tick.SpawnNPC(spec)
	return &entity.EntityData
}

func (tick gameTick) After(delay uint64, run func(gametypes.Tick)) {
	tick.tick.After(delay, func(later *Tick) { run(gameTick{tick: later}) })
}

func (tick gameTick) entity(data *gametypes.EntityData) *Entity {
	if data == nil {
		return nil
	}
	entity := tick.tick.Entity(data.ID)
	if entity == nil || &entity.EntityData != data {
		return nil
	}
	return entity
}

var _ gametypes.Zone = (*Zone)(nil)
