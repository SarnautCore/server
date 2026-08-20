package combat

import (
	"fmt"
	"math"
	"time"

	"github.com/SarnautCore/server/internal/pack"
)

// The invented constants of mechanics/combat.md section 3.
//
// They are here rather than in the pack on purpose, and the line is worth
// stating because the rest of this package works hard to stay on the other
// side of it. What lives in content is anything that varies per ability, per
// mob or per spawn slot: range, damage, cooldown, level, hp_mod, aggro radius,
// leash radius, respawn window. What lives here is the shape of the formulae
// those numbers are fed into — a level-to-HP line, a level-to-armour line, and
// an armour softcap. Section 7.1 records that the real curve is not in the
// data tree at all, and that if it is ever recovered as a table it becomes
// content and these disappear.
const (
	hpBase               = 100.0
	hpPerLevel           = 20.0
	armorBase            = 10.0
	armorPerLevel        = 20.0
	attackPowerBase      = 12.0
	attackPowerPerLevel  = 2.0
	armorSoftcapBase     = 100.0
	armorSoftcapPerLevel = 50.0

	// globalCooldown is one timer per caster shared by every ability
	// (rule 5.4). Its value is the retail `<globalCooldown>` of SpellRoot.xdb.
	globalCooldown = 1000 * time.Millisecond
	// rangeTolerance absorbs client and server position drift over one
	// snapshot interval (rule 5.3.2).
	rangeTolerance = 0.5
	// corpseTimer is how long a corpse lasts (rule 5.9.4). Rule 5.9.8 records
	// that retail makes this per-mob and that M2 does not.
	corpseTimer = 30 * time.Second
	// chaseSpeedMultiplier: a mob chases at its own walk speed (rule 5.8.3).
	chaseSpeedMultiplier = 1.0

	// defaultRespawnMin and defaultRespawnMax apply to a spawn slot whose
	// placement authored no window. The values are the ones the reference
	// tree's InstLeague1 air-elemental spawn table carries.
	defaultRespawnMin = 10 * time.Second
	defaultRespawnMax = 14 * time.Second

	// defaultHPMod is rule 5.1.1's default for a mob record that carries none.
	defaultHPMod = 1.0

	// damageEffectKind is the only effect kind M2 resolves. An ability whose
	// effects are all something else deals no damage rather than being
	// rejected: the ability existed and the cooldown was paid.
	damageEffectKind = "damage"
)

// Rules is every gameplay input the shard resolves combat against, read from
// one content pack.
//
// It is a value with no behaviour of its own beyond arithmetic, so a test can
// build one from a fixture pack and assert the worked example of
// mechanics/combat.md section 6.1 without standing up a zone.
type Rules struct {
	abilities     map[string]pack.Ability
	abilityOrder  []string
	factions      map[string]pack.Faction
	mobs          map[string]pack.Mob
	playerFaction string
}

// RulesFromPack reads the ability, faction and mob tables of a loaded pack.
//
// It fails rather than defaulting: a pack with no player faction, or two, has
// no answer to "is this target hostile", and a shard that guessed would be
// deciding a gameplay rule in Go.
func RulesFromPack(content *pack.Pack) (Rules, error) {
	abilities := content.Abilities()
	rules := Rules{
		abilities:    make(map[string]pack.Ability, len(abilities)),
		abilityOrder: make([]string, 0, len(abilities)),
		factions:     make(map[string]pack.Faction),
		mobs:         make(map[string]pack.Mob),
	}
	for _, ability := range abilities {
		rules.abilities[ability.ID] = ability
		rules.abilityOrder = append(rules.abilityOrder, ability.ID)
	}

	for _, spawn := range content.NPCSpawns() {
		mob, ok := content.Mob(spawn.MobID)
		if !ok {
			return Rules{}, fmt.Errorf(
				"content pack %s: placement %q spawns mob %q, which the pack does not describe",
				content.ID(), spawn.PlacementID, spawn.MobID,
			)
		}
		rules.mobs[mob.ID] = mob
		if mob.FactionID == "" {
			return Rules{}, fmt.Errorf("content pack %s: mob %q names no faction", content.ID(), mob.ID)
		}
		faction, ok := content.Faction(mob.FactionID)
		if !ok {
			return Rules{}, fmt.Errorf(
				"content pack %s: mob %q names faction %q, which the pack does not describe",
				content.ID(), mob.ID, mob.FactionID,
			)
		}
		rules.factions[faction.ID] = faction
		for _, relation := range faction.Relations {
			if related, ok := content.Faction(relation.FactionID); ok {
				rules.factions[related.ID] = related
			}
		}
	}

	// A pack normally declares several playable factions, and which one a
	// character belongs to is chargen's answer, not this package's
	// (ADR 0032). Until the chargen row reaches the pack, the default is the
	// first in canonical-id order, which is deterministic and content-derived;
	// Options.PlayerFaction overrides it.
	for _, faction := range rules.factions {
		if !faction.PlayerFaction {
			continue
		}
		if rules.playerFaction == "" || faction.ID < rules.playerFaction {
			rules.playerFaction = faction.ID
		}
	}
	if rules.playerFaction == "" {
		return Rules{}, fmt.Errorf("content pack %s: no faction is marked player_faction", content.ID())
	}
	return rules, nil
}

