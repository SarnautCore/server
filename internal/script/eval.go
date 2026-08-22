package script

import (
	"context"
	"errors"
	"fmt"
)

// RefusedError is what a refused-tier node produces. It names the content row,
// the node key and the opcode, because those three are what an operator needs to
// find the row and what CI needs to tell one refusal from another. ADR 0036
// makes a refused node reached by scripts/m3-tutorial-driver a hard CI failure
// even when a caller catches this error.
type RefusedError struct {
	SourceID string
	NodeKey  string
	Family   Family
	Opcode   string
	// Reason distinguishes "this build has no handler" from "the handler exists
	// but the node is malformed".
	Reason string
}

func (refusal *RefusedError) Error() string {
	return fmt.Sprintf(
		"script node %s (%s %s) in content row %s is refused: %s",
		refusal.NodeKey, refusal.Family, refusal.Opcode, refusal.SourceID, refusal.Reason,
	)
}

// Census records what a run reached. The counts feed
// testdata/inst-league1-tier-counts.json, which CI diffs against the extractor
// and pack report so that widening coverage is a reviewable diff rather than a
// quiet behaviour change.
type Census struct {
	Implemented map[string]int
	Inert       map[string]int
	Refused     map[string]int
}

func newCensus() *Census {
	return &Census{
		Implemented: make(map[string]int),
		Inert:       make(map[string]int),
		Refused:     make(map[string]int),
	}
}

func (census *Census) record(tier Tier, opcode string) {
	switch tier {
	case TierImplemented:
		census.Implemented[opcode]++
	case TierInert:
		census.Inert[opcode]++
	default:
		census.Refused[opcode]++
	}
}

// Options gates the interpreter. ADR 0033 §2 admits no caller for this package
// yet — M3-05 owns the allow-list amendment — so Enabled defaults to false and
// the shard wires it from Content/Script config the same way
// SkipUnsupportedQuests is wired. Nothing in internal/quests consults it yet.
type Options struct {
	Enabled bool
	// StrictInert turns inert-and-counted into refused, which is how a
	// pre-merge job proves that a tier demotion was deliberate.
	StrictInert bool
}

// Evaluator walks a Node tree against a Host.
type Evaluator struct {
	host             Host
	options          Options
	handlers         map[string]handler
	census           *Census
	ordinal          uint64
	lifecycleOrdinal uint64
}

type handler func(context.Context, *Evaluator, *Node, Frame) error

// ErrDisabled is returned when the interpreter is invoked with Options.Enabled
// false. It is not a refusal: the node was never reached.
var ErrDisabled = errors.New("script: interpreter disabled by feature flag")

// New builds an evaluator over the M3 handler set.
func New(host Host, options Options) *Evaluator {
	return &Evaluator{
		host:     host,
		options:  options,
		handlers: m3Handlers(),
		census:   newCensus(),
	}
}

// Census returns the per-opcode tier counts this evaluator has recorded.
func (evaluator *Evaluator) Census() *Census { return evaluator.census }

// Evaluate runs one node and its children. It is the single entry point for all
// four callers of ADR 0036.
func (evaluator *Evaluator) Evaluate(ctx context.Context, node *Node, frame Frame) error {
	if !evaluator.options.Enabled {
		return ErrDisabled
	}
	return evaluator.eval(ctx, node, frame)
}

func (evaluator *Evaluator) eval(ctx context.Context, node *Node, frame Frame) error {
	if node == nil {
		return nil
	}
	evaluator.ordinal++
	frame.ActivationOrdinal = evaluator.ordinal

	admitted, err := evaluator.admit(node, frame, "opcode is outside the M3 implemented tier")
	if err != nil || !admitted {
		return err
	}

	run, ok := evaluator.handlers[node.Opcode]
	if !ok {
		// A node tiered implemented with no handler is a build error, not a
		// content error. Say so rather than silently doing nothing.
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "tiered implemented but this build registers no handler",
		}
	}
	return run(ctx, evaluator, node, frame)
}

// admit applies ADR 0036's coverage policy to one node and records the census
// hit. It reports whether the node should run. Every family goes through it —
// impacts, effects, calcers, scalers and finders — so that a node's tier is
// enforced in exactly one place and the census counts every node the run
// reached, which is what makes testdata/inst-league1-tier-counts.json a
// reviewable diff rather than a hopeful one.
func (evaluator *Evaluator) admit(node *Node, frame Frame, reason string) (bool, error) {
	tier := node.Tier
	if tier == TierInert && evaluator.options.StrictInert {
		tier = TierRefused
	}
	evaluator.census.record(tier, node.Opcode)

	switch tier {
	case TierInert:
		// Parsed, counted, no effect, and deliberately no descent: an inert node's
		// children have no meaning without it.
		return false, nil
	case TierImplemented:
		return true, nil
	default:
		return false, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: reason,
		}
	}
}

// evalAll runs a list-valued field's children in stored order, stopping at the
// first error. Order is load-bearing: the quest-2-20 reward cascade and every
// ImpactsDeferred child list depend on it.
func (evaluator *Evaluator) evalAll(ctx context.Context, node *Node, field string, frame Frame) error {
	for _, child := range node.Nodes(field) {
		if err := evaluator.eval(ctx, child, frame); err != nil {
			return err
		}
	}
	return nil
}

