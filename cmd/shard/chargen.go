package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/store"
)

// startingHealth is the health a fresh character materializes with when the
// chargen option's starting stats do not name one.
//
// It is the one number here that is not in the pack, and it is here rather than
// in `internal/store` so that the seam that owns character state stays free of
// gameplay defaults.
// TODO(m2-combat): derive health from the option's stats and the level curve,
// and delete this.
const startingHealth = 100

// healthStat is the starting-stat name that overrides [startingHealth].
const healthStat = "health"

// chargenTemplateSet is the [store.ChargenTemplates] the shard materializes a
// first login from: one plain snapshot per chargen option, built once at boot
// from the compiled pack.
//
// Building it here rather than inside `internal/store` is what keeps ADR 0031's
// rule true in both directions — the persistence package holds no content
// types, and the pack reader holds no database types.
type chargenTemplateSet map[string]store.Snapshot

func (templates chargenTemplateSet) Template(chargenOptionID string) (store.Snapshot, bool) {
	snapshot, ok := templates[chargenOptionID]
	if !ok {
		return store.Snapshot{}, false
	}
	// Hand out a copy: a materialization that mutated the template would give
	// the second character of the day the first one's leftovers.
	return store.Snapshot{
		State:     snapshot.State,
		Inventory: append([]store.InventoryItem(nil), snapshot.Inventory...),
		Quests:    append([]store.QuestState(nil), snapshot.Quests...),
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

	templates := make(chargenTemplateSet, len(options))
	for _, option := range options {
		spawn := option.SpawnPosition
		if spawn == (pack.Vec3{}) {
			spawn = fallbackSpawn
		}
		level := int32(option.StartingLevel)
		if level < 1 {
			level = 1
		}
		snapshot := store.Snapshot{
			State: store.CharacterState{
				Position: store.Vec3{X: spawn.X, Y: spawn.Y, Z: spawn.Z},
				Heading:  option.SpawnHeading,
				Level:    level,
				Health:   startingHealth,
			},
		}
		for _, stat := range option.StartingStats {
			if strings.EqualFold(stat.Stat, healthStat) && stat.Value > 0 {
				snapshot.State.Health = int32(stat.Value)
			}
		}
		for index, item := range option.StartingLoadout {
			if item.Quantity == 0 {
				continue
			}
			snapshot.Inventory = append(snapshot.Inventory, store.InventoryItem{
				// The authored order is the slot order. Equipment slots arrive
				// with the inventory module; until then `bag` is every slot.
				Slot:     int32(index),
				ItemID:   item.ItemID,
				Quantity: int32(item.Quantity),
			})
		}
		for _, questID := range option.StartingQuests {
			snapshot.Quests = append(snapshot.Quests, store.QuestState{
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
