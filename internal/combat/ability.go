package combat

import "github.com/SarnautCore/server/internal/gametypes"

// AbilityRequest is one client ability use, already decoded.
//
// The caster is not in it: the server derives the actor from the session
// (protocol/session.md rule 5.2.6), so a client that names someone else is
// simply ignored rather than refused.
type AbilityRequest struct {
	// Seq is the client's command sequence number, used only to discard a
	// retransmit. Zero means the client is not sequencing, and nothing is
	// deduplicated.
	Seq uint64
	// TargetID is the entity to resolve against.
	TargetID uint64
	// AbilityID selects the ability. Empty picks the caster's first.
	AbilityID string
}

// UseAbility validates one ability use, applies it, and publishes what
// happened.
//
// It has the same three parts, in the same order, as the movement entry point:
// validate the request, discard anything that is not newer than the last one
// accepted, then mutate under the zone lock. A refusal returns a Rejection as
// its error and changes nothing at all — no damage, no threat, and no
// cooldown, which is what mechanics/combat.md section 6.2 requires.
func (module *Module) UseAbility(casterID uint64, request AbilityRequest) (Event, error) {
	var event Event
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		var outcome error
		event, outcome = module.useAbility(tick, casterID, request)
		return outcome
	})
	return event, err
}

// useAbility resolves an already server-owned request while the zone lock is
// held. Keeping this apart from GameCommand lets ActivateSlot resolve its slot
// and target in the same critical section as the existing validation and use.
func (module *Module) useAbility(
	tick gametypes.Tick,
	casterID uint64,
	request AbilityRequest,
) (Event, error) {
	caster := tick.Entity(casterID)
	state := module.casters[casterID]
	if caster == nil || state == nil {
		return Event{}, gametypes.ErrUnknownEntity
	}
	if request.Seq != 0 && state.hasSeq && request.Seq <= state.lastSeq {
		return Event{}, ErrDuplicateCommand
	}
	var err error

	abilityID := request.AbilityID
	if abilityID == "" {
		abilityID = state.defaultAbility()
	}
	ability, ok := module.rules.Ability(abilityID)
	var profile ScriptActionProfile
	var scripted bool
	rejected := RejectionNone
	if !ok || !state.knows(abilityID) {
		rejected = RejectionUnknownAbility
	} else {
		profile, scripted, err = module.actionProfile(casterID, ability.ID)
		if err != nil {
			return Event{}, err
		}
		if module.rules.IsNativeAction(ability.ID) && !scripted {
			rejected = RejectionUnknownAbility
		} else if scripted {
			rejected = module.validate(tick, caster, ability, request.TargetID, &profile)
		} else {
			rejected = module.validate(tick, caster, ability, request.TargetID, nil)
		}
	}

	if rejected != RejectionNone {
		event := Event{
			Kind:       EventKindAbility,
			ServerTick: tick.Number(),
			ZoneID:     tick.ZoneID(),
			CasterID:   casterID,
			TargetID:   request.TargetID,
			AbilityID:  abilityID,
			Rejection:  rejected,
			PrivateTo:  casterID,
		}
		module.publish(event)
		return event, rejected.err()
	}

	activationOrdinal := state.actionOrdinal + 1
	invocation := ScriptActionInvocation{
		CasterID: casterID, TargetID: request.TargetID, AbilityID: ability.ID,
		ActionGroupID: profile.ActionGroupID, Sequence: request.Seq,
		ActivationOrdinal: activationOrdinal,
		DefinitionDigest:  profile.DefinitionDigest,
	}
	if scripted {
		rejected, err = module.scriptActions.ValidateAction(tick, invocation)
		if err != nil {
			return Event{}, err
		}
		if rejected != RejectionNone {
			event := module.rejectAbility(tick, invocation, rejected)
			return event, rejected.err()
		}
		rejected = validateActionResourceCost(state, profile)
		if rejected != RejectionNone {
			event := module.rejectAbility(tick, invocation, rejected)
			return event, rejected.err()
		}
	}

	var damage int32
	if !scripted {
		damage = Damage(ability, caster.Level, tick.Entity(request.TargetID).Level)
		if module.damageEffects != nil {
			damage, err = module.damageEffects.ScaleDamage(tick, DamageEffectRequest{
				Magnitude: damage,
				CasterID:  caster.ID,
				TargetID:  request.TargetID,
				AbilityID: ability.ID,
			})
			if err != nil {
				return Event{}, err
			}
		}
	}

	previousHasSeq, previousLastSeq := state.hasSeq, state.lastSeq
	state.hasSeq, state.lastSeq = true, request.Seq
	previousGCD := state.gcdReadyTick
	previousCast := state.castReadyTick
	previousOrdinal := state.actionOrdinal
	previousResource := state.resource.currentMilli
	readyKey := module.actionCooldownKey(ability.ID, profile, scripted)
	previousReady, hadPreviousReady := state.readyTick[readyKey]

	// Rule 5.4.3: the cooldown is consumed before damage resolves, so an
	// ability that kills its target still costs the caster its turn.
	if (!scripted && ability.TriggersGCD) || scripted && profile.TriggersGlobalCooldown {
		state.gcdReadyTick = tick.Number() + module.gcdTicks(tick)
	}
	cooldown := ability.Cooldown
	if scripted {
		cooldown = profile.Cooldown
	}
	if cooldown > 0 {
		if state.readyTick == nil {
			state.readyTick = make(map[string]uint64)
		}
		state.readyTick[readyKey] = tick.Number() + ticksIn(cooldown, tick.Interval())
	}
	if scripted {
		state.actionOrdinal = activationOrdinal
		consumeActionResource(state, profile)
		if profile.PrepareDuration > 0 {
			prepareTicks := ticksIn(profile.PrepareDuration, tick.Interval())
			state.castReadyTick = tick.Number() + prepareTicks
			startEvent := Event{
				Kind: EventKindAbility, ServerTick: tick.Number(), ZoneID: tick.ZoneID(),
				CasterID: casterID, TargetID: request.TargetID, AbilityID: ability.ID,
				ActionGroupID: profile.ActionGroupID,
			}
			module.publish(startEvent)
			tick.After(prepareTicks, func(later gametypes.Tick) {
				module.finishScriptAction(later, ability, profile, invocation)
			})
			return startEvent, nil
		}
		event, executeErr := module.scriptActions.ExecuteAction(tick, invocation)
		if executeErr != nil && event.Kind == EventKindUnspecified {
			state.hasSeq, state.lastSeq = previousHasSeq, previousLastSeq
			state.gcdReadyTick = previousGCD
			state.castReadyTick = previousCast
			state.actionOrdinal = previousOrdinal
			state.resource.currentMilli = previousResource
			if hadPreviousReady {
				state.readyTick[readyKey] = previousReady
			} else {
				delete(state.readyTick, readyKey)
			}
		}
		return event, executeErr
	}
	// applyDamage publishes: the ability event has to reach the client before
	// the death it caused, and only it knows the order.
	return module.applyDamage(tick, caster, tick.Entity(request.TargetID), ability, damage, "", 1), nil
}

