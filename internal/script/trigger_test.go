package script_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

// Content ids for the two count-special shapes. DressTrigger is quest 1-20's
// equip objective and the M3-09 golden fixture; RatKiller lives in
// Mechanics/Spells/QuestSpells/IL_QuestSpells and is quest 1-30's kill counter,
// which is the directory ADR 0036's amendment added to the census scope after
// the survey found three objectives with no incrementer in scope.
const (
	dressTriggerID  = "trigger.inst-league1.quest-1-20.dress-trigger"
	ratKillerID     = "trigger.il-questspells.rat-killer"
	ratSpawnTableID = "spawntable.inst-league1.rat1-1"

	firstRatID  = "mob.inst-league1.rat.001"
	secondRatID = "mob.inst-league1.rat.002"
)

// dressTrigger reproduces Quest_1_20/DressTrigger.(TriggerResource).xdb: two
// EquipTrigger effects, MAINHAND and TWOHANDED, each holding a Switch whose
// impactsOn increments CountId_1. Two slots because a two-handed weapon does not
// occupy the main hand and the objective is "arm yourself" either way.
func dressTrigger() *script.Node {
	slotEffect := func(slot string) *script.Node {
		key := "dress-trigger/effects[" + slot + "]"
		return effect(key, "EquipTrigger",
			field("slot", text(slot)),
			field("effects", nodeList(
				effect(key+"/switch", "Switch",
					field("impactsOn", nodeList(
						increaseQuestCount(key+"/switch/impactsOn[0]", dressCountID),
					)),
				),
			)),
		)
	}
	return triggerNode("dress-trigger", "TriggerResource",
		field("effects", nodeList(slotEffect("MAINHAND"), slotEffect("TWOHANDED"))),
	)
}

// ratKiller reproduces RatKiller.(TriggerResource).xdb: a HealthTrigger whose
// healthOn is FullHealthCalcer(multiplier=0) — zero health, which is death — and
// whose healthOff is FloatZero. Its impactsOn is a ReturningImpact wrapping
// ImpactIncreaseQuestCount, which is what moves the credit from the corpse to
// the killer.
func ratKiller() *script.Node {
	return triggerNode("rat-killer", "TriggerResource",
		field("effects", nodeList(
			effect("rat-killer/effects[0]", "HealthTrigger",
				field("healthOn", node(
					calcer("rat-killer/effects[0]/healthOn", "FullHealthCalcer",
						field("multiplier", integer(0)),
					),
				)),
				field("healthOff", node(
					basic("rat-killer/effects[0]/healthOff", "FloatZero"),
				)),
				field("impactsOn", nodeList(
					impact("rat-killer/effects[0]/impactsOn[0]", "ReturningImpact",
						field("impact", nodeList(
							increaseQuestCount("rat-killer/effects[0]/impactsOn[0]/impact", ratCountID),
						)),
					),
				)),
			),
		)),
	)
}

// deliverTo fires an event at the attachment the evaluator itself created,
// with the trigger document the host would have loaded from the referenced row.
// Going through host.attached rather than through a hand-built Attachment is
// deliberate: it is what makes the two halves of the shape one trace instead of
// two independent tests that agree by coincidence.
func deliverTo(
	t *testing.T, host *fakeHost, evaluator *script.Evaluator,
	index int, document *script.Node, event script.Event,
) error {
	t.Helper()

	if index >= len(host.attached) {
		t.Fatalf("attachment %d does not exist; the host recorded %d", index, len(host.attached))
	}
	attachment := host.attached[index]
	attachment.Trigger = document
	return evaluator.Fire(context.Background(), attachment, event)
}

