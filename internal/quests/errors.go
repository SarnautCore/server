package quests

import "errors"

// Refusal names why a quest verb resolved to nothing.
//
// It is a typed value and never a bare string, and it is deliberately not a
// protocol ErrorCode: a protocol error is a violation and closes the
// connection, while arriving at a finisher with a full bag or clicking accept
// twice is ordinary play that must leave the session running
// (mechanics/quests.md transitions T4, T12 and T13).
type Refusal uint8

const (
	// RefusalNone means the verb resolved.
	RefusalNone Refusal = iota
	// RefusalUnknownQuest means the runtime pack carries no quest with that id.
	RefusalUnknownQuest
	// RefusalUnavailable is rule 5.3: a prerequisite is unfinished, the
	// required level is not met, or an instance already exists. T4.
	RefusalUnavailable
	// RefusalLogFull is T3's capacity guard.
	RefusalLogFull
	// RefusalOutOfRange means the character is further than TURN_IN_RANGE_M
	// from the NPC. T4, T13.
	RefusalOutOfRange
	// RefusalWrongNPC means the entity named is not this quest's starter, or
	// not its finisher. T13.
	RefusalWrongNPC
	// RefusalNotComplete is a turn-in before every counter reached its limit.
	RefusalNotComplete
	// RefusalAlreadyComplete is T17. A retransmitted turn-in lands here and
	// grants nothing.
	RefusalAlreadyComplete
	// RefusalBagFull is rule 5.7.3. No experience, no money, no honor, no
	// items, and the instance is unchanged. T12.
	RefusalBagFull
	// RefusalCannotCancel is T15: the definition sets can_cancel false.
	RefusalCannotCancel
	// RefusalInternal means the grant could not be committed. Nothing was
	// applied.
	RefusalInternal
	// RefusalNotAQuestGiver means the interacted entity starts and finishes no
	// quest. It never reaches a client: `interact` is the generic "use the
	// thing I am looking at" verb, and a corpse is not a protocol error.
	RefusalNotAQuestGiver
)

func (refusal Refusal) String() string {
	switch refusal {
	case RefusalNone:
		return "none"
	case RefusalUnknownQuest:
		return "unknown_quest"
	case RefusalUnavailable:
		return "unavailable"
	case RefusalLogFull:
		return "log_full"
	case RefusalOutOfRange:
		return "out_of_range"
	case RefusalWrongNPC:
		return "wrong_npc"
	case RefusalNotComplete:
		return "not_complete"
	case RefusalAlreadyComplete:
		return "already_complete"
	case RefusalBagFull:
		return "bag_full"
	case RefusalCannotCancel:
		return "cannot_cancel"
	case RefusalInternal:
		return "internal"
	case RefusalNotAQuestGiver:
		return "not_a_quest_giver"
	default:
		return "unknown"
	}
}

// Errors a Go caller can match with [errors.Is]. Every one of them is also
// reported to the client as its [Refusal]; these exist so that the slice
// driver and the tests can branch without re-deriving the reason.
var (
	ErrUnknownQuest           = errors.New("quests: the content pack carries no such quest")
	ErrQuestUnavailable       = errors.New("quests: this quest is not available to this character")
	ErrQuestLogFull           = errors.New("quests: the quest log is full")
	ErrOutOfRange             = errors.New("quests: too far from the quest giver")
	ErrWrongNPC               = errors.New("quests: that is not this quest's giver")
	ErrQuestNotComplete       = errors.New("quests: the objectives are not complete")
	ErrQuestAlreadyComplete   = errors.New("quests: this quest is already turned in")
	ErrBagFull                = errors.New("quests: the bag cannot hold the reward")
	ErrQuestCannotBeCancelled = errors.New("quests: this quest cannot be abandoned")
	ErrNotAQuestGiver         = errors.New("quests: that entity gives no quest")
)

func (refusal Refusal) err() error {
	switch refusal {
	case RefusalUnknownQuest:
		return ErrUnknownQuest
	case RefusalUnavailable:
		return ErrQuestUnavailable
	case RefusalLogFull:
		return ErrQuestLogFull
	case RefusalOutOfRange:
		return ErrOutOfRange
	case RefusalWrongNPC:
		return ErrWrongNPC
	case RefusalNotComplete:
		return ErrQuestNotComplete
	case RefusalAlreadyComplete:
		return ErrQuestAlreadyComplete
	case RefusalBagFull:
		return ErrBagFull
	case RefusalCannotCancel:
		return ErrQuestCannotBeCancelled
	case RefusalNotAQuestGiver:
		return ErrNotAQuestGiver
	case RefusalNone, RefusalInternal:
		return nil
	default:
		return nil
	}
}
