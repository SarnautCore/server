package combat

import (
	"fmt"
	"math"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
)

// DamageEffectRequest is the combat-owned description of one resolved damage
// value. An adapter may scale it, but it receives no script opcode or node.
type DamageEffectRequest struct {
	Magnitude int32
	CasterID  uint64
	TargetID  uint64
	// AbilityID proves an active action exists. It is not an action-group id:
	// the current compiled ability row carries no such field, so an adapter
	// must leave group-filtered modifiers unmatched.
	AbilityID string
	// ActionGroupID is the extracted retail action group. Empty means the
	// legacy direct-damage path has no group identity.
	ActionGroupID string
}

// DamageEffectHost applies persistent effects owned outside combat. It runs
// with the zone lock held and must not block or retain tick.
type DamageEffectHost interface {
	ScaleDamage(gametypes.Tick, DamageEffectRequest) (int32, error)
}

// GuardUpdate is the combat-owned projection of the script registry's shared
// Guard state. Session translates the registry value before it crosses this
// boundary, keeping combat free of interpreter types.
type GuardUpdate struct {
	Active           bool
	ObserverRadius   float32
	NoticeTarget     bool
	RecheckEvery     time.Duration
	AggroMarkDelta   int
	RemoveAggroState bool
}

type guardObserver struct {
	radius float32
	// The current wire protocol has no creature-notice message. Retaining the
	// flag here preserves the shared observer state without inventing a client
	// event or silently treating it as false.
	noticeTarget    bool
	recheckTicks    uint64
	nextRecheckTick uint64
}

// SetDamageEffectHost installs the optional persistent-effect adapter. The
// update uses the zone lock, as does every read from the combat path.
func (module *Module) SetDamageEffectHost(host DamageEffectHost) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		module.damageEffects = host
		return nil
	})
}

// ApplyGuardUpdate creates, updates, or removes one mob's shared Guard
// observer. It is called by the session adapter from an evaluation that already
// holds the zone lock.
func (module *Module) ApplyGuardUpdate(
	tick gametypes.Tick,
	entityID uint64,
	update GuardUpdate,
) error {
	if tick == nil {
		return fmt.Errorf("combat: guard update for entity %d has no tick", entityID)
	}
	entity := tick.Entity(entityID)
	state, ok := module.mobs[entityID]
	if entity == nil || !ok || entity.Kind != gametypes.EntityKindNPC {
		return fmt.Errorf("combat: guard owner %d is not a live mob", entityID)
	}

	nextMarks := state.guardAggroMarks + update.AggroMarkDelta
	if nextMarks < 0 || nextMarks > 1 {
		return fmt.Errorf(
			"combat: guard owner %d aggro mark would move from %d to %d",
			entityID, state.guardAggroMarks, nextMarks,
		)
	}

	if update.RemoveAggroState {
		if update.Active || nextMarks != 0 {
			return fmt.Errorf("combat: guard removal for entity %d retains active state", entityID)
		}
		state.guardAggroMarks = 0
		state.guard = nil
		return nil
	}
	if !update.Active {
		if update.AggroMarkDelta != 0 {
			return fmt.Errorf("combat: inactive guard update for entity %d changes its aggro mark", entityID)
		}
		return nil
	}
	if update.ObserverRadius < 0 || math.IsNaN(float64(update.ObserverRadius)) ||
		math.IsInf(float64(update.ObserverRadius), 0) || update.RecheckEvery <= 0 {
		return fmt.Errorf("combat: guard update for entity %d has invalid observer state", entityID)
	}

	state.guardAggroMarks = nextMarks
	state.guard = &guardObserver{
		radius:          update.ObserverRadius,
		noticeTarget:    update.NoticeTarget,
		recheckTicks:    max(1, ticksIn(update.RecheckEvery, tick.Interval())),
		nextRecheckTick: tick.Number(),
	}
	return nil
}