// TestShapeAEquipTriggerCountsThroughItsSwitch is the first count-special shape
// end to end: quest accepted, trigger bound to the player through
// TriggerAgentSelf, player equips a weapon, EquipTrigger's gate opens, the
// Switch inside it runs impactsOn once, and the counter reaches its limit of 1.
//
// The slot table is the point. DressTrigger names two slots and the tutorial
// hands out one-handed and two-handed newbie weapons depending on class, so a
// build that matched only MAINHAND would leave every two-handed class unable to
// finish quest 1-20.
func TestShapeAEquipTriggerCountsThroughItsSwitch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		slot      string
		equipped  bool
		wantCount bool
	}{
		{name: "a main-hand weapon completes the objective", slot: "MAINHAND", equipped: true, wantCount: true},
		{name: "a two-handed weapon completes it too", slot: "TWOHANDED", equipped: true, wantCount: true},
		{name: "an unrelated slot does not", slot: "FEET", equipped: true, wantCount: false},
		{name: "unequipping does not", slot: "MAINHAND", equipped: false, wantCount: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			binder := triggerNode("quest-1-20/triggerAgents[0]", "TriggerAgentSelf",
				field("trigger", ref(dressTriggerID)),
			)

			evaluator, err := run(host, binder, newFrame())
			if err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}

			attachTrace := "apply attach-trigger " + dressTriggerID +
				" to " + playerID + " key=eval-1|quest-1-20/triggerAgents[0]|" + playerID
			assertTrace(t, host.trace, []string{attachTrace})

			err = deliverTo(t, host, evaluator, 0, dressTrigger(), script.Event{
				Kind:     script.EventEquipChanged,
				EntityID: playerID,
				Slot:     testCase.slot,
				Equipped: testCase.equipped,
			})
			if err != nil {
				t.Fatalf("Fire() error = %v, want nil", err)
			}

			want := []string{attachTrace}
			if testCase.wantCount {
				want = append(want, "apply increase-quest-count "+dressCountID+
					" +1 on "+playerID+
					" key=eval-1|dress-trigger/effects["+testCase.slot+"]/switch/impactsOn[0]")
			}
			assertTrace(t, host.trace, want)
		})
	}
}

// TestShapeBKillCountingCreditsTheKillerNotTheCorpse is the second shape end to
// end: the quest's startImpacts resolve a spawn table, attach RatKiller to every
// mob in it and tag each for kill credit; then a rat dies and the counter is
// incremented on the player.
//
// The addressee assertion is the whole test. Inside the trigger the addressee is
// the dying rat, and only ReturningImpact moves it to the caster. ADR 0036's
// amendment says it outright: a test that attributes the count to the addressee
// passes trivially and proves nothing.
func TestShapeBKillCountingCreditsTheKillerNotTheCorpse(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	host.resolved["ImpactFindSpawnTable"] = []string{firstRatID, secondRatID}
	host.maxHealth[firstRatID] = 40

	startImpacts := impact("quest-1-30/startImpacts[0]", "ImpactFindSpawnTable",
		field("spawnResource", ref(ratSpawnTableID)),
		field("impacts", nodeList(
			impact("quest-1-30/startImpacts[0]/impacts[0]", "ImpactAttachTrigger",
				field("trigger", ref(ratKillerID)),
			),
			impact("quest-1-30/startImpacts[0]/impacts[1]", "TagMobForKill"),
		)),
	)

	evaluator, err := run(host, startImpacts, newFrame())
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}

	attachKey := "eval-1|quest-1-30/startImpacts[0]/impacts[0]|"
	tagKey := "eval-1|quest-1-30/startImpacts[0]/impacts[1]|"
	setup := []string{
		"resolve ImpactFindSpawnTable " + ratSpawnTableID + " -> " + firstRatID + "," + secondRatID,
		"apply attach-trigger " + ratKillerID + " to " + firstRatID + " key=" + attachKey + firstRatID,
		"apply tag-mob-for-kill " + firstRatID + " key=" + tagKey + firstRatID,
		"apply attach-trigger " + ratKillerID + " to " + secondRatID + " key=" + attachKey + secondRatID,
		"apply tag-mob-for-kill " + secondRatID + " key=" + tagKey + secondRatID,
	}
	assertTrace(t, host.trace, setup)

	// The player kills the first rat: health crosses from 12 to 0 with the
	// player as the cause.
	err = deliverTo(t, host, evaluator, 0, ratKiller(), script.Event{
		Kind:           script.EventHealthChanged,
		EntityID:       firstRatID,
		CauseID:        playerID,
		PreviousHealth: 12,
		Health:         0,
	})
	if err != nil {
		t.Fatalf("Fire() error = %v, want nil", err)
	}

	want := append(setup,
		"apply increase-quest-count "+ratCountID+" +1 on "+playerID+
			" key=eval-1|rat-killer/effects[0]/impactsOn[0]/impact",
	)
	assertTrace(t, host.trace, want)

	// A multiplier of zero is zero times anything, so the threshold needs no
	// round trip to the host. No max-health query may appear in the trace.
	for _, line := range host.trace {
		if strings.HasPrefix(line, "query max-health") {
			t.Errorf(
				"trace = %v, want no max-health query: FullHealthCalcer(multiplier=0) is zero "+
					"without asking, which is what lets a dying mob's death test not depend on "+
					"the host still answering for it",
				host.trace,
			)
		}
	}
}

