package session

import (
	"context"
	"fmt"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
)

// WarriorAction is the runtime projection of one extracted action document.
// The private pack adapter owns source parsing; this value has only product
// ids, typed script nodes, exact scales, and fixed-point resource cost.
type WarriorAction struct {
	AbilityID           string
	ActionGroupID       string
	ResourceKind        string
	ResourceCostMilli   int64
	PhysicalScale       script.Decimal
	PhysicalRangedScale script.Decimal
	WeaponSpeedScale    script.Decimal
	TargetImpacts       []*script.Node
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
	return combat.ScriptActionProfile{
		AbilityID: abilityID, ActionGroupID: action.ActionGroupID,
		ResourceKind: action.ResourceKind, ResourceCostMilli: action.ResourceCostMilli,
	}, true
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
	if !ok || action.AbilityID != invocation.AbilityID || action.ActionGroupID != invocation.ActionGroupID {
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
	frame := script.Frame{
		EvaluationID: fmt.Sprintf("action|%d|%s|%d", invocation.CasterID, invocation.AbilityID, invocation.Sequence),
		Event:        "ActionActivated",
		ZoneID:       tick.ZoneID(),
		SourceID:     invocation.AbilityID,
		CasterID:     formatEntityID(invocation.CasterID),
		TargetID:     formatEntityID(invocation.TargetID),
		Addressee:    formatEntityID(invocation.TargetID),
	}
	for _, node := range action.TargetImpacts {
		if err := driver.evaluator.Evaluate(context.Background(), node, frame); err != nil {
			if driver.activeWarriorAction.mutated && driver.activeWarriorAction.event.Kind == combat.EventKindUnspecified {
				driver.activeWarriorAction.event, _ = driver.combat.CompleteScriptAction(tick, invocation)
			}
			return driver.activeWarriorAction.event, fmt.Errorf("session: execute Warrior action %s node %s: %w", invocation.AbilityID, node.Key, err)
		}
	}
	if !driver.activeWarriorAction.damaged {
		return driver.combat.CompleteScriptAction(tick, invocation)
	}
	return driver.activeWarriorAction.event, nil
}

func (driver *ScriptDriver) activeScale(query script.Query) (script.Value, error) {
	active := driver.activeWarriorAction
	if active == nil || query.EntityID != formatEntityID(active.invocation.CasterID) {
		return script.Value{}, fmt.Errorf("session: action scale query has no matching active caster")
	}
	var value script.Decimal
	switch query.Kind {
	case script.QueryPhysicalScale:
		if query.Slot != "" && query.Slot != "Mainhand" {
			return script.Value{}, fmt.Errorf("session: physical scale does not support slot %q", query.Slot)
		}
		value = active.action.PhysicalScale
	case script.QueryPhysicalRangedScale:
		if query.Slot != "" && query.Slot != "Ranged" {
			return script.Value{}, fmt.Errorf("session: ranged scale does not support slot %q", query.Slot)
		}
		value = active.action.PhysicalRangedScale
	case script.QueryWeaponSpeedScale:
		value = active.action.WeaponSpeedScale
	default:
		return script.Value{}, fmt.Errorf("session: query %d is not an action scale", query.Kind)
	}
	if value.Mantissa == 0 {
		return script.Value{}, fmt.Errorf("session: Warrior action %q carries a zero scale", active.action.AbilityID)
	}
	return script.Value{Kind: script.ValueDecimal, Mantissa: value.Mantissa, Scale: value.Scale}, nil
}

var _ combat.ScriptActionHost = (*ScriptDriver)(nil)
