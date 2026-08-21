package script_test

import (
	"errors"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/script"
)

const mapResourceID = "ext.maps.inst-league-start.map-resource"

func linear(key string, mantissa int64, scale int32) *script.Node {
	return scaler(key, "LinearEffectScaler", field("coeff", decimal(mantissa, scale)))
}

func persistentTrigger(key string, effects ...*script.Node) *script.Node {
	return triggerNode(key, "TriggerResource", field("effects", nodeList(effects...)))
}

func TestPersistentEffectsAttachInSourceOrderAndDetachInReverse(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	evaluator := script.New(host, enabled())
	guard := effect("gibber/effects[0]", "Guard", field("scanRadius", integer(15)))
	input := effect("gibber/effects[1]", "ScalerAllInputDamage",
		field("scaler", node(linear("gibber/effects[1]/scaler", 100, 0))),
	)
	attachment := script.Attachment{
		ID: "attach-gibber", EntityID: ratID,
		TriggerRef: script.Ref{ID: "trigger.inst-league1.quest-4-30.gibber-death"},
		Trigger:    persistentTrigger("gibber", guard, input), Frame: newFrame(),
	}

	if err := evaluator.ActivateAttachment(t.Context(), attachment); err != nil {
		t.Fatalf("ActivateAttachment() error = %v", err)
	}
	if err := evaluator.Detach(t.Context(), attachment); err != nil {
		t.Fatalf("Detach() error = %v", err)
	}

	assertTrace(t, host.trace, []string{
		"apply attach-guard " + ratID + " radius=15 notice=false key=attach-gibber|gibber/effects[0]|attach",
		"apply attach-damage-modifier " + ratID + " direction=1 coeff=100 stacks=1 key=attach-gibber|gibber/effects[1]|attach",
		"apply detach-damage-modifier " + ratID + " effect=attach-gibber|gibber/effects[1] key=attach-gibber|gibber/effects[1]|detach",
		"apply detach-guard " + ratID + " effect=attach-gibber|gibber/effects[0] key=attach-gibber|gibber/effects[0]|detach",
		"apply detach-trigger trigger.inst-league1.quest-4-30.gibber-death from " + ratID + " key=eval-1|detach|trigger.inst-league1.quest-4-30.gibber-death",
	})
}

func TestPersistentModifierParsesCasterPredicateGroupAndStackFilters(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	evaluator := script.New(host, enabled())
	condition := predicate("filters/avatar", "PredicateIsAvatar")
	input := effect("filters/input", "ScalerAllInputDamage",
		field("attackerConditions", nodeList(condition)),
		field("onlyFromCaster", script.Value{Kind: script.ValueBool, Bool: true}),
		field("scaler", node(linear("filters/input/scaler", -5, 1))),
		field("stackCount", integer(2)),
	)
	output := effect("filters/output", "ScalerAllOutputDamage",
		field("group", script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: "action-group.fire", RowType: "action-group"}}),
		field("scaler", node(linear("filters/output/scaler", 2, 1))),
		field("stackCount", integer(3)),
	)
	attachment := script.Attachment{
		ID: "filters", EntityID: ratID,
		TriggerRef: script.Ref{ID: "trigger.filters"},
		Trigger:    persistentTrigger("trigger.filters", input, output), Frame: newFrame(),
	}
	if err := evaluator.ActivateAttachment(t.Context(), attachment); err != nil {
		t.Fatalf("ActivateAttachment() error = %v", err)
	}
	if len(host.commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(host.commands))
	}
	gotInput := host.commands[0].DamageModifier
	if gotInput.CapturedOffenderID != playerID || len(gotInput.AttackerPredicates) != 1 || gotInput.StackCount != 2 {
		t.Fatalf("input modifier = %#v", gotInput)
	}
	gotOutput := host.commands[1].DamageModifier
	if gotOutput.ActionGroup == nil || gotOutput.ActionGroup.ID != "action-group.fire" || gotOutput.StackCount != 3 {
		t.Fatalf("output modifier = %#v", gotOutput)
	}

	// -0.5 at two stacks is exactly a zero factor. The formula is not clamped.
	result, err := evaluator.ScaleDamage(t.Context(), damageEvent(ratID, playerID, true), []script.DamageModifier{*gotInput})
	if err != nil || result != (script.Decimal{}) {
		t.Fatalf("stacked input result = %#v, %v, want exact zero", result, err)
	}
}