// validate runs rules 5.2 to 5.4 in the order the spec states them, because
// the order decides which reason a use that breaks two rules comes back with.
func (module *Module) validate(
	tick gametypes.Tick,
	caster *gametypes.EntityData,
	ability gametypes.Ability,
	targetID uint64,
	profile *ScriptActionProfile,
) Rejection {
	if rejected := module.validateTarget(tick, caster, ability, targetID); rejected != RejectionNone {
		return rejected
	}
	state := module.casters[caster.ID]
	if tick.Number() < state.castReadyTick {
		return RejectionOnCooldown
	}
	triggersGCD := ability.TriggersGCD
	ignoresGCD := false
	readyKey := module.actionCooldownKey(ability.ID, ScriptActionProfile{}, false)
	if profile != nil {
		triggersGCD = profile.TriggersGlobalCooldown
		ignoresGCD = profile.IgnoresGlobalCooldown
		readyKey = module.actionCooldownKey(ability.ID, *profile, true)
	}
	if triggersGCD && !ignoresGCD && tick.Number() < state.gcdReadyTick {
		return RejectionOnCooldown
	}
	if ready, ok := state.readyTick[readyKey]; ok && tick.Number() < ready {
		return RejectionOnCooldown
	}
	return RejectionNone
}

func (module *Module) validateTarget(
	tick gametypes.Tick,
	caster *gametypes.EntityData,
	ability gametypes.Ability,
	targetID uint64,
) Rejection {
	if !caster.Alive {
		return RejectionInvalidTarget
	}
	// Rule 5.2.2.
	if targetID == 0 {
		return RejectionNoTarget
	}
	target := tick.Entity(targetID)
	if target == nil {
		return RejectionNoTarget
	}
	// Rule 5.2.3. The caster itself, an entity with no combatant state, and a
	// mob that is walking home and untargetable (rule 5.8.6) all land here.
	if target.ID == caster.ID || target.MaxHealth <= 0 || !target.Replicated {
		return RejectionInvalidTarget
	}
	if state, ok := module.mobs[target.ID]; ok && state.phase == phaseReturning {
		return RejectionInvalidTarget
	}
	// Rule 5.2.4.
	if !target.Alive {
		return RejectionTargetDead
	}
	// Rule 5.2.5.
	if !module.hostile(caster.Faction, target.Faction) {
		return RejectionInvalidTarget
	}
	// Rule 5.3.
	if gametypes.Distance(tick.Position(caster), tick.Position(target)) > ability.RangeM+rangeTolerance {
		return RejectionOutOfRange
	}
	return RejectionNone
}

