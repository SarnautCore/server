package scriptqueue

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type memoryState struct {
	nextSequence int64
	rows         map[string]Work
}

func newMemoryState() *memoryState {
	return &memoryState{nextSequence: 1, rows: make(map[string]Work)}
}

func (state *memoryState) clone() *memoryState {
	copyState := &memoryState{nextSequence: state.nextSequence, rows: make(map[string]Work, len(state.rows))}
	for id, row := range state.rows {
		row.Payload = append([]byte(nil), row.Payload...)
		copyState.rows[id] = row
	}
	return copyState
}

type memoryStore struct {
	mu    sync.Mutex
	state *memoryState
	root  *memoryStore
}

func NewMemory() Store { return &memoryStore{state: newMemoryState()} }

func (store *memoryStore) withState(ctx context.Context) (*memoryState, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if store.root != nil {
		return store.state, func() {}, nil
	}
	store.mu.Lock()
	return store.state, store.mu.Unlock, nil
}

func (store *memoryStore) RunInTx(ctx context.Context, run func(context.Context, Store) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.root != nil {
		return run(ctx, store)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	working := store.state.clone()
	view := &memoryStore{state: working, root: store}
	if err := run(ctx, view); err != nil {
		return err
	}
	store.state = working
	return nil
}

func (store *memoryStore) Enqueue(ctx context.Context, work Work) (Work, error) {
	if work.AvailableAtMS == 0 {
		work.AvailableAtMS = work.DueAtMS
	}
	if err := Validate(work); err != nil {
		return Work{}, err
	}
	state, done, err := store.withState(ctx)
	if err != nil {
		return Work{}, err
	}
	defer done()
	if existing, ok := state.rows[work.ID]; ok {
		if !sameWork(existing, work) {
			return Work{}, ErrConflict
		}
		existing.Payload = append([]byte(nil), existing.Payload...)
		return existing, nil
	}
	work.Sequence = state.nextSequence
	state.nextSequence++
	work.Payload = append([]byte(nil), work.Payload...)
	state.rows[work.ID] = work
	return work, nil
}

func (store *memoryStore) LoadZone(ctx context.Context, zoneID string) ([]Work, error) {
	state, done, err := store.withState(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	rows := make([]Work, 0)
	for _, row := range state.rows {
		if row.ZoneID == zoneID {
			row.Payload = append([]byte(nil), row.Payload...)
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].DueAtMS != rows[j].DueAtMS {
			return rows[i].DueAtMS < rows[j].DueAtMS
		}
		return rows[i].Sequence < rows[j].Sequence
	})
	return rows, nil
}

func (store *memoryStore) Claim(
	ctx context.Context, id, owner string, nowMS, leaseUntilMS int64,
) (Claim, error) {
	if owner == "" || leaseUntilMS <= nowMS {
		return Claim{}, fmt.Errorf("script queue: invalid lease for %q", id)
	}
	state, done, err := store.withState(ctx)
	if err != nil {
		return Claim{}, err
	}
	defer done()
	row, ok := state.rows[id]
	if !ok {
		return Claim{State: ClaimMissing}, nil
	}
	for _, earlier := range state.rows {
		if earlier.ZoneID != row.ZoneID || earlier.ScopeID != row.ScopeID || earlier.ID == row.ID {
			continue
		}
		if earlier.DueAtMS < row.DueAtMS ||
			(earlier.DueAtMS == row.DueAtMS && earlier.Sequence < row.Sequence) {
			return Claim{State: ClaimBlocked, Work: row}, nil
		}
	}
	if row.AvailableAtMS > nowMS ||
		(row.LeaseOwner != "" && row.LeaseOwner != owner && row.LeaseUntilMS > nowMS) {
		return Claim{State: ClaimBlocked, Work: row}, nil
	}
	if row.LeaseOwner != owner || row.LeaseUntilMS <= nowMS {
		row.Attempts++
	}
	row.LeaseOwner = owner
	row.LeaseUntilMS = leaseUntilMS
	state.rows[id] = row
	row.Payload = append([]byte(nil), row.Payload...)
	return Claim{State: ClaimAcquired, Work: row}, nil
}

func (store *memoryStore) Complete(ctx context.Context, id, owner string) error {
	state, done, err := store.withState(ctx)
	if err != nil {
		return err
	}
	defer done()
	row, ok := state.rows[id]
	if !ok || row.LeaseOwner != owner {
		return ErrLeaseLost
	}
	delete(state.rows, id)
	return nil
}

func (store *memoryStore) Retry(
	ctx context.Context, id, owner string, availableAtMS int64, lastError string,
) error {
	state, done, err := store.withState(ctx)
	if err != nil {
		return err
	}
	defer done()
	row, ok := state.rows[id]
	if !ok || row.LeaseOwner != owner {
		return ErrLeaseLost
	}
	row.AvailableAtMS = availableAtMS
	row.LeaseOwner = ""
	row.LeaseUntilMS = 0
	row.LastError = lastError
	state.rows[id] = row
	return nil
}
