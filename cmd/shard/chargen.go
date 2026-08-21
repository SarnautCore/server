package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
)

// startingHealth is the health a fresh character materializes with when the
// chargen option's starting stats do not name one.
//
// It is the one number here that is not in the pack, and it is here rather than
// in `internal/charstore` so that the seam that owns character state stays free of
// gameplay defaults.
// TODO(m2-combat): derive health from the option's stats and the level curve,
// and delete this.
const startingHealth = 100

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

func (templates chargenTemplateSet) Template(chargenOptionID string) (charstore.Snapshot, bool) {
	snapshot, ok := templates[chargenOptionID]
	if !ok {
		return charstore.Snapshot{}, false
	}
	// Hand out a copy: a materialization that mutated the template would give
	// the second character of the day the first one's leftovers.
	cloned := charstore.Snapshot{
		State:     snapshot.State,
		Inventory: append([]charstore.InventoryItem(nil), snapshot.Inventory...),
		Quests:    append([]charstore.QuestState(nil), snapshot.Quests...),
	}
	if snapshot.HUD != nil {
		hud := *snapshot.HUD
		hud.Equipment = append([]charstore.EquipmentItem(nil), snapshot.HUD.Equipment...)
		for index := range hud.Equipment {
			cloneEquipmentItemPointers(&hud.Equipment[index].ItemInstance)
		}
		hud.BagLayout.Partitions = append([]charstore.BagPartition(nil), snapshot.HUD.BagLayout.Partitions...)
		if snapshot.HUD.Bag != nil {
			bag := *snapshot.HUD.Bag
			cloneEquipmentItemPointers(&bag)
			hud.Bag = &bag
		}
		for index, stat := range snapshot.HUD.Stats {
			hud.Stats[index] = stat
			hud.Stats[index].Base = cloneOptionalFloat32(stat.Base)
			hud.Stats[index].Result = cloneOptionalFloat32(stat.Result)
			hud.Stats[index].ResultLongTerm = cloneOptionalFloat32(stat.ResultLongTerm)
		}
		for index, action := range snapshot.HUD.Actions {
			hud.Actions[index] = action
			if action.AbilityID != nil {
				abilityID := *action.AbilityID
				hud.Actions[index].AbilityID = &abilityID
			}
		}
		cloned.HUD = &hud
	}
	return cloned, true
}

// chargenTemplates turns the pack's chargen table into fresh-character
// snapshots. Every field it reads is a pack row: the spawn, the level, the
// starting loadout and the starting quests all come from the data, which is
// what makes the second playable option a data change (ADR 0032).
//
// fallbackSpawn is used only by an option whose spawn is the origin, which the
// compiler should have refused; it is the zone's configured player spawn and
// exists so a bad row does not drop a player into the void.
func chargenTemplates(
	content *pack.Pack,
	fallbackSpawn pack.Vec3,
) (chargenTemplateSet, error) {
	options := content.ChargenOptions()
	return chargenTemplatesFrom(options, fallbackSpawn, content.Item, content.Directory())
}

