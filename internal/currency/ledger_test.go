package currency

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestBalancesStayDistinctByAuthoredResource(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if balance, err := ledger.Credit(t.Context(), characterID, ZoneChatSpecialResource(), 2); err != nil || balance != 2 {
		t.Fatalf("credit zone balance = %d, error %v, want 2", balance, err)
	}
	if balance, err := ledger.Credit(t.Context(), characterID, WorldChatResource(), 3); err != nil || balance != 3 {
		t.Fatalf("credit world balance = %d, error %v, want 3", balance, err)
	}
	if spent, err := ledger.Spend(t.Context(), characterID, ZoneChatSpecialResource(), 1); err != nil || !spent {
		t.Fatalf("spend zone = %v, error %v, want true", spent, err)
	}
	assertBalance(t, ledger, characterID, ZoneChatSpecialResource(), 1)
	assertBalance(t, ledger, characterID, WorldChatResource(), 3)
}

func TestSpendFailsClosedWithoutMutatingState(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if _, err := ledger.Credit(t.Context(), characterID, ZoneChatSpecialResource(), 2); err != nil {
		t.Fatalf("fund balance: %v", err)
	}

	tests := []struct {
		name      string
		character uuid.UUID
		resource  Resource
		amount    uint64
		want      error
	}{
		{name: "unknown id", character: characterID, resource: Resource{ResourceID: 7, SysName: "unknown"}, amount: 1, want: ErrUnknownResource},
		{name: "mismatched sys name", character: characterID, resource: Resource{ResourceID: ZoneChatSpecialResourceID, SysName: "world_chat"}, amount: 1, want: ErrUnknownResource},
		{name: "zero amount", character: characterID, resource: ZoneChatSpecialResource(), amount: 0, want: ErrInvalidAmount},
		{name: "more than authored cost", character: characterID, resource: ZoneChatSpecialResource(), amount: 2, want: ErrInvalidAmount},
		{name: "missing character", character: uuid.Nil, resource: ZoneChatSpecialResource(), amount: 1, want: ErrInvalidCharacter},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spent, err := ledger.Spend(t.Context(), test.character, test.resource, test.amount)
			if spent || !errors.Is(err, test.want) {
				t.Fatalf("Spend() = %v, %v, want false and %v", spent, err, test.want)
			}
		})
	}
	assertBalance(t, ledger, characterID, ZoneChatSpecialResource(), 2)
}

func TestProductIdentityAdapterSpendsOnlyExactChatResources(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if _, err := ledger.Credit(t.Context(), characterID, WorldChatResource(), 1); err != nil {
		t.Fatalf("fund world chat: %v", err)
	}
	spent, err := ledger.SpendProductIdentity(
		t.Context(),
		characterID,
		WorldChatResourceID,
		WorldChatSysName,
		1,
	)
	if err != nil || !spent {
		t.Fatalf("exact SpendProductIdentity() = %v, %v, want true, nil", spent, err)
	}

	spent, err = ledger.SpendProductIdentity(
		t.Context(),
		characterID,
		ZoneChatSpecialResourceID,
		WorldChatSysName,
		1,
	)
	if spent || !errors.Is(err, ErrUnknownResource) {
		t.Fatalf("mismatched SpendProductIdentity() = %v, %v, want false, ErrUnknownResource", spent, err)
	}
}

func TestInsufficientBalanceDoesNotCreateOrUnderflowARow(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	for attempt := 0; attempt < 2; attempt++ {
		spent, err := ledger.Spend(t.Context(), characterID, WorldChatResource(), 1)
		if err != nil || spent {
			t.Fatalf("empty Spend() = %v, %v, want false, nil", spent, err)
		}
	}
	assertBalance(t, ledger, characterID, WorldChatResource(), 0)
}

func TestConcurrentSpendersClaimTheLastUnitOnce(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if _, err := ledger.Credit(t.Context(), characterID, WorldChatResource(), 1); err != nil {
		t.Fatalf("fund balance: %v", err)
	}

	const attempts = 128
	results := make(chan bool, attempts)
	errorsSeen := make(chan error, attempts)
	var group sync.WaitGroup
	for attempt := 0; attempt < attempts; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			spent, err := ledger.Spend(t.Context(), characterID, WorldChatResource(), 1)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- spent
		}()
	}
	group.Wait()
	close(results)
	close(errorsSeen)

	for err := range errorsSeen {
		t.Errorf("concurrent Spend() error: %v", err)
	}
	accepted := 0
	for spent := range results {
		if spent {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("successful spends = %d, want 1", accepted)
	}
	assertBalance(t, ledger, characterID, WorldChatResource(), 0)
}

func TestMemoryStoreSurvivesLedgerReconstruction(t *testing.T) {
	t.Parallel()

	storage := newMemoryStore()
	first := newLedger(storage)
	characterID := uuid.New()
	if _, err := first.Credit(t.Context(), characterID, ZoneChatSpecialResource(), 4); err != nil {
		t.Fatalf("credit first ledger: %v", err)
	}

	second := newLedger(storage)
	if spent, err := second.Spend(t.Context(), characterID, ZoneChatSpecialResource(), 1); err != nil || !spent {
		t.Fatalf("spend reconstructed ledger = %v, %v, want true, nil", spent, err)
	}
	assertBalance(t, first, characterID, ZoneChatSpecialResource(), 3)
}

func TestCanceledAndOverflowingMutationsRollBack(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if _, err := ledger.Credit(t.Context(), characterID, ZoneChatSpecialResource(), MaxBalance); err != nil {
		t.Fatalf("credit maximum balance: %v", err)
	}
	if _, err := ledger.Credit(t.Context(), characterID, ZoneChatSpecialResource(), 1); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("overflow credit error = %v, want ErrBalanceOverflow", err)
	}
	assertBalance(t, ledger, characterID, ZoneChatSpecialResource(), MaxBalance)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	spent, err := ledger.Spend(canceled, characterID, ZoneChatSpecialResource(), 1)
	if spent || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Spend() = %v, %v, want false, context.Canceled", spent, err)
	}
	assertBalance(t, ledger, characterID, ZoneChatSpecialResource(), MaxBalance)
}

func TestInvalidCreditDoesNotCreateABalance(t *testing.T) {
	t.Parallel()

	ledger := NewMemory()
	characterID := uuid.New()
	if _, err := ledger.Credit(t.Context(), characterID, WorldChatResource(), 0); !errors.Is(err, ErrInvalidCredit) {
		t.Fatalf("zero credit error = %v, want ErrInvalidCredit", err)
	}
	if _, err := ledger.Credit(t.Context(), characterID, WorldChatResource(), MaxBalance+1); !errors.Is(err, ErrInvalidCredit) {
		t.Fatalf("out-of-range credit error = %v, want ErrInvalidCredit", err)
	}
	assertBalance(t, ledger, characterID, WorldChatResource(), 0)
}

func assertBalance(
	t *testing.T,
	ledger *Ledger,
	characterID uuid.UUID,
	resource Resource,
	want uint64,
) {
	t.Helper()
	got, err := ledger.Balance(t.Context(), characterID, resource)
	if err != nil || got != want {
		t.Fatalf("Balance(%s) = %d, %v, want %d, nil", resource.SysName, got, err, want)
	}
}
