package combat

import (
	"errors"
	"fmt"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
)

// ActionBarSlotCount is the authored 1.1 HUD capacity. Slot indices are
// always zero based, so the only valid range is 0 through 35.
const ActionBarSlotCount = 36

// ActionBinding assigns one pack ability to one authored action-bar slot.
type ActionBinding struct {
	SlotIndex uint32
	AbilityID string
}

// ActionUnavailableReason explains why a filled or empty slot cannot be used
// in the current authoritative state.
type ActionUnavailableReason uint8

const (
	ActionUnavailableUnspecified ActionUnavailableReason = iota
	ActionAvailable
	ActionUnavailableEmptySlot
	ActionUnavailableOnCooldown
	ActionUnavailableNoTarget
	ActionUnavailableInvalidTarget
	ActionUnavailableOutOfRange
	ActionUnavailableActorDead
	ActionUnavailableNoResource
	ActionUnavailableDisabled
)

// ActionSlotState is one ordered entry in a complete action-bar snapshot.
// Cooldown values use project milliseconds and never go below zero.
type ActionSlotState struct {
	SlotIndex           uint32
	AbilityID           string
	CooldownRemainingMS int64
	CooldownDurationMS  int64
	Available           bool
	UnavailableReason   ActionUnavailableReason
}

// ActionBar is a complete snapshot. The array type makes omitting an empty
// slot impossible and fixes the capacity at the authored 36 entries.
type ActionBar struct {
	Slots [ActionBarSlotCount]ActionSlotState
}

// ActionRejection is a refusal that happens before ability validation. These
// failures never consume a sequence number or a cooldown.
type ActionRejection uint8

const (
	ActionRejectionNone ActionRejection = iota
	ActionRejectionInvalidSlot
	ActionRejectionEmptySlot
)

var (
	ErrInvalidActionSlot   error = ActionRejectionInvalidSlot
	ErrEmptyActionSlot     error = ActionRejectionEmptySlot
	ErrDuplicateActionSlot       = errors.New("combat action slot is assigned more than once")
)

func (rejection ActionRejection) Error() string {
	switch rejection {
	case ActionRejectionNone:
		return "action resolved"
	case ActionRejectionInvalidSlot:
		return "action slot is outside 0 through 35"
	case ActionRejectionEmptySlot:
		return "action slot is empty"
	default:
		return "unknown action refusal"
	}
}

func (module *Module) validateActionBindings(
	bindings []ActionBinding,
) ([]string, [ActionBarSlotCount]string, error) {
	var actionBar [ActionBarSlotCount]string
	for _, binding := range bindings {
		if binding.SlotIndex >= ActionBarSlotCount {
			return nil, actionBar, fmt.Errorf(
				"bind ability %q to slot %d: %w",
				binding.AbilityID, binding.SlotIndex, ErrInvalidActionSlot,
			)
		}
		if actionBar[binding.SlotIndex] != "" {
			return nil, actionBar, fmt.Errorf(
				"bind slot %d: %w", binding.SlotIndex, ErrDuplicateActionSlot,
			)
		}
		if _, ok := module.rules.Ability(binding.AbilityID); !ok {
			return nil, actionBar, fmt.Errorf(
				"bind slot %d to %q: %w",
				binding.SlotIndex, binding.AbilityID, ErrUnknownAbility,
			)
		}
		actionBar[binding.SlotIndex] = binding.AbilityID
	}
	// Derive the known-ability order from slot order, not caller slice order.
	// The same authored assignments therefore produce the same default for the
	// legacy direct-use seam and the same state on every admission.
	abilities := make([]string, 0, len(bindings))
	known := make(map[string]struct{}, len(bindings))
	for _, abilityID := range actionBar {
		if abilityID == "" {
			continue
		}
		if _, duplicate := known[abilityID]; duplicate {
			continue
		}
		known[abilityID] = struct{}{}
		abilities = append(abilities, abilityID)
	}
	return abilities, actionBar, nil
}

// ActionBar returns all 36 slots in index order from one zone-locked view of
// the caster, target, and cooldown state.
func (module *Module) ActionBar(entityID uint64) (ActionBar, error) {
	var result ActionBar
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		caster := tick.Entity(entityID)
		state := module.casters[entityID]
		if caster == nil || state == nil {
			return gametypes.ErrUnknownEntity
		}
		selected := module.selectedTarget(tick, state)
		for index := range ActionBarSlotCount {
			result.Slots[index] = module.actionSlotState(
				tick, caster, state, selected, uint32(index),
			)
		}
		return nil
	})
	return result, err
}