func TestGuardUsesRetailDefaultsWhenFieldsAreOmitted(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	evaluator := script.New(host, enabled())
	attachment := script.Attachment{
		ID: "default-guard", EntityID: ratID,
		TriggerRef: script.Ref{ID: "trigger.default-guard"},
		Trigger:    persistentTrigger("trigger.default-guard", effect("guard/default", "Guard")),
		Frame:      newFrame(),
	}
	if err := evaluator.ActivateAttachment(t.Context(), attachment); err != nil {
		t.Fatalf("ActivateAttachment() error = %v", err)
	}
	guard := host.commands[0].Guard
	if guard.Radius != (script.Decimal{Mantissa: 425, Scale: 1}) || guard.NoticeTarget {
		t.Fatalf("default Guard = %#v, want radius 42.5 notice false", guard)
	}
}

func TestDamageModifierRegistryIsIdempotentAndGuardAggregationOwnsLastRemoval(t *testing.T) {
	t.Parallel()

	registry := script.NewEffectRegistry()
	owner := script.EffectOwner{Mob: true, CellPlaced: true}
	guard15 := script.Command{
		Kind: script.CommandAttachGuard, EntityID: ratID, EffectID: "guard-15",
		Guard: &script.Guard{Radius: script.Decimal{Mantissa: 15}},
	}
	guard50 := script.Command{
		Kind: script.CommandAttachGuard, EntityID: ratID, EffectID: "guard-50",
		Guard: &script.Guard{Radius: script.Decimal{Mantissa: 50}, NoticeTarget: true},
	}
	first, err := registry.Apply(guard15, owner)
	if err != nil || first.AggroMarkDelta != 1 {
		t.Fatalf("first guard = %#v, %v, want AggroMarkDelta +1", first, err)
	}
	replay, err := registry.Apply(guard15, owner)
	if err != nil || replay.Changed || replay.AggroMarkDelta != 0 {
		t.Fatalf("replayed guard = %#v, %v, want idempotent no-op", replay, err)
	}
	if _, err := registry.Apply(guard50, owner); err != nil {
		t.Fatalf("attach guard 50: %v", err)
	}
	state := registry.GuardState(ratID, script.Decimal{Mantissa: 30})
	if !state.GuardActive || state.ObserverRadius != (script.Decimal{Mantissa: 30}) ||
		!state.NoticeTarget || state.RecheckEvery != 2*time.Second {
		t.Fatalf("aggregated guard = %#v, want active radius 30 and notice", state)
	}
	if _, err := registry.Apply(script.Command{
		Kind: script.CommandDetachGuard, EntityID: ratID, EffectID: "guard-50",
	}, owner); err != nil {
		t.Fatalf("detach guard 50: %v", err)
	}
	state = registry.GuardState(ratID, script.Decimal{Mantissa: 30})
	if state.ObserverRadius != (script.Decimal{Mantissa: 15}) || !state.NoticeTarget {
		t.Fatalf("guard after max removal = %#v, want radius 15 and retained notice", state)
	}
	last, err := registry.Apply(script.Command{
		Kind: script.CommandDetachGuard, EntityID: ratID, EffectID: "guard-15",
	}, owner)
	if err != nil || last.AggroMarkDelta != -1 || !last.RemoveAggroState {
		t.Fatalf("last detach = %#v, %v, want one aggro teardown", last, err)
	}
	replayedDetach, err := registry.Apply(script.Command{
		Kind: script.CommandDetachGuard, EntityID: ratID, EffectID: "guard-15",
	}, owner)
	if err != nil || replayedDetach.Changed {
		t.Fatalf("replayed detach = %#v, %v, want no-op", replayedDetach, err)
	}
}

func TestGuardRejectsOwnersWithoutBothRuntimeCapabilities(t *testing.T) {
	t.Parallel()
	for _, owner := range []script.EffectOwner{{}, {Mob: true}, {CellPlaced: true}} {
		registry := script.NewEffectRegistry()
		_, err := registry.Apply(script.Command{
			Kind: script.CommandAttachGuard, EntityID: ratID, EffectID: "guard",
			Guard: &script.Guard{Radius: script.Decimal{Mantissa: 15}},
		}, owner)
		if err == nil {
			t.Fatalf("owner %#v attached Guard, want rejection", owner)
		}
		if state := registry.GuardState(ratID, script.Decimal{Mantissa: 30}); state.GuardActive {
			t.Fatalf("owner %#v left guard state %#v", owner, state)
		}
	}
}

