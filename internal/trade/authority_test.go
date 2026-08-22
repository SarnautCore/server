package trade_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/trade"
)

type limits map[string]int32

func (values limits) StackLimit(itemID string) (int32, bool) {
	value, found := values[itemID]
	return value, found
}

type capacity map[uuid.UUID]int32

func (values capacity) Slots(characterID uuid.UUID) (int32, bool) {
	value, found := values[characterID]
	return value, found
}

type binding struct {
	mu    sync.Mutex
	bound map[uuid.UUID]map[int32]bool
}

func (values *binding) Bound(
	_ context.Context,
	characterID uuid.UUID,
	item charstore.InventoryItem,
) (bool, error) {
	values.mu.Lock()
	defer values.mu.Unlock()
	return values.bound[characterID][item.Slot], nil
}

type fixture struct {
	t          *testing.T
	ctx        context.Context
	repository charstore.Repository
	authority  *trade.Authority
	bindings   *binding
	capacity   capacity
	first      uuid.UUID
	second     uuid.UUID
}

func newFixture(t *testing.T, firstCapacity, secondCapacity int32) *fixture {
	return newFixtureWithClock(t, firstCapacity, secondCapacity, time.Now)
}

func newFixtureWithClock(
	t *testing.T,
	firstCapacity, secondCapacity int32,
	clock trade.Clock,
) *fixture {
	t.Helper()
	repository := charstore.NewMemory()
	first := uuid.New()
	second := uuid.New()
	capacities := capacity{first: firstCapacity, second: secondCapacity}
	bindings := &binding{bound: map[uuid.UUID]map[int32]bool{
		first:  {},
		second: {},
	}}
	store, err := trade.NewRepositoryStore(
		repository,
		limits{"sword": 1, "gem": 20, "tonic": 20, "bound-relic": 1},
		capacities,
		bindings,
	)
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	authority, err := trade.NewWithClock(store, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result := &fixture{
		t: t, ctx: context.Background(), repository: repository, authority: authority,
		bindings: bindings, capacity: capacities, first: first, second: second,
	}
	result.seed(first, 100, []charstore.InventoryItem{
		{Slot: 0, ItemID: "sword", Quantity: 1},
		{Slot: 1, ItemID: "tonic", Quantity: 2},
	})
	result.seed(second, 50, []charstore.InventoryItem{
		{Slot: 0, ItemID: "gem", Quantity: 3},
	})
	result.connect(first, 11, "First", trade.Vec3{}, "league")
	result.connect(second, 22, "Second", trade.Vec3{X: 5}, "league")
	return result
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *manualClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *manualClock) Advance(elapsed time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(elapsed)
}

func (fixture *fixture) seed(
	characterID uuid.UUID,
	money int64,
	items []charstore.InventoryItem,
) {
	fixture.t.Helper()
	err := charstore.SaveCharacter(fixture.ctx, fixture.repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "league",
			Level:       1,
			Health:      100,
			Currency:    money,
			SaveSeq:     1,
		},
		Inventory: items,
	})
	if err != nil {
		fixture.t.Fatalf("seed %s: %v", characterID, err)
	}
}

func (fixture *fixture) connect(
	characterID uuid.UUID,
	entityID uint64,
	name string,
	position trade.Vec3,
	faction string,
) {
	fixture.t.Helper()
	fixture.authority.Connect(trade.Presence{
		CharacterID: characterID,
		EntityID:    entityID,
		Name:        name,
		ZoneID:      "league",
		Position:    position,
		Faction:     faction,
		Alive:       true,
	})
}

func (fixture *fixture) invite() uuid.UUID {
	fixture.t.Helper()
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: fixture.second},
	)
	view := requireView(fixture.t, deliveries, fixture.first)
	if view.State != trade.StateInvitation {
		fixture.t.Fatalf("invite state = %v", view.State)
	}
	if view.ClientResponseWindow != trade.ClientInvitationWindow {
		fixture.t.Fatalf("client response window = %s, want %s", view.ClientResponseWindow, trade.ClientInvitationWindow)
	}
	return view.ExchangeID
}

func (fixture *fixture) accept(exchangeID uuid.UUID) {
	fixture.t.Helper()
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.Respond{ExchangeID: exchangeID, Accept: true},
	)
	if view := requireView(fixture.t, deliveries, fixture.second); view.State != trade.StateInProgress {
		fixture.t.Fatalf("accepted state = %v", view.State)
	}
}

