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

type deferredTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newDeferredTestClock() *deferredTestClock {
	return &deferredTestClock{now: time.UnixMilli(1_000_000)}
}

func (clock *deferredTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *deferredTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

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
) (*ScriptDriver, *deferredTestClock) {
	t.Helper()
	driver := NewScriptDriver(slog.New(slog.DiscardHandler), zone, nil,
		deferredSource{root: root}, script.Options{Enabled: true})
	clock := newDeferredTestClock()
	driver.deferredNow = clock.Now
	if err := driver.BindDeferredQueue(t.Context(), store, worker); err != nil {
		t.Fatalf("BindDeferredQueue() error = %v", err)
	}
	return driver, clock
}

func TestDeferredBatchSurvivesDriverAndZoneRestart(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 100, inertImpact("quest/deferred/done"))
	firstZone := deferredZone(t, "restart-zone")
	first, _ := deferredDriver(t, firstZone, store, "worker-before", root)
	if err := first.QuestActivatedCommitted(t.Context(), 7, "quest.restart"); err != nil {
		t.Fatalf("QuestActivatedCommitted() error = %v", err)
	}
	rows, err := store.LoadZone(t.Context(), "restart-zone")
	if err != nil || len(rows) != 1 {
		t.Fatalf("persisted rows before restart = %#v, %v, want one", rows, err)
	}

	secondZone := deferredZone(t, "restart-zone")
	_, secondClock := deferredDriver(t, secondZone, store, "worker-after", root)
	secondClock.Advance(100 * time.Millisecond)
	secondZone.Step()
	secondZone.Step()
	rows, err = store.LoadZone(t.Context(), "restart-zone")
	if err != nil || len(rows) != 0 {
		t.Fatalf("persisted rows after restart execution = %#v, %v, want none", rows, err)
	}
}

func TestPlanningQuestActivationWritesNoQueueRows(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 100, inertImpact("quest/deferred/done"))
	zone := deferredZone(t, "plan-zone")
	driver, _ := deferredDriver(t, zone, store, "worker", root)
	plan, err := driver.PlanQuestActivation(t.Context(), 7, "quest.plan")
	if err != nil {
		t.Fatalf("PlanQuestActivation() error = %v", err)
	}
	if len(plan.Deferred) != 1 || plan.Deferred[0].ScopeKind != scriptqueue.ScopeActivation {
		t.Fatalf("planned deferred work = %#v, want one activation-scoped row", plan.Deferred)
	}
	rows, err := store.LoadZone(t.Context(), zone.ID())
	if err != nil || len(rows) != 0 {
		t.Fatalf("queue after planning = %#v, %v, want empty", rows, err)
	}
}

func TestOneEvaluationUsesOneWallClockInstant(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 100,
		inertImpact("quest/deferred/first"), inertImpact("quest/deferred/second"))
	zone := deferredZone(t, "clock-zone")
	driver, _ := deferredDriver(t, zone, store, "worker", root)
	now := time.UnixMilli(1_000_000)
	driver.deferredNow = func() time.Time {
		current := now
		now = now.Add(time.Millisecond)
		return current
	}
	plan, err := driver.PlanQuestActivation(t.Context(), 7, "quest.clock")
	if err != nil {
		t.Fatalf("PlanQuestActivation() error = %v", err)
	}
	if len(plan.Deferred) != 2 {
		t.Fatalf("planned deferred rows = %#v, want two", plan.Deferred)
	}
	if plan.Deferred[0].DueAtMS != plan.Deferred[1].DueAtMS {
		t.Fatalf("sibling due times = %d and %d, want one evaluation instant",
			plan.Deferred[0].DueAtMS, plan.Deferred[1].DueAtMS)
	}
}

func TestActivationIDsDoNotCollideAcrossDriverRestart(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 100, inertImpact("quest/deferred/done"))
	first, _ := deferredDriver(t, deferredZone(t, "boot-zone"), store, "before", root)
	if err := first.QuestActivatedCommitted(t.Context(), 7, "quest.boot"); err != nil {
		t.Fatalf("first QuestActivatedCommitted() error = %v", err)
	}
	second, _ := deferredDriver(t, deferredZone(t, "boot-zone"), store, "after", root)
	if err := second.QuestActivatedCommitted(t.Context(), 7, "quest.boot"); err != nil {
		t.Fatalf("second QuestActivatedCommitted() error = %v", err)
	}
	rows, err := store.LoadZone(t.Context(), "boot-zone")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows after two process activations = %#v, %v, want two", rows, err)
	}
	if rows[0].ID == rows[1].ID {
		t.Fatalf("activation row ids collide: %q", rows[0].ID)
	}
}

func TestFailedDeferredImpactRetainsRowAndBlocksAuthoredSuccessor(t *testing.T) {
	t.Parallel()
	store := scriptqueue.NewMemory()
	root := deferredRoot("quest/deferred", 0,
		refusedImpact("quest/deferred/first"), inertImpact("quest/deferred/second"))
	zone := deferredZone(t, "failure-zone")
	driver, clock := deferredDriver(t, zone, store, "worker", root)
	if err := driver.QuestActivatedCommitted(t.Context(), 7, "quest.failure"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Millisecond)
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
	driver, clock := deferredDriver(t, zone, store, "worker", nested)
	if err := driver.QuestActivatedCommitted(t.Context(), 7, "quest.nested"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Millisecond)
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
	origin, _ := deferredDriver(t, originZone, store, "origin", root)
	if err := origin.QuestActivatedCommitted(t.Context(), 7, "quest.race"); err != nil {
		t.Fatal(err)
	}

	leftZone := deferredZone(t, "race-zone")
	rightZone := deferredZone(t, "race-zone")
	_, leftClock := deferredDriver(t, leftZone, store, "left", root)
	_, rightClock := deferredDriver(t, rightZone, store, "right", root)
	leftClock.Advance(time.Millisecond)
	rightClock.Advance(time.Millisecond)
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
