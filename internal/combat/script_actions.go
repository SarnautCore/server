package combat

import (
	"errors"
	"fmt"
	"math"

	"github.com/SarnautCore/server/internal/gametypes"
)

const actionResourceScale int64 = 1000

// ActionResource is one server-owned character resource. Values use thousandths
// so extracted decimals such as 12.5 stay exact without floating-point state.
type ActionResource struct {
	Kind         string
	CurrentMilli int64
	MaximumMilli int64
}

// ActionLoadout is the complete combat admission state authored for one
// character.
type ActionLoadout struct {
	Bindings []ActionBinding
	Resource ActionResource
}

// ScriptActionProfile is the combat projection of an extracted action. The
// interpreter tree and character-stat scalers stay hidden in its adapter.
type ScriptActionProfile struct {
	AbilityID         string
	ActionGroupID     string
	ResourceKind      string
	ResourceCostMilli int64
}

// ScriptActionInvocation is one action combat has admitted and paid for.
type ScriptActionInvocation struct {
	CasterID      uint64
	TargetID      uint64
	AbilityID     string
	ActionGroupID string
	Sequence      uint64
}

// ScriptActionHost executes extracted action trees. It runs under the zone
// lock and must not retain the tick or block.
type ScriptActionHost interface {
	Profile(casterID uint64, abilityID string) (ScriptActionProfile, bool)
	ExecuteAction(gametypes.Tick, ScriptActionInvocation) (Event, error)
}

// ScriptDamageRequest is a typed, already-evaluated damage command. No script
// opcode or node crosses into combat.
type ScriptDamageRequest struct {
	CasterID         uint64
	TargetID         uint64
	AbilityID        string
	ActionGroupID    string
	Damage           int32
	ThreatMultiplier float64
	CanBeAvoided     bool
}

type actionResource struct {
	kind         string
	currentMilli int64
	maximumMilli int64
}

func validateActionResource(resource ActionResource) (actionResource, error) {
	if resource.Kind == "" {
		if resource.CurrentMilli != 0 || resource.MaximumMilli != 0 {
			return actionResource{}, errors.New("combat action resource values require a kind")
		}
		return actionResource{}, nil
	}
	if resource.MaximumMilli <= 0 || resource.CurrentMilli < 0 || resource.CurrentMilli > resource.MaximumMilli {
		return actionResource{}, fmt.Errorf(
			"combat action resource %q has invalid state %d/%d",
			resource.Kind, resource.CurrentMilli, resource.MaximumMilli,
		)
	}
	return actionResource{
		kind: resource.Kind, currentMilli: resource.CurrentMilli, maximumMilli: resource.MaximumMilli,
	}, nil
}

// SetScriptActionHost installs the extracted-action adapter.
func (module *Module) SetScriptActionHost(host ScriptActionHost) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		module.scriptActions = host
		return nil
	})
}

// ActionResourceState returns one authoritative resource snapshot.
func (module *Module) ActionResourceState(entityID uint64) (ActionResource, error) {
	var result ActionResource
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		if tick.Entity(entityID) == nil || module.casters[entityID] == nil {
			return gametypes.ErrUnknownEntity
		}
		resource := module.casters[entityID].resource
		result = ActionResource{
			Kind: resource.kind, CurrentMilli: resource.currentMilli, MaximumMilli: resource.maximumMilli,
		}
		return nil
	})
	return result, err
}

func (module *Module) actionProfile(casterID uint64, abilityID string) (ScriptActionProfile, bool, error) {
	if module.scriptActions == nil {
		return ScriptActionProfile{}, false, nil
	}
	profile, ok := module.scriptActions.Profile(casterID, abilityID)
	if !ok {
		return ScriptActionProfile{}, false, nil
	}
	if profile.AbilityID != abilityID || profile.ActionGroupID == "" || profile.ResourceCostMilli < 0 {
		return ScriptActionProfile{}, false, fmt.Errorf("combat: malformed script action profile for %q", abilityID)
	}
	if profile.ResourceCostMilli > 0 && profile.ResourceKind == "" {
		return ScriptActionProfile{}, false, fmt.Errorf("combat: action %q has a cost without a resource kind", abilityID)
	}
	return profile, true, nil
}

