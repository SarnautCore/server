package script_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

// Content ids for the two mobWorld-scoped trigger agents. Quest_4_10 binds
// QuestCompl to the LI_Necromancer mob world through TriggerAgentSimple;
// Quest_2_10 binds GibberSummon to the RuffianMageMiniboss2_2 world through
// TriggerAgentOnTagged, after its startImpacts have tagged that world's spawn
// table. Both agents carry exactly one field over the TriggerAgentResource
// base — mobWorld — plus OnTagged's unused onSelf.
const (
	questComplID   = "trigger.inst-league1.quest-4-10.quest-compl"
	gibberSummonID = "trigger.inst-league1.hum-mobs.gibber-summon"

	necromancerWorldID = "mob.inst-league1.kania-male.li-necromancer"
	minibossWorldID    = "mob.inst-league1.hum-mobs.ruffian-mage-miniboss2-2"

	necromancerID   = "mob.inst-league1.necromancer.001"
	minibossID      = "mob.inst-league1.miniboss.001"
	quest410CountID = "questcount.inst-league1.quest-4-10.count-id-1"
)

// questCompl reproduces Quest_4_10/QuestCompl.(TriggerResource).xdb: a
// HealthTrigger whose healthOn is FullHealthCalcer(multiplier=0.1) — the
// necromancer reduced to a tenth of full health, not dead — and whose impactsOn
// is a ReturningImpact wrapping ImpactIncreaseQuestCount. No healthOff, no
// nested effects: the document is exactly this.
func questCompl() *script.Node {
	return triggerNode("quest-compl", "TriggerResource",
		field("effects", nodeList(
			effect("quest-compl/effects[0]", "HealthTrigger",
				field("healthOn", node(
					calcer("quest-compl/effects[0]/healthOn", "FullHealthCalcer",
						field("multiplier", decimal(1, 1)),
					),
				)),
				field("impactsOn", nodeList(
					impact("quest-compl/effects[0]/impactsOn[0]", "ReturningImpact",
						field("impact", nodeList(
							increaseQuestCount("quest-compl/effects[0]/impactsOn[0]/impact", quest410CountID),
						)),
					),
				)),
			),
		)),
	)
}

// gibberSummon reproduces Characters/HumMobs/Instances/InstLeague1/
// GibberSummon.(TriggerResource).xdb: a HealthTrigger at half health whose
// impactsOn defers a TagMobForKill by three seconds. It is the OnTagged
// trigger of quest 2-10's miniboss.
func gibberSummon() *script.Node {
	return triggerNode("gibber-summon", "TriggerResource",
		field("effects", nodeList(
			effect("gibber-summon/effects[0]", "HealthTrigger",
				field("healthOn", node(
					calcer("gibber-summon/effects[0]/healthOn", "FullHealthCalcer",
						field("multiplier", decimal(5, 1)),
					),
				)),
				field("impactsOn", nodeList(
					impact("gibber-summon/effects[0]/impactsOn[0]", "ImpactsDeferred",
						field("delay", duration(3000)),
						field("impacts", nodeList(
							impact("gibber-summon/effects[0]/impactsOn[0]/impacts[0]", "TagMobForKill"),
						)),
					),
				)),
			),
		)),
	)
}

// TestTriggerAgentSimpleAttachesAcrossAMobWorld is quest 4-10's shape end to
// end. The binder names a mob world, not an entity, so the evaluator emits a
// spawn-scoped attachment and the host decides which live necromancers — now
// and after a respawn — carry QuestCompl. The host then materializes a
// per-entity view and delivers a wounding, and the trigger fires at a tenth of
// full health: the objective is "bring the necromancer low", not "kill him".
func TestTriggerAgentSimpleAttachesAcrossAMobWorld(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		previous int64
		current  int64
		wantFire bool
	}{
		{name: "crossing a tenth of full health fires", previous: 6, current: 3, wantFire: true},
		{name: "the killing blow after the crossing does not re-fire", previous: 3, current: 0, wantFire: false},
		{name: "a wound above the threshold does not", previous: 40, current: 24, wantFire: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			host.maxHealth[necromancerID] = 40

			binder := triggerNode("quest-4-10/triggerAgents[0]", "TriggerAgentSimple",
				field("trigger", ref(questComplID)),
				field("mobWorld", ref(necromancerWorldID)),
			)

			evaluator, err := run(host, binder, newFrame())
			if err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}

			attachTrace := "apply attach-trigger " + questComplID +
				" to mobworld:" + necromancerWorldID +
				" key=eval-1|quest-4-10/triggerAgents[0]|" + necromancerWorldID
			assertTrace(t, host.trace, []string{attachTrace})

			attachment := host.attached[0]
			if attachment.EntityID != "" || attachment.MobWorld.ID != necromancerWorldID || attachment.OnlyTagged {
				t.Fatalf(
					"attachment = %+v, want a spawn scope over %s with no tagged restriction",
					attachment, necromancerWorldID,
				)
			}

			// The host materializes the spawn scope onto one live necromancer:
			// same attachment, bearer filled in, trigger row loaded.
			attachment.EntityID = necromancerID
			attachment.Trigger = questCompl()
			err = evaluator.Fire(context.Background(), attachment, script.Event{
				Kind:           script.EventHealthChanged,
				EntityID:       necromancerID,
				CauseID:        playerID,
				PreviousHealth: testCase.previous,
				Health:         testCase.current,
			})
			if err != nil {
				t.Fatalf("Fire() error = %v, want nil", err)
			}

			want := []string{attachTrace, "query max-health " + necromancerID + " -> 40"}
			if testCase.wantFire {
				want = append(want, "apply increase-quest-count "+quest410CountID+
					" +1 on "+playerID+
					" key=eval-1|quest-compl/effects[0]/impactsOn[0]/impact")
			}
			assertTrace(t, host.trace, want)
		})
	}
}