func (fixture *fixture) offer(
	actor uuid.UUID,
	exchangeID uuid.UUID,
	offerSlot uint32,
	bagSlot int32,
) []trade.Delivery {
	fixture.t.Helper()
	return fixture.authority.Execute(fixture.ctx, actor, trade.SetOfferItem{
		ExchangeID: exchangeID,
		OfferSlot:  offerSlot,
		BagSlot:    bagSlot,
	})
}

func requireView(t *testing.T, deliveries []trade.Delivery, recipient uuid.UUID) trade.View {
	t.Helper()
	for _, delivery := range deliveries {
		if delivery.Recipient == recipient && delivery.Event.View != nil {
			return *delivery.Event.View
		}
	}
	t.Fatalf("no view delivered to %s: %+v", recipient, deliveries)
	return trade.View{}
}

func requireRefusal(t *testing.T, deliveries []trade.Delivery, want trade.Refusal) {
	t.Helper()
	for _, delivery := range deliveries {
		if delivery.Event.Refusal == want {
			return
		}
	}
	t.Fatalf("refusals = %+v, want %v", deliveries, want)
}

func TestRetailStateOrdinalsAndProductTerminalStatesAreLocked(t *testing.T) {
	want := []trade.State{
		trade.StateInvitation,
		trade.StateInProgress,
		trade.StateCompleted,
		trade.StateCanceled,
		trade.StateFailed,
		trade.StateNoBagSpace,
		trade.StateLost,
	}
	for ordinal, state := range want {
		if state != trade.State(ordinal) {
			t.Fatalf("state %v = %d, want retail ordinal %d", state, state, ordinal)
		}
	}
	if trade.StateFailed == trade.StateCompleted ||
		trade.StateNoBagSpace == trade.StateCompleted ||
		trade.StateLost == trade.StateCompleted {
		t.Fatal("product protocol collapsed a retail failure state into completed")
	}
	if trade.OfferSlots != 5 || trade.ClientInvitationWindow.Seconds() != 30 {
		t.Fatalf("offer slots/window = %d/%s, want 5/30s", trade.OfferSlots, trade.ClientInvitationWindow)
	}
}

func TestInviteUsesServerPresenceAndExactFiveMetreBoundary(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	if exchangeID == uuid.Nil {
		t.Fatal("exchange id is nil")
	}

	third := uuid.New()
	fixture.seed(third, 0, nil)
	fixture.connect(third, 33, "Third", trade.Vec3{X: 5.01}, "league")
	requireRefusal(t, fixture.authority.Execute(
		fixture.ctx, third, trade.Invite{TargetCharacterID: fixture.first},
	), trade.RefusalTargetBusy)

	fixture.authority.Execute(fixture.ctx, fixture.first, trade.Cancel{ExchangeID: exchangeID})
	requireRefusal(t, fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: third},
	), trade.RefusalTooFar)
}

func TestInviteRefusesDeathOccupancyVisibilityFactionAndIgnore(t *testing.T) {
	tests := []struct {
		name   string
		change func(*fixture)
		want   trade.Refusal
	}{
		{"actor dead", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.first, Alive: false})
		}, trade.RefusalActorDead},
		{"target dead", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.second, Alive: false})
		}, trade.RefusalTargetDead},
		{"actor occupied", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.first, Alive: true, Occupied: true})
		}, trade.RefusalActorBusy},
		{"target occupied", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.second, Alive: true, Occupied: true})
		}, trade.RefusalTargetBusy},
		{"actor invisible", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.first, Alive: true, Invisible: true})
		}, trade.RefusalActorInvisible},
		{"target invisible", func(f *fixture) {
			f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.second, Alive: true, Invisible: true})
		}, trade.RefusalTargetNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, 8, 8)
			test.change(fixture)
			requireRefusal(t, fixture.authority.Execute(
				fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: fixture.second},
			), test.want)
		})
	}

	t.Run("faction", func(t *testing.T) {
		fixture := newFixture(t, 8, 8)
		fixture.authority.Disconnect(fixture.second)
		fixture.connect(fixture.second, 22, "Second", trade.Vec3{X: 1}, "empire")
		requireRefusal(t, fixture.authority.Execute(
			fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: fixture.second},
		), trade.RefusalNotFriendly)
	})

	t.Run("ignore", func(t *testing.T) {
		fixture := newFixture(t, 8, 8)
		fixture.authority.Disconnect(fixture.second)
		fixture.authority.Connect(trade.Presence{
			CharacterID: fixture.second, EntityID: 22, Name: "Second", ZoneID: "league",
			Position: trade.Vec3{X: 1}, Faction: "league", Alive: true,
			Ignored: map[uuid.UUID]struct{}{fixture.first: {}},
		})
		requireRefusal(t, fixture.authority.Execute(
			fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: fixture.second},
		), trade.RefusalTargetIgnoredActor)
	})
}