func TestLinearDamageScalingHonoursMissingOffenderAndFilters(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	evaluator := script.New(host, enabled())
	predicateAvatar := predicate("condition/avatar", "PredicateIsAvatar")
	base := script.DamageModifier{
		EffectID: "input", EntityID: ratID, Direction: script.DamageIncoming,
		Priority:   script.DamagePriorityScaleAll,
		Scaler:     script.LinearScaler{Coefficient: script.Decimal{Mantissa: -9, Scale: 1}},
		StackCount: 1,
	}

	for _, testCase := range []struct {
		name  string
		event script.DamageEvent
		mod   script.DamageModifier
		want  script.Decimal
	}{
		{
			name: "resolved offender", event: damageEvent(ratID, playerID, true), mod: base,
			want: script.Decimal{Mantissa: 10},
		},
		{
			name: "no offender still scales", event: damageEvent(ratID, "", false), mod: base,
			want: script.Decimal{Mantissa: 10},
		},
		{
			name: "unresolved offender still scales", event: damageEvent(ratID, "ghost", false), mod: base,
			want: script.Decimal{Mantissa: 10},
		},
		{
			name: "captured caster mismatch", event: damageEvent(ratID, "other", true),
			mod: withCaptured(base, playerID), want: script.Decimal{Mantissa: 100},
		},
		{
			name: "avatar predicate true", event: damageEvent(ratID, playerID, true),
			mod: withPredicates(base, predicateAvatar), want: script.Decimal{Mantissa: 10},
		},
		{
			name: "avatar predicate false", event: damageEvent(ratID, "mob.not-avatar", true),
			mod: withPredicates(base, predicateAvatar), want: script.Decimal{Mantissa: 100},
		},
		{
			name:  "coefficient one hundred is factor one hundred one",
			event: damageEvent(ratID, playerID, true), mod: withCoefficient(base, script.Decimal{Mantissa: 100}),
			want: script.Decimal{Mantissa: 10_100},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := evaluator.ScaleDamage(t.Context(), testCase.event, []script.DamageModifier{testCase.mod})
			if err != nil {
				t.Fatalf("ScaleDamage() error = %v", err)
			}
			if got != testCase.want {
				t.Errorf("ScaleDamage() = %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func TestOutputActionGroupAndStableEqualPriorityOrder(t *testing.T) {
	t.Parallel()
	evaluator := script.New(newFakeHost(), enabled())
	first := script.DamageModifier{
		EffectID: "first", EntityID: playerID, Direction: script.DamageOutgoing,
		Priority: script.DamagePriorityScaleAll,
		Scaler:   script.LinearScaler{Coefficient: script.Decimal{Mantissa: 1}}, StackCount: 1,
	}
	second := first
	second.EffectID = "second"
	second.Scaler.Coefficient = script.Decimal{Mantissa: 2}
	group := script.Ref{ID: "action-group.fire"}
	grouped := first
	grouped.ActionGroup = &group

	for _, testCase := range []struct {
		name  string
		event script.DamageEvent
		mods  []script.DamageModifier
		want  script.Decimal
	}{
		{
			name:  "null group scales without an active action",
			event: script.DamageEvent{Magnitude: script.Decimal{Mantissa: 10}, OwnerID: playerID},
			mods:  []script.DamageModifier{first}, want: script.Decimal{Mantissa: 20},
		},
		{
			name:  "authored group needs an active action",
			event: script.DamageEvent{Magnitude: script.Decimal{Mantissa: 10}, OwnerID: playerID},
			mods:  []script.DamageModifier{grouped}, want: script.Decimal{Mantissa: 10},
		},
		{
			name:  "wrong active group",
			event: script.DamageEvent{Magnitude: script.Decimal{Mantissa: 10}, OwnerID: playerID, HasActiveAction: true, ActionGroup: script.Ref{ID: "other"}},
			mods:  []script.DamageModifier{grouped}, want: script.Decimal{Mantissa: 10},
		},
		{
			name:  "matching active group",
			event: script.DamageEvent{Magnitude: script.Decimal{Mantissa: 10}, OwnerID: playerID, HasActiveAction: true, ActionGroup: group},
			mods:  []script.DamageModifier{grouped}, want: script.Decimal{Mantissa: 20},
		},
		{
			name:  "equal priority keeps input order",
			event: script.DamageEvent{Magnitude: script.Decimal{Mantissa: 10}, OwnerID: playerID},
			mods:  []script.DamageModifier{second, first}, want: script.Decimal{Mantissa: 60},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := evaluator.ScaleDamage(t.Context(), testCase.event, testCase.mods)
			if err != nil {
				t.Fatalf("ScaleDamage() error = %v", err)
			}
			if got != testCase.want {
				t.Errorf("ScaleDamage() = %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func TestDestinationLocatorResolvesAbsolutePositionAndDefaultsYaw(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	host.located[mapResourceID+"|Firewall"] = script.Position{X: 1.25, Y: -2.5, Z: 7}
	evaluator := script.New(host, enabled())
	locator := basic("destination/locator", "Struct",
		field("map", script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: mapResourceID, RowType: "map-resource"}}),
		field("scriptID", text("Firewall")),
	)
	destination := impact("destination", "DestinationLocator", field("locator", node(locator)))

	got, err := evaluator.ResolveDestination(t.Context(), destination, newFrame())
	if err != nil {
		t.Fatalf("ResolveDestination() error = %v", err)
	}
	if got.Map.ID != mapResourceID || got.Position != (script.Position{X: 1.25, Y: -2.5, Z: 7}) || got.Yaw != (script.Decimal{}) {
		t.Fatalf("destination = %#v", got)
	}
	assertTrace(t, host.trace, []string{"locate " + mapResourceID + "/Firewall -> 1.250,-2.500,7.000"})
}

func TestDestinationLocatorRejectsMalformedPointersBeforeHostLookup(t *testing.T) {
	t.Parallel()
	for _, locator := range []*script.Node{
		basic("bad", "Struct", field("scriptID", text("Firewall"))),
		basic("bad", "Struct",
			field("map", script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: mapResourceID, RowType: "wrong"}}),
			field("scriptID", text("Firewall")),
		),
		basic("bad", "Struct",
			field("map", script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: mapResourceID, RowType: "map-resource"}}),
			field("scriptID", text("")),
		),
	} {
		host := newFakeHost()
		evaluator := script.New(host, enabled())
		_, err := evaluator.ResolveDestination(t.Context(), impact("destination", "DestinationLocator", field("locator", node(locator))), newFrame())
		var refusal *script.RefusedError
		if !errors.As(err, &refusal) {
			t.Fatalf("ResolveDestination() error = %v, want RefusedError", err)
		}
		if len(host.trace) != 0 {
			t.Fatalf("malformed locator reached host: %v", host.trace)
		}
	}
}

func TestPredicateIsAvatarQueriesExactlyOnce(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	evaluator := script.New(host, enabled())
	node := predicate("final/predicate", "PredicateIsAvatar", field("toLog", script.Value{Kind: script.ValueBool}))

	ok, err := evaluator.Predicate(t.Context(), node, newFrame())
	if err != nil || !ok {
		t.Fatalf("Predicate() = %t, %v, want true", ok, err)
	}
	assertTrace(t, host.trace, []string{"query is-avatar " + playerID + " -> true"})
}

func damageEvent(owner, offender string, resolved bool) script.DamageEvent {
	return script.DamageEvent{
		Magnitude: script.Decimal{Mantissa: 100}, OwnerID: owner,
		OffenderID: offender, OffenderResolved: resolved,
	}
}

func withCaptured(modifier script.DamageModifier, offender string) script.DamageModifier {
	modifier.CapturedOffenderID = offender
	return modifier
}

func withPredicates(modifier script.DamageModifier, predicates ...*script.Node) script.DamageModifier {
	modifier.AttackerPredicates = predicates
	return modifier
}

func withCoefficient(modifier script.DamageModifier, coefficient script.Decimal) script.DamageModifier {
	modifier.Scaler.Coefficient = coefficient
	return modifier
}
