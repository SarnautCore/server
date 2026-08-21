package social

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestIgnoreListsReplaceAndSetPreserveDirection(t *testing.T) {
	owner, first, second := uuid.New(), uuid.New(), uuid.New()
	lists := NewIgnoreLists()
	lists.Replace(owner, []uuid.UUID{first, first, owner, uuid.Nil})

	assertIgnores(t, lists, owner, first, true)
	assertIgnores(t, lists, first, owner, false)
	lists.Set(owner, second, true)
	assertIgnores(t, lists, owner, second, true)
	lists.Set(owner, first, false)
	assertIgnores(t, lists, owner, first, false)
	lists.Replace(owner, nil)
	assertIgnores(t, lists, owner, second, false)
}

func TestIgnoreListsRespectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ignored, err := NewIgnoreLists().Ignores(ctx, uuid.New(), uuid.New())
	if err == nil || ignored {
		t.Fatalf("Ignores(cancelled) = %t, %v; want false and cancellation", ignored, err)
	}
}

func TestIgnoreListsConcurrentReadersAndWriters(t *testing.T) {
	owner, subject := uuid.New(), uuid.New()
	lists := NewIgnoreLists()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(offset int) {
			defer group.Done()
			for index := 0; index < 1000; index++ {
				lists.Set(owner, subject, (index+offset)%2 == 0)
				_, _ = lists.Ignores(context.Background(), owner, subject)
			}
		}(worker)
	}
	group.Wait()
}

func assertIgnores(t *testing.T, lists *IgnoreLists, owner, subject uuid.UUID, want bool) {
	t.Helper()
	got, err := lists.Ignores(context.Background(), owner, subject)
	if err != nil {
		t.Fatalf("Ignores() error = %v", err)
	}
	if got != want {
		t.Errorf("Ignores(%s, %s) = %t, want %t", owner, subject, got, want)
	}
}