func TestOnlyInviteeCanAcceptTheExactExchange(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	requireRefusal(t, fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.Respond{ExchangeID: exchangeID, Accept: true},
	), trade.RefusalNotParticipant)
	requireRefusal(t, fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.Respond{ExchangeID: uuid.New(), Accept: true},
	), trade.RefusalNotParticipant)
	fixture.accept(exchangeID)
}

func TestStateReplacementRevisionAdvancesOnlyOnVisibleMutation(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	accepted := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.Respond{ExchangeID: exchangeID, Accept: true},
	)
	view := requireView(t, accepted, fixture.first)
	if view.Revision != 2 {
		t.Fatalf("accepted revision = %d, want 2", view.Revision)
	}
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.SetMoney{ExchangeID: exchangeID, Money: 0},
	); len(deliveries) != 0 {
		t.Fatalf("no-op money update delivered %+v", deliveries)
	}
	view = requireView(t, fixture.offer(fixture.first, exchangeID, 0, 0), fixture.first)
	if view.Revision != 3 {
		t.Fatalf("first visible mutation revision = %d, want 3", view.Revision)
	}
}

func TestInvitationExpiresAuthoritativelyAndReleasesBothPlayers(t *testing.T) {
	clock := &manualClock{now: time.Unix(1_800_000_000, 0)}
	fixture := newFixtureWithClock(t, 8, 8, clock.Now)
	exchangeID := fixture.invite()

	clock.Advance(trade.ClientInvitationWindow - time.Nanosecond)
	if deliveries := fixture.authority.ExpireInvitations(); len(deliveries) != 0 {
		t.Fatalf("invitation expired before deadline: %+v", deliveries)
	}
	clock.Advance(time.Nanosecond)
	deliveries := fixture.authority.ExpireInvitations()
	for _, recipient := range []uuid.UUID{fixture.first, fixture.second} {
		view := requireView(t, deliveries, recipient)
		if view.ExchangeID != exchangeID || view.State != trade.StateCanceled ||
			view.EndReason != trade.EndReasonInvitationExpired || view.Revision != 2 {
			t.Fatalf("expired view for %s = %+v", recipient, view)
		}
	}
	if deliveries := fixture.authority.ExpireInvitations(); len(deliveries) != 0 {
		t.Fatalf("second expiration delivered %+v", deliveries)
	}

	newExchangeID := fixture.invite()
	if newExchangeID == exchangeID {
		t.Fatal("expiration did not release players for a new exchange")
	}
}

func TestExpiredInvitationIsSweptBeforeAnotherCommand(t *testing.T) {
	clock := &manualClock{now: time.Unix(1_800_000_000, 0)}
	fixture := newFixtureWithClock(t, 8, 8, clock.Now)
	oldExchangeID := fixture.invite()
	clock.Advance(trade.ClientInvitationWindow)

	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.Invite{TargetCharacterID: fixture.second},
	)
	var oldCanceled, replacement bool
	for _, delivery := range deliveries {
		if delivery.Recipient != fixture.first || delivery.Event.View == nil {
			continue
		}
		view := delivery.Event.View
		switch {
		case view.ExchangeID == oldExchangeID:
			oldCanceled = view.State == trade.StateCanceled &&
				view.EndReason == trade.EndReasonInvitationExpired
		case view.ExchangeID != uuid.Nil:
			replacement = view.State == trade.StateInvitation
		}
	}
	if !oldCanceled || !replacement {
		t.Fatalf("expiry sweep/replacement deliveries = %+v", deliveries)
	}
}

