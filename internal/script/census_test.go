package script_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

// TestTheCensusSeparatesRefusedFromInertOutsideTheWidenedTiers is the check that
// makes ADR 0036's coverage table mean something after the amendment widened it.
//
// The amendment added around fifty types to the implemented tier. Two categories
// deliberately stayed out, and they behave differently on purpose:
//
//   - ImpactsToGroupMembers is refused. Survey §8 puts group, pet, fairy and
//     faction impacts out of M3 because nothing in the tutorial closure reaches
//     them. Refused means it fails loudly, naming the row, and a refused node
//     reached by scripts/m3-tutorial-driver is a hard CI failure.
//   - EffectDisableMove is inert-and-counted. The amendment records that as a
//     stated concession: quest 3-10's AnimationBuff pins the player through a
//     cutscene, omitting it cannot corrupt authoritative state — which is the
//     inert tier's admission test — but it does let the player walk out of the
//     scripted beat.
//
// Both must land in the census, because the per-tier count table is the only
// thing that makes a coverage change a reviewable diff.
func TestTheCensusSeparatesRefusedFromInertOutsideTheWidenedTiers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		opcode    string
		tier      script.Tier
		wantError bool
	}{
		{
			name:      "a group impact is refused and named",
			opcode:    "ImpactsToGroupMembers",
			tier:      script.TierRefused,
			wantError: true,
		},
		{
			name:   "a movement lockout is inert and counted",
			opcode: "EffectDisableMove",
			tier:   script.TierInert,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			// The unsupported node sits inside a tree that also does real work,
			// which is how content actually reaches it: quest 3-10's buff and
			// quest 1-20's rewards are lists, not single nodes.
			outsider := impact("quest/impacts[1]", testCase.opcode)
			outsider.Tier = testCase.tier
			outsider.Fields = []script.Field{field("impacts", nodeList(
				increaseQuestCount("quest/impacts[1]/impacts[0]", dressCountID),
			))}

			tree := impact("quest/impacts[0]", "ImpactsDeferred",
				field("delay", duration(0)),
				field("impacts", nodeList(
					increaseQuestCount("quest/impacts[0]/impacts[0]", dressCountID),
				)),
			)
			// An unconditional ImpactIfTarget, which is how the data spells a
			// plain ordered list: no predicates means "always". A deferred node
			// would not do — its children are enqueued rather than evaluated, so
			// nothing inside it would be reached in this call at all.
			root := impact("quest", "ImpactIfTarget",
				field("impacts", nodeList(tree, outsider)),
			)

			evaluator, err := run(host, root, newFrame())
			census := evaluator.Census()

			if testCase.wantError {
				var refusal *script.RefusedError
				if !errors.As(err, &refusal) {
					t.Fatalf("Evaluate() error = %v, want a *script.RefusedError", err)
				}
				if census.Refused[testCase.opcode] != 1 {
					t.Errorf("refused census = %v, want one hit for %s", census.Refused, testCase.opcode)
				}
			} else {
				if err != nil {
					t.Fatalf("Evaluate() error = %v, want nil", err)
				}
				if census.Inert[testCase.opcode] != 1 {
					t.Errorf("inert census = %v, want one hit for %s", census.Inert, testCase.opcode)
				}
				if len(census.Refused) != 0 {
					t.Errorf("refused census = %v, want empty", census.Refused)
				}
			}

			// Neither tier descends. An inert node's children have no meaning
			// without it, and a refused node fails before any child runs — so
			// the increment nested inside the outsider must never be counted and
			// must never reach the host.
			if census.Implemented["ImpactIncreaseQuestCount"] != 0 {
				t.Errorf(
					"census counted %d ImpactIncreaseQuestCount; a %s node must not descend into its children",
					census.Implemented["ImpactIncreaseQuestCount"], testCase.tier,
				)
			}
			for _, line := range host.trace {
				if strings.Contains(line, "quest/impacts[1]") {
					t.Errorf("host trace = %v, want nothing from inside the unsupported node", host.trace)
				}
			}
		})
	}
}

// TestTheCensusCountsEveryFamilyItWalks pins that the widened families reach the
// census too. Before the amendment the tier table had rows for impact,
// predicate, effect, addressee finder and scaler; calcer and basic had none, so
// HealthTrigger's own operands defaulted to refused and the first rat death in
// quest 1-30 failed the build. If a family is walked but not counted, that
// regression becomes invisible again.
func TestTheCensusCountsEveryFamilyItWalks(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, enabled())
	attachment := script.Attachment{
		TriggerRef: script.Ref{ID: ratKillerID},
		Trigger:    ratKiller(),
		EntityID:   firstRatID,
		Frame:      newFrame(),
	}

	err := evaluator.Fire(t.Context(), attachment, script.Event{
		Kind: script.EventHealthChanged, EntityID: firstRatID,
		CauseID: playerID, PreviousHealth: 12, Health: 0,
	})
	if err != nil {
		t.Fatalf("Fire() error = %v, want nil", err)
	}

	// One walk of RatKiller: the trigger document, its HealthTrigger effect, the
	// FullHealthCalcer threshold, the ReturningImpact and the increment. The
	// FloatZero healthOff is not counted because RatKiller has no impactsOff, so
	// the off-threshold is never evaluated.
	want := map[string]int{
		"TriggerResource":          1,
		"HealthTrigger":            1,
		"FullHealthCalcer":         1,
		"ReturningImpact":          1,
		"ImpactIncreaseQuestCount": 1,
	}
	for opcode, count := range want {
		if evaluator.Census().Implemented[opcode] != count {
			t.Errorf(
				"census[%s] = %d, want %d; full census = %v",
				opcode, evaluator.Census().Implemented[opcode], count,
				evaluator.Census().Implemented,
			)
		}
	}
	if len(evaluator.Census().Implemented) != len(want) {
		t.Errorf(
			"census reached %v, want exactly %d opcodes",
			evaluator.Census().Implemented, len(want),
		)
	}
}
