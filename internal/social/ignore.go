// Package social owns authoritative player-to-player social relationships.
package social

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// IgnoreLists is the current server-owned ignore-list projection. A session
// loads its complete list with Replace. Later mutations use Set so chat and
// other live systems observe the committed relationship without keeping their
// own copies.
type IgnoreLists struct {
	mu      sync.RWMutex
	ignored map[uuid.UUID]map[uuid.UUID]struct{}
}

// NewIgnoreLists constructs an empty projection.
func NewIgnoreLists() *IgnoreLists {
	return &IgnoreLists{ignored: make(map[uuid.UUID]map[uuid.UUID]struct{})}
}

// Replace publishes one owner's complete authoritative ignore list.
func (lists *IgnoreLists) Replace(owner uuid.UUID, ignored []uuid.UUID) {
	if lists == nil || owner == uuid.Nil {
		return
	}
	next := make(map[uuid.UUID]struct{}, len(ignored))
	for _, subject := range ignored {
		if subject != uuid.Nil && subject != owner {
			next[subject] = struct{}{}
		}
	}
	lists.mu.Lock()
	defer lists.mu.Unlock()
	if len(next) == 0 {
		delete(lists.ignored, owner)
		return
	}
	lists.ignored[owner] = next
}

// Set changes one relationship after its durable mutation commits.
func (lists *IgnoreLists) Set(owner, subject uuid.UUID, ignored bool) {
	if lists == nil || owner == uuid.Nil || subject == uuid.Nil || owner == subject {
		return
	}
	lists.mu.Lock()
	defer lists.mu.Unlock()
	if ignored {
		owned := lists.ignored[owner]
		if owned == nil {
			owned = make(map[uuid.UUID]struct{})
			lists.ignored[owner] = owned
		}
		owned[subject] = struct{}{}
		return
	}
	owned := lists.ignored[owner]
	delete(owned, subject)
	if len(owned) == 0 {
		delete(lists.ignored, owner)
	}
}

// Ignores reports the directed relationship owner -> subject.
func (lists *IgnoreLists) Ignores(ctx context.Context, owner, subject uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if lists == nil || owner == uuid.Nil || subject == uuid.Nil {
		return false, nil
	}
	lists.mu.RLock()
	defer lists.mu.RUnlock()
	_, ignored := lists.ignored[owner][subject]
	return ignored, nil
}
