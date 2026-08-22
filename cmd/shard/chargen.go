package main

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
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
	loadout session.CombatLoadout
}

type chargenCombatLoadouts map[string]chargenCombatLoadout

func (loadouts chargenCombatLoadouts) StartingCombat(
	chargenOptionID string,
) (session.CombatLoadout, bool) {
	loadout, ok := loadouts[chargenOptionID]
	result := loadout.loadout
	result.AbilityIDs = append([]string(nil), result.AbilityIDs...)
	if result.ActionBindings != nil {
		result.ActionBindings = append([]combat.ActionBinding{}, result.ActionBindings...)
	}
	return result, ok
}

func combatLoadouts(content *pack.Pack) (chargenCombatLoadouts, error) {
	loadouts := make(chargenCombatLoadouts)
	for _, option := range content.ChargenOptions() {
		abilityIDs := append([]string(nil), option.StartingAbility...)
		bindings := make([]combat.ActionBinding, 0, len(option.StartingActions))
		for _, action := range option.StartingActions {
			abilityIDs = append(abilityIDs, action.ActionID)
			if action.SlotIndex != nil {
				bindings = append(bindings, combat.ActionBinding{
					SlotIndex: *action.SlotIndex,
					AbilityID: action.ActionID,
				})
			}
		}
		if len(abilityIDs) == 0 {
			return nil, fmt.Errorf("chargen option %q carries no starting abilities", option.ID)
		}
		if option.StartingMaxHealth == 0 {
			return nil, fmt.Errorf("chargen option %q carries no authored maximum health", option.ID)
		}
		loadout := session.CombatLoadout{
			AbilityIDs: abilityIDs, ActionBindings: bindings,
			UsesNativeActions: len(option.StartingActions) > 0,
			MaxHealth:         int32(option.StartingMaxHealth),
		}
		if loadout.UsesNativeActions {
			resource, combatant, err := nativeActionAdmission(option)
			if err != nil {
				return nil, err
			}
			loadout.ActionResource = resource
			loadout.Combatant = combatant
		}
		loadouts[option.ID] = chargenCombatLoadout{loadout: loadout}
	}
	if len(loadouts) == 0 {
		return nil, fmt.Errorf("content pack carries no authored combat loadouts")
	}
	return loadouts, nil
}

func nativeActionAdmission(option pack.ChargenOption) (
	combat.ActionResource,
	session.WarriorCombatant,
	error,
) {
	if option.CombatStats == nil {
		return combat.ActionResource{}, session.WarriorCombatant{},
			fmt.Errorf("chargen option %q carries native actions without combat stats", option.ID)
	}
	stats := option.CombatStats
	initial, err := decimalMilli(stats.Resource.Initial)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{},
			fmt.Errorf("chargen option %q initial resource: %w", option.ID, err)
	}
	maximum, err := decimalMilli(stats.Resource.Maximum)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{},
			fmt.Errorf("chargen option %q maximum resource: %w", option.ID, err)
	}
	mainhandSpeed, err := secondsDecimal(stats.Mainhand.SpeedMS)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{}, err
	}
	rangedSpeed, err := secondsDecimal(stats.Ranged.SpeedMS)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{}, err
	}
	physical, err := physicalScale(stats, stats.Mainhand)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{},
			fmt.Errorf("chargen option %q mainhand physical scale: %w", option.ID, err)
	}
	physicalRanged, err := physicalScale(stats, stats.Ranged)
	if err != nil {
		return combat.ActionResource{}, session.WarriorCombatant{},
			fmt.Errorf("chargen option %q ranged physical scale: %w", option.ID, err)
	}
	equipped := make(map[string]bool)
	for _, item := range option.StartingLoadout {
		if item.Slot != "" && item.Slot != "bag" {
			equipped[item.Slot] = true
		}
	}
	return combat.ActionResource{
		Kind: stats.Resource.Kind, CurrentMilli: initial, MaximumMilli: maximum,
	}, session.WarriorCombatant{
		PhysicalScale: physical, PhysicalRangedScale: physicalRanged,
		WeaponSpeedScale: map[string]script.Decimal{
			"Mainhand": mainhandSpeed,
			"Ranged":   rangedSpeed,
		},
		EquippedDressTypes: equipped,
	}, nil
}

func decimalMilli(value pack.Decimal) (int64, error) {
	mantissa, scale := value.Mantissa, value.Scale
	if mantissa < 0 || scale < 0 || scale > 9 {
		return 0, fmt.Errorf("invalid non-negative decimal %dE-%d", mantissa, scale)
	}
	for scale > 3 && mantissa%10 == 0 {
		mantissa /= 10
		scale--
	}
	if scale > 3 {
		return 0, fmt.Errorf("decimal %dE-%d is not exact to thousandths", mantissa, scale)
	}
	for scale < 3 {
		if mantissa > math.MaxInt64/10 {
			return 0, fmt.Errorf("decimal exceeds int64 thousandths")
		}
		mantissa *= 10
		scale++
	}
	return mantissa, nil
}

func secondsDecimal(milliseconds pack.Decimal) (script.Decimal, error) {
	return normalizeDecimal(milliseconds.Mantissa, milliseconds.Scale+3)
}

