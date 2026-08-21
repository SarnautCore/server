package session

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/scriptqueue"
	"github.com/SarnautCore/server/internal/world"
)

type deferredSource struct{ root *script.Node }

func (source deferredSource) QuestActivation(string) (QuestActivation, bool) {
	return QuestActivation{StartImpacts: []*script.Node{source.root}}, true
}
func (deferredSource) Trigger(script.Ref) (*script.Node, bool)   { return nil, false }
func (deferredSource) Counter(script.Ref) (CounterBinding, bool) { return CounterBinding{}, false }
func (deferredSource) SpawnTableMobs(script.Ref) []string        { return nil }

func deferredRoot(key string, delay uint64, children ...*script.Node) *script.Node {
	values := make([]script.Value, 0, len(children))
	for _, child := range children {
		values = append(values, script.Value{Kind: script.ValueNode, Node: child})
	}
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: "ImpactsDeferred", Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "delay", Value: script.Value{Kind: script.ValueDurationMS, DurationMS: delay}},
			{Name: "impacts", Value: script.Value{Kind: script.ValueList, List: values}},
		},
	}
}

func inertImpact(key string) *script.Node {
	return &script.Node{Key: key, Family: script.FamilyImpact, Opcode: "PresentationOnly", Tier: script.TierInert}
}

func refusedImpact(key string) *script.Node {
	return &script.Node{Key: key, Family: script.FamilyImpact, Opcode: "UnknownAuthority", Tier: script.TierRefused}
}

func deferredZone(t *testing.T, id string) *world.Zone {
	t.Helper()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: id, TickInterval: 50 * time.Millisecond,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

func deferredDriver(
	t *testing.T, zone gametypes.Zone, store scriptqueue.Store, worker string, root *script.Node,
) *ScriptDriver {
	t.Helper()
	driver := NewScriptDriver(slog.New(slog.DiscardHandler), zone, nil,
		deferredSource{root: root}, script.Options{Enabled: true})
	if err := driver.BindDeferredQueue(t.Context(), store, worker); err != nil {
		t.Fatalf("BindDeferredQueue() error = %v", err)
	}
	return driver
}

func TestDeferredBatchSurvivesDriverAndZoneRestart(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 100, inertImpact("quest/deferred/done"))
	firstZone := deferredZone(t, "restart-zone")
	first := deferredDriver(t, firstZone, store, "worker-before", root)
	if err := first.QuestActivatedCommitted(t.Context(), 7, "quest.restart"); err != nil {
		t.Fatalf("QuestActivatedCommitted() error = %v", err)
	}
	rows, err := store.LoadZone(t.Context(), "restart-zone")
	if err != nil || len(rows) != 1 {
		t.Fatalf("persisted rows before restart = %#v, %v, want one", rows, err)
	}

	secondZone := deferredZone(t, "restart-zone")
	_ = deferredDriver(t, secondZone, store, "worker-after", root)
	secondZone.Step()
	secondZone.Step()
	rows, err = store.LoadZone(t.Context(), "restart-zone")
	if err != nil || len(rows) != 0 {
		t.Fatalf("persisted rows after restart execution = %#v, %v, want none", rows, err)
	}
}

func TestFailedDeferredImpactRetainsRowAndBlocksAuthoredSuccessor(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 0,
		refusedImpact("quest/deferred/first"), inertImpact("quest/deferred/second"))
	zone := deferredZone(t, "failure-zone")
	driver := deferredDriver(t, zone, store, "worker", root)
	if err := driver.QuestActivatedCommitted(t.Context(), 7, "quest.failure"); err != nil {
		t.Fatal(err)
	}
	zone.Step()
	rows, err := store.LoadZone(t.Context(), "failure-zone")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Attempts != 1 || rows[0].LastError == "" || rows[1].Attempts != 0 {
		t.Fatalf("rows after refused first impact = %#v", rows)
	}
}

func TestNestedDeferredCommitReplacesParentAtomically(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	nested := deferredRoot("quest/deferred/outer", 0,
		deferredRoot("quest/deferred/inner", 0, inertImpact("quest/deferred/inner/done")))
	zone := deferredZone(t, "nested-zone")
	driver := deferredDriver(t, zone, store, "worker", nested)
	if err := driver.QuestActivatedCommitted(t.Context(), 7, "quest.nested"); err != nil {
		t.Fatal(err)
	}
	zone.Step()
	rows, err := store.LoadZone(t.Context(), "nested-zone")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows after outer execution = %#v, want one nested row", rows)
	}
	item, err := decodeDeferred(rows[0])
	if err != nil || item.Node.Key != "quest/deferred/inner/done" {
		t.Fatalf("nested row = %#v, %v", item.Node, err)
	}
}

func TestTwoRecoveredWorkersDoNotExecuteOneAttemptTwice(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 0, refusedImpact("quest/deferred/first"))
	originZone := deferredZone(t, "race-zone")
	origin := deferredDriver(t, originZone, store, "origin", root)
	if err := origin.QuestActivatedCommitted(t.Context(), 7, "quest.race"); err != nil {
		t.Fatal(err)
	}

	leftZone := deferredZone(t, "race-zone")
	rightZone := deferredZone(t, "race-zone")
	_ = deferredDriver(t, leftZone, store, "left", root)
	_ = deferredDriver(t, rightZone, store, "right", root)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, zone := range []*world.Zone{leftZone, rightZone} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			zone.Step()
		}()
	}
	close(start)
	wait.Wait()
	rows, err := store.LoadZone(context.Background(), "race-zone")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after raced recovery = %#v, %v", rows, err)
	}
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts after raced recovery = %d, want 1", rows[0].Attempts)
	}
}
