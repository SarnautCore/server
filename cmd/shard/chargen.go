package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/session"
)

// legacyFixtureStartingHealth keeps the pre-native public demo pack readable
// in unit tests. The shard requires player-progression at startup, which puts
// production on the strict path and makes this value unreachable there.
const legacyFixtureStartingHealth = 100

// healthStat is the starting-stat name that overrides [startingHealth].
const healthStat = "health"

// chargenTemplateSet is the [charstore.ChargenTemplates] the shard materializes a
// first login from: one plain snapshot per chargen option, built once at boot
// from the compiled pack.
//
// Building it here rather than inside `internal/charstore` is what keeps ADR 0031's
// rule true in both directions — the persistence package holds no content
// types, and the pack reader holds no database types.
type chargenTemplateSet map[string]charstore.Snapshot

type chargenCombatLoadout struct {
	abilityIDs []string
	maxHealth  int32
}

type chargenCombatLoadouts map[string]chargenCombatLoadout

func (loadouts chargenCombatLoadouts) StartingCombat(
	chargenOptionID string,
) (session.CombatLoadout, bool) {
	loadout, ok := loadouts[chargenOptionID]
	return session.CombatLoadout{
		AbilityIDs: append([]string(nil), loadout.abilityIDs...),
		MaxHealth:  loadout.maxHealth,
	}, ok
}

func combatLoadouts(content *pack.Pack) (chargenCombatLoadouts, error) {
	loadouts := make(chargenCombatLoadouts)
	for _, option := range content.ChargenOptions() {
		abilityIDs := append([]string(nil), option.StartingAbility...)
		if len(abilityIDs) == 0 {
			for _, action := range option.StartingActions {
				abilityIDs = append(abilityIDs, action.ActionID)
			}
		}
		if len(abilityIDs) == 0 {
			return nil, fmt.Errorf("chargen option %q carries no starting abilities", option.ID)
		}
		if option.StartingMaxHealth == 0 {
			return nil, fmt.Errorf("chargen option %q carries no authored maximum health", option.ID)
		}
		loadouts[option.ID] = chargenCombatLoadout{
			abilityIDs: abilityIDs,
			maxHealth:  int32(option.StartingMaxHealth),
		}
	}
	if len(loadouts) == 0 {
		return nil, fmt.Errorf("content pack carries no authored combat loadouts")
	}
	return loadouts, nil
}

func (templates chargenTemplateSet) Template(chargenOptionID string) (charstore.Snapshot, bool) {
	snapshot, ok := templates[chargenOptionID]
	if !ok {
		return charstore.Snapshot{}, false
	}
	// Hand out a copy: a materialization that mutated the template would give
	// the second character of the day the first one's leftovers.
	return charstore.Snapshot{
		State:     snapshot.State,
		Inventory: append([]charstore.InventoryItem(nil), snapshot.Inventory...),
		Quests:    append([]charstore.QuestState(nil), snapshot.Quests...),
	}, true
}

// chargenTemplates turns the pack's chargen table into fresh-character
// snapshots. Every field it reads is a pack row: the spawn, the level, the
// starting loadout and the starting quests all come from the data, which is
// what makes the second playable option a data change (ADR 0032).
//
// fallbackSpawn is used only by an option whose spawn is the origin, which the
// compiler should have refused; it is the zone's configured player spawn and
// exists so a bad row does not drop a player into the void.
func chargenTemplates(content *pack.Pack, fallbackSpawn pack.Vec3) (chargenTemplateSet, error) {
	options := content.ChargenOptions()
	if len(options) == 0 {
		return nil, fmt.Errorf(
			"content pack %q carries no chargen table: a character has nothing to be "+
				"materialized from (ADR 0032). Rebuild the pack from a source tree with "+
				"chargen documents", content.Directory(),
		)
	}

	_, strictNative := content.PlayerProgression()
	templates := make(chargenTemplateSet, len(options))
	for _, option := range options {
		spawn := option.SpawnPosition
		if spawn == (pack.Vec3{}) {
			if strictNative {
				return nil, fmt.Errorf("chargen option %q carries no authored spawn", option.ID)
			}
			spawn = fallbackSpawn
		}
		level := int32(option.StartingLevel)
		if level < 1 {
			if strictNative {
				return nil, fmt.Errorf("chargen option %q carries no authored starting level", option.ID)
			}
			level = 1
		}
		health := int32(option.StartingHealth)
		if health <= 0 {
			if strictNative {
				return nil, fmt.Errorf("chargen option %q carries no authored starting health", option.ID)
			}
			health = legacyFixtureStartingHealth
		}
		snapshot := charstore.Snapshot{
			State: charstore.CharacterState{
				Position: charstore.Vec3{X: spawn.X, Y: spawn.Y, Z: spawn.Z},
				Heading:  option.SpawnHeading,
				Level:    level,
				Health:   health,
			},
		}
		for _, stat := range option.StartingStats {
			if !strictNative && strings.EqualFold(stat.Stat, healthStat) && stat.Value > 0 {
				snapshot.State.Health = int32(stat.Value)
			}
		}
		for index, item := range option.StartingLoadout {
			if item.Quantity == 0 {
				continue
			}
			snapshot.Inventory = append(snapshot.Inventory, charstore.InventoryItem{
				// The authored order is the slot order. Equipment slots arrive
				// with the inventory module; until then `bag` is every slot.
				Slot:     int32(index),
				ItemID:   item.ItemID,
				Quantity: int32(item.Quantity),
			})
		}
		for _, questID := range option.StartingQuests {
			snapshot.Quests = append(snapshot.Quests, charstore.QuestState{
				QuestID: questID,
				// The quest module owns the state machine; a granted starter
				// quest begins offered, which is the one transition every
				// reading of `../mechanics/quests.md` agrees on.
				State: "offered",
			})
		}
		templates[option.ID] = snapshot
	}
	return templates, nil
}

// shardInstanceID identifies this process in the Valkey play lock. Two shards
// on one host must not claim to be each other, or one would silently renew the
// other's lock.
func shardInstanceID(configured string) string {
	if configured != "" {
		return configured
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "shard"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// sortedOptionIDs is for logging: a stable list of what the shard can
// materialize.
func sortedOptionIDs(templates chargenTemplateSet) []string {
	ids := make([]string, 0, len(templates))
	for id := range templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
