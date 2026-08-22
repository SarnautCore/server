package itemactions_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/itemactions"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *clock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *clock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

type nativeCatalog map[string]itemactions.NativeAction

func (catalog nativeCatalog) NativeAction(id string) (itemactions.NativeAction, bool) {
	action, ok := catalog[id]
	return action, ok
}

type worldHost struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (host *worldHost) PrepareItemAction(
	_ context.Context, _ uint64, _ itemactions.NativeAction, _ uint64,
) (itemactions.PreparedWorldAction, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.fail != nil {
		return nil, host.fail
	}
	return &preparedWorld{host: host}, nil
}

type preparedWorld struct {
	host *worldHost
	done bool
}

func (prepared *preparedWorld) Commit() {
	prepared.host.mu.Lock()
	defer prepared.host.mu.Unlock()
	if !prepared.done {
		prepared.host.calls++
		prepared.done = true
	}
}

func (prepared *preparedWorld) Abort() {
	prepared.host.mu.Lock()
	defer prepared.host.mu.Unlock()
	prepared.done = true
}

type cooldownSink struct {
	mu     sync.Mutex
	events []itemactions.CooldownEvent
}

func (sink *cooldownSink) EnqueueItemCooldown(event itemactions.CooldownEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
}

func TestCooldownAuthorityEmitsStartAndFinishAtServerDeadlines(t *testing.T) {
	clock := &clock{now: time.Unix(100, 0)}
	sink := &cooldownSink{}
	authority, err := itemactions.NewCooldownAuthority(clock, nativeCatalog{
		potionAction: {ActionID: potionAction, CooldownGroupID: "item-action-group.healing", Cooldown: 5 * time.Minute,
			TriggersGCD: true, GlobalCooldown: time.Second, Target: itemactions.TargetSelf, PlanID: "item-action-plan.potion"},
	}, &worldHost{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := authority.PrepareItemAction(context.Background(), 7, 41, potionAction, 1)
	if err != nil {
		t.Fatalf("PrepareItemAction() error = %v", err)
	}
	result := prepared.Commit()
	if result.Remaining != 5*time.Minute || result.Duration != 5*time.Minute {
		t.Fatalf("cooldown = %+v", result)
	}
	if len(sink.events) != 1 || sink.events[0].Phase != itemactions.CooldownStarted {
		t.Fatalf("start events = %+v", sink.events)
	}
	clock.advance(5*time.Minute - time.Millisecond)
	if got := authority.Advance(); len(got) != 0 {
		t.Fatalf("early finish = %+v", got)
	}
	clock.advance(time.Millisecond)
	if got := authority.Advance(); len(got) != 1 || got[0].Phase != itemactions.CooldownFinished {
		t.Fatalf("finish = %+v", got)
	}
}

func TestCooldownAuthoritySharesGroupsAndDoesNotChargeRefusals(t *testing.T) {
	clock := &clock{now: time.Unix(100, 0)}
	host := &worldHost{}
	const secondAction = "item-action.item.elixir"
	authority, err := itemactions.NewCooldownAuthority(clock, nativeCatalog{
		potionAction: {ActionID: potionAction, CooldownGroupID: "item-action-group.healing", Cooldown: 5 * time.Minute, PlanID: "item-action-plan.potion"},
		secondAction: {ActionID: secondAction, CooldownGroupID: "item-action-group.healing", Cooldown: time.Minute, PlanID: "item-action-plan.elixir"},
	}, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := authority.PrepareItemAction(context.Background(), 7, 41, potionAction, 1)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if _, err := authority.PrepareItemAction(context.Background(), 7, 42, secondAction, 2); !errors.Is(err, itemactions.ErrActionOnCooldown) {
		t.Fatalf("shared group error = %v", err)
	}
	if host.calls != 1 {
		t.Fatalf("host calls after cooldown refusal = %d", host.calls)
	}
	clock.advance(5 * time.Minute)
	host.fail = errors.New("effect refused")
	if _, err := authority.PrepareItemAction(context.Background(), 7, 42, secondAction, 2); !errors.Is(err, host.fail) {
		t.Fatalf("host refusal = %v", err)
	}
	host.fail = nil
	prepared, err = authority.PrepareItemAction(context.Background(), 7, 42, secondAction, 2)
	if err != nil {
		t.Fatalf("refusal consumed request or cooldown: %v", err)
	}
	prepared.Commit()
}

func TestZeroCooldownEmitsStartAndFinishWithoutInventingDuration(t *testing.T) {
	clock := &clock{now: time.Unix(100, 0)}
	sink := &cooldownSink{}
	const elixir = "item-action.item.elixir"
	authority, err := itemactions.NewCooldownAuthority(clock, nativeCatalog{
		elixir: {ActionID: elixir, Cooldown: 0, Target: itemactions.TargetSelf, PlanID: "item-action-plan.elixir"},
	}, &worldHost{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := authority.PrepareItemAction(context.Background(), 7, 41, elixir, 1)
	if err != nil {
		t.Fatal(err)
	}
	result := prepared.Commit()
	if result.Duration != 0 || result.Remaining != 0 || len(sink.events) != 2 ||
		sink.events[0].Phase != itemactions.CooldownStarted || sink.events[1].Phase != itemactions.CooldownFinished {
		t.Fatalf("zero cooldown result=%+v events=%+v", result, sink.events)
	}
}

func TestCooldownAuthoritySerializesConcurrentUse(t *testing.T) {
	clock := &clock{now: time.Unix(100, 0)}
	host := &worldHost{}
	authority, err := itemactions.NewCooldownAuthority(clock, nativeCatalog{
		potionAction: {ActionID: potionAction, Cooldown: time.Minute, PlanID: "item-action-plan.potion"},
	}, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 64
	results := make(chan error, callers)
	var wait sync.WaitGroup
	for index := range callers {
		wait.Add(1)
		go func(requestID uint64) {
			defer wait.Done()
			prepared, err := authority.PrepareItemAction(context.Background(), 7, requestID+1, potionAction, requestID+1)
			if err == nil {
				prepared.Commit()
			}
			results <- err
		}(uint64(index))
	}
	wait.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, itemactions.ErrActionOnCooldown) && !errors.Is(err, itemactions.ErrDuplicateUseRequest) {
			t.Fatalf("concurrent activation error = %v", err)
		}
	}
	if succeeded != 1 || host.calls != 1 {
		t.Fatalf("concurrent activation successes=%d host calls=%d", succeeded, host.calls)
	}
}

func TestAbortedPreparationConsumesNeitherCooldownNorRequestSequence(t *testing.T) {
	clock := &clock{now: time.Unix(100, 0)}
	host := &worldHost{}
	sink := &cooldownSink{}
	authority, err := itemactions.NewCooldownAuthority(clock, nativeCatalog{
		potionAction: {ActionID: potionAction, Cooldown: time.Minute, PlanID: "item-action-plan.potion"},
	}, host, sink)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := authority.PrepareItemAction(context.Background(), 7, 41, potionAction, 9)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Abort()
	prepared, err = authority.PrepareItemAction(context.Background(), 7, 41, potionAction, 9)
	if err != nil {
		t.Fatalf("aborted preparation retained reservation or sequence: %v", err)
	}
	prepared.Commit()
	if host.calls != 1 || len(sink.events) != 1 || sink.events[0].Phase != itemactions.CooldownStarted {
		t.Fatalf("host calls=%d events=%+v", host.calls, sink.events)
	}
}
