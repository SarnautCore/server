// Package visibility owns directional world-visibility decisions.
package visibility

import (
	"context"
	"sync"
)

// Influence is the current server-owned canInfluence projection between live
// entities. Missing relationships are denied. The visibility-controller
// system publishes its decisions with SetCanInfluence instead of making each
// consumer reproduce plane logic.
type Influence struct {
	mu      sync.RWMutex
	allowed map[uint64]map[uint64]struct{}
}

// NewInfluence constructs an empty, fail-closed projection.
func NewInfluence() *Influence {
	return &Influence{allowed: make(map[uint64]map[uint64]struct{})}
}

// SetCanInfluence publishes one directional controller decision.
func (influence *Influence) SetCanInfluence(actor, target uint64, allowed bool) {
	if influence == nil || actor == 0 || target == 0 || actor == target {
		return
	}
	influence.mu.Lock()
	defer influence.mu.Unlock()
	if allowed {
		targets := influence.allowed[actor]
		if targets == nil {
			targets = make(map[uint64]struct{})
			influence.allowed[actor] = targets
		}
		targets[target] = struct{}{}
		return
	}
	targets := influence.allowed[actor]
	delete(targets, target)
	if len(targets) == 0 {
		delete(influence.allowed, actor)
	}
}

// CanInfluence reports the directed relationship actor -> target.
func (influence *Influence) CanInfluence(ctx context.Context, actor, target uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if influence == nil || actor == 0 || target == 0 {
		return false, nil
	}
	if actor == target {
		return true, nil
	}
	influence.mu.RLock()
	defer influence.mu.RUnlock()
	_, allowed := influence.allowed[actor][target]
	return allowed, nil
}

// Forget removes every relationship involving a departed entity.
func (influence *Influence) Forget(entity uint64) {
	if influence == nil || entity == 0 {
		return
	}
	influence.mu.Lock()
	defer influence.mu.Unlock()
	delete(influence.allowed, entity)
	for actor, targets := range influence.allowed {
		delete(targets, entity)
		if len(targets) == 0 {
			delete(influence.allowed, actor)
		}
	}
}
