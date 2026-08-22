package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
)

// WarriorAction is the runtime projection of one extracted action document.
// The private pack adapter owns source parsing; this value has only product
// ids, typed script nodes, exact scales, and fixed-point resource cost.
type WarriorAction struct {
	AbilityID                   string
	ActionGroupID               string
	PrepareDuration             time.Duration
	Cooldown                    time.Duration
	CooldownGroupID             string
	CooldownScalesByWeaponSpeed bool
	CooldownSource              string
	TriggersGlobalCooldown      bool
	IgnoresGlobalCooldown       bool
	ResourceKind                string
	ResourceCost                script.Decimal
	ScaleCostByWeaponSpeed      bool
	ResourceSource              string
	CasterConditions            []*script.Node
	CasterImpacts               []*script.Node
	TargetImpacts               []*script.Node
}

// WarriorCombatant is the live character/equipment projection used by
// scalers and equipment predicates. Action rows never own these values.
type WarriorCombatant struct {
	PhysicalScale       script.Decimal
	PhysicalRangedScale script.Decimal
	WeaponSpeedScale    map[string]script.Decimal
	EquippedDressTypes  map[string]bool
}

// WarriorActionSource resolves extracted actions. Production uses the private
// pack adapter and tests use an in-memory source.
type WarriorActionSource interface {
	WarriorAction(abilityID string) (WarriorAction, bool)
}

type activeWarriorAction struct {
	invocation combat.ScriptActionInvocation
	action     WarriorAction
	event      combat.Event
	damaged    bool
	mutated    bool
}

// ValidateAction runs the extracted caster conditions before combat spends a
// resource or starts a timer. The supported League-start slice is deliberately
// atomic: one target impact, with no deferred or multi-command subtree.
func (driver *ScriptDriver) ValidateAction(
	tick gametypes.Tick,
	invocation combat.ScriptActionInvocation,
) (combat.Rejection, error) {
	source, ok := driver.source.(WarriorActionSource)
	if !ok {
		return combat.RejectionUnknownAbility, nil
	}
	action, ok := source.WarriorAction(invocation.AbilityID)
	actor, actorOK := driver.warriorCombatants[invocation.CasterID]
	if !ok || !actorOK || warriorActionDigest(action, actor) != invocation.DefinitionDigest {
		return combat.RejectionUnknownAbility, nil
	}
	if err := validateWarriorActionShape(action); err != nil {
		return combat.RejectionNone, err
	}
	previousTick := driver.tick
	driver.tick = tick
	defer func() { driver.tick = previousTick }()
	frame := warriorActionFrame(tick, invocation)
	frame.Addressee = frame.CasterID
	for _, condition := range action.CasterConditions {
		allowed, err := driver.evaluator.Predicate(context.Background(), condition, frame)
		if err != nil {
			return combat.RejectionNone, fmt.Errorf("session: validate Warrior action %s condition %s: %w", invocation.AbilityID, condition.Key, err)
		}
		if !allowed {
			return combat.RejectionInvalidTarget, nil
		}
	}
	return combat.RejectionNone, nil
}

func validateWarriorActionShape(action WarriorAction) error {
	if len(action.CasterImpacts) != 0 {
		return fmt.Errorf("session: Warrior action %q has unsupported caster impacts", action.AbilityID)
	}
	if len(action.TargetImpacts) != 1 || action.TargetImpacts[0] == nil {
		return fmt.Errorf("session: Warrior action %q must have exactly one atomic target impact", action.AbilityID)
	}
	switch action.TargetImpacts[0].Opcode {
	case "ScaledPhysicalWeaponDamage", "ScaledPhysicalDamage", "ImpactSetTarget":
		return nil
	default:
		return fmt.Errorf("session: Warrior action %q target impact %q is outside the atomic caller", action.AbilityID, action.TargetImpacts[0].Opcode)
	}
}

// Profile implements combat.ScriptActionHost without exposing interpreter
// nodes to combat.
func (driver *ScriptDriver) Profile(casterID uint64, abilityID string) (combat.ScriptActionProfile, bool) {
	if driver == nil || casterID == 0 {
		return combat.ScriptActionProfile{}, false
	}
	source, ok := driver.source.(WarriorActionSource)
	if !ok {
		return combat.ScriptActionProfile{}, false
	}
	action, ok := source.WarriorAction(abilityID)
	if !ok {
		return combat.ScriptActionProfile{}, false
	}
	actor, ok := driver.warriorCombatants[casterID]
	if !ok {
		return combat.ScriptActionProfile{}, false
	}
	cost, err := actionResourceMilli(action, actor)
	if err != nil {
		cost = -1
	}
	cooldown, cooldownErr := actionCooldown(action, actor)
	if cooldownErr != nil {
		cooldown = -1
	}
	return combat.ScriptActionProfile{
		AbilityID: abilityID, ActionGroupID: action.ActionGroupID,
		ResourceKind: action.ResourceKind, ResourceCostMilli: cost,
		PrepareDuration: action.PrepareDuration, Cooldown: cooldown,
		CooldownGroupID:        action.CooldownGroupID,
		TriggersGlobalCooldown: action.TriggersGlobalCooldown,
		IgnoresGlobalCooldown:  action.IgnoresGlobalCooldown,
		DefinitionDigest:       warriorActionDigest(action, actor),
	}, true
}

