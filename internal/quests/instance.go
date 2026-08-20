package quests

import (
	"encoding/json"
	"fmt"

	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/store"
)

// State is the quest state machine of mechanics/quests.md rule 5.1.
//
// `unavailable` and `offered` are states of the *pair* (character, definition)
// and no instance exists in either; the rest are states of an instance. They
// share one type because rule 5.2's transition table moves between them, and
// splitting them would make T1, T3 and T16 cross a type boundary for no gain.
type State uint8

const (
	StateUnspecified State = iota
	// StateUnavailable means the gates of rule 5.3 fail.
	StateUnavailable
	// StateOffered means the gates pass and the starter will hand it over.
	StateOffered
	// StateAccepted means the instance exists and every counter is zero.
	StateAccepted
	// StateInProgress means at least one counter is above zero and at least one
	// is below its limit.
	StateInProgress
	// StateCompletable means every counter has reached its limit.
	StateCompletable
	// StateTurnedIn is terminal. Rewards granted.
	StateTurnedIn
	// StateAbandoned is terminal for this instance; the quest may be offered
	// again (T16).
	StateAbandoned
)

func (state State) String() string {
	switch state {
	case StateUnavailable:
		return "unavailable"
	case StateOffered:
		return "offered"
	case StateAccepted:
		return "accepted"
	case StateInProgress:
		return "in-progress"
	case StateCompletable:
		return "completable"
	case StateTurnedIn:
		return "turned-in"
	case StateAbandoned:
		return "abandoned"
	case StateUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("quest-state(%d)", uint8(state))
	}
}

// parseState reads back what [State.String] wrote.
//
// A row whose state this build does not recognise is an error and not a
// default: silently reading it as `unavailable` would re-offer a quest the
// character has already turned in, and reading it as `turned-in` would grant
// nothing and hide a bug.
func parseState(value string) (State, error) {
	switch value {
	case "unavailable":
		return StateUnavailable, nil
	case "offered":
		return StateOffered, nil
	case "accepted":
		return StateAccepted, nil
	case "in-progress":
		return StateInProgress, nil
	case "completable":
		return StateCompletable, nil
	case "turned-in":
		return StateTurnedIn, nil
	case "abandoned":
		return StateAbandoned, nil
	default:
		return StateUnspecified, fmt.Errorf("quests: stored quest state %q is not one this build knows", value)
	}
}

// Held reports whether an instance exists in this state. Rule 5.3.1 gates on
// exactly this: a quest in any state other than `abandoned` is not offered
// again.
func (state State) Held() bool {
	switch state {
	case StateAccepted, StateInProgress, StateCompletable, StateTurnedIn:
		return true
	default:
		return false
	}
}

// Active reports whether progress can still be made. Rule 5.4.3 credits kills
// to exactly these.
func (state State) Active() bool {
	return state == StateAccepted || state == StateInProgress
}

// instance is one character's progress against one definition.
type instance struct {
	questID  string
	state    State
	counters []int32
	// acceptedAtTick is the server tick T3 created the instance at.
	acceptedAtTick uint64
	// inFlight marks a grant that is committing. It is the reservation that
	// makes the accept and the turn-in safe across the window in which the zone
	// lock is released: a retransmit is refused rather than committing twice.
	inFlight bool
}

// objectiveRecord is the jsonb payload of shard.character_quests.objectives.
//
// The column is jsonb precisely so the counter shape is this package's to
// choose without a migration per change (ADR 0031). The field names are stable
// because stored rows outlive a deploy.
type objectiveRecord struct {
	Counters       []int32 `json:"counters"`
	AcceptedAtTick uint64  `json:"accepted_at_tick"`
}

func (held *instance) row() (store.QuestState, error) {
	counters := held.counters
	if counters == nil {
		counters = []int32{}
	}
	encoded, err := json.Marshal(objectiveRecord{
		Counters:       counters,
		AcceptedAtTick: held.acceptedAtTick,
	})
	if err != nil {
		return store.QuestState{}, fmt.Errorf("encode quest %q counters: %w", held.questID, err)
	}
	return store.QuestState{
		QuestID:    held.questID,
		State:      held.state.String(),
		Objectives: encoded,
	}, nil
}

// instanceFromRow rebuilds one instance from persisted bytes.
//
// The counter list is resized to the definition's objective count. That is the
// migration hazard mechanics/quests.md section 7.5 names: counters are bound to
// objectives by position, so a definition that grew has its new counters read
// as zero and one that shrank drops the tail. Resizing is the conservative
// answer — the alternative is an index panic on a row nobody can edit.
func instanceFromRow(row store.QuestState, definition pack.Quest) (*instance, error) {
	state, err := parseState(row.State)
	if err != nil {
		return nil, fmt.Errorf("quest %q: %w", row.QuestID, err)
	}
	var record objectiveRecord
	if len(row.Objectives) > 0 {
		if err := json.Unmarshal(row.Objectives, &record); err != nil {
			return nil, fmt.Errorf("quest %q: decode counters: %w", row.QuestID, err)
		}
	}
	counters := make([]int32, len(definition.Objectives))
	copy(counters, record.Counters)
	return &instance{
		questID:        row.QuestID,
		state:          state,
		counters:       counters,
		acceptedAtTick: record.AcceptedAtTick,
	}, nil
}

// satisfied is rule 5.6.2's guard: every counter has reached its limit.
// Vacuously true over an empty counter list, which is what makes the
// zero-objective quest of rule 5.6 need no special case.
func satisfied(definition pack.Quest, counters []int32) bool {
	for index, objective := range definition.Objectives {
		if index >= len(counters) {
			return false
		}
		if counters[index] < objective.Limit {
			return false
		}
	}
	return true
}

// touched reports whether any counter has moved off zero, which is what
// separates `accepted` from `in-progress` (rule 5.1).
func touched(counters []int32) bool {
	for _, counter := range counters {
		if counter > 0 {
			return true
		}
	}
	return false
}

// progressState is the state an active instance is in given its counters. It is
// where T5, T6, T8, T9 and T10 all land: the transition table distinguishes
// them by where they came from, and the destination is a function of the
// counters alone.
func progressState(definition pack.Quest, counters []int32) State {
	switch {
	case satisfied(definition, counters):
		return StateCompletable
	case touched(counters):
		return StateInProgress
	default:
		return StateAccepted
	}
}