func TestConcurrentExpirationClosesInvitationOnce(t *testing.T) {
	clock := &manualClock{now: time.Unix(1_800_000_000, 0)}
	fixture := newFixtureWithClock(t, 8, 8, clock.Now)
	exchangeID := fixture.invite()
	clock.Advance(trade.ClientInvitationWindow)

	results := make(chan []trade.Delivery, 16)
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- fixture.authority.ExpireInvitations()
		}()
	}
	wait.Wait()
	close(results)

	terminalViews := 0
	for deliveries := range results {
		for _, delivery := range deliveries {
			if delivery.Event.View != nil && delivery.Event.View.ExchangeID == exchangeID &&
				delivery.Event.View.State == trade.StateCanceled {
				terminalViews++
			}
		}
	}
	if terminalViews != 2 {
		t.Fatalf("expiration terminal views = %d, want one broadcast to two players", terminalViews)
	}
}

func TestSameValueConfirmationsAreNoOps(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)

	primary := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	primaryRevision := requireView(t, primary, fixture.first).Revision
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true},
	); len(deliveries) != 0 {
		t.Fatalf("same primary confirmation delivered %+v", deliveries)
	}
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second,
		trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: false},
	); len(deliveries) != 0 {
		t.Fatalf("default primary confirmation delivered %+v", deliveries)
	}
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second,
		trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: false},
	); len(deliveries) != 0 {
		t.Fatalf("default final confirmation delivered %+v", deliveries)
	}

	fixture.authority.Execute(
		fixture.ctx, fixture.second,
		trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	final := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	finalRevision := requireView(t, final, fixture.first).Revision
	if finalRevision <= primaryRevision {
		t.Fatalf("final revision = %d, want greater than primary revision %d", finalRevision, primaryRevision)
	}
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	); len(deliveries) != 0 {
		t.Fatalf("same final confirmation delivered %+v", deliveries)
	}
	unfinal := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: false},
	)
	if view := requireView(t, unfinal, fixture.first); view.Revision != finalRevision+1 ||
		view.SelfOffer.FinalConfirmed {
		t.Fatalf("changed final confirmation view = %+v", view)
	}
	if deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.first,
		trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: false},
	); len(deliveries) != 0 {
		t.Fatalf("same cleared final confirmation delivered %+v", deliveries)
	}
}

func TestAuthorityRejectsNilClock(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	if _, err := trade.NewWithClock(mustRepositoryStore(t, fixture), nil); err == nil {
		t.Fatal("NewWithClock() accepted nil clock")
	}
}

func TestOfferAuthenticatesWholeUnboundBagStacks(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)

	view := requireView(t, fixture.offer(fixture.first, exchangeID, 0, 1), fixture.first)
	if item := view.SelfOffer.Items[0]; item == nil || item.ItemID != "tonic" || item.Count != 2 {
		t.Fatalf("offered item = %+v, want whole two-count tonic stack", item)
	}
	requireRefusal(t, fixture.offer(fixture.first, exchangeID, 1, 1), trade.RefusalItemAlreadyOffered)
	requireRefusal(t, fixture.offer(fixture.first, exchangeID, 0, 0), trade.RefusalOfferSlotUsed)
	requireRefusal(t, fixture.offer(fixture.first, exchangeID, 5, 0), trade.RefusalOfferSlotOutOfRange)
	requireRefusal(t, fixture.offer(fixture.first, exchangeID, 1, 99), trade.RefusalBagSlotOutOfRange)

	fixture.bindings.mu.Lock()
	fixture.bindings.bound[fixture.first][0] = true
	fixture.bindings.mu.Unlock()
	requireRefusal(t, fixture.offer(fixture.first, exchangeID, 1, 0), trade.RefusalItemBound)
}

func TestOfferMutationResetsBothConfirmationStages(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	fixture.offer(fixture.second, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})

	view := requireView(t, fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.SetMoney{ExchangeID: exchangeID, Money: 7},
	), fixture.first)
	if view.SelfOffer.PrimaryConfirmed || view.SelfOffer.FinalConfirmed ||
		view.OtherOffer.PrimaryConfirmed || view.OtherOffer.FinalConfirmed {
		t.Fatalf("confirmation survived offer mutation: %+v", view)
	}
}

func TestFinalConfirmationRequiresBothPrimary(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	requireRefusal(t, fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	), trade.RefusalPrimaryConfirmationRequired)
}