func normalizeDecimal(mantissa int64, scale int32) (script.Decimal, error) {
	for scale > 0 && mantissa%10 == 0 {
		mantissa /= 10
		scale--
	}
	if mantissa <= 0 || scale < 0 || scale > 9 {
		return script.Decimal{}, fmt.Errorf("native action scale %dE-%d is not representable", mantissa, scale)
	}
	return script.Decimal{Mantissa: mantissa, Scale: scale}, nil
}

const (
	millisecondsPerSecond = int64(1000)
	// The recovered server scaler chain normalizes weapon speed against 2500ms
	// in WeaponSpeedBSVScaler/RangedWeaponSpeedBSVScaler. It is part of the
	// formula, not an action or chargen default.
	retailWeaponSpeedBaseMS = int64(2500)
	physicalScaleDigits     = int32(9)
)

// physicalScale implements the authored PhysicalScaler chain:
//
//	WeaponDamageBSVScaler -> StrengthDPSScaler -> WeaponSpeedBSVScaler
//
// The ranged scaler uses the same chain over the ranged profile. Every
// character-varying term comes from StartingCharacterStats; the two integer
// factors are unit conversion and the recovered formula's speed base.
func physicalScale(stats *pack.StartingCombatStats, weapon pack.WeaponProfile) (script.Decimal, error) {
	strength, ok := exactStat(stats.Innate, "strength")
	if !ok {
		return script.Decimal{}, fmt.Errorf("no authored strength stat")
	}
	fairy, err := positiveRat(stats.FairyScaler, "fairy scaler")
	if err != nil {
		return script.Decimal{}, err
	}
	minimum, err := nonNegativeRat(weapon.MinimumDamage, "minimum weapon damage")
	if err != nil {
		return script.Decimal{}, err
	}
	maximum, err := positiveRat(weapon.MaximumDamage, "maximum weapon damage")
	if err != nil {
		return script.Decimal{}, err
	}
	speed, err := positiveRat(weapon.SpeedMS, "weapon speed")
	if err != nil {
		return script.Decimal{}, err
	}
	weaponDPSDefault, err := positiveRat(stats.WeaponDPSDefault, "default weapon DPS")
	if err != nil {
		return script.Decimal{}, err
	}
	strengthValue, err := positiveRat(strength, "strength")
	if err != nil {
		return script.Decimal{}, err
	}
	baseStat, err := positiveRat(stats.BaseStatValue, "base stat value")
	if err != nil {
		return script.Decimal{}, err
	}

	averageDamage := new(big.Rat).Add(minimum, maximum)
	averageDamage.Quo(averageDamage, big.NewRat(2, 1))
	weaponDPS := new(big.Rat).Mul(averageDamage, big.NewRat(millisecondsPerSecond, 1))
	weaponDPS.Quo(weaponDPS, speed)
	result := new(big.Rat).Mul(fairy, weaponDPS)
	result.Quo(result, weaponDPSDefault)
	result.Mul(result, strengthValue)
	result.Quo(result, baseStat)
	result.Mul(result, speed)
	result.Quo(result, big.NewRat(retailWeaponSpeedBaseMS, 1))
	return roundedScriptDecimal(result, physicalScaleDigits)
}

func exactStat(values []pack.ExactStatValue, id string) (pack.Decimal, bool) {
	for _, value := range values {
		if strings.EqualFold(value.Stat, id) {
			return value.Value, true
		}
	}
	return pack.Decimal{}, false
}

func positiveRat(value pack.Decimal, field string) (*big.Rat, error) {
	result, err := decimalRat(value)
	if err != nil || result.Sign() <= 0 {
		return nil, fmt.Errorf("%s is not a positive representable decimal", field)
	}
	return result, nil
}

func nonNegativeRat(value pack.Decimal, field string) (*big.Rat, error) {
	result, err := decimalRat(value)
	if err != nil || result.Sign() < 0 {
		return nil, fmt.Errorf("%s is not a non-negative representable decimal", field)
	}
	return result, nil
}

func decimalRat(value pack.Decimal) (*big.Rat, error) {
	if value.Scale < 0 || value.Scale > 18 {
		return nil, fmt.Errorf("decimal %dE-%d has unsupported scale", value.Mantissa, value.Scale)
	}
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(value.Scale)), nil)
	return new(big.Rat).SetFrac(big.NewInt(value.Mantissa), denominator), nil
}

// roundedScriptDecimal converts the rational scaler once at the interpreter's
// maximum exact precision. Positive ties round upward, matching the combat
// damage boundary's half-up rule.
func roundedScriptDecimal(value *big.Rat, scale int32) (script.Decimal, error) {
	if value == nil || value.Sign() <= 0 || scale < 0 || scale > 9 {
		return script.Decimal{}, fmt.Errorf("physical scale is not a positive supported value")
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	numerator := new(big.Int).Mul(value.Num(), factor)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, value.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(value.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return script.Decimal{}, fmt.Errorf("physical scale exceeds int64 decimal precision")
	}
	return normalizeDecimal(quotient.Int64(), scale)
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
