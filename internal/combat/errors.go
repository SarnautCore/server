package combat

import "errors"

// Rejection is why an ability use resolved to nothing.
//
// It is an error value, so a caller writes `errors.Is(err, combat.ErrOutOfRange)`
// and a test names the reason it expects rather than matching a string. It is
// deliberately not a protocol ErrorCode: an ErrorCode is a protocol violation
// and closes the connection, while casting at a target that walked out of
// range is ordinary play.
type Rejection uint8

const (
	// RejectionNone means the ability resolved.
	RejectionNone Rejection = iota
	// RejectionNoTarget is rule 5.2.2.
	RejectionNoTarget
	// RejectionInvalidTarget is rules 5.2.3 and 5.2.5, and rule 5.8.6's
	// untargetable returning mob.
	RejectionInvalidTarget
	// RejectionTargetDead is rule 5.2.4.
	RejectionTargetDead
	// RejectionOutOfRange is rule 5.3.2.
	RejectionOutOfRange
	// RejectionOnCooldown is rule 5.4.1, and the per-ability cooldown the pack
	// may carry on top of it.
	RejectionOnCooldown
	// RejectionUnknownAbility means the caster does not know the ability, or
	// the pack does not carry it.
	RejectionUnknownAbility
	// RejectionNoResource means the server-owned action cost exceeds the
	// caster's current authored resource pool.
	RejectionNoResource
)

// The rejections mechanics/combat.md names, as error values.
var (
	ErrNoTarget       error = RejectionNoTarget
	ErrInvalidTarget  error = RejectionInvalidTarget
	ErrTargetDead     error = RejectionTargetDead
	ErrOutOfRange     error = RejectionOutOfRange
	ErrOnCooldown     error = RejectionOnCooldown
	ErrUnknownAbility error = RejectionUnknownAbility
	ErrNoResource     error = RejectionNoResource
)

// ErrDuplicateCommand reports a command that is not newer than the last one
// this caster had accepted.
//
// It is not a Rejection: nothing was refused, the frame was a retransmit of
// something already resolved. Casting faster than the cooldown allows is a
// different thing and comes back as ErrOnCooldown.
var ErrDuplicateCommand = errors.New("combat command is not newer than the last accepted one")

func (rejection Rejection) Error() string { return rejection.String() }

func (rejection Rejection) String() string {
	switch rejection {
	case RejectionNone:
		return "resolved"
	case RejectionNoTarget:
		return "no such target"
	case RejectionInvalidTarget:
		return "target cannot be attacked"
	case RejectionTargetDead:
		return "target is dead"
	case RejectionOutOfRange:
		return "target is out of range"
	case RejectionOnCooldown:
		return "ability is on cooldown"
	case RejectionUnknownAbility:
		return "caster does not know that ability"
	case RejectionNoResource:
		return "caster has insufficient resource"
	default:
		return "unknown combat rejection"
	}
}

// err converts a rejection into the error a caller sees, so the resolved case
// is a nil error rather than a sentinel meaning success.
func (rejection Rejection) err() error {
	if rejection == RejectionNone {
		return nil
	}
	return rejection
}
