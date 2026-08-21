package combat

import "github.com/SarnautCore/server/internal/gametypes"

// SelectTarget changes one admitted actor's authoritative target. A zero
// target clears the selection. On refusal it returns the actor's current valid
// selection along with a typed combat rejection.
func (module *Module) SelectTarget(actorID, targetID uint64) (uint64, error) {
	var authoritative uint64
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		actor := tick.Entity(actorID)
		state := module.casters[actorID]
		if actor == nil || state == nil {
			return gametypes.ErrUnknownEntity
		}
		authoritative = module.selectedTarget(tick, state)
		if targetID == 0 {
			state.selected = 0
			authoritative = 0
			return nil
		}
		if rejection := module.validateSelection(tick, targetID); rejection != RejectionNone {
			return rejection
		}
		state.selected = targetID
		authoritative = targetID
		return nil
	})
	return authoritative, err
}

// SelectedTarget returns the actor's current valid authoritative target. A
// target that died, despawned, or stopped replicating is cleared and returns
// zero.
func (module *Module) SelectedTarget(actorID uint64) (uint64, error) {
	var selected uint64
	err := module.zone.GameCommand(func(tick gametypes.Tick) error {
		actor := tick.Entity(actorID)
		state := module.casters[actorID]
		if actor == nil || state == nil {
			return gametypes.ErrUnknownEntity
		}
		selected = module.selectedTarget(tick, state)
		return nil
	})
	return selected, err
}

func (module *Module) selectedTarget(
	tick gametypes.Tick,
	state *casterState,
) uint64 {
	if state.selected == 0 {
		return 0
	}
	if module.validateSelection(tick, state.selected) != RejectionNone {
		state.selected = 0
	}
	return state.selected
}

func (module *Module) validateSelection(
	tick gametypes.Tick,
	targetID uint64,
) Rejection {
	target := tick.Entity(targetID)
	if target == nil {
		return RejectionNoTarget
	}
	if !target.Replicated {
		return RejectionInvalidTarget
	}
	if !target.Alive {
		return RejectionTargetDead
	}
	return RejectionNone
}

func (module *Module) clearSelectedTarget(targetID uint64) {
	for _, state := range module.casters {
		if state.selected == targetID {
			state.selected = 0
		}
	}
}
