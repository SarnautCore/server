package currency

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

type balanceKey struct {
	characterID uuid.UUID
	resourceID  uint32
}

// memoryStore is durable for the lifetime of the process and gives tests the
// same atomic debit behavior as PostgreSQL. One mutex covers the check and the
// write, so concurrent one-unit spends cannot both observe the same unit.
type memoryStore struct {
	mu       sync.Mutex
	balances map[balanceKey]uint64
}

// NewMemory constructs an empty in-process ledger.
func NewMemory() *Ledger {
	return newLedger(newMemoryStore())
}

func newMemoryStore() *memoryStore {
	return &memoryStore{balances: make(map[balanceKey]uint64)}
}

func (store *memoryStore) Balance(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.balances[balanceKey{characterID: characterID, resourceID: resourceID}], nil
}

func (store *memoryStore) Credit(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
	amount uint64,
) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	key := balanceKey{characterID: characterID, resourceID: resourceID}
	current := store.balances[key]
	if amount > MaxBalance-current {
		return 0, fmt.Errorf("%w: current %d credit %d", ErrBalanceOverflow, current, amount)
	}
	current += amount
	store.balances[key] = current
	return current, nil
}

func (store *memoryStore) DebitOne(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	key := balanceKey{characterID: characterID, resourceID: resourceID}
	if store.balances[key] == 0 {
		return false, nil
	}
	store.balances[key]--
	return true, nil
}