func (module *Module) actionSlotState(
	tick gametypes.Tick,
	caster *gametypes.EntityData,
	state *casterState,
	selected uint64,
	index uint32,
) ActionSlotState {
	result := ActionSlotState{
		SlotIndex:         index,
		AbilityID:         state.actionBar[index],
		UnavailableReason: ActionUnavailableEmptySlot,
	}
	if result.AbilityID == "" {
		return result
	}
	ability, ok := module.rules.Ability(result.AbilityID)
	if !ok || !state.knows(result.AbilityID) {
		result.UnavailableReason = ActionUnavailableDisabled
		return result
	}
	result.CooldownRemainingMS, result.CooldownDurationMS = module.cooldownMilliseconds(
		tick, state, ability,
	)
	if !caster.Alive {
		result.UnavailableReason = ActionUnavailableActorDead
		return result
	}
	rejection := module.validate(tick, caster, ability, selected)
	if rejection == RejectionNone {
		profile, scripted, err := module.actionProfile(caster.ID, ability.ID)
		if err != nil {
			result.UnavailableReason = ActionUnavailableDisabled
			return result
		}
		if scripted {
			rejection = validateActionResourceCost(state, profile)
		}
	}
	result.UnavailableReason = actionUnavailableReason(rejection)
	result.Available = rejection == RejectionNone
	return result
}

func actionUnavailableReason(rejection Rejection) ActionUnavailableReason {
	switch rejection {
	case RejectionNone:
		return ActionAvailable
	case RejectionNoTarget:
		return ActionUnavailableNoTarget
	case RejectionInvalidTarget, RejectionTargetDead, RejectionUnknownAbility:
		return ActionUnavailableInvalidTarget
	case RejectionOutOfRange:
		return ActionUnavailableOutOfRange
	case RejectionOnCooldown:
		return ActionUnavailableOnCooldown
	case RejectionNoResource:
		return ActionUnavailableNoResource
	default:
		return ActionUnavailableDisabled
	}
}

// ActivateSlot resolves both the ability and target from server-owned caster
// state. The client supplies neither an ability id nor a target id.
func (module *Module) ActivateSlot(
	casterID uint64,
	slotIndex uint32,
	seq uint64,
) (Event, error) {
	var event Event
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		caster := tick.Entity(casterID)
		state := module.casters[casterID]
		if caster == nil || state == nil {
			return gametypes.ErrUnknownEntity
		}
		if slotIndex >= ActionBarSlotCount {
			return ErrInvalidActionSlot
		}
		abilityID := state.actionBar[slotIndex]
		if abilityID == "" {
			return ErrEmptyActionSlot
		}
		selected := module.selectedTarget(tick, state)
		var outcome error
		event, outcome = module.useAbility(tick, casterID, AbilityRequest{
			Seq:       seq,
			TargetID:  selected,
			AbilityID: abilityID,
		})
		return outcome
	})
	return event, err
}

func (module *Module) cooldownMilliseconds(
	tick gametypes.Tick,
	state *casterState,
	ability gametypes.Ability,
) (remainingMS, durationMS int64) {
	current := tick.Number()
	var readyTick, durationTicks uint64
	if ability.TriggersGCD {
		readyTick = state.gcdReadyTick
		durationTicks = module.gcdTicks(tick)
	}
	abilityReady := state.readyTick[ability.ID]
	abilityDuration := ticksIn(ability.Cooldown, tick.Interval())
	if abilityReady > readyTick {
		readyTick = abilityReady
		durationTicks = abilityDuration
	} else if readyTick <= current && abilityDuration > durationTicks {
		// When no timer is running, report the full effective duration that a
		// fresh activation would start.
		durationTicks = abilityDuration
	}
	return remainingTickMilliseconds(readyTick, current, tick.Interval()),
		tickMilliseconds(durationTicks, tick.Interval())
}

func remainingTickMilliseconds(readyTick, currentTick uint64, interval time.Duration) int64 {
	if readyTick <= currentTick {
		return 0
	}
	return tickMilliseconds(readyTick-currentTick, interval)
}

func tickMilliseconds(ticks uint64, interval time.Duration) int64 {
	if ticks == 0 || interval <= 0 {
		return 0
	}
	// Saturate before converting uint64 to time.Duration. Real cooldowns are
	// tiny compared with this bound, but a corrupt state must not wrap below
	// zero in a client snapshot.
	const maxDuration = time.Duration(1<<63 - 1)
	if ticks > uint64(maxDuration/interval) {
		return maxDuration.Milliseconds()
	}
	milliseconds := (time.Duration(ticks) * interval).Milliseconds()
	return max(0, milliseconds)
}
