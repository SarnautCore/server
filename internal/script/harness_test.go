package script_test

import (
	"context"
	"fmt"
	"time"

	"github.com/SarnautCore/server/internal/script"
)

// Content ids from the real tutorial documents, so a failure names something a
// reader can go and look at. Quest_1_20 is the DressTrigger quest and the first
// quest-count-special objective in the chain; Quest_1_30 is the RatKiller
// kill-counting shape.
const (
	dressCountID = "questcount.inst-league1.quest-1-20.count-id-1"
	ratCountID   = "questcount.inst-league1.quest-1-30.count-id-1"
	warriorClass = "class.warrior"
	druidClass   = "class.druid"

	playerID = "character.tester"
	ratID    = "mob.inst-league1.rat"
)

// fakeHost records an ordered call trace instead of touching the world. ADR 0036
// makes golden call traces over the discovered trigger corpus a binding check
// rather than an optional example, so the trace format is the assertion surface:
// one line per host call, in the order the evaluator made them.
type fakeHost struct {
	nowMS int64
	// class answers QueryCharacterClass per entity.
	class map[string]string
	// applyErr, when set, makes the next Apply fail.
	applyErr error

	trace []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		// A fixed clock: deferred due times are asserted exactly, and a wall
		// clock would make that flaky for no benefit.
		nowMS: 1_000_000,
		class: map[string]string{playerID: warriorClass},
	}
}

func (host *fakeHost) Now() time.Time { return time.UnixMilli(host.nowMS) }

func (host *fakeHost) Query(_ context.Context, query script.Query) (script.Value, error) {
	switch query.Kind {
	case script.QueryCharacterClass:
		got := host.class[query.EntityID]
		host.trace = append(host.trace, fmt.Sprintf("query class %s -> %s", query.EntityID, got))
		return script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: got}}, nil
	default:
		return script.Value{}, fmt.Errorf("fake host has no answer for query kind %d", query.Kind)
	}
}

func (host *fakeHost) Resolve(_ context.Context, request script.ResolveRequest) ([]string, error) {
	host.trace = append(host.trace, "resolve "+request.Finder)
	return nil, nil
}

func (host *fakeHost) Apply(_ context.Context, command script.Command) error {
	if host.applyErr != nil {
		return host.applyErr
	}
	host.trace = append(host.trace, fmt.Sprintf(
		"apply increase-quest-count %s +%d on %s key=%s",
		command.Ref.ID, command.Count, command.EntityID, command.ExecutionKey,
	))
	return nil
}

func (host *fakeHost) Enqueue(_ context.Context, deferred script.Deferred) error {
	host.trace = append(host.trace, fmt.Sprintf(
		"enqueue %s due=%d", deferred.Node.Opcode, deferred.DueAtMS,
	))
	return nil
}

// --- node builders -------------------------------------------------------
//
// These keep the tests readable without introducing a second representation:
// every builder returns a plain *script.Node.

func impact(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func predicate(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyPredicate, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func field(name string, value script.Value) script.Field {
	return script.Field{Name: name, Value: value}
}

func nodeList(nodes ...*script.Node) script.Value {
	values := make([]script.Value, 0, len(nodes))
	for _, node := range nodes {
		values = append(values, script.Value{Kind: script.ValueNode, Node: node})
	}
	return script.Value{Kind: script.ValueList, List: values}
}

func ref(id string) script.Value {
	return script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: id}}
}

func duration(ms uint64) script.Value {
	return script.Value{Kind: script.ValueDurationMS, DurationMS: ms}
}

// increaseQuestCount is the leaf every quest-count-special objective ends at.
func increaseQuestCount(key, countID string) *script.Node {
	return impact(key, "ImpactIncreaseQuestCount", field("id", ref(countID)))
}

func newFrame() script.Frame {
	return script.Frame{
		EvaluationID: "eval-1",
		PackID:       "pack-1",
		SourceID:     "quest.inst-league1.quest-1-20",
		ZoneID:       "zone.inst-league1",
		CasterID:     playerID,
		TargetID:     playerID,
		Addressee:    playerID,
	}
}

func enabled() script.Options { return script.Options{Enabled: true} }

func run(host *fakeHost, node *script.Node, frame script.Frame) (*script.Evaluator, error) {
	evaluator := script.New(host, enabled())
	return evaluator, evaluator.Evaluate(context.Background(), node, frame)
}