func validateActionResourceCost(state *casterState, profile ScriptActionProfile) Rejection {
	if profile.ResourceCostMilli == 0 {
		return RejectionNone
	}
	if state.resource.kind != profile.ResourceKind || state.resource.currentMilli < profile.ResourceCostMilli {
		return RejectionNoResource
	}
	return RejectionNone
}

func consumeActionResource(state *casterState, profile ScriptActionProfile) {
	state.resource.currentMilli -= profile.ResourceCostMilli
}

// ApplyScriptDamage commits one evaluator-produced damage command through the
// same health, threat, death, kill-sink, and event path as ordinary combat.
func (module *Module) ApplyScriptDamage(
	tick gametypes.Tick,
	request ScriptDamageRequest,
) (Event, error) {
	if tick == nil {
		return Event{}, errors.New("combat: script damage has no active tick")
	}
	caster := tick.Entity(request.CasterID)
	target := tick.Entity(request.TargetID)
	ability, ok := module.rules.Ability(request.AbilityID)
	if caster == nil || module.casters[request.CasterID] == nil {
		return Event{}, gametypes.ErrUnknownEntity
	}
	if target == nil || !ok || request.ActionGroupID == "" {
		return Event{}, fmt.Errorf("combat: script damage names an unknown target, ability, or action group")
	}
	if request.Damage < 0 || math.IsNaN(request.ThreatMultiplier) ||
		math.IsInf(request.ThreatMultiplier, 0) || request.ThreatMultiplier < 0 {
		return Event{}, fmt.Errorf("combat: script damage for %q has invalid magnitude or threat", request.AbilityID)
	}
	if !target.Alive {
		return Event{}, ErrTargetDead
	}
	damage := request.Damage
	if module.damageEffects != nil {
		var err error
		damage, err = module.damageEffects.ScaleDamage(tick, DamageEffectRequest{
			Magnitude: damage, CasterID: caster.ID, TargetID: target.ID,
			AbilityID: ability.ID, ActionGroupID: request.ActionGroupID,
		})
		if err != nil {
			return Event{}, err
		}
	}
	return module.applyDamage(
		tick, caster, target, ability, damage, request.ActionGroupID, request.ThreatMultiplier,
	), nil
}

// CompleteScriptAction publishes a successfully evaluated action that dealt
// no direct damage, such as an extracted target-control action.
func (module *Module) CompleteScriptAction(
	tick gametypes.Tick,
	invocation ScriptActionInvocation,
) (Event, error) {
	if tick == nil || tick.Entity(invocation.CasterID) == nil || tick.Entity(invocation.TargetID) == nil {
		return Event{}, gametypes.ErrUnknownEntity
	}
	event := Event{
		Kind: EventKindAbility, ServerTick: tick.Number(), ZoneID: tick.ZoneID(),
		CasterID: invocation.CasterID, TargetID: invocation.TargetID,
		AbilityID: invocation.AbilityID, ActionGroupID: invocation.ActionGroupID,
	}
	module.publish(event)
	return event, nil
}

// SetScriptTarget applies ImpactSetTarget to a live combat actor.
func (module *Module) SetScriptTarget(tick gametypes.Tick, actorID, targetID uint64) error {
	if tick == nil {
		return errors.New("combat: script target change has no active tick")
	}
	actor := tick.Entity(actorID)
	target := tick.Entity(targetID)
	if actor == nil || target == nil || !actor.Alive || actorID == targetID {
		return ErrInvalidTarget
	}
	if rejection := module.validateSelection(tick, targetID); rejection != RejectionNone {
		return rejection
	}
	if state := module.casters[actorID]; state != nil {
		state.selected = targetID
		return nil
	}
	if state := module.mobs[actorID]; state != nil {
		state.phase = phaseAggro
		state.aggroTarget = targetID
		return nil
	}
	return gametypes.ErrUnknownEntity
}

// MilliResource converts an extracted decimal resource amount to the combat
// storage scale. It rejects values that cannot be represented exactly.
func MilliResource(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("combat resource amount is not finite and non-negative")
	}
	scaled := value * float64(actionResourceScale)
	if scaled > math.MaxInt64 || math.Round(scaled) != scaled {
		return 0, errors.New("combat resource amount is not representable in thousandths")
	}
	return int64(scaled), nil
}
