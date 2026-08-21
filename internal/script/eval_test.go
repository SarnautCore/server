package script_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

// TestTheInterpreterIsInertUntilItsFlagIsSet pins the feature gate. ADR 0033 §2
// admits no caller for internal/script — M3-05 owns that amendment — so the
// default build must not evaluate anything even if someone wires it early.
func TestTheInterpreterIsInertUntilItsFlagIsSet(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, script.Options{})
	err := evaluator.Evaluate(context.Background(), increaseQuestCount("n1", dressCountID), newFrame())

	if !errors.Is(err, script.ErrDisabled) {
		t.Fatalf("Evaluate() error = %v, want ErrDisabled", err)
	}
	if len(host.trace) != 0 {
		t.Errorf("host trace = %v, want no calls: a disabled interpreter must not reach the world", host.trace)
	}
}

// TestIncreaseQuestCountIsTheWholeOfCountSpecial evaluates the leaf that every
// one of the tutorial's ten quest-count-special objectives ends at, and pins the
// execution key.
//
// The key matters more than it looks. Quest_1_20/CountId_1 has two independent
// incrementers — DressTrigger, when the player equips a weapon in MAINHAND or
// TWOHANDED, and BrokenDoorExploit, when the player interacts with the door —
// against a limit of 1. The host clamps, and the key is what lets it tell a
// replay of one increment from a genuine second one.
func TestIncreaseQuestCountIsTheWholeOfCountSpecial(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	_, err := run(host, increaseQuestCount("quest-1-20/startImpacts[0]", dressCountID), newFrame())
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}

	want := []string{
		"apply increase-quest-count " + dressCountID +
			" +1 on " + playerID + " key=eval-1|quest-1-20/startImpacts[0]",
	}
	assertTrace(t, host.trace, want)
}

// TestImpactIfCasterRebindsTheAddresseeBeforeItsPredicate is the shape of
// quest-1-20's startImpacts: eight ImpactIfCaster branches, one per character
// class, each handing the player that class's newbie weapon. Only the branch
// matching the caster's class may fire, and the predicate must read the caster
// rather than whatever the enclosing frame happened to address.
func TestImpactIfCasterRebindsTheAddresseeBeforeItsPredicate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		playerHas  string
		branchWant string
		wantApply  bool
	}{
		{
			name:       "the caster's own class branch fires",
			playerHas:  warriorClass,
			branchWant: warriorClass,
			wantApply:  true,
		},
		{
			name:       "a sibling class branch does not fire",
			playerHas:  warriorClass,
			branchWant: druidClass,
			wantApply:  false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			host.class[playerID] = testCase.playerHas

			node := impact("q1-20/if-caster", "ImpactIfCaster",
				field("predicates", nodeList(
					predicate("q1-20/if-caster/pred", "PredicateCharacterClass",
						field("characterClass", ref(testCase.branchWant)),
					),
				)),
				field("impacts", nodeList(
					increaseQuestCount("q1-20/if-caster/count", dressCountID),
				)),
			)

			_, err := run(host, node, newFrame())
			if err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}

			applied := false
			for _, line := range host.trace {
				if strings.HasPrefix(line, "apply ") {
					applied = true
				}
			}
			if applied != testCase.wantApply {
				t.Errorf(
					"applied = %t, want %t; trace = %v",
					applied, testCase.wantApply, host.trace,
				)
			}
			if len(host.trace) == 0 || !strings.HasPrefix(host.trace[0], "query class "+playerID) {
				t.Errorf(
					"first host call = %v, want a class query against the caster: "+
						"ImpactIfCaster must rebind the addressee before it asks",
					host.trace,
				)
			}
		})
	}
}

// TestDeferredChildrenAreScheduledNotRun covers the most common impact in the
// tutorial at 144 uses, including the nesting that forced ADR 0036's recursive
// representation.
//
// The rule under test is that a nested delay is relative to execution of its
// parent, not to the parent's original enqueue. So the outer node schedules only
// its immediate children, and the inner node's own delay is not yet added to
// anything — it will be added when the host runs that child.
func TestDeferredChildrenAreScheduledNotRun(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	inner := impact("q1-20/deferred/inner", "ImpactsDeferred",
		field("delay", duration(8000)),
		field("impacts", nodeList(increaseQuestCount("q1-20/deferred/inner/count", dressCountID))),
	)
	outer := impact("q1-20/deferred", "ImpactsDeferred",
		field("delay", duration(1500)),
		field("impacts", nodeList(
			increaseQuestCount("q1-20/deferred/first", dressCountID),
			inner,
		)),
	)

	_, err := run(host, outer, newFrame())
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}

	// 1_000_000 is the fake clock; 1500 is the outer delay. Both children are
	// due at the same moment and in stored order.
	want := []string{
		"enqueue ImpactIncreaseQuestCount due=1001500",
		"enqueue ImpactsDeferred due=1001500",
	}
	assertTrace(t, host.trace, want)
}

