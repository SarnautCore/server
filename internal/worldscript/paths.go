package worldscript

import (
	"fmt"
	"math"

	"github.com/SarnautCore/server/internal/gametypes"
)

// StartPath starts one compiled GoThroughPath request.
func (module *Module) StartPath(request PathRequest) error {
	return module.zone.GameCommand(func(tick gametypes.Tick) error {
		return module.startPath(tick, request)
	})
}

func (module *Module) startPath(tick gametypes.Tick, request PathRequest) error {
	entity := tick.Entity(request.EntityID)
	path, ok := module.paths[request.PathID]
	if entity == nil || entity.Kind != gametypes.EntityKindNPC {
		return fmt.Errorf("worldscript: path entity %d is not a live NPC", request.EntityID)
	}
	if !ok {
		return fmt.Errorf("worldscript: unknown path %q", request.PathID)
	}
	if request.Speed == 0 {
		request.Speed = path.DefaultSpeed
	}
	if request.Speed <= 0 || !gametypes.Finite(request.Speed) {
		return fmt.Errorf("worldscript: path speed must be positive and finite")
	}
	if module.alreadyExecuted(request.ExecutionKey) {
		return nil
	}
	module.motions[request.EntityID] = &motion{request: request}
	entity.Animation = gametypes.AnimationStateMoving
	module.emit(tick, Event{
		Kind: EventPathStarted, EntityID: entity.ID, ContentID: entity.ContentID,
		PlacementID: entity.PlacementID, PathID: request.PathID, ExecutionKey: request.ExecutionKey,
	})
	module.cues = append(module.cues, Cue{
		Kind: CuePathStarted, EntityID: entity.ID, ResourceID: request.PathID,
		ExecutionKey: request.ExecutionKey,
	})
	module.markExecuted(request.ExecutionKey)
	if module.teleportPaths {
		endpoint := path.Waypoints[len(path.Waypoints)-1]
		tick.MoveTo(entity, endpoint)
		if !reachedHorizontal(tick.Position(entity), endpoint) {
			module.blockPath(tick, entity, request)
			return nil
		}
		module.completePath(tick, entity, request)
	}
	return nil
}

// PausePath yields movement to combat without losing the waypoint cursor.
func (module *Module) PausePath(entityID uint64) {
	_ = module.zone.GameCommand(func(_ gametypes.Tick) error {
		if active := module.motions[entityID]; active != nil {
			active.paused = true
		}
		return nil
	})
}

// ResumePath lets a route-linked mob continue after its leash return.
func (module *Module) ResumePath(entityID uint64) {
	_ = module.zone.GameCommand(func(_ gametypes.Tick) error {
		if active := module.motions[entityID]; active != nil {
			active.paused = false
		}
		return nil
	})
}

func (module *Module) stepPaths(tick gametypes.Tick) {
	for _, entityID := range module.sortedMotionIDs() {
		active := module.motions[entityID]
		if active == nil || active.paused {
			continue
		}
		entity := tick.Entity(entityID)
		path, ok := module.paths[active.request.PathID]
		if entity == nil || !ok || !entity.Alive || !entity.Replicated {
			delete(module.motions, entityID)
			continue
		}
		if active.waypoint >= len(path.Waypoints) {
			module.completePath(tick, entity, active.request)
			continue
		}
		from := tick.Position(entity)
		to := path.Waypoints[active.waypoint]
		dx, dy := to.X-from.X, to.Y-from.Y
		distance := float32(math.Hypot(float64(dx), float64(dy)))
		step := active.request.Speed * float32(tick.Interval().Seconds())
		if distance == 0 || step >= distance {
			tick.MoveTo(entity, to)
			if !reachedHorizontal(tick.Position(entity), to) {
				module.blockPath(tick, entity, active.request)
				continue
			}
			active.waypoint++
			if active.waypoint == len(path.Waypoints) {
				if active.request.Loop || path.Loop {
					active.waypoint = 0
				} else {
					module.completePath(tick, entity, active.request)
				}
			}
			continue
		}
		direction := gametypes.Vec3{X: dx / distance, Y: dy / distance}
		destination := gametypes.Vec3{X: from.X + direction.X*step, Y: from.Y + direction.Y*step, Z: from.Z}
		tick.MoveTo(entity, destination)
		moved := tick.Position(entity)
		if !reachedHorizontal(moved, destination) {
			module.blockPath(tick, entity, active.request)
			continue
		}
		entity.Heading = float32(math.Atan2(float64(direction.Y), float64(direction.X)))
		entity.Velocity = direction.Scale(active.request.Speed)
		entity.Animation = gametypes.AnimationStateMoving
	}
}

func reachedHorizontal(actual, expected gametypes.Vec3) bool {
	return actual.X == expected.X && actual.Y == expected.Y
}

func (module *Module) blockPath(tick gametypes.Tick, entity *gametypes.EntityData, request PathRequest) {
	delete(module.motions, entity.ID)
	entity.Velocity = gametypes.Vec3{}
	entity.Animation = gametypes.AnimationStateIdle
	module.emit(tick, Event{
		Kind: EventPathBlocked, EntityID: entity.ID, ContentID: entity.ContentID,
		PlacementID: entity.PlacementID, PathID: request.PathID, ExecutionKey: request.ExecutionKey,
	})
}

func (module *Module) completePath(tick gametypes.Tick, entity *gametypes.EntityData, request PathRequest) {
	delete(module.motions, entity.ID)
	entity.Velocity = gametypes.Vec3{}
	entity.Animation = gametypes.AnimationStateIdle
	event := Event{
		Kind: EventPathCompleted, EntityID: entity.ID, ContentID: entity.ContentID,
		PlacementID: entity.PlacementID, PathID: request.PathID, ExecutionKey: request.ExecutionKey,
	}
	module.emit(tick, event)
	module.cues = append(module.cues, Cue{
		Kind: CuePathCompleted, EntityID: entity.ID, ResourceID: request.PathID,
		ExecutionKey: request.ExecutionKey,
	})
}
