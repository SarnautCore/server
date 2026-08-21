package visibility

import (
	"context"
	"sync"
	"testing"
)

func TestInfluenceIsDirectionalFailClosedAndForgettable(t *testing.T) {
	influence := NewInfluence()
	assertInfluence(t, influence, 10, 20, false)
	influence.SetCanInfluence(10, 20, true)
	assertInfluence(t, influence, 10, 20, true)
	assertInfluence(t, influence, 20, 10, false)
	influence.Forget(20)
	assertInfluence(t, influence, 10, 20, false)

	influence.SetCanInfluence(10, 20, true)
	influence.SetCanInfluence(10, 20, false)
	assertInfluence(t, influence, 10, 20, false)
}

func TestInfluenceRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allowed, err := NewInfluence().CanInfluence(ctx, 10, 20)
	if err == nil || allowed {
		t.Fatalf("CanInfluence(cancelled) = %t, %v; want false and cancellation", allowed, err)
	}
}

func TestInfluenceConcurrentReadersAndWriters(t *testing.T) {
	influence := NewInfluence()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(offset int) {
			defer group.Done()
			for index := 0; index < 1000; index++ {
				influence.SetCanInfluence(10, 20, (index+offset)%2 == 0)
				_, _ = influence.CanInfluence(context.Background(), 10, 20)
			}
		}(worker)
	}
	group.Wait()
}

func assertInfluence(t *testing.T, influence *Influence, actor, target uint64, want bool) {
	t.Helper()
	got, err := influence.CanInfluence(context.Background(), actor, target)
	if err != nil {
		t.Fatalf("CanInfluence() error = %v", err)
	}
	if got != want {
		t.Errorf("CanInfluence(%d, %d) = %t, want %t", actor, target, got, want)
	}
}