// TestAHealthTriggerFiresOnTheCrossingNotTheLevel is why the event carries the
// health before the change as well as after it. A corpse sits at zero, and the
// host may deliver further health events for it — a resurrect, a despawn, a
// second hit landing in the same tick. A level test would re-run impactsOn every
// time, and quest 1-30's limit of 3 would complete on one rat.
func TestAHealthTriggerFiresOnTheCrossingNotTheLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		previous int64
		current  int64
		wantFire bool
	}{
		{name: "the killing blow fires", previous: 12, current: 0, wantFire: true},
		{name: "a wound does not", previous: 40, current: 12, wantFire: false},
		{name: "a corpse does not fire again", previous: 0, current: 0, wantFire: false},
		{name: "overkill below zero still fires once", previous: 3, current: -9, wantFire: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			evaluator := script.New(host, enabled())
			attachment := script.Attachment{
				TriggerRef: script.Ref{ID: ratKillerID},
				Trigger:    ratKiller(),
				EntityID:   firstRatID,
				Frame:      newFrame(),
			}

			err := evaluator.Fire(context.Background(), attachment, script.Event{
				Kind:           script.EventHealthChanged,
				EntityID:       firstRatID,
				CauseID:        playerID,
				PreviousHealth: testCase.previous,
				Health:         testCase.current,
			})
			if err != nil {
				t.Fatalf("Fire() error = %v, want nil", err)
			}

			fired := len(host.trace) > 0
			if fired != testCase.wantFire {
				t.Errorf("fired = %t, want %t; trace = %v", fired, testCase.wantFire, host.trace)
			}
		})
	}
}

// TestFullHealthCalcerScalesTheThresholdByItsMultiplier pins the calcer's
// semantics as a multiplier over the bearer's full health rather than as a
// constant. Zero is death and is the only multiplier the tutorial's kill
// counters use, but the family exists to express a fraction, and a build that
// treated FullHealthCalcer as "zero" would be right for the wrong reason and
// would break on the first wounded-threshold trigger.
func TestFullHealthCalcerScalesTheThresholdByItsMultiplier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		multiplier script.Value
		previous   int64
		current    int64
		wantFire   bool
	}{
		{name: "half of 40 is 20, and 25 to 18 crosses it", multiplier: decimal(5, 1), previous: 25, current: 18, wantFire: true},
		{name: "half of 40 is 20, and 30 to 25 does not", multiplier: decimal(5, 1), previous: 30, current: 25, wantFire: false},
		{name: "a quarter of 40 is 10, and 12 to 9 crosses it", multiplier: decimal(25, 2), previous: 12, current: 9, wantFire: true},
		// A threshold at full health is a trigger that is already on when it
		// attaches, so nothing can cross into it later. The reading is a state
		// with a downward edge — health at or below healthOn turns it on — and
		// that is the reading death depends on: with multiplier 0 the threshold
		// is 0 and a corpse is at 0, so the test has to be "at or below" rather
		// than "below". The same "at or below" makes a full-health threshold
		// degenerate, which is consistent rather than a special case.
		{name: "a threshold at full health is already on and never crosses", multiplier: integer(1), previous: 40, current: 39, wantFire: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			host.maxHealth[firstRatID] = 40
			evaluator := script.New(host, enabled())

			document := triggerNode("wounded", "TriggerResource",
				field("effects", nodeList(
					effect("wounded/effects[0]", "HealthTrigger",
						field("healthOn", node(
							calcer("wounded/effects[0]/healthOn", "FullHealthCalcer",
								field("multiplier", testCase.multiplier),
							),
						)),
						field("impactsOn", nodeList(
							increaseQuestCount("wounded/effects[0]/impactsOn[0]", ratCountID),
						)),
					),
				)),
			)

			err := evaluator.Fire(context.Background(), script.Attachment{
				TriggerRef: script.Ref{ID: "trigger.wounded"},
				Trigger:    document,
				EntityID:   firstRatID,
				Frame:      newFrame(),
			}, script.Event{
				Kind:           script.EventHealthChanged,
				EntityID:       firstRatID,
				CauseID:        playerID,
				PreviousHealth: testCase.previous,
				Health:         testCase.current,
			})
			if err != nil {
				t.Fatalf("Fire() error = %v, want nil", err)
			}

			applied := false
			for _, line := range host.trace {
				if strings.HasPrefix(line, "apply ") {
					applied = true
				}
			}
			if applied != testCase.wantFire {
				t.Errorf("applied = %t, want %t; trace = %v", applied, testCase.wantFire, host.trace)
			}
			if len(host.trace) == 0 || !strings.HasPrefix(host.trace[0], "query max-health "+firstRatID) {
				t.Errorf(
					"first host call = %v, want a max-health query against the bearer: "+
						"a non-zero multiplier has to read full health",
					host.trace,
				)
			}
		})
	}
}