func TestBothFinalConfirmationsCommitItemsAndMoneyAtomically(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	fixture.offer(fixture.second, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetMoney{ExchangeID: exchangeID, Money: 10})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetMoney{ExchangeID: exchangeID, Money: 5})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	if view := requireView(t, deliveries, fixture.first); view.State != trade.StateCompleted || view.EndReason != trade.EndReasonCompleted {
		t.Fatalf("terminal view = %+v", view)
	}

	firstState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.first)
	secondState, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.second)
	if firstState.Currency != 95 || secondState.Currency != 55 {
		t.Fatalf("currencies = %d/%d, want 95/55", firstState.Currency, secondState.Currency)
	}
	firstItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.first)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if _, found := storedItem(firstItems, "gem"); !found {
		t.Fatalf("first inventory = %+v, missing received gem", firstItems)
	}
	if _, found := storedItem(secondItems, "sword"); !found {
		t.Fatalf("second inventory = %+v, missing received sword", secondItems)
	}
}

func TestNoBagSpaceClosesWithoutWritingEitherSide(t *testing.T) {
	fixture := newFixture(t, 1, 1)
	if err := fixture.repository.ReplaceInventory(fixture.ctx, fixture.first, []charstore.InventoryItem{
		{Slot: 0, ItemID: "sword", Quantity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.second, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	view := requireView(t, deliveries, fixture.first)
	if view.State != trade.StateNoBagSpace || view.EndReason != trade.EndReasonNoBagSpace {
		t.Fatalf("terminal view = %+v", view)
	}
	firstItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.first)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if len(firstItems) != 1 || len(secondItems) != 1 {
		t.Fatalf("inventories changed after no-space: first=%+v second=%+v", firstItems, secondItems)
	}
}

func TestCommitRevalidatesExactStackAndFailsWithoutPartialWrite(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	if err := fixture.repository.ReplaceInventory(fixture.ctx, fixture.first, []charstore.InventoryItem{
		{Slot: 0, ItemID: "sword", Quantity: 2},
	}); err != nil {
		t.Fatal(err)
	}
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	)
	if view := requireView(t, deliveries, fixture.first); view.State != trade.StateFailed {
		t.Fatalf("state = %v, want failed", view.State)
	}
	requireRefusal(t, deliveries, trade.RefusalHoldingsChanged)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if _, found := storedItem(secondItems, "sword"); found {
		t.Fatalf("second inventory gained an invalidated sword: %+v", secondItems)
	}
}

func TestMovementRetargetsUnchangedOfferWithoutReset(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	if err := fixture.repository.MoveItem(fixture.ctx, fixture.first, 0, 2); err != nil {
		t.Fatal(err)
	}
	deliveries := fixture.authority.InventoryMoved(
		fixture.ctx, fixture.first, []trade.SlotMove{{From: 0, To: 2}},
	)
	if len(deliveries) != 0 {
		t.Fatalf("pure bag-slot retarget produced client delivery: %+v", deliveries)
	}
	view := requireView(t, fixture.authority.Execute(
		fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true},
	), fixture.first)
	if !view.SelfOffer.PrimaryConfirmed || !view.OtherOffer.PrimaryConfirmed {
		t.Fatalf("pure slot move reset confirmations: %+v", view)
	}
}

func TestContentChangeAndWalletDecreaseResetOffers(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetMoney{ExchangeID: exchangeID, Money: 80})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})

	if err := fixture.repository.ReplaceInventory(fixture.ctx, fixture.first, []charstore.InventoryItem{
		{Slot: 0, ItemID: "gem", Quantity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	view := requireView(t, fixture.authority.InventoryChanged(fixture.ctx, fixture.first), fixture.first)
	if view.SelfOffer.Items[0] != nil || view.SelfOffer.PrimaryConfirmed || view.OtherOffer.PrimaryConfirmed {
		t.Fatalf("content change did not remove/reset: %+v", view)
	}

	state, _ := fixture.repository.LoadCharacterState(fixture.ctx, fixture.first)
	state.Currency = 30
	state.SaveSeq++
	if err := fixture.repository.SaveCharacterState(fixture.ctx, state); err != nil {
		t.Fatal(err)
	}
	view = requireView(t, fixture.authority.MoneyChanged(fixture.ctx, fixture.first), fixture.first)
	if view.SelfOffer.Money != 30 {
		t.Fatalf("clamped money = %d, want 30", view.SelfOffer.Money)
	}
}

func TestDeathDistanceAndSessionLossUseDistinctTerminalStates(t *testing.T) {
	tests := []struct {
		name   string
		close  func(*fixture) []trade.Delivery
		state  trade.State
		reason trade.EndReason
	}{
		{"death", func(f *fixture) []trade.Delivery {
			return f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.first, Position: trade.Vec3{}, Alive: false})
		}, trade.StateCanceled, trade.EndReasonDeath},
		{"distance", func(f *fixture) []trade.Delivery {
			return f.authority.UpdatePresence(trade.PresenceChange{CharacterID: f.first, Position: trade.Vec3{X: -0.01}, Alive: true})
		}, trade.StateCanceled, trade.EndReasonDistance},
		{"session", func(f *fixture) []trade.Delivery {
			return f.authority.Disconnect(f.first)
		}, trade.StateLost, trade.EndReasonSessionEnded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, 8, 8)
			fixture.invite()
			view := requireView(t, test.close(fixture), fixture.first)
			if view.State != test.state || view.EndReason != test.reason {
				t.Fatalf("view = %+v, want state %v reason %v", view, test.state, test.reason)
			}
		})
	}
}

