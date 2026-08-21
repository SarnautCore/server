package session

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/worldscript"
)

const goneThroughPathEventClass = "gameMechanics.world.mob.behaviour.aiEvents.GoneThroughPathEvent"

// BindWorldScripts attaches the world-state runtime after both modules exist.
// NewScriptDriver remains usable for packs with no world-script product.
func (driver *ScriptDriver) BindWorldScripts(module *worldscript.Module) {
	if driver != nil {
		driver.worldscripts = module
	}
}

// WorldScriptEventSource is the optional compiled-pack projection for script
// zone conditions and entry/leave impacts.
type WorldScriptEventSource interface {
	ScriptZoneConditions(zoneID string) []*script.Node
	ScriptZoneImpacts(zoneID string, entering bool) []*script.Node
}

// HandleWorldEvent implements worldscript.ScriptHost under the zone tick lock.
func (driver *ScriptDriver) HandleWorldEvent(tick gametypes.Tick, event worldscript.Event) bool {
	if driver == nil || tick == nil {
		return false
	}
	previous := driver.tick
	driver.tick = tick
	defer func() { driver.tick = previous }()

	source, hasZoneSource := driver.source.(WorldScriptEventSource)
	switch event.Kind {
	case worldscript.EventScriptZoneEntering:
		if !hasZoneSource {
			return false
		}
		frame := driver.worldEventFrame(event)
		for _, condition := range source.ScriptZoneConditions(event.ZoneID) {
			allowed, err := driver.evaluator.Predicate(context.Background(), condition, frame)
			if err != nil {
				driver.logger.Warn("script zone condition failed", "zone", event.ZoneID, "error", err)
				return false
			}
			if !allowed {
				return false
			}
		}
		return true
	case worldscript.EventScriptZoneEntered, worldscript.EventScriptZoneLeft:
		if !hasZoneSource {
			return true
		}
		entering := event.Kind == worldscript.EventScriptZoneEntered
		frame := driver.worldEventFrame(event)
		for _, impact := range source.ScriptZoneImpacts(event.ZoneID, entering) {
			if err := driver.evaluator.Evaluate(context.Background(), impact, frame); err != nil {
				driver.logger.Warn("script zone impact failed", "zone", event.ZoneID, "error", err)
			}
		}
		return true
	case worldscript.EventPathCompleted:
		entityID := event.EntityID
		scriptEvent := script.Event{
			EntityID: formatEntityID(entityID), SourceID: formatEntityID(entityID),
			EventClass: goneThroughPathEventClass,
		}
		for _, attachment := range append([]script.Attachment(nil), driver.attachments[entityID]...) {
			if err := driver.evaluator.Fire(context.Background(), attachment, scriptEvent); err != nil {
				driver.logger.Warn("script trigger failed on path completion",
					"entity_id", entityID, "trigger", attachment.TriggerRef.ID, "error", err)
			}
		}
		return true
	default:
		return true
	}
}

func (driver *ScriptDriver) worldEventFrame(event worldscript.Event) script.Frame {
	driver.evaluations++
	actor := formatEntityID(event.ActorEntityID)
	return script.Frame{
		EvaluationID: fmt.Sprintf("world|%d|%d", driver.tick.Number(), driver.evaluations),
		ZoneID:       driver.tick.ZoneID(), SourceID: event.ZoneID,
		CasterID: actor, TargetID: actor, Addressee: actor,
	}
}

func (driver *ScriptDriver) applyWorldCommand(command script.Command) (bool, error) {
	module := driver.worldscripts
	switch command.Kind {
	case script.CommandClientData, script.CommandClientDataCoords:
		if module == nil {
			return true, fmt.Errorf("session: client cue %s has no world-script host", command.Ref.ID)
		}
		actor, err := parseEntityID(command.EntityID)
		if err != nil {
			return true, err
		}
		destinations := make([]gametypes.Vec3, 0, len(command.Destinations))
		for _, destination := range command.Destinations {
			destinations = append(destinations, gametypes.Vec3{
				X: destination.Position.X, Y: destination.Position.Y, Z: destination.Position.Z,
			})
		}
		return true, module.EmitCueAt(driver.tick, worldscript.Cue{
			Kind: worldscript.CueClientData, ActorEntityID: actor,
			ResourceID: command.Ref.ID, Destinations: destinations,
			ExecutionKey: command.ExecutionKey,
		})
	case script.CommandSetScriptZoneDisabled:
		if module == nil {
			return true, fmt.Errorf("session: script zone %s has no world-script host", command.Ref.ID)
		}
		return true, module.SetZoneDisabledAt(driver.tick, command.Ref.ID, command.Bool, command.ExecutionKey)
	case script.CommandAddScriptZoneVariable:
		if module == nil {
			return true, fmt.Errorf("session: script variable %s has no world-script host", command.OtherRef.ID)
		}
		return true, module.UpdateVariableAt(
			driver.tick, command.OtherRef.ID, command.Count, command.Bool, command.ExecutionKey,
		)
	case script.CommandGoThroughPath:
		if module == nil {
			return true, fmt.Errorf("session: path command has no world-script host")
		}
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return true, err
		}
		speed, err := driver.pathSpeed(entityID, command.Bool)
		if err != nil {
			return true, err
		}
		waypoints := make([]gametypes.Vec3, 0, len(command.Destinations))
		for _, destination := range command.Destinations {
			waypoints = append(waypoints, gametypes.Vec3{
				X: destination.Position.X, Y: destination.Position.Y, Z: destination.Position.Z,
			})
		}
		return true, module.StartWaypointsAt(driver.tick, entityID, waypoints, speed, command.ExecutionKey)
	default:
		return false, nil
	}
}