// TestDetachRunsTheSwitchOffBranchThenReportsTheDetach covers the other half of
// the attachment lifecycle. ADR 0036 says a Switch's impactsOff "runs once when
// it detaches or expires", so the order matters: the off-branch runs while the
// attachment still exists, and the detach command follows it.
func TestDetachRunsTheSwitchOffBranchThenReportsTheDetach(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, enabled())
	document := triggerNode("timer", "TriggerResource",
		field("effects", nodeList(
			effect("timer/effects[0]", "Switch",
				field("impactsOn", nodeList(
					increaseQuestCount("timer/effects[0]/impactsOn[0]", dressCountID),
				)),
				field("impactsOff", nodeList(
					increaseQuestCount("timer/effects[0]/impactsOff[0]", ratCountID),
				)),
			),
		)),
	)

	err := evaluator.Detach(context.Background(), script.Attachment{
		TriggerRef: script.Ref{ID: dressTriggerID},
		Trigger:    document,
		EntityID:   playerID,
		Frame:      newFrame(),
	})
	if err != nil {
		t.Fatalf("Detach() error = %v, want nil", err)
	}

	assertTrace(t, host.trace, []string{
		"apply increase-quest-count " + ratCountID + " +1 on " + playerID +
			" key=eval-1|timer/effects[0]/impactsOff[0]",
		"apply detach-trigger " + dressTriggerID + " from " + playerID +
			" key=eval-1|detach|" + dressTriggerID,
	})
}

