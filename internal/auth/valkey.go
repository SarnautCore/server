package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// valkeyKeyValue is the [KeyValue] the service runs on. `internal/auth` is
// one of the two packages ADR 0033 lets touch a client library directly; the
// interface above is what keeps that fact from spreading.
type valkeyKeyValue struct {
	client *redis.Client
}

// NewValkeyKeyValue wraps the client `internal/infra` already opens.
func NewValkeyKeyValue(client *redis.Client) KeyValue {
	return &valkeyKeyValue{client: client}
}

func (store *valkeyKeyValue) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := store.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("valkey set: %w", err)
	}
	return nil
}

func (store *valkeyKeyValue) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := store.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoEntry
	}
	if err != nil {
		return nil, fmt.Errorf("valkey get: %w", err)
	}
	return value, nil
}

// Take is GETDEL, which is atomic, so a replayed shard ticket finds nothing and
// is refused (ADR 0030 section 2).
func (store *valkeyKeyValue) Take(ctx context.Context, key string) ([]byte, error) {
	value, err := store.client.GetDel(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoEntry
	}
	if err != nil {
		return nil, fmt.Errorf("valkey getdel: %w", err)
	}
	return value, nil
}

func (store *valkeyKeyValue) Delete(ctx context.Context, key string) error {
	if err := store.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("valkey del: %w", err)
	}
	return nil
}

func (store *valkeyKeyValue) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	taken, err := store.client.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("valkey setnx: %w", err)
	}
	return taken, nil
}

// refreshIfScript extends a key's expiry only while it still holds the expected
// value. Read-then-write would let a shard that already lost the lock renew it
// back between the two commands.
var refreshIfScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

var deleteIfScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

func (store *valkeyKeyValue) RefreshIf(ctx context.Context, key string, expected []byte, ttl time.Duration) (bool, error) {
	result, err := refreshIfScript.Run(ctx, store.client, []string{key}, expected, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("valkey refresh: %w", err)
	}
	return result == 1, nil
}

func (store *valkeyKeyValue) DeleteIf(ctx context.Context, key string, expected []byte) (bool, error) {
	result, err := deleteIfScript.Run(ctx, store.client, []string{key}, expected).Int64()
	if err != nil {
		return false, fmt.Errorf("valkey delete: %w", err)
	}
	return result == 1, nil
}