// TestTriggerAgentOnTaggedNarrowsTheScopeAndDefersTheTag is quest 2-10's
// shape. The attachment carries the tagged-only restriction — the host may
// materialize it only onto mobs TagMobForKill has marked — and GibberSummon's
// half-health crossing schedules a deferred TagMobForKill rather than running
// one, so the enqueue is the observable outcome.
func TestTriggerAgentOnTaggedNarrowsTheScopeAndDefersTheTag(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	host.maxHealth[minibossID] = 40

	binder := triggerNode("quest-2-10/triggerAgents[0]", "TriggerAgentOnTagged",
		field("trigger", ref(gibberSummonID)),
		field("mobWorld", ref(minibossWorldID)),
	)

	evaluator, err := run(host, binder, newFrame())
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}

	attachTrace := "apply attach-trigger " + gibberSummonID +
		" to mobworld:" + minibossWorldID + " tagged-only" +
		" key=eval-1|quest-2-10/triggerAgents[0]|" + minibossWorldID
	assertTrace(t, host.trace, []string{attachTrace})

	attachment := host.attached[0]
	if !attachment.OnlyTagged {
		t.Fatalf("attachment = %+v, want the tagged-only restriction", attachment)
	}

	attachment.EntityID = minibossID
	attachment.Trigger = gibberSummon()
	err = evaluator.Fire(context.Background(), attachment, script.Event{
		Kind:           script.EventHealthChanged,
		EntityID:       minibossID,
		CauseID:        playerID,
		PreviousHealth: 25,
		Health:         15,
	})
	if err != nil {
		t.Fatalf("Fire() error = %v, want nil", err)
	}

	assertTrace(t, host.trace, []string{
		attachTrace,
		"query max-health " + minibossID + " -> 40",
		"enqueue TagMobForKill due=1003000",
	})
}

// TestASpawnScopedAttachmentMustBeMaterializedBeforeFiring guards the seam the
// scope introduces. A spawn-scoped attachment names no bearer; delivering an
// event straight to it would run the trigger with no addressee, so Fire
// refuses until the host has filled the entity in.
func TestASpawnScopedAttachmentMustBeMaterializedBeforeFiring(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, enabled())

	err := evaluator.Fire(context.Background(), script.Attachment{
		TriggerRef: script.Ref{ID: questComplID},
		Trigger:    questCompl(),
		MobWorld:   script.Ref{ID: necromancerWorldID},
		Frame:      newFrame(),
	}, script.Event{
		Kind:           script.EventHealthChanged,
		EntityID:       necromancerID,
		CauseID:        playerID,
		PreviousHealth: 6,
		Health:         3,
	})

	if err == nil || !strings.Contains(err.Error(), "spawn-scoped") {
		t.Fatalf("Fire() error = %v, want a refusal naming the unmaterialized spawn scope", err)
	}
	if len(host.trace) != 0 {
		t.Errorf("host trace = %v, want no calls", host.trace)
	}
}

// TestTheMobWorldAgentsRefuseWhatTheDataDoesNotSpell pins the two ways a
// binder can be malformed or out of scope: no mobWorld reference, and
// OnTagged's onSelf field set — which no tutorial document uses, so its
// semantics are unverified and refusal beats a guess.
func TestTheMobWorldAgentsRefuseWhatTheDataDoesNotSpell(t *testing.T) {
	t.Parallel()

	boolValue := func(value bool) script.Value {
		return script.Value{Kind: script.ValueBool, Bool: value}
	}

	cases := []struct {
		name       string
		binder     *script.Node
		wantReason string
	}{
		{
			name: "a Simple binder with no mobWorld is refused",
			binder: triggerNode("agents[0]", "TriggerAgentSimple",
				field("trigger", ref(questComplID)),
			),
			wantReason: "mobWorld",
		},
		{
			name: "onSelf=true is outside the implemented shape",
			binder: triggerNode("agents[0]", "TriggerAgentOnTagged",
				field("trigger", ref(gibberSummonID)),
				field("mobWorld", ref(minibossWorldID)),
				field("onSelf", boolValue(true)),
			),
			wantReason: "onSelf",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			_, err := run(host, testCase.binder, newFrame())

			var refusal *script.RefusedError
			if !errors.As(err, &refusal) {
				t.Fatalf("Evaluate() error = %v, want a *script.RefusedError", err)
			}
			if !strings.Contains(refusal.Reason, testCase.wantReason) {
				t.Errorf("refusal reason = %q, want it to name %q", refusal.Reason, testCase.wantReason)
			}
			if len(host.attached) != 0 {
				t.Errorf("attachments = %v, want none", host.attached)
			}
		})
	}
}

// TestOnSelfFalseIsTheSchemaDefaultAndBinds keeps the refusal surgical: a
// document that spells onSelf as false spells the default, and refusing it
// would reject content that means exactly what an omitted field means.
func TestOnSelfFalseIsTheSchemaDefaultAndBinds(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	binder := triggerNode("agents[0]", "TriggerAgentOnTagged",
		field("trigger", ref(gibberSummonID)),
		field("mobWorld", ref(minibossWorldID)),
		field("onSelf", script.Value{Kind: script.ValueBool, Bool: false}),
	)

	if _, err := run(host, binder, newFrame()); err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if len(host.attached) != 1 {
		t.Fatalf("attachments = %d, want one", len(host.attached))
	}
}