// TestAHealthTriggerRunsItsOffImpactsAtDetachNotOnRecovery pins a correction the
// data forced. The reflection schema describes HealthTrigger as firing impacts
// when health drops below a level, and says of the second set: "called on
// detach, if the first was called first — their meaning is cleanup".
//
// So impactsOff is not an upward crossing of healthOff. Healing a mob back past
// the threshold must run nothing; ending the attachment must run the cleanup.
func TestAHealthTriggerRunsItsOffImpactsAtDetachNotOnRecovery(t *testing.T) {
	t.Parallel()

	document := triggerNode("wounded", "TriggerResource",
		field("effects", nodeList(
			effect("wounded/effects[0]", "HealthTrigger",
				field("healthOn", node(
					calcer("wounded/effects[0]/healthOn", "FullHealthCalcer",
						field("multiplier", decimal(5, 1)),
					),
				)),
				field("healthOff", node(
					basic("wounded/effects[0]/healthOff", "FloatZero"),
				)),
				field("impactsOff", nodeList(
					increaseQuestCount("wounded/effects[0]/impactsOff[0]", ratCountID),
				)),
			),
		)),
	)
	attachment := script.Attachment{
		TriggerRef: script.Ref{ID: "trigger.wounded"},
		Trigger:    document,
		EntityID:   firstRatID,
		Frame:      newFrame(),
	}

	t.Run("healing back past the threshold runs nothing", func(t *testing.T) {
		t.Parallel()

		host := newFakeHost()
		host.maxHealth[firstRatID] = 40
		evaluator := script.New(host, enabled())

		err := evaluator.Fire(t.Context(), attachment, script.Event{
			Kind: script.EventHealthChanged, EntityID: firstRatID,
			CauseID: playerID, PreviousHealth: 8, Health: 34,
		})
		if err != nil {
			t.Fatalf("Fire() error = %v, want nil", err)
		}
		for _, line := range host.trace {
			if strings.HasPrefix(line, "apply ") {
				t.Errorf("host trace = %v, want no impacts: impactsOff is cleanup, not recovery", host.trace)
			}
		}
	})

	t.Run("detaching runs the cleanup", func(t *testing.T) {
		t.Parallel()

		host := newFakeHost()
		evaluator := script.New(host, enabled())

		if err := evaluator.Detach(t.Context(), attachment); err != nil {
			t.Fatalf("Detach() error = %v, want nil", err)
		}
		// The cleanup lands on the bearer, not on the player: this fixture has
		// no ReturningImpact, and without one the addressee is whoever the
		// trigger is attached to. That is exactly the distinction shape B needs
		// ReturningImpact for.
		assertTrace(t, host.trace, []string{
			"apply increase-quest-count " + ratCountID + " +1 on " + firstRatID +
				" key=eval-1|wounded/effects[0]/impactsOff[0]",
			"apply detach-trigger trigger.wounded from " + firstRatID +
				" key=eval-1|detach|trigger.wounded",
		})
	})
}

// TestFireRefusesAMisdirectedEvent guards the one thing the host can get wrong
// that the evaluator can still catch. An attachment names its bearer; delivering
// another entity's event to it would run a trigger against the wrong character,
// and for shape B that is a quest count on a bystander.
func TestFireRefusesAMisdirectedEvent(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, enabled())
	err := evaluator.Fire(context.Background(), script.Attachment{
		TriggerRef: script.Ref{ID: ratKillerID},
		Trigger:    ratKiller(),
		EntityID:   firstRatID,
		Frame:      newFrame(),
	}, script.Event{
		Kind:           script.EventHealthChanged,
		EntityID:       secondRatID,
		CauseID:        playerID,
		PreviousHealth: 12,
		Health:         0,
	})

	if err == nil {
		t.Fatal("Fire() error = nil, want a refusal: an event must reach only its own bearer")
	}
	if len(host.trace) != 0 {
		t.Errorf("host trace = %v, want no calls", host.trace)
	}
}

// TestTheTriggerSurfaceHonoursTheFeatureFlag keeps the second and third entry
// points behind the same gate as Evaluate. ADR 0033 §2 still admits no caller
// for this package, and a trigger path that ran while the flag was off would be
// a way in through the side door.
func TestTheTriggerSurfaceHonoursTheFeatureFlag(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, script.Options{})
	attachment := script.Attachment{
		TriggerRef: script.Ref{ID: ratKillerID},
		Trigger:    ratKiller(),
		EntityID:   firstRatID,
		Frame:      newFrame(),
	}

	fireErr := evaluator.Fire(context.Background(), attachment, script.Event{
		Kind: script.EventHealthChanged, EntityID: firstRatID,
		CauseID: playerID, PreviousHealth: 12, Health: 0,
	})
	if !errors.Is(fireErr, script.ErrDisabled) {
		t.Errorf("Fire() error = %v, want ErrDisabled", fireErr)
	}

	detachErr := evaluator.Detach(context.Background(), attachment)
	if !errors.Is(detachErr, script.ErrDisabled) {
		t.Errorf("Detach() error = %v, want ErrDisabled", detachErr)
	}
	if len(host.trace) != 0 {
		t.Errorf("host trace = %v, want no calls", host.trace)
	}
}