func chargenTemplatesFrom(
	options []pack.ChargenOption,
	fallbackSpawn pack.Vec3,
	itemByID func(string) (pack.Item, bool),
	source string,
) (chargenTemplateSet, error) {
	if len(options) == 0 {
		return nil, fmt.Errorf(
			"content pack %q carries no chargen table: a character has nothing to be "+
				"materialized from (ADR 0032). Rebuild the pack from a source tree with "+
				"chargen documents", source,
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
		hud := charstore.CharacterHUDState{
			Stats:   charstore.EmptyOrderedStats(),
			Actions: charstore.EmptyOrderedActionSlots(),
		}
		snapshot := charstore.Snapshot{
			State: charstore.CharacterState{
				Position: charstore.Vec3{X: spawn.X, Y: spawn.Y, Z: spawn.Z},
				Heading:  option.SpawnHeading,
				Level:    level,
				Health:   startingHealth,
			},
			HUD: &hud,
		}
		authoredStats := make(map[charstore.StatOrdinal]struct{})
		for _, stat := range option.StartingStats {
			if strings.EqualFold(stat.Stat, healthStat) && stat.Value > 0 {
				snapshot.State.Health = int32(stat.Value)
				continue
			}
			ordinal, ok := chargenStatOrdinal(stat.Stat)
			if !ok {
				return nil, fmt.Errorf("chargen option %q has unknown starting stat %q", option.ID, stat.Stat)
			}
			if _, duplicate := authoredStats[ordinal]; duplicate {
				return nil, fmt.Errorf("chargen option %q authors stat ordinal %d twice", option.ID, ordinal)
			}
			authoredStats[ordinal] = struct{}{}
			value := stat.Value
			snapshot.HUD.Stats[ordinal].Base = &value
		}
		occupiedEquipment := make(map[charstore.EquipmentSlot]struct{})
		var nextItemInstanceID uint64 = 1
		for _, item := range option.StartingLoadout {
			if item.Quantity == 0 {
				continue
			}
			slotName := strings.ToLower(strings.TrimSpace(item.Slot))
			instanceID := nextItemInstanceID
			nextItemInstanceID++
			switch slotName {
			case "bag":
				if snapshot.HUD.Bag != nil {
					return nil, fmt.Errorf("chargen option %q authors equipped bag slot twice", option.ID)
				}
				definition, ok := itemByID(item.ItemID)
				if !ok || definition.BagLayout == nil {
					return nil, fmt.Errorf("chargen option %q equips bag item %q without an admitted compiled bag layout",
						option.ID, item.ItemID)
				}
				snapshot.HUD.Bag = &charstore.ItemInstance{
					InstanceID: instanceID, ItemID: item.ItemID, Quantity: int32(item.Quantity),
				}
				snapshot.HUD.BagLayout = productBagLayout(*definition.BagLayout)
			default:
				slot, ok := chargenEquipmentSlot(slotName, occupiedEquipment)
				if !ok {
					return nil, fmt.Errorf("chargen option %q has unknown or occupied equipment slot %q", option.ID, item.Slot)
				}
				occupiedEquipment[slot] = struct{}{}
				snapshot.HUD.Equipment = append(snapshot.HUD.Equipment, charstore.EquipmentItem{
					Slot: slot,
					ItemInstance: charstore.ItemInstance{
						InstanceID: instanceID, ItemID: item.ItemID, Quantity: int32(item.Quantity),
					},
				})
			}
		}
		if snapshot.HUD.Bag == nil {
			return nil, fmt.Errorf("chargen option %q has no authored equipped bag", option.ID)
		}
		if err := charstore.ValidateProductBagLayout(snapshot.HUD.BagLayout); err != nil {
			return nil, fmt.Errorf("chargen option %q bag layout: %w", option.ID, err)
		}
		if len(option.StartingAbility) > charstore.ActionSlotCount {
			return nil, fmt.Errorf("chargen option %q authors %d abilities, action capacity is %d",
				option.ID, len(option.StartingAbility), charstore.ActionSlotCount)
		}
		for index, abilityID := range option.StartingAbility {
			if strings.TrimSpace(abilityID) == "" {
				return nil, fmt.Errorf("chargen option %q has an empty starting ability at index %d", option.ID, index)
			}
			value := abilityID
			snapshot.HUD.Actions[index].AbilityID = &value
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

func productBagLayout(source pack.BagLayout) charstore.ProductBagLayout {
	partitions := make([]charstore.BagPartition, len(source.PartitionSizes))
	for ordinal, capacity := range source.PartitionSizes {
		partitions[ordinal] = charstore.BagPartition{Ordinal: int16(ordinal), Capacity: int32(capacity)}
	}
	return charstore.ProductBagLayout{LayoutID: source.ID, Partitions: partitions}
}

func chargenStatOrdinal(name string) (charstore.StatOrdinal, bool) {
	if strings.EqualFold(strings.TrimSpace(name), "endurance") {
		// The current demo fixture predates the retail enum audit and calls
		// Stamina "endurance". This alias is consumed at materialization and is
		// never persisted as a source name.
		return charstore.StatStamina, true
	}
	return charstore.StatOrdinalByProductID(name)
}

func chargenEquipmentSlot(
	name string,
	occupied map[charstore.EquipmentSlot]struct{},
) (charstore.EquipmentSlot, bool) {
	if name == "ring" {
		if _, used := occupied[charstore.EquipmentRing1]; !used {
			return charstore.EquipmentRing1, true
		}
		if _, used := occupied[charstore.EquipmentRing2]; !used {
			return charstore.EquipmentRing2, true
		}
		return 0, false
	}
	slots := map[string]charstore.EquipmentSlot{
		"helm": charstore.EquipmentHelm, "armor": charstore.EquipmentArmor,
		"pants": charstore.EquipmentPants, "boots": charstore.EquipmentBoots,
		"mantle": charstore.EquipmentMantle, "gloves": charstore.EquipmentGloves,
		"bracers": charstore.EquipmentBracers, "belt": charstore.EquipmentBelt,
		"ring-1": charstore.EquipmentRing1, "ring1": charstore.EquipmentRing1,
		"ring-2": charstore.EquipmentRing2, "ring2": charstore.EquipmentRing2,
		"earrings": charstore.EquipmentEarrings, "necklace": charstore.EquipmentNecklace,
		"cloak": charstore.EquipmentCloak, "shirt": charstore.EquipmentShirt,
		"mainhand": charstore.EquipmentMainhand, "offhand": charstore.EquipmentOffhand,
		"ranged": charstore.EquipmentRanged, "tabard": charstore.EquipmentTabard,
		"trinket":         charstore.EquipmentTrinket,
		"death-insurance": charstore.EquipmentDeathInsurance,
		"deathinsurance":  charstore.EquipmentDeathInsurance,
	}
	slot, ok := slots[name]
	if !ok {
		return 0, false
	}
	_, used := occupied[slot]
	return slot, !used
}

func cloneOptionalFloat32(source *float32) *float32 {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

func cloneEquipmentItemPointers(item *charstore.ItemInstance) {
	if item.RemoveTime != nil {
		value := *item.RemoveTime
		item.RemoveTime = &value
	}
	if item.RuneResourceID != nil {
		value := *item.RuneResourceID
		item.RuneResourceID = &value
	}
	if item.RuneSlotResourceID != nil {
		value := *item.RuneSlotResourceID
		item.RuneSlotResourceID = &value
	}
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
