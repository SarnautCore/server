// Package scriptqueue owns durable scheduled script work.
//
// The interpreter gives this package opaque payload bytes and stable scope and
// work identifiers. It does not know SQL, and this package does not interpret
// script nodes. Rows remain until a leased worker completes them. A failure
// clears the lease and moves the row's next attempt forward without allowing
// later rows in the same scope to pass it.
package scriptqueue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrConflict  = errors.New("script queue: work id has different contents")
	ErrLeaseLost = errors.New("script queue: lease lost")
)

type Work struct {
	ID            string
	ZoneID        string
	ScopeID       string
	DueAtMS       int64
	AvailableAtMS int64
	Sequence      int64
	Payload       []byte
	Attempts      int32
	LeaseOwner    string
	LeaseUntilMS  int64
	LastError     string
}

type ClaimState uint8

const (
	ClaimMissing ClaimState = iota
	ClaimBlocked
	ClaimAcquired
)

type Claim struct {
	State ClaimState
	Work  Work
}

type Store interface {
	Enqueue(context.Context, Work) (Work, error)
	LoadZone(context.Context, string) ([]Work, error)
	Claim(context.Context, string, string, int64, int64) (Claim, error)
	Complete(context.Context, string, string) error
	Retry(context.Context, string, string, int64, string) error
	RunInTx(context.Context, func(context.Context, Store) error) error
}

// EnqueueBatch inserts every row in one transaction. A conflict or storage
// failure leaves none of the new rows visible.
func EnqueueBatch(ctx context.Context, store Store, works []Work) ([]Work, error) {
	if store == nil {
		return nil, errors.New("script queue: store is required")
	}
	inserted := make([]Work, 0, len(works))
	err := store.RunInTx(ctx, func(ctx context.Context, tx Store) error {
		for _, work := range works {
			row, err := tx.Enqueue(ctx, work)
			if err != nil {
				return err
			}
			inserted = append(inserted, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

func Validate(work Work) error {
	if work.ID == "" {
		return errors.New("script queue: work id is required")
	}
	if work.ZoneID == "" {
		return errors.New("script queue: zone id is required")
	}
	if work.ScopeID == "" {
		return errors.New("script queue: scope id is required")
	}
	if work.DueAtMS < 0 || work.AvailableAtMS < 0 {
		return fmt.Errorf("script queue: negative time for work %q", work.ID)
	}
	if len(work.Payload) == 0 {
		return fmt.Errorf("script queue: work %q has no payload", work.ID)
	}
	return nil
}

func sameWork(left, right Work) bool {
	return left.ID == right.ID && left.ZoneID == right.ZoneID &&
		left.ScopeID == right.ScopeID && left.DueAtMS == right.DueAtMS &&
		string(left.Payload) == string(right.Payload)
}

func retryDelay(attempts int32, interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Second
	}
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 6 {
		shift = 6
	}
	return interval * time.Duration(1<<shift)
}

// RetryAtMS returns a capped exponential retry time. It is exported so the
// session adapter and its tests use the same rule without duplicating it.
func RetryAtMS(nowMS int64, attempts int32, interval time.Duration) int64 {
	return nowMS + retryDelay(attempts, interval).Milliseconds()
}