// Predicate evaluates a predicate subtree to a boolean.
func (evaluator *Evaluator) Predicate(ctx context.Context, node *Node, frame Frame) (bool, error) {
	if node == nil {
		// No predicate means an unconditional branch, which is how the data
		// spells "always".
		return true, nil
	}
	evaluator.census.record(node.Tier, node.Opcode)
	if node.Tier != TierImplemented {
		return false, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "predicate is outside the M3 implemented tier",
		}
	}

	switch node.Opcode {
	case "PredicateAnd":
		for _, child := range node.Nodes("predicates") {
			ok, err := evaluator.Predicate(ctx, child, frame)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil

	case "PredicateOr":
		// quests.md §5.3.4 says a disjunctive form "has never been observed".
		// The tutorial reaches PredicateOr three times, so it has.
		for _, child := range node.Nodes("predicates") {
			ok, err := evaluator.Predicate(ctx, child, frame)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil

	case "PredicateNot":
		children := node.Nodes("predicates")
		if len(children) != 1 {
			return false, &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: fmt.Sprintf("expects exactly one child predicate, found %d", len(children)),
			}
		}
		ok, err := evaluator.Predicate(ctx, children[0], frame)
		return !ok, err

	case "PredicateCharacterClass":
		return evaluator.queryRefEquals(ctx, node, frame, QueryCharacterClass, "characterClass")

	case "PredicateCharacterRace":
		return evaluator.queryRefEquals(ctx, node, frame, QueryCharacterRace, "characterRace")

	case "PredicateIsAvatar":
		// The extracted toLog=false field is source-side residue. Retail's Java
		// predicate has no fields and tests only whether the addressee is an
		// AvatarReplica. A true residue value is refused instead of invented.
		if len(node.Fields) > 1 || len(node.Fields) == 1 && node.Fields[0].Name != "toLog" {
			return false, &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "PredicateIsAvatar carries an unknown field",
			}
		}
		if toLog, ok := node.Field("toLog"); ok && (toLog.Kind != ValueBool || toLog.Bool) {
			return false, &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "field \"toLog\" is non-semantic residue and must be absent or false",
			}
		}
		answer, err := evaluator.host.Query(ctx, Query{
			Kind: QueryIsAvatar, EntityID: frame.Addressee,
		})
		if err != nil {
			return false, fmt.Errorf("query avatar kind for %s: %w", frame.Addressee, err)
		}
		if answer.Kind != ValueBool {
			return false, &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "the host answered is-avatar with a non-boolean value",
			}
		}
		return answer.Bool, nil

	default:
		return false, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no predicate handler registered",
		}
	}
}

// queryRefEquals asks the host what the addressee's <field> is and compares it
// with the reference the node names. Both PredicateCharacterClass and
// PredicateCharacterRace are this shape, and together they are 100 of the
// tutorial's 187 predicate uses.
func (evaluator *Evaluator) queryRefEquals(
	ctx context.Context, node *Node, frame Frame, kind QueryKind, field string,
) (bool, error) {
	want, ok := node.Field(field)
	if !ok || want.Kind != ValueRef {
		return false, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: fmt.Sprintf("field %q is missing or is not a content reference", field),
		}
	}
	got, err := evaluator.host.Query(ctx, Query{
		Kind: kind, EntityID: frame.Addressee, Ref: want.Ref,
	})
	if err != nil {
		return false, fmt.Errorf("query %s for %s: %w", field, frame.Addressee, err)
	}
	return got.Kind == ValueRef && got.Ref.ID == want.Ref.ID, nil
}

// m3Handlers registers the implemented-tier opcodes this build executes.
//
// The control-flow set came first: ImpactsDeferred is the most common impact in
// the tutorial at 144 uses, ImpactIfTarget is the most common branch at 62, and
// ImpactIncreaseQuestCount is the whole of quest-count-special and therefore of
// M3-26.
//
// The rest are the two count-special shapes of the survey §3 and the Warrior
// auto-attack path of §6, which ADR 0036's amendment moved into the implemented
// tier. An opcode in that tier with no row here is a build error rather than a
// content error, and eval says so: the tier table and the handler set disagreeing
// must never look like a node quietly doing nothing.
func m3Handlers() map[string]handler {
	return map[string]handler{
		// Control flow.
		"ImpactsDeferred": evalImpactsDeferred,
		"ImpactIfTarget":  evalImpactIfRole,
		"ImpactIfCaster":  evalImpactIfRole,
		"ReturningImpact": evalReturningImpact,
		"MarkedImpact":    evalMarkedImpact,

		// Quest counting, both shapes.
		"ImpactIncreaseQuestCount": evalImpactIncreaseQuestCount,
		"ImpactAddExperience":      evalImpactAddExperience,
		"TagMobForKill":            evalTagMobForKill,

		// Trigger binding: shape B finds the mobs, shape A binds to the player,
		// and the two mobWorld agents bind across a spawn scope the host owns.
		"ImpactFindSpawnTable":     evalFindSpawnTable,
		"ImpactAttachTrigger":      evalAttachTrigger,
		"TriggerAgentSelf":         evalTriggerAgent,
		"TriggerAgentInterlocutor": evalTriggerAgent,
		"TriggerAgentSimple":       evalTriggerAgent,
		"TriggerAgentOnTagged":     evalTriggerAgent,

		// Warrior kit.
		"ScaledPhysicalWeaponDamage": evalScaledPhysicalWeaponDamage,
		"ImpactSetTarget":            evalImpactSetTarget,

		// Authored world state used by quest 4-30's Firewall sequence.
		"ImpactTurnMob": evalImpactTurnMob,
		"ImpactSummon":  evalImpactSummon,
	}
}