// PlayerFaction is the faction an entering character belongs to by default.
func (rules Rules) PlayerFaction() string { return rules.playerFaction }

// HasFaction reports whether the rules describe a faction.
func (rules Rules) HasFaction(id string) bool {
	_, ok := rules.factions[id]
	return ok
}

// Ability returns one ability by canonical id.
func (rules Rules) Ability(id string) (pack.Ability, bool) {
	ability, ok := rules.abilities[id]
	return ability, ok
}

// AbilityIDs lists every ability the pack carries, in canonical-id order.
func (rules Rules) AbilityIDs() []string {
	result := make([]string, len(rules.abilityOrder))
	copy(result, rules.abilityOrder)
	return result
}

// Mob returns one creature record by canonical id.
func (rules Rules) Mob(id string) (pack.Mob, bool) {
	mob, ok := rules.mobs[id]
	return mob, ok
}

// MaxHealth is rule 5.1.1. `hpMod` of zero means the content record carried
// none, which the rule defines as 1.0 rather than as zero health.
func MaxHealth(level uint32, hpMod float64) int32 {
	if hpMod <= 0 {
		hpMod = defaultHPMod
	}
	return int32(roundHalfUp((hpBase + hpPerLevel*float64(levelAtLeastOne(level)-1)) * hpMod))
}

// armor is rule 5.1.2.
func armor(level uint32) float64 {
	return armorBase + armorPerLevel*float64(levelAtLeastOne(level)-1)
}

// attackPower is rule 5.1.3.
func attackPower(level uint32) float64 {
	return attackPowerBase + attackPowerPerLevel*float64(levelAtLeastOne(level)-1)
}

// Damage is rule 5.5, computed in float64 and rounded exactly once.
//
// Rounding an intermediate is a defect the rule calls out by name: it makes
// the worked example in section 6.1 unreproducible.
func Damage(ability pack.Ability, casterLevel, targetLevel uint32) int32 {
	var raw float64
	for _, effect := range ability.Effects {
		if effect.Kind != damageEffectKind {
			continue
		}
		raw += effect.Amount + effect.AttackPowerCoeff*attackPower(casterLevel)
	}
	if raw <= 0 {
		return 0
	}
	softcap := armorSoftcapBase + armorSoftcapPerLevel*float64(levelAtLeastOne(casterLevel))
	targetArmor := armor(targetLevel)
	mitigated := raw * (1 - targetArmor/(targetArmor+softcap))
	damage := int32(roundHalfUp(mitigated))
	if damage < 1 {
		return 1
	}
	return damage
}

// ticksIn converts a duration into whole ticks, rounding up.
//
// The tolerance is not a fudge factor, it is the tick rate not being
// representable. 30 Hz is 33.333... ms and time.Duration truncates it to
// 33333333 ns, so one second is thirty ticks plus three nanoseconds. Without
// the tolerance every duration that is a whole number of intended ticks would
// cost one extra, and the thirty-tick global cooldown of mechanics/combat.md
// rule 5.4.2 would be thirty-one.
func ticksIn(duration, interval time.Duration) uint64 {
	if duration <= 0 || interval <= 0 {
		return 0
	}
	tolerance := interval / 1000
	if tolerance < 1 {
		tolerance = 1
	}
	if duration <= tolerance {
		return 1
	}
	return uint64((duration - tolerance + interval - 1) / interval)
}

func roundHalfUp(value float64) float64 { return math.Floor(value + 0.5) }

// levelAtLeastOne guards the level-minus-one terms of rules 5.1.1 to 5.1.3
// against an unset level, which would otherwise underflow to a huge uint32.
func levelAtLeastOne(level uint32) uint32 {
	if level < 1 {
		return 1
	}
	return level
}
