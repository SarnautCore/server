package chataudience

import (
	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
)

// WorldAvatar builds one admission from an authenticated identity and a world
// entity. Scanner and mission are explicit because the current world package
// does not own visibility-controller topology.
func WorldAvatar(
	characterID uuid.UUID,
	entityID uint64,
	scannerID string,
	missionID string,
	zone gametypes.Zone,
) Avatar {
	if zone == nil {
		return Avatar{}
	}
	return Avatar{
		CharacterID: characterID,
		EntityID:    entityID,
		ZoneID:      zone.ID(),
		ScannerID:   scannerID,
		MissionID:   missionID,
		Observe:     ObserveWorldAvatar(zone, entityID),
	}
}

// ObserveWorldAvatar reads one player entity under its zone lock. NPCs,
// departed entities, and entities without a faction are not avatars.
func ObserveWorldAvatar(zone gametypes.Zone, entityID uint64) ObserveAvatar {
	return func() (Observation, bool) {
		if zone == nil || entityID == 0 {
			return Observation{}, false
		}
		var observation Observation
		found := false
		err := zone.GameCommand(func(tick gametypes.Tick) error {
			entity := tick.Entity(entityID)
			if entity == nil || entity.Kind != gametypes.EntityKindPlayer || entity.Faction == "" {
				return nil
			}
			observation = Observation{
				Position:  tick.Position(entity),
				FactionID: entity.Faction,
			}
			found = true
			return nil
		})
		if err != nil {
			return Observation{}, false
		}
		return observation, found
	}
}
