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
	var (
		event    Event
		outcome  error
		rejected Rejection
	)
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		caster := tick.Entity(casterID)
		state := module.casters[casterID]
		if caster == nil || state == nil {
			return gametypes.ErrUnknownEntity
		}
		if request.Seq != 0 && state.hasSeq && request.Seq <= state.lastSeq {
			return ErrDuplicateCommand
		}

		abilityID := request.AbilityID
		if abilityID == "" {
			abilityID = state.defaultAbility()
		}
		ability, ok := module.rules.Ability(abilityID)
		if !ok || !state.knows(abilityID) {
			rejected = RejectionUnknownAbility
		} else {
			rejected = module.validate(tick, caster, ability, request.TargetID)
		}

		if rejected != RejectionNone {
			event = Event{
				Kind:       EventKindAbility,
				ServerTick: tick.Number(),
				ZoneID:     tick.ZoneID(),
				CasterID:   casterID,
				TargetID:   request.TargetID,
				AbilityID:  abilityID,
				Rejection:  rejected,
				PrivateTo:  casterID,
			}
			outcome = rejected.err()
			module.publish(event)
			return nil
		}

		damage := Damage(ability, caster.Level, tick.Entity(request.TargetID).Level)
		if module.damageEffects != nil {
			damage, outcome = module.damageEffects.ScaleDamage(tick, DamageEffectRequest{
				Magnitude: damage,
				CasterID:  caster.ID,
				TargetID:  request.TargetID,
				AbilityID: ability.ID,
			})
			if outcome != nil {
				return nil
			}
		}
		state.hasSeq, state.lastSeq = true, request.Seq

		// Rule 5.4.3: the cooldown is consumed before damage resolves, so an
		// ability that kills its target still costs the caster its turn.
		state.gcdReadyTick = tick.Number() + module.gcdTicks(tick)
		if ability.Cooldown > 0 {
			if state.readyTick == nil {
				state.readyTick = make(map[string]uint64)
			}
			state.readyTick[ability.ID] = tick.Number() + ticksIn(ability.Cooldown, tick.Interval())
		}
		// applyDamage publishes: the ability event has to reach the client
		// before the death it caused, and only it knows the order.
		event = module.applyDamage(tick, caster, tick.Entity(request.TargetID), ability, damage)
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return event, outcome
}

// validate runs rules 5.2 to 5.4 in the order the spec states them, because
// the order decides which reason a use that breaks two rules comes back with.
func (module *Module) validate(
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
	// Rule 5.4.1, plus the per-ability cooldown the pack may carry.
	state := module.casters[caster.ID]
	if ability.TriggersGCD && tick.Number() < state.gcdReadyTick {
		return RejectionOnCooldown
	}
	if ready, ok := state.readyTick[ability.ID]; ok && tick.Number() < ready {
		return RejectionOnCooldown
	}
	return RejectionNone
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
) Event {
	// Rule 5.5 computes the damage and rule 5.6.1 clamps the health, in that
	// order. The event reports what the ability did, not what was left to
	// absorb it, so an overkill reads as an overkill.
	target.Health = max(0, target.Health-damage)

	if state, ok := module.mobs[target.ID]; ok {
		state.addThreat(caster.ID, int64(damage))
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
