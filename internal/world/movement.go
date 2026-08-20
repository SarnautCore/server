package world

import (
	"fmt"
	"time"
)

// maxIntentDuration caps how much simulated time one client input may buy. A
// client that claims a long delta cannot teleport with it.
const maxIntentDuration = 250 * time.Millisecond

// MoveIntent is one movement sample from a client.
//
// Duration, not a float count of seconds: clamping it is a simulation
// invariant, while seconds-as-float32 is a wire encoding choice that belongs to
// the mapping layer (ADR 0028).
type MoveIntent struct {
	Seq      uint64
	Input    Vec3
	Heading  float32
	Duration time.Duration
}

// ApplyMoveIntent records the newest valid input for fixed-tick integration.
//
// It validates, discards anything not newer than the last accepted sample, and
// mutates under the zone lock. Every other client verb entering the simulation
// is expected to have the same three parts in the same order.
func (zone *Zone) ApplyMoveIntent(entityID uint64, intent MoveIntent) error {
	if !intent.Input.Finite() || !finite(intent.Heading) {
		return fmt.Errorf("move intent contains a non-finite value")
	}

	zone.mu.Lock()
	defer zone.mu.Unlock()
	current := zone.registry.get(entityID)
	if current == nil || current.Kind != EntityKindPlayer {
		return fmt.Errorf("move entity %d: %w", entityID, ErrUnknownEntity)
	}
	if current.hasIntent && intent.Seq <= current.lastIntentSeq {
		return nil
	}
	current.hasIntent = true
	current.lastIntentSeq = intent.Seq
	current.Heading = intent.Heading

	x, y := normalized(intent.Input.X, intent.Input.Y)
	current.Velocity = Vec3{X: x * zone.config.MaxMoveSpeed, Y: y * zone.config.MaxMoveSpeed}
	duration := intent.Duration
	if duration <= 0 {
		duration = zone.config.TickInterval
	}
	if duration > maxIntentDuration {
		duration = maxIntentDuration
	}
	current.intentRemaining = duration
	if x == 0 && y == 0 {
		current.Animation = AnimationStateIdle
	} else {
		current.Animation = AnimationStateMoving
	}
	return nil
}

// integrateLocked advances intent-driven movement by one tick.
//
// Only players have an intent. A mob is steered by a registered System, which
// calls Tick.MoveTo with the position its own rule computed; that is why this
// loop skipping non-players is correct now, where before the combat module
// existed it was the reason nothing but a player ever moved.
func (zone *Zone) integrateLocked() {
	zone.registry.each(func(current *Entity) bool {
		if current.Kind == EntityKindPlayer {
			zone.integratePlayerLocked(current)
		}
		return true
	})
}

func (zone *Zone) integratePlayerLocked(current *Entity) {
	if current.intentRemaining <= 0 {
		return
	}
	delta := min(zone.config.TickInterval, current.intentRemaining)
	step := float32(delta.Seconds())
	zone.registry.moveTo(current, Vec3{
		X: current.position.X + current.Velocity.X*step,
		Y: current.position.Y + current.Velocity.Y*step,
		// TODO(SAR-19): resolve terrain height and collisions before assigning Z movement.
		Z: current.position.Z,
	})
	current.intentRemaining -= delta
	if current.intentRemaining <= 0 {
		current.Velocity = Vec3{}
		current.Animation = AnimationStateIdle
	}
}