// evalMarkedImpact is a transparent wrapper: it marks its child for the client's
// combat log and runs it. ScaledPhysicalWeaponDamage carries its on-hit impacts
// through one, so descending is the whole behaviour.
func evalMarkedImpact(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if err := evaluator.evalAll(ctx, node, "impact", frame); err != nil {
		return err
	}
	return evaluator.evalAll(ctx, node, "impacts", frame)
}

// evalImpactsDeferred schedules its children rather than running them. A child's
// due time is the host's clock now plus this node's delay, so a nested delay is
// relative to execution of its parent. Zero-delay work still enters the queue,
// which is what keeps ordering identical across a restart.
func evalImpactsDeferred(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	var delay uint64
	if value, ok := node.Field("delay"); ok {
		switch value.Kind {
		case ValueDurationMS:
			delay = value.DurationMS
		case ValueInteger:
			if value.Integer < 0 {
				return &RefusedError{
					SourceID: frame.SourceID, NodeKey: node.Key,
					Family: node.Family, Opcode: node.Opcode,
					Reason: fmt.Sprintf("negative delay %d", value.Integer),
				}
			}
			delay = uint64(value.Integer)
		default:
			return &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "delay is neither a duration nor an integer",
			}
		}
	}

	dueAt := uint64(evaluator.host.Now().UnixMilli()) + delay
	for _, child := range node.Nodes("impacts") {
		if err := evaluator.host.Enqueue(ctx, Deferred{
			Node: child, Frame: frame, DueAtMS: dueAt,
		}); err != nil {
			return fmt.Errorf("enqueue deferred child %s: %w", child.Key, err)
		}
	}
	return nil
}

// evalImpactIfRole covers ImpactIfTarget and ImpactIfCaster, which differ only
// in which invocation role the predicate reads. Both branch on `predicates` and
// run `impacts` when it holds.
func evalImpactIfRole(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	branch := frame
	if node.Opcode == "ImpactIfCaster" {
		branch.Addressee = frame.CasterID
	} else {
		branch.Addressee = frame.TargetID
	}

	// The data spells the condition as a list even when it holds one entry, and
	// a list of predicates is conjunctive.
	for _, predicate := range node.Nodes("predicates") {
		ok, err := evaluator.Predicate(ctx, predicate, branch)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return evaluator.evalAll(ctx, node, "impacts", branch)
}

// evalImpactIncreaseQuestCount is the entire quest-count-special mechanism.
// The command is idempotent under its execution key and the host clamps at the
// objective's limit, because Quest_1_20/CountId_1 has two independent
// increment paths (DressTrigger and BrokenDoorExploit) against a limit of 1.
func evalImpactIncreaseQuestCount(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	id, ok := node.Field("id")
	if !ok || id.Kind != ValueRef {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"id\" is missing or is not a QuestCountId reference",
		}
	}
	// The delta field is spelled "value", not "count". The reflection schema in
	// Types/types.xml gives ImpactIncreaseQuestCount exactly two fields — id,
	// required, and value, an Integer defaulting to 1 — and the overwhelming
	// majority of the 1486 uses omit it. Reading the wrong name was silent:
	// every increment defaulted to 1, which is right until content asks for
	// more.
	count := int64(1)
	if value, ok := node.Field("value"); ok && value.Kind == ValueInteger {
		count = value.Integer
	}
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandIncreaseQuestCount,
		EntityID:     frame.Addressee,
		Ref:          id.Ref,
		Count:        count,
		ExecutionKey: frame.EvaluationID + "|" + node.Key,
	})
}

func evalImpactAddExperience(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	mobCount, countOK := node.Field("mobCount")
	mobLevel, levelOK := node.Field("mobLevel")
	if !countOK || mobCount.Kind != ValueInteger || mobCount.Integer <= 0 {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"mobCount\" is missing or is not a positive integer",
		}
	}
	if !levelOK || mobLevel.Kind != ValueInteger || mobLevel.Integer <= 0 {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"mobLevel\" is missing or is not a positive integer",
		}
	}
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandAddExperience,
		EntityID:     frame.Addressee,
		Count:        mobCount.Integer,
		MobLevel:     mobLevel.Integer,
		ExecutionKey: frame.EvaluationID + "|" + node.Key,
	})
}
