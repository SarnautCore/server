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
	// QueryMaxHealth answers FullHealthCalcer, which is HealthTrigger's threshold
	// operand at 28 uses in the bounded reach set. The calcer multiplies the
	// answer, so the host returns full health and never a threshold.
	QueryMaxHealth
	// The three scale queries answer the three per-opcode scalers. Each returns
	// a unit-less multiplier for the entity and slot named by the query, and the
	// node the scaler hangs from supplies the base magnitude. See scaler.go for
	// why that is the shape: the scaler nodes in the content carry no fields.
	QueryPhysicalScale
	QueryPhysicalRangedScale
	QueryWeaponSpeedScale
)

type Query struct {
	Kind QueryKind
	// EntityID is whom the question is about, normally Frame.Addressee.
	EntityID string
	// Ref is what the question is about: a class, race, quest or item row.
	Ref Ref
	// Slot names an equipment slot for the weapon queries, spelled as the
	// content spells it (MAINHAND, TWOHANDED, RANGED). Empty for everything
	// else.
	Slot string
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
	// CommandAttachTrigger registers a trigger against an entity. It is how both
	// count-special shapes bind: shape A binds DressTrigger to the player through
	// TriggerAgentSelf, shape B binds RatKiller to every mob of a spawn table
	// through ImpactAttachTrigger. The host owns the registry; the evaluator
	// holds no per-entity state, exactly as it holds no deferred queue.
	CommandAttachTrigger
	// CommandDetachTrigger is the counterpart. The host issues the lifecycle and
	// calls Evaluator.Detach, which runs the off-branches and then emits this so
	// that one code path ends an attachment.
	CommandDetachTrigger
	// CommandTagMobForKill answers TagMobForKill (7 uses): the mob is marked
	// quest-relevant so that credit and loot follow the tag rather than the
	// aggro table.
	CommandTagMobForKill
	// CommandDamage answers ScaledPhysicalWeaponDamage and ScaledPhysicalDamage.
	// The magnitude is already computed by the scaler; the host applies the
	// combat hook, which is where LifeGuard's clamp lives. ADR 0036's
	// pre-commitment holds: if a scaler handler ever wants internal/combat, the
	// seam is wrong and the change stops.
	CommandDamage
	// CommandSetTarget answers ImpactSetTarget, which is where
	// AddresseeFinderCaster appears in Mechanics/Spells/Warrior.
	CommandSetTarget
)

type Command struct {
	Kind     CommandKind
	EntityID string
	Ref      Ref
	Count    int64
	// Magnitude is the scaler-computed amount for CommandDamage. It stays an
	// exact decimal rather than a rounded integer because rounding is a combat
	// decision and the combat hook is where ADR 0036 puts combat decisions —
	// the same hook that owns LifeGuard's clamp.
	Magnitude Decimal
	// CanBeAvoided carries ScaledPhysicalWeaponDamage's own avoidance flag to
	// that hook. It is not decoration: dropping it would silently make every
	// auto-attack in the game unavoidable.
	CanBeAvoided bool
	// ThreatMultiplier scales the aggro the damage generates. Both auto-attacks
	// author it as 1.
	ThreatMultiplier Decimal
	// TargetID is whom CommandSetTarget points the entity at.
	TargetID string
	// Attachment carries the trigger for CommandAttachTrigger and
	// CommandDetachTrigger. It is nil for every other kind.
	Attachment *Attachment
	// ExecutionKey makes a replay idempotent. It is the deferred queue row id
	// and the node key, so a crash between applying a command and deleting its
	// queue row cannot double-apply.
	ExecutionKey string
}

// Attachment is a trigger bound to an entity. It carries everything needed to
// re-enter the evaluator later, for the same reason a deferred row carries
// node bytes rather than a pointer: the attachment must survive a restart and
// must not start executing a different trigger after a pack change.
type Attachment struct {
	// TriggerRef names the trigger content row. ImpactAttachTrigger and the
	// TriggerAgent binders reference a TriggerResource document rather than
	// inlining it — a trigger is its own content row and ADR 0036 resolves every
	// href to a canonical content id at extraction — so the host loads the row
	// and fills Trigger before it fires anything.
	TriggerRef Ref
	// Trigger is the TriggerResource node. Its effects decide what fires.
	Trigger *Node
	// EntityID is the bearer: the player for shape A, each rat for shape B.
	EntityID string
	// Frame is the invocation that attached the trigger, restored when an event
	// fires it. Its CasterID is the character whose quest the trigger serves,
	// which is what makes shape B credit the killer rather than the corpse.
	Frame Frame
	// DetachesOnDeath is the lifetime flag from the TriggerAgentResource base.
	// It is the host registry's business, not the evaluator's, so it rides here
	// rather than becoming behaviour in a handler.
	DetachesOnDeath bool
}

// EventKind is the closed set of host events that can fire a trigger. It is
// small on purpose: an event exists here only because a trigger effect in the
// tutorial corpus reads it.
type EventKind uint8

const (
	EventUnspecified EventKind = iota
	// EventHealthChanged fires HealthTrigger. Shape B's whole kill count is this
	// event crossing a FullHealthCalcer(multiplier=0) threshold, which is death.
	EventHealthChanged
	// EventEquipChanged fires EquipTrigger. Shape A's whole objective is this
	// event naming MAINHAND or TWOHANDED.
	EventEquipChanged
)

// Event is what the host delivers to an attachment. The evaluator never polls
// and never reaches for ambient state: everything an effect needs to decide
// whether it fires is either in this struct or answered by a Query.
type Event struct {
	Kind EventKind
	// EntityID is the bearer the event happened to. It must match the
	// attachment, and Fire refuses the pairing if it does not.
	EntityID string
	// CauseID is who caused it: the damage source for EventHealthChanged. This
	// is the killer, and it becomes the frame's caster so that ReturningImpact
	// lands the quest count on a player rather than on the dying rat.
	CauseID string
	// Health and PreviousHealth bracket the change. Both are needed because a
	// health trigger fires on the crossing, not on the level: a corpse stays at
	// zero, and a level test would re-fire on every later event.
	Health         int64
	PreviousHealth int64
	// Slot and Equipped describe EventEquipChanged. Slot is spelled as the
	// content spells it: MAINHAND, TWOHANDED.
	Slot     string
	Equipped bool
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