func (module *Module) actionCooldownKey(
	abilityID string, profile ScriptActionProfile, scripted bool,
) string {
	if scripted && profile.CooldownGroupID != "" {
		return "group:" + profile.CooldownGroupID
	}
	return "ability:" + abilityID
}

func (module *Module) rejectAbility(
	tick gametypes.Tick, invocation ScriptActionInvocation, rejection Rejection,
) Event {
	event := Event{
		Kind: EventKindAbility, ServerTick: tick.Number(), ZoneID: tick.ZoneID(),
		CasterID: invocation.CasterID, TargetID: invocation.TargetID,
		AbilityID: invocation.AbilityID, ActionGroupID: invocation.ActionGroupID,
		Rejection: rejection, PrivateTo: invocation.CasterID,
	}
	module.publish(event)
	return event
}

func (module *Module) finishScriptAction(
	tick gametypes.Tick,
	ability gametypes.Ability,
	profile ScriptActionProfile,
	invocation ScriptActionInvocation,
) {
	caster := tick.Entity(invocation.CasterID)
	state := module.casters[invocation.CasterID]
	if caster == nil || state == nil || !caster.Alive {
		module.rejectAbility(tick, invocation, RejectionInvalidTarget)
		return
	}
	if rejected := module.validateTarget(tick, caster, ability, invocation.TargetID); rejected != RejectionNone {
		module.rejectAbility(tick, invocation, rejected)
		return
	}
	if current, ok, err := module.actionProfile(invocation.CasterID, invocation.AbilityID); err != nil || !ok || current != profile {
		module.rejectAbility(tick, invocation, RejectionUnknownAbility)
		return
	}
	if rejected, err := module.scriptActions.ValidateAction(tick, invocation); err != nil || rejected != RejectionNone {
		if rejected == RejectionNone {
			rejected = RejectionInvalidTarget
		}
		module.rejectAbility(tick, invocation, rejected)
		return
	}
	if event, err := module.scriptActions.ExecuteAction(tick, invocation); err != nil && event.Kind == EventKindUnspecified {
		module.rejectAbility(tick, invocation, RejectionInvalidTarget)
	}
}

// hostile answers rule 5.2.5: is the target's faction hostile to the caster's?
// The relation is directed, and the target's own attackable flag can veto it.
func (module *Module) hostile(casterFaction, targetFaction string) bool {
	if casterFaction == "" || targetFaction == "" {
		return false
	}
	target, ok := module.rules.factions[targetFaction]
	if !ok || !target.Attackable {
		return false
	}
	return target.StanceTowards(casterFaction) == gametypes.StanceHostile
}

// applyDamage is rules 5.5 and 5.6.
func (module *Module) applyDamage(
	tick gametypes.Tick,
	caster *gametypes.EntityData,
	target *gametypes.EntityData,
	ability gametypes.Ability,
	damage int32,
	actionGroupID string,
	threatMultiplier float64,
) Event {
	// Rule 5.5 computes the damage and rule 5.6.1 clamps the health, in that
	// order. The event reports what the ability did, not what was left to
	// absorb it, so an overkill reads as an overkill.
	target.Health = max(0, target.Health-damage)

	if state, ok := module.mobs[target.ID]; ok {
		state.addThreat(caster.ID, int64(roundHalfUp(float64(damage)*threatMultiplier)))
		// Rule 5.6.3: being hit always pulls, whatever the distance.
		if state.phase == phaseIdle {
			state.phase = phaseAggro
			state.aggroTarget = caster.ID
		}
	}

	event := Event{
		Kind:            EventKindAbility,
		ServerTick:      tick.Number(),
		ZoneID:          tick.ZoneID(),
		CasterID:        caster.ID,
		TargetID:        target.ID,
		AbilityID:       ability.ID,
		ActionGroupID:   actionGroupID,
		Damage:          damage,
		TargetHealth:    target.Health,
		TargetMaxHealth: target.MaxHealth,
		KillingBlow:     target.Health == 0,
	}
	module.publish(event)
	if target.Health == 0 {
		module.kill(tick, target, caster.ID)
	}
	return event
}

func (module *Module) gcdTicks(tick gametypes.Tick) uint64 {
	return ticksIn(globalCooldown, tick.Interval())
}
