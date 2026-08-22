package combat

import (
	"errors"
	"testing"
)

func TestScriptDamageReplayRetainsOnlyCurrentActivationPerCaster(t *testing.T) {
	module := &Module{
		scriptDamageEvents:     make(map[uint64]scriptDamageReplay),
		scriptTargetExecutions: make(map[uint64]scriptTargetReplay),
	}
	first := ScriptDamageRequest{
		CasterID: 7, TargetID: 11, AbilityID: "ability.one", ActionGroupID: "group.one",
		ActivationOrdinal: 1, DefinitionDigest: "digest-one", ExecutionKey: "action|7|ability.one|1|damage",
	}
	firstEvent := Event{CasterID: 7, TargetID: 11, Damage: 10}
	module.rememberScriptDamage(first, firstEvent)
	if got, ok, err := module.cachedScriptDamage(first); err != nil || !ok || got != firstEvent {
		t.Fatalf("cached first damage = (%+v, %t, %v), want original event", got, ok, err)
	}

	latest := first
	for ordinal := uint64(2); ordinal <= 100; ordinal++ {
		latest.ActivationOrdinal = ordinal
		latest.ExecutionKey = "next-damage"
		if _, ok, err := module.cachedScriptDamage(latest); err != nil || ok {
			t.Fatalf("activation %d cache lookup = (_, %t, %v), want miss", ordinal, ok, err)
		}
		module.rememberScriptDamage(latest, Event{CasterID: 7, TargetID: 11, Damage: int32(ordinal)})
	}
	if got := len(module.scriptDamageEvents); got != 1 {
		t.Fatalf("caster replay count = %d, want 1", got)
	}
	if got := len(module.scriptDamageEvents[7].events); got != 1 {
		t.Fatalf("activation replay count = %d, want 1", got)
	}
	if _, _, err := module.cachedScriptDamage(first); !errors.Is(err, ErrDuplicateCommand) {
		t.Fatalf("retired activation error = %v, want ErrDuplicateCommand", err)
	}
}

func TestScriptTargetReplayRetainsOnlyLatestMutationPerActor(t *testing.T) {
	module := &Module{scriptTargetExecutions: make(map[uint64]scriptTargetReplay)}
	for targetID := uint64(1); targetID <= 100; targetID++ {
		module.rememberScriptTarget(7, targetID, "next-target")
	}
	if got := len(module.scriptTargetExecutions); got != 1 {
		t.Fatalf("actor replay count = %d, want 1", got)
	}
	if got := module.scriptTargetExecutions[7].targetID; got != 100 {
		t.Fatalf("retained target = %d, want latest 100", got)
	}
}

func TestRetireScriptReplaysAllowsCasterOrdinalReuse(t *testing.T) {
	module := &Module{
		scriptDamageEvents:     make(map[uint64]scriptDamageReplay),
		scriptTargetExecutions: make(map[uint64]scriptTargetReplay),
	}
	request := ScriptDamageRequest{
		CasterID: 7, TargetID: 11, AbilityID: "ability.one",
		ActivationOrdinal: 1, DefinitionDigest: "digest-one", ExecutionKey: "action|7|ability.one|1|damage",
	}
	module.rememberScriptDamage(request, Event{Damage: 10})
	module.scriptTargetExecutions[7] = scriptTargetReplay{executionKey: "target-one", targetID: 11}

	module.retireScriptReplays(7)
	if len(module.scriptDamageEvents) != 0 || len(module.scriptTargetExecutions) != 0 {
		t.Fatalf("retired replay state = damage %d, target %d, want empty",
			len(module.scriptDamageEvents), len(module.scriptTargetExecutions))
	}
	if _, ok, err := module.cachedScriptDamage(request); err != nil || ok {
		t.Fatalf("reused ordinal cache lookup = (_, %t, %v), want miss", ok, err)
	}
}
