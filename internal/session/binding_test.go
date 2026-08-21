package session

import (
	"reflect"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
)

func TestKillSinkOrdersLootBeforePlainQuestBeforeScript(t *testing.T) {
	t.Parallel()

	var calls []string
	sink := orderedKillSinks(
		orderedSink{name: "loot", calls: &calls},
		orderedSink{name: "plain quest", calls: &calls},
		orderedSink{name: "script", calls: &calls},
	)
	sink.MobKilled(nil, combat.Kill{})

	want := []string{"loot", "plain quest", "script"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("kill sink order = %v, want %v", calls, want)
	}
}

type orderedSink struct {
	name  string
	calls *[]string
}

func (sink orderedSink) MobKilled(gametypes.Tick, combat.Kill) {
	*sink.calls = append(*sink.calls, sink.name)
}
