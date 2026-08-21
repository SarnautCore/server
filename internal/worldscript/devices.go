package worldscript

import (
	"fmt"
	"sort"

	"github.com/SarnautCore/server/internal/gametypes"
)

func (module *Module) populateDevices(devices []DeviceSpec) error {
	return module.zone.GameCommand(func(tick gametypes.Tick) error {
		ordered := append([]DeviceSpec(nil), devices...)
		sort.Slice(ordered, func(left, right int) bool {
			return ordered[left].PlacementID < ordered[right].PlacementID
		})
		for _, spec := range ordered {
			entity := tick.SpawnNPC(gametypes.NPCSpec{
				ContentID: spec.PresentationID, PlacementID: spec.PlacementID,
				Position: spec.Position, Heading: spec.Heading,
			})
			entity.Alive = false
			entity.Health = 0
			entity.MaxHealth = 0
			module.devices[entity.ID] = spec
			module.byDevice[spec.ID] = append(module.byDevice[spec.ID], entity.ID)
		}
		return nil
	})
}

// ResolveDevices is the runtime half of ImpactFindPermanentDevice and
// ImpactFindSingleDevice. The interpreter's finder owns which query it asks.
func (module *Module) ResolveDevices(query DeviceQuery) []uint64 {
	var result []uint64
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		result = module.ResolveDevicesAt(tick, query)
		return nil
	})
	return result
}

// ResolveDevicesAt is the non-reentrant finder path for the script host.
func (module *Module) ResolveDevicesAt(tick gametypes.Tick, query DeviceQuery) []uint64 {
	var result []uint64
	ids := module.byDevice[query.ContentID]
	if query.ContentID == "" {
		ids = make([]uint64, 0, len(module.devices))
		for entityID := range module.devices {
			ids = append(ids, entityID)
		}
		sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	}
	for _, entityID := range ids {
		spec, ok := module.devices[entityID]
		entity := tick.Entity(entityID)
		if !ok || entity == nil || module.state.OpenedDevices[spec.PlacementID] ||
			(query.MapID != "" && spec.MapID != query.MapID) ||
			(query.ScriptID != "" && spec.ScriptID != query.ScriptID) {
			continue
		}
		if query.Radius > 0 && gametypes.Distance(query.Origin, tick.Position(entity)) > query.Radius {
			continue
		}
		result = append(result, entityID)
		if !query.Permanent {
			break
		}
	}
	return result
}

// Interact opens one required tutorial chest through the loot module seam.
func (module *Module) Interact(actorEntityID, deviceEntityID uint64) (InteractResult, error) {
	result := InteractResult{DeviceEntityID: deviceEntityID}
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		actor := tick.Entity(actorEntityID)
		spec, ok := module.devices[deviceEntityID]
		device := tick.Entity(deviceEntityID)
		switch {
		case actor == nil || actor.Kind != gametypes.EntityKindPlayer || !actor.Alive:
			result.Refusal = InteractUnknownActor
		case !ok || device == nil:
			result.Refusal = InteractUnknownDevice
		case module.state.OpenedDevices[spec.PlacementID]:
			result.Refusal = InteractAlreadyUsed
		case gametypes.Distance(tick.Position(actor), tick.Position(device)) > spec.InteractRadius:
			result.Refusal = InteractTooFar
		case module.loot == nil:
			result.Refusal = InteractUnavailable
		default:
			containerID, err := module.loot.OpenDevice(tick, DeviceInteraction{
				ActorEntityID: actorEntityID, DeviceEntityID: deviceEntityID,
				DeviceID: spec.ID, PlacementID: spec.PlacementID, LootTableID: spec.LootTableID,
			})
			if err != nil {
				result.Refusal = InteractUnavailable
				return err
			}
			result.ContainerEntityID = containerID
			module.state.OpenedDevices[spec.PlacementID] = spec.OneShot
			module.emit(tick, Event{
				Kind: EventDeviceInteracted, ActorEntityID: actorEntityID, EntityID: deviceEntityID,
				ContentID: spec.ID, PlacementID: spec.PlacementID,
			})
			module.cues = append(module.cues, Cue{
				Kind: CueDeviceOpened, ActorEntityID: actorEntityID,
				EntityID: containerID, ResourceID: spec.ID,
			})
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("worldscript: open device %d: %w", deviceEntityID, err)
	}
	return result, nil
}
