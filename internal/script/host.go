package script

import (
	"context"
	"time"
)

// Frame names the invocation roles a node reads. ADR 0036 fixes these: the
// evaluator never reaches for ambient state, so replaying a frame replays the
// evaluation.
type Frame struct {
	// EvaluationID is stable for one activation and is a probability input.
	EvaluationID string
	PackID       string
	Event        string
	ZoneID       string
	// SourceID is the content row the tree came from; it appears in every
	// refusal so an operator knows which row to fix.
	SourceID       string
	CasterID       string
	TargetID       string
	InterlocutorID string
	// Addressee is who the current impact applies to. ImpactFindSingleMob and
	// the addressee finders rewrite it for their subtree; ReturningImpact
	// restores it to CasterID, which is how a rat's death credits the killer.
	Addressee string
	// ActivationOrdinal disambiguates repeated activations of the same node for
	// probability replay. Depth-first node order, never map iteration order.
	ActivationOrdinal uint64
}

// QueryKind and CommandKind are closed unions owned by this package. The host
// never receives an opcode and never decides a coverage tier; that asymmetry is
// what keeps domain modules ignorant of the interpreter.
type QueryKind uint8

const (
	QueryUnspecified QueryKind = iota
	// QueryCharacterClass answers PredicateCharacterClass (58 uses, the most
	// common predicate in the tutorial).
	QueryCharacterClass
	// QueryCharacterRace answers PredicateCharacterRace (42 uses).
	QueryCharacterRace
	// QueryQuestStatus answers PredicateQuestStatus (46 uses).
	QueryQuestStatus
	// QueryHasItem answers PredicateHasItem (12 uses).
	QueryHasItem
)

type Query struct {
	Kind QueryKind
	// EntityID is whom the question is about, normally Frame.Addressee.
	EntityID string
	// Ref is what the question is about: a class, race, quest or item row.
	Ref Ref
}

type CommandKind uint8

const (
	CommandUnspecified CommandKind = iota
	// CommandIncreaseQuestCount is the whole of quest-count-special. The host
	// adapter clamps at the objective's limit and is idempotent under
	// ExecutionKey, because Quest_1_20/CountId_1 has two independent
	// incrementers and a limit of 1.
	CommandIncreaseQuestCount
	// CommandGiveItem answers ImpactGiveItem (54 uses).
	CommandGiveItem
	// CommandClientData answers ImpactClientData (83 uses) — presentation only.
	CommandClientData
)

type Command struct {
	Kind     CommandKind
	EntityID string
	Ref      Ref
	Count    int64
	// ExecutionKey makes a replay idempotent. It is the deferred queue row id
	// and the node key, so a crash between applying a command and deleting its
	// queue row cannot double-apply.
	ExecutionKey string
}

// ResolveRequest asks the host for entity ids. Resolve returns them in bytewise
// id order so that iteration is deterministic across runs.
type ResolveRequest struct {
	// Finder is the opcode that asked: AddresseeFinderSelf, ...Target,
	// ...SingleMob, ...Caster, or an ImpactFind* / spawn-table resolution.
	Finder string
	Frame  Frame
	Ref    Ref
}

// Deferred is one scheduled child of an ImpactsDeferred node, at 144 uses the
// most common impact in the tutorial. DueAtMS is the host's integer millisecond
// clock when the enclosing deferred node ran, plus that node's delay — a nested
// delay is relative to execution of its parent, not to the parent's enqueue.
type Deferred struct {
	Node    *Node
	Frame   Frame
	DueAtMS uint64
}

// Host is the only way out of the evaluator.
type Host interface {
	Now() time.Time
	Query(context.Context, Query) (Value, error)
	Resolve(context.Context, ResolveRequest) ([]string, error)
	Apply(context.Context, Command) error
	Enqueue(context.Context, Deferred) error
}
