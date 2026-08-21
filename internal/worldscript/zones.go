package worldscript

import "github.com/SarnautCore/server/internal/gametypes"

func (module *Module) stepScriptZones(tick gametypes.Tick) {
	for _, zoneID := range sortedKeys(module.zones) {
		if module.state.DisabledZones[zoneID] {
			continue
		}
		spec := module.zones[zoneID]
		inside := make(map[uint64]bool)
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.Kind != gametypes.EntityKindPlayer || !entity.Alive || !entity.Replicated {
				return true
			}
			if spec.Bounds.Contains(tick.Position(entity)) {
				inside[entity.ID] = true
				if !module.members[zoneID][entity.ID] {
					event := Event{Kind: EventScriptZoneEntering, ZoneID: zoneID, ActorEntityID: entity.ID}
					if module.emit(tick, event) {
						if module.members[zoneID] == nil {
							module.members[zoneID] = make(map[uint64]bool)
						}
						module.members[zoneID][entity.ID] = true
						module.emit(tick, Event{Kind: EventScriptZoneEntered, ZoneID: zoneID, ActorEntityID: entity.ID})
					}
				}
			}
			return true
		})
		for entityID := range module.members[zoneID] {
			if !inside[entityID] {
				delete(module.members[zoneID], entityID)
				module.emit(tick, Event{Kind: EventScriptZoneLeft, ZoneID: zoneID, ActorEntityID: entityID})
			}
		}
	}
}
