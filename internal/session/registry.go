package session

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// evictionTimeout bounds how long a new session waits for the one it replaced
// to finish tearing down. It is generous against the five-second save timeout
// and short enough that a wedged session cannot lock a player out.
const evictionTimeout = 10 * time.Second

// sessionRegistry keeps one live session per character.
//
// The play lock in Valkey already refuses a second shard (ADR 0030 §4). What it
// cannot do is arbitrate two connections on the *same* shard, which is exactly
// what a reconnect after a half-open connection looks like: same holder, so the
// lock is renewed rather than refused. The registry is that arbitration —
// the newer connection wins and the older one is torn down before the new
// entity exists, so the zone never holds two entities for one character.
type sessionRegistry struct {
	mu   sync.Mutex
	live map[uuid.UUID]*liveSession
}

type liveSession struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{live: make(map[uuid.UUID]*liveSession)}
}

// claim registers a session for characterID, evicting and waiting for any
// session that already held it. It returns the release function the new session
// calls when its own teardown is complete.
func (registry *sessionRegistry) claim(
	ctx context.Context,
	characterID uuid.UUID,
	cancel context.CancelFunc,
) func() {
	if registry == nil {
		// A Server built by hand rather than by Serve. It admits sessions and
		// arbitrates nothing, which is the right answer for a single-session
		// test harness and is never the case in a running shard.
		return func() {}
	}
	registry.mu.Lock()
	previous := registry.live[characterID]
	entry := &liveSession{cancel: cancel, done: make(chan struct{})}
	registry.live[characterID] = entry
	registry.mu.Unlock()

	if previous != nil {
		previous.cancel()
		select {
		case <-previous.done:
		case <-time.After(evictionTimeout):
			// The old session is wedged. Proceeding is the lesser evil: its
			// entity is evicted when it finally returns, and its save is
			// rejected by the sequence guard if it lands after ours.
		case <-ctx.Done():
		}
	}

	return func() {
		registry.mu.Lock()
		if current, ok := registry.live[characterID]; ok && current == entry {
			delete(registry.live, characterID)
		}
		registry.mu.Unlock()
		close(entry.done)
	}
}