func (driver *ScriptDriver) pathSpeed(entityID uint64, running bool) (float32, error) {
	entity := driver.tick.Entity(entityID)
	if entity == nil || driver.combat == nil {
		return 0, fmt.Errorf("session: path entity %d has no combat content", entityID)
	}
	mob, ok := driver.combat.Rules().Mob(entity.ContentID)
	if !ok || mob.WalkSpeed <= 0 {
		return 0, fmt.Errorf("session: path entity %d has no authored movement speed", entityID)
	}
	// The current compiled mob row carries one movement speed. RunningMode is
	// retained by the command, but inventing a multiplier here would make it a
	// server constant. The pack must carry a distinct run speed before the two
	// modes can differ.
	_ = running
	return mob.WalkSpeed, nil
}

func (driver *ScriptDriver) resolveWorldEntities(request script.ResolveRequest) ([]string, bool, error) {
	if driver.worldscripts == nil {
		return nil, false, nil
	}
	query := worldscript.DeviceQuery{}
	switch request.Finder {
	case "ImpactFindPermanentDevice":
		query.Permanent = true
	case "ImpactFindSingleDevice":
	case "ImpactDevicesAround":
		query.ContentID = request.Ref.ID
		radius, err := decimalFloat32(request.Radius)
		if err != nil {
			return nil, true, err
		}
		query.Radius = radius
		actorID, err := parseEntityID(request.Frame.Addressee)
		if err != nil {
			return nil, true, err
		}
		actor := driver.tick.Entity(actorID)
		if actor == nil {
			return nil, true, fmt.Errorf("session: device finder actor %d is absent", actorID)
		}
		query.Origin = driver.tick.Position(actor)
	default:
		return nil, false, nil
	}
	if request.Locator != nil {
		query.MapID = request.Locator.Map.ID
		query.ScriptID = request.Locator.ScriptID
	}
	ids := driver.worldscripts.ResolveDevicesAt(driver.tick, query)
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		result = append(result, formatEntityID(id))
	}
	sort.Strings(result)
	return result, true, nil
}

// DeviceLootAdapter routes ElixirChest drops through internal/loot.
type DeviceLootAdapter struct {
	Module        *loot.Module
	LifetimeTicks uint64
}

func (adapter DeviceLootAdapter) OpenDevice(
	tick gametypes.Tick,
	interaction worldscript.DeviceInteraction,
) (uint64, error) {
	if adapter.Module == nil {
		return 0, fmt.Errorf("session: device loot adapter has no loot module")
	}
	device := tick.Entity(interaction.DeviceEntityID)
	if device == nil {
		return 0, fmt.Errorf("session: device %d is absent", interaction.DeviceEntityID)
	}
	despawnTick := tick.Number() + adapter.LifetimeTicks
	if adapter.LifetimeTicks == 0 {
		despawnTick = tick.Number() + uint64((30*time.Second)/tick.Interval())
	}
	return adapter.Module.OpenDevice(tick, loot.DeviceOpen{
		ActorEntityID: interaction.ActorEntityID, DeviceEntityID: interaction.DeviceEntityID,
		ContentID: interaction.DeviceID, PlacementID: interaction.PlacementID,
		LootTableID: interaction.LootTableID, Position: tick.Position(device),
		Heading: device.Heading, DespawnTick: despawnTick,
	})
}

var _ worldscript.ScriptHost = (*ScriptDriver)(nil)
var _ worldscript.DeviceLootSink = DeviceLootAdapter{}