func TestConcurrentFinalConfirmationsCommitOnce(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.offer(fixture.first, exchangeID, 0, 0)
	fixture.offer(fixture.second, exchangeID, 0, 0)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})

	var wait sync.WaitGroup
	results := make(chan []trade.Delivery, 2)
	for _, actor := range []uuid.UUID{fixture.first, fixture.second} {
		actor := actor
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- fixture.authority.Execute(fixture.ctx, actor, trade.SetFinalConfirmation{
				ExchangeID: exchangeID,
				Confirmed:  true,
			})
		}()
	}
	wait.Wait()
	close(results)
	completed := 0
	for deliveries := range results {
		for _, delivery := range deliveries {
			if delivery.Event.View != nil && delivery.Event.View.State == trade.StateCompleted {
				completed++
			}
		}
	}
	if completed != 2 {
		t.Fatalf("completed deliveries = %d, want exactly one broadcast to two players", completed)
	}
	firstItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.first)
	secondItems, _ := fixture.repository.LoadInventory(fixture.ctx, fixture.second)
	if _, found := storedItem(firstItems, "gem"); !found {
		t.Fatal("first player did not receive gem")
	}
	if _, found := storedItem(secondItems, "sword"); !found {
		t.Fatal("second player did not receive sword")
	}
}

func TestDeclineUsesOrdinaryCancelState(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	exchangeID := fixture.invite()
	deliveries := fixture.authority.Execute(
		fixture.ctx, fixture.second, trade.Respond{ExchangeID: exchangeID, Accept: false},
	)
	view := requireView(t, deliveries, fixture.first)
	if view.State != trade.StateCanceled || view.EndReason != trade.EndReasonDeclined {
		t.Fatalf("decline view = %+v", view)
	}
}

func storedItem(items []charstore.InventoryItem, itemID string) (charstore.InventoryItem, bool) {
	for _, item := range items {
		if item.ItemID == itemID {
			return item, true
		}
	}
	return charstore.InventoryItem{}, false
}

type failingStore struct {
	trade.Store
	err error
}

func (store failingStore) Transfer(context.Context, trade.Transfer) (trade.TransferResult, error) {
	return trade.TransferResult{}, store.err
}

func TestGenericStoreFailureUsesFailedState(t *testing.T) {
	fixture := newFixture(t, 8, 8)
	wrapped, err := trade.New(failingStore{Store: mustRepositoryStore(t, fixture), err: errors.New("boom")})
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority = wrapped
	fixture.connect(fixture.first, 11, "First", trade.Vec3{}, "league")
	fixture.connect(fixture.second, 22, "Second", trade.Vec3{X: 1}, "league")
	exchangeID := fixture.invite()
	fixture.accept(exchangeID)
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetPrimaryConfirmation{ExchangeID: exchangeID, Confirmed: true})
	fixture.authority.Execute(fixture.ctx, fixture.first, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})
	deliveries := fixture.authority.Execute(fixture.ctx, fixture.second, trade.SetFinalConfirmation{ExchangeID: exchangeID, Confirmed: true})
	if view := requireView(t, deliveries, fixture.first); view.State != trade.StateFailed {
		t.Fatalf("state = %v, want failed", view.State)
	}
	requireRefusal(t, deliveries, trade.RefusalInternal)
}

func mustRepositoryStore(t *testing.T, fixture *fixture) trade.Store {
	t.Helper()
	store, err := trade.NewRepositoryStore(
		fixture.repository,
		limits{"sword": 1, "gem": 20, "tonic": 20, "bound-relic": 1},
		fixture.capacity,
		fixture.bindings,
	)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
