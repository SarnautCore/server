package pack

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
	"github.com/SarnautCore/server/internal/gametypes"
)

// Table names carrying the rules mechanics/combat.md reads.
const (
	tableAbilities = "abilities"
	tableFactions  = "factions"
	tableMobs      = "mobs"
)

// Stances a faction relation may declare.
const (
	StanceHostile  = gametypes.StanceHostile
	StanceNeutral  = gametypes.StanceNeutral
	StanceFriendly = gametypes.StanceFriendly
)

// AbilityEffect is one effect an ability applies.
type AbilityEffect = gametypes.AbilityEffect

// Ability is one activatable ability, as authored.
type Ability = gametypes.Ability

// FactionRelation is one directed stance towards another faction.
type FactionRelation = gametypes.FactionRelation

// Faction is a hostility table entry.
type Faction = gametypes.Faction

// Mob is one creature record: the combat inputs of mechanics/combat.md
// section 4, plus the ids later systems resolve.
type Mob = gametypes.Mob

// Ability returns one ability by canonical id.
func (p *Pack) Ability(id string) (Ability, bool) {
	value, ok := p.abilities[id]
	return value, ok
}

// Abilities lists every ability in canonical-id order.
func (p *Pack) Abilities() []Ability { return sortedByID(p.abilities, p.abilityIDs) }

// Faction returns one faction by canonical id.
func (p *Pack) Faction(id string) (Faction, bool) {
	value, ok := p.factions[id]
	return value, ok
}

// Mob returns one creature record by canonical id.
func (p *Pack) Mob(id string) (Mob, bool) {
	value, ok := p.mobs[id]
	return value, ok
}

func sortedByID[T any](values map[string]T, order []string) []T {
	result := make([]T, 0, len(order))
	for _, id := range order {
		result = append(result, values[id])
	}
	return result
}

func readAbilities(tables map[string]*table) (map[string]Ability, []string, error) {
	loaded, ok := tables[tableAbilities]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", ErrMissingTable, tableAbilities)
	}
	abilities := make(map[string]Ability, loaded.rowCount)
	order := make([]string, 0, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.Ability
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, nil, fmt.Errorf("%w: decode ability row: %w", ErrMalformedTable, err)
		}
		if row.GetId() == "" {
			return nil, nil, fmt.Errorf("%w: table %q holds a row with no id", ErrMalformedTable, tableAbilities)
		}
		effects := make([]AbilityEffect, 0, len(row.GetEffects()))
		for _, effect := range row.GetEffects() {
			effects = append(effects, AbilityEffect{
				Kind:             effect.GetKind(),
				Element:          effect.GetElement(),
				Amount:           float64(effect.GetAmount()),
				AttackPowerCoeff: float64(effect.GetAttackPowerCoeff()),
			})
		}
		abilities[row.GetId()] = Ability{
			ID:          row.GetId(),
			NameKey:     row.GetNameKey(),
			Target:      row.GetTarget(),
			RangeM:      row.GetRangeM(),
			CastTime:    time.Duration(row.GetCastTimeMs()) * time.Millisecond,
			Cooldown:    time.Duration(row.GetCooldownMs()) * time.Millisecond,
			TriggersGCD: row.GetTriggersGcd(),
			Effects:     effects,
		}
		order = append(order, row.GetId())
	}
	return abilities, order, nil
}

func readFactions(tables map[string]*table) (map[string]Faction, error) {
	loaded, ok := tables[tableFactions]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMissingTable, tableFactions)
	}
	factions := make(map[string]Faction, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.Faction
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode faction row: %w", ErrMalformedTable, err)
		}
		relations := make([]FactionRelation, 0, len(row.GetRelations()))
		for _, relation := range row.GetRelations() {
			relations = append(relations, FactionRelation{
				FactionID: relation.GetFactionId(),
				Stance:    relation.GetStance(),
			})
		}
		factions[row.GetId()] = Faction{
			ID:            row.GetId(),
			NameKey:       row.GetNameKey(),
			PlayerFaction: row.GetPlayerFaction(),
			Attackable:    row.GetAttackable(),
			DefaultStance: row.GetDefaultStance(),
			Relations:     relations,
		}
	}
	return factions, nil
}

func readMobs(tables map[string]*table) (map[string]Mob, error) {
	loaded, ok := tables[tableMobs]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMissingTable, tableMobs)
	}
	mobs := make(map[string]Mob, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.Mob
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode mob row: %w", ErrMalformedTable, err)
		}
		if row.GetLevelMax() < row.GetLevelMin() {
			return nil, fmt.Errorf(
				"%w: mob %q declares level_min %d above level_max %d",
				ErrMalformedTable, row.GetId(), row.GetLevelMin(), row.GetLevelMax(),
			)
		}
		mobs[row.GetId()] = Mob{
			ID:           row.GetId(),
			NameKey:      row.GetNameKey(),
			FactionID:    row.GetFactionId(),
			MobKindID:    row.GetMobKindId(),
			LevelMin:     row.GetLevelMin(),
			LevelMax:     row.GetLevelMax(),
			WalkSpeed:    row.GetWalkSpeed(),
			HPMod:        float64(row.GetHpMod()),
			AggroRadiusM: row.GetAggroRadiusM(),
			LeashRadiusM: row.GetLeashRadiusM(),
			AbilityIDs:   row.GetAbilityIds(),
			LootTableID:  row.GetLootTableId(),
		}
	}
	return mobs, nil
}
