package script_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

func TestGiveItemEmitsTypedAuthoritativeGrant(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	item := script.Ref{ID: "item.inst-league1.newbie-weapon", RowType: "item"}
	node := impact("quest/start/give", "ImpactGiveItem",
		field("item", script.Value{Kind: script.ValueRef, Ref: item}),
		field("count", integer(2)),
		field("isCursed", script.Value{Kind: script.ValueBool, Bool: true}),
	)

	if _, err := run(host, node, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(host.commands))
	}
	got := host.commands[0]
	if got.Kind != script.CommandGiveItem || got.Ref != item || got.Count != 2 ||
		got.EntityID != playerID || !got.IsCursed {
		t.Fatalf("give command = %+v", got)
	}
	if got.ExecutionKey != "eval-1|quest/start/give|"+playerID {
		t.Fatalf("execution key = %q", got.ExecutionKey)
	}
}

func TestGiveItemRequiresExplicitMutableCurseState(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	node := impact("quest/start/give", "ImpactGiveItem", field("item", script.Value{
		Kind: script.ValueRef, Ref: script.Ref{ID: "item.x", RowType: "item"},
	}))
	_, err := run(host, node, newFrame())
	var refusal *script.RefusedError
	if !errors.As(err, &refusal) {
		t.Fatalf("Evaluate() error = %v, want refusal", err)
	}
	if len(host.commands) != 0 {
		t.Fatalf("commands = %+v, want none", host.commands)
	}
}

func TestImpactIfTargetReadsRetailSingularFields(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	node := impact("quest/if-target", "ImpactIfTarget",
		field("predicate", node(predicate("quest/if-target/predicate", "PredicateIsAvatar"))),
		field("impactsIf", nodeList(increaseQuestCount("quest/if-target/count", dressCountID))),
	)

	if _, err := run(host, node, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(host.commands) != 1 || host.commands[0].Kind != script.CommandIncreaseQuestCount {
		t.Fatalf("commands = %+v", host.commands)
	}
}

func TestBuffDetacherUsesTheAuthoredFalseDefault(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	node := impact("quest/detach", "BuffDetacher", field("buff", script.Value{
		Kind: script.ValueRef, Ref: script.Ref{ID: "buff.quest", RowType: "buff-resource"},
	}))
	if _, err := run(host, node, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(host.commands) != 1 || host.commands[0].Kind != script.CommandDetachBuff || host.commands[0].Bool {
		t.Fatalf("detach command = %+v", host.commands)
	}
}

func TestDeferredAuditedDefaultsFailClosed(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		field script.Field
	}{
		{name: "limit", field: field("limit", integer(2))},
		{name: "spell envelope", field: field("useSpellEnvelopeTargetEffects", script.Value{Kind: script.ValueBool, Bool: true})},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakeHost()
			node := impact("quest/deferred", "ImpactsDeferred",
				field("delay", duration(0)),
				field("impacts", nodeList(increaseQuestCount("quest/deferred/count", dressCountID))),
				testCase.field,
			)
			_, err := run(host, node, newFrame())
			var refusal *script.RefusedError
			if !errors.As(err, &refusal) {
				t.Fatalf("Evaluate() error = %v, want refusal", err)
			}
			if len(host.trace) != 0 {
				t.Fatalf("trace = %v, want no enqueue", host.trace)
			}
		})
	}
}

func TestCoverageAuditFindsImplementedBuildGapsWithoutExecuting(t *testing.T) {
	t.Parallel()
	known := impact("quest/known", "ImpactGiveItem",
		field("item", script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: "item.x", RowType: "item"}}),
		field("metadata", nodeList(&script.Node{
			Key: "quest/known/metadata", Family: script.FamilyBasic,
			Opcode: "Struct", Tier: script.TierInert,
		})),
	)
	unknown := impact("quest/unknown", "ImpactFutureAuthoritativeState")
	refused := impact("quest/refused", "ImpactUnsupported")
	refused.Tier = script.TierRefused

	report := script.AuditCoverage(known, unknown, refused)
	if report.Implemented != 2 || report.Inert != 1 || report.Refused != 1 {
		t.Fatalf("coverage counts = %+v", report)
	}
	if len(report.Gaps) != 1 || report.Gaps[0].Key != unknown.Key || report.Gaps[0].Opcode != unknown.Opcode {
		t.Fatalf("coverage gaps = %+v", report.Gaps)
	}
}

func TestCombatStateTriggerRunsTheMatchingBranch(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	trigger := triggerNode("trigger", "TriggerResource", field("effects", nodeList(
		effect("trigger/combat", "CombatStateTrigger",
			field("onEnter", nodeList(increaseQuestCount("trigger/combat/enter", dressCountID))),
			field("onLeave", nodeList(increaseQuestCount("trigger/combat/leave", ratCountID))),
		),
	)))
	evaluator := script.New(host, enabled())
	err := evaluator.Fire(t.Context(), script.Attachment{
		ID: "attachment", TriggerRef: script.Ref{ID: "trigger"}, Trigger: trigger,
		EntityID: playerID, Frame: newFrame(),
	}, script.Event{Kind: script.EventCombatStateChanged, EntityID: playerID, InCombat: false})
	if err != nil {
		t.Fatalf("Fire() error = %v", err)
	}
	if len(host.commands) != 1 || host.commands[0].Ref.ID != ratCountID {
		t.Fatalf("commands = %+v", host.commands)
	}
}