func warriorActionDigest(action WarriorAction, actor WarriorCombatant) string {
	payload, err := json.Marshal(struct {
		Action WarriorAction
		Actor  WarriorCombatant
	}{Action: action, Actor: actor})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func actionResourceMilli(action WarriorAction, actor WarriorCombatant) (int64, error) {
	mantissa := action.ResourceCost.Mantissa
	scale := action.ResourceCost.Scale
	if mantissa < 0 || scale < 0 || scale > 9 {
		return 0, fmt.Errorf("invalid action resource cost %dE-%d", mantissa, scale)
	}
	if action.ScaleCostByWeaponSpeed {
		other, ok := actor.WeaponSpeedScale[action.ResourceSource]
		if !ok {
			return 0, fmt.Errorf("action resource source %q has no equipped weapon speed", action.ResourceSource)
		}
		if other.Mantissa <= 0 || other.Scale < 0 || other.Scale > 9 ||
			other.Mantissa != 0 && (mantissa > math.MaxInt64/other.Mantissa) {
			return 0, fmt.Errorf("action resource cost times weapon speed is not representable")
		}
		mantissa *= other.Mantissa
		scale += other.Scale
	}
	for scale > 3 && mantissa%10 == 0 {
		mantissa /= 10
		scale--
	}
	if scale > 3 {
		return 0, fmt.Errorf("action resource cost is not exact to thousandths")
	}
	for scale < 3 {
		if mantissa > math.MaxInt64/10 {
			return 0, fmt.Errorf("action resource cost exceeds int64 thousandths")
		}
		mantissa *= 10
		scale++
	}
	return mantissa, nil
}

func actionCooldown(action WarriorAction, actor WarriorCombatant) (time.Duration, error) {
	if !action.CooldownScalesByWeaponSpeed {
		return action.Cooldown, nil
	}
	speed, ok := actor.WeaponSpeedScale[action.CooldownSource]
	if !ok {
		return 0, fmt.Errorf("action cooldown source %q has no equipped weapon speed", action.CooldownSource)
	}
	if action.Cooldown < 0 || speed.Mantissa <= 0 || speed.Scale < 0 || speed.Scale > 9 {
		return 0, fmt.Errorf("action cooldown or weapon speed is invalid")
	}
	nanoseconds := action.Cooldown.Nanoseconds()
	if speed.Mantissa != 0 && nanoseconds > math.MaxInt64/speed.Mantissa {
		return 0, fmt.Errorf("scaled action cooldown exceeds time.Duration")
	}
	nanoseconds *= speed.Mantissa
	for range speed.Scale {
		nanoseconds /= 10
	}
	return time.Duration(nanoseconds), nil
}

// SetWarriorCombatant installs one live character/equipment projection. The
// caller invokes it alongside combat admission and removes it on release.
func (driver *ScriptDriver) SetWarriorCombatant(entityID uint64, actor WarriorCombatant) error {
	if driver == nil || driver.zone == nil {
		return fmt.Errorf("session: Warrior combatant has no script driver")
	}
	return driver.zone.GameCommand(func(tick gametypes.Tick) error {
		if tick.Entity(entityID) == nil {
			return gametypes.ErrUnknownEntity
		}
		driver.warriorCombatants[entityID] = cloneWarriorCombatant(actor)
		return nil
	})
}

func (driver *ScriptDriver) ReleaseWarriorCombatant(entityID uint64) {
	if driver == nil || driver.zone == nil {
		return
	}
	_ = driver.zone.GameCommand(func(gametypes.Tick) error {
		delete(driver.warriorCombatants, entityID)
		return nil
	})
}

func cloneWarriorCombatant(actor WarriorCombatant) WarriorCombatant {
	result := actor
	result.WeaponSpeedScale = make(map[string]script.Decimal, len(actor.WeaponSpeedScale))
	for source, value := range actor.WeaponSpeedScale {
		result.WeaponSpeedScale[source] = value
	}
	result.EquippedDressTypes = make(map[string]bool, len(actor.EquippedDressTypes))
	for dressType, equipped := range actor.EquippedDressTypes {
		result.EquippedDressTypes[dressType] = equipped
	}
	return result
}

// ExecuteAction implements combat.ScriptActionHost. Combat has already
// validated the target, range, cooldown, sequence, and resource before this
// method evaluates the ordered target impacts.
func (driver *ScriptDriver) ExecuteAction(
	tick gametypes.Tick,
	invocation combat.ScriptActionInvocation,
) (combat.Event, error) {
	if driver == nil || tick == nil {
		return combat.Event{}, fmt.Errorf("session: Warrior action has no driver or active tick")
	}
	source, ok := driver.source.(WarriorActionSource)
	if !ok {
		return combat.Event{}, fmt.Errorf("session: script source carries no Warrior actions")
	}
	action, ok := source.WarriorAction(invocation.AbilityID)
	actor, actorOK := driver.warriorCombatants[invocation.CasterID]
	if !ok || !actorOK || action.AbilityID != invocation.AbilityID ||
		action.ActionGroupID != invocation.ActionGroupID ||
		warriorActionDigest(action, actor) != invocation.DefinitionDigest {
		return combat.Event{}, fmt.Errorf("session: Warrior action %q is absent or changed after admission", invocation.AbilityID)
	}
	if len(action.TargetImpacts) == 0 {
		return combat.Event{}, fmt.Errorf("session: Warrior action %q has no target impacts", invocation.AbilityID)
	}

	previousTick := driver.tick
	previousAction := driver.activeWarriorAction
	driver.tick = tick
	driver.activeWarriorAction = &activeWarriorAction{invocation: invocation, action: action}
	defer func() {
		driver.activeWarriorAction = previousAction
		driver.tick = previousTick
	}()

	driver.evaluations++
	frame := warriorActionFrame(tick, invocation)
	if err := driver.evaluator.EvaluateAll(context.Background(), action.TargetImpacts, frame); err != nil {
		return driver.activeWarriorAction.event, fmt.Errorf("session: execute Warrior action %s: %w", invocation.AbilityID, err)
	}
	if !driver.activeWarriorAction.damaged {
		return driver.combat.CompleteScriptAction(tick, invocation)
	}
	return driver.activeWarriorAction.event, nil
}

func warriorActionFrame(tick gametypes.Tick, invocation combat.ScriptActionInvocation) script.Frame {
	return script.Frame{
		EvaluationID: fmt.Sprintf("action|%d|%s|%d", invocation.CasterID, invocation.AbilityID, invocation.ActivationOrdinal),
		Event:        "ActionActivated", ZoneID: tick.ZoneID(), SourceID: invocation.AbilityID,
		CasterID: formatEntityID(invocation.CasterID), TargetID: formatEntityID(invocation.TargetID),
		Addressee: formatEntityID(invocation.TargetID),
	}
}

func (driver *ScriptDriver) activeScale(query script.Query) (script.Value, error) {
	active := driver.activeWarriorAction
	if active == nil || query.EntityID != formatEntityID(active.invocation.CasterID) {
		return script.Value{}, fmt.Errorf("session: action scale query has no matching active caster")
	}
	actor, ok := driver.warriorCombatants[active.invocation.CasterID]
	if !ok {
		return script.Value{}, fmt.Errorf("session: Warrior caster %d has no live combat stats", active.invocation.CasterID)
	}
	var value script.Decimal
	switch query.Kind {
	case script.QueryPhysicalScale:
		if query.Slot != "" && !strings.EqualFold(query.Slot, "Mainhand") {
			return script.Value{}, fmt.Errorf("session: physical scale does not support slot %q", query.Slot)
		}
		value = actor.PhysicalScale
	case script.QueryPhysicalRangedScale:
		if query.Slot != "" && !strings.EqualFold(query.Slot, "Ranged") {
			return script.Value{}, fmt.Errorf("session: ranged scale does not support slot %q", query.Slot)
		}
		value = actor.PhysicalRangedScale
	case script.QueryWeaponSpeedScale:
		value, ok = decimalByFold(actor.WeaponSpeedScale, query.Slot)
		if !ok {
			return script.Value{}, fmt.Errorf("session: Warrior caster %d has no %q weapon speed", active.invocation.CasterID, query.Slot)
		}
	default:
		return script.Value{}, fmt.Errorf("session: query %d is not an action scale", query.Kind)
	}
	if value.Mantissa == 0 {
		return script.Value{}, fmt.Errorf("session: Warrior action %q carries a zero scale", active.action.AbilityID)
	}
	return script.Value{Kind: script.ValueDecimal, Mantissa: value.Mantissa, Scale: value.Scale}, nil
}

func boolByFold(values map[string]bool, key string) bool {
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}
	return false
}

func decimalByFold(values map[string]script.Decimal, key string) (script.Decimal, bool) {
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return script.Decimal{}, false
}

var _ combat.ScriptActionHost = (*ScriptDriver)(nil)