// TestAZeroDelayStillEntersTheQueue is the ordering rule, not a micro-optimisation
// question. quest-1-20's rewardImpacts open with an ImpactsDeferred of delay 0
// wrapping the whole Paladin choreography. Running it inline would make its
// effects land before work already queued at the same millisecond, and the order
// would then differ after a restart.
func TestAZeroDelayStillEntersTheQueue(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	node := impact("q1-20/rewardImpacts[0]", "ImpactsDeferred",
		field("delay", duration(0)),
		field("impacts", nodeList(increaseQuestCount("q1-20/rewardImpacts[0]/count", dressCountID))),
	)

	if _, err := run(host, node, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	assertTrace(t, host.trace, []string{"enqueue ImpactIncreaseQuestCount due=1000000"})
}

// TestTheThreeTiersBehaveDifferently is ADR 0036's coverage policy. A refused
// node must fail loudly and name the row, the node key and the opcode; an
// inert-and-counted node must not; and both must be visible in the census,
// because the per-tier count table is what makes widening coverage a reviewable
// diff.
func TestTheThreeTiersBehaveDifferently(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		tier      script.Tier
		opcode    string
		wantError bool
	}{
		{
			name:      "a refused node fails loudly",
			tier:      script.TierRefused,
			opcode:    "ImpactsToGroupMembers",
			wantError: true,
		},
		{
			name:      "an inert-and-counted node does nothing quietly",
			tier:      script.TierInert,
			opcode:    "ImpactsOverTime",
			wantError: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			node := impact("q1-20/tiered", testCase.opcode)
			node.Tier = testCase.tier

			evaluator, err := run(host, node, newFrame())
			if testCase.wantError {
				if err == nil {
					t.Fatalf("Evaluate() error = nil, want a refusal naming %s", testCase.opcode)
				}
				var refusal *script.RefusedError
				if !errors.As(err, &refusal) {
					t.Fatalf("Evaluate() error = %v, want a *script.RefusedError", err)
				}
				for _, part := range []string{node.Key, testCase.opcode, "quest.inst-league1.quest-1-20"} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf(
							"error = %q, want it to name %q: an operator has to know which row to fix",
							err, part,
						)
					}
				}
				if evaluator.Census().Refused[testCase.opcode] != 1 {
					t.Errorf("refused census = %v, want one hit for %s",
						evaluator.Census().Refused, testCase.opcode)
				}
				return
			}

			if err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}
			if len(host.trace) != 0 {
				t.Errorf("host trace = %v, want no calls: an inert node has no effect", host.trace)
			}
			if evaluator.Census().Inert[testCase.opcode] != 1 {
				t.Errorf("inert census = %v, want one hit for %s",
					evaluator.Census().Inert, testCase.opcode)
			}
		})
	}
}

// TestAnImplementedOpcodeWithNoHandlerIsRefusedAsABuildError separates a content
// problem from a build problem. A node tiered implemented that this build has no
// handler for is the tier table and the code disagreeing, and saying nothing
// would make a coverage regression invisible.
//
// ImpactGiveItem is a real instance of that gap rather than an invented one:
// ADR 0036's amendment puts it in the implemented tier at 54 uses, and this
// build registers no handler for it yet — the give-item path waits for the
// round that wires the inventory grant. All four TriggerAgent binders, which
// previously played this role, now have handlers.
func TestAnImplementedOpcodeWithNoHandlerIsRefusedAsABuildError(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	node := impact("q1-20/rewardImpacts[0]", "ImpactGiveItem")

	_, err := run(host, node, newFrame())
	if err == nil {
		t.Fatal("Evaluate() error = nil; a tier table ahead of the handler set must not pass silently")
	}
	if !strings.Contains(err.Error(), "registers no handler") {
		t.Errorf("error = %q, want it to say the build registers no handler", err)
	}
}

// TestStrictInertDemotesInertNodesToRefused gives a pre-merge job a way to prove
// that every inert-and-counted tiering was deliberate.
func TestStrictInertDemotesInertNodesToRefused(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	node := impact("q3-10/over-time", "ImpactsOverTime")
	node.Tier = script.TierInert

	evaluator := script.New(host, script.Options{Enabled: true, StrictInert: true})
	err := evaluator.Evaluate(context.Background(), node, newFrame())

	var refusal *script.RefusedError
	if !errors.As(err, &refusal) {
		t.Fatalf("Evaluate() error = %v, want a *script.RefusedError under StrictInert", err)
	}
}

// TestAnIncreaseQuestCountReadsItsDeltaFromValue pins the field name against the
// reflection schema. ImpactIncreaseQuestCount has exactly two fields, id and
// value, and value defaults to 1. Reading a field named "count" instead was
// silent, because 1486 of the uses omit the delta and every one of them would
// still have incremented by the default.
func TestAnIncreaseQuestCountReadsItsDeltaFromValue(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	node := impact("exploit/impacts[0]", "ImpactIncreaseQuestCount",
		field("id", ref(dressCountID)),
		field("value", integer(3)),
	)

	if _, err := run(host, node, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	assertTrace(t, host.trace, []string{
		"apply increase-quest-count " + dressCountID + " +3 on " + playerID +
			" key=eval-1|exploit/impacts[0]",
	})
}

func assertTrace(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("host trace = %v (%d calls), want %v (%d calls)", got, len(got), want, len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("host call %d = %q, want %q", index, got[index], want[index])
		}
	}
}
