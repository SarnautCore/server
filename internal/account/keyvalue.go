package account

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNoEntry reports that a key is absent or has expired. Expiry and absence
// are deliberately the same answer: a caller that could tell them apart would
// be able to prove a token once existed.
var ErrNoEntry = errors.New("account: no such entry")

// KeyValue is the short-lived state auth keeps outside PostgreSQL: account
// sessions, shard tickets and play locks (ADR 0030 section 2). Everything here
// may be lost without consequence — losing it logs everybody out, and nothing
// worse.
//
// The interface is small on purpose. It is what Valkey does, and it is what an
// in-memory implementation can honour exactly, so a test that passes against
// memory is evidence about the real store rather than a separate universe.
type KeyValue interface {
	// Set writes value with a time to live. An existing key is overwritten.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Get reads a key, or returns [ErrNoEntry].
	Get(ctx context.Context, key string) ([]byte, error)

	// Take reads and deletes a key atomically — Valkey's GETDEL. It is what
	// makes a shard ticket single use: a replayed ticket finds nothing.
	Take(ctx context.Context, key string) ([]byte, error)

	// Delete removes a key. Removing an absent key is not an error.
	Delete(ctx context.Context, key string) error

	// SetNX writes value only if the key is absent, reporting whether it did.
	// This is the play lock.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)

	// RefreshIf extends a key's time to live only while it still holds
	// expected, reporting whether it did. A shard that lost the lock must not
	// be able to renew it back.
	RefreshIf(ctx context.Context, key string, expected []byte, ttl time.Duration) (bool, error)

	// DeleteIf removes a key only while it still holds expected.
	DeleteIf(ctx context.Context, key string, expected []byte) (bool, error)
}

// memoryKeyValue is the DSN-free [KeyValue]. It exists so `go test ./...`
// proves the whole auth surface with no container running, and so the slice
// script can boot auth against nothing.
type memoryKeyValue struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
	// now is injectable so an expiry test advances a clock instead of sleeping.
	now func() time.Time
}

type memoryEntry struct {
	value   []byte
	expires time.Time
}

// NewMemoryKeyValue returns an in-memory [KeyValue] with the same observable
// behaviour as the Valkey one, expiry included.
func NewMemoryKeyValue() KeyValue {
	return &memoryKeyValue{entries: make(map[string]memoryEntry), now: time.Now}
}

// newTestKeyValue returns an in-memory store whose clock the caller drives.
func newTestKeyValue(clock func() time.Time) *memoryKeyValue {
	return &memoryKeyValue{entries: make(map[string]memoryEntry), now: clock}
}

func (store *memoryKeyValue) lookupLocked(key string) ([]byte, bool) {
	entry, ok := store.entries[key]
	if !ok {
		return nil, false
	}
	if !entry.expires.IsZero() && !store.now().Before(entry.expires) {
		delete(store.entries, key)
		return nil, false
	}
	return entry.value, true
}

func (store *memoryKeyValue) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.entries[key] = memoryEntry{value: append([]byte(nil), value...), expires: store.now().Add(ttl)}
	return nil
}

func (store *memoryKeyValue) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.lookupLocked(key)
	if !ok {
		return nil, ErrNoEntry
	}
	return append([]byte(nil), value...), nil
}

func (store *memoryKeyValue) Take(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.lookupLocked(key)
	if !ok {
		return nil, ErrNoEntry
	}
	delete(store.entries, key)
	return append([]byte(nil), value...), nil
}

func (store *memoryKeyValue) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.entries, key)
	return nil
}

func (store *memoryKeyValue) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.lookupLocked(key); ok {
		return false, nil
	}
	store.entries[key] = memoryEntry{value: append([]byte(nil), value...), expires: store.now().Add(ttl)}
	return true, nil
}

func (store *memoryKeyValue) RefreshIf(ctx context.Context, key string, expected []byte, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.lookupLocked(key)
	if !ok || string(value) != string(expected) {
		return false, nil
	}
	store.entries[key] = memoryEntry{value: append([]byte(nil), value...), expires: store.now().Add(ttl)}
	return true, nil
}

func (store *memoryKeyValue) DeleteIf(ctx context.Context, key string, expected []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.lookupLocked(key)
	if !ok || string(value) != string(expected) {
		return false, nil
	}
	delete(store.entries, key)
	return true, nil
}
