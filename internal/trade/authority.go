package trade

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// Authority serializes every live exchange on one shard. This makes occupancy,
// confirmation resets, and the transition into the store transaction one
// decision instead of several checks that can race each other.
type Authority struct {
	mu sync.Mutex

	store Store

	presences map[uuid.UUID]Presence
	active    map[uuid.UUID]*exchange
	byPlayer  map[uuid.UUID]uuid.UUID
}

type exchange struct {
	id       uuid.UUID
	revision uint64
	state    State
	inviter  uuid.UUID
	invitee  uuid.UUID
	end      EndReason
	first    offer
	second   offer
}

type offer struct {
	items   [OfferSlots]*Item
	money   int64
	primary bool
	final   bool
}

func New(store Store) (*Authority, error) {
	if store == nil {
		return nil, errors.New("trade: a store is required")
	}
	return &Authority{
		store:     store,
		presences: make(map[uuid.UUID]Presence),
		active:    make(map[uuid.UUID]*exchange),
		byPlayer:  make(map[uuid.UUID]uuid.UUID),
	}, nil
}

// Connect registers server-owned session presence. Replacing a live session
// ends its old exchange as LOST before the new presence becomes visible.
func (authority *Authority) Connect(presence Presence) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	var deliveries []Delivery
	if _, exists := authority.presences[presence.CharacterID]; exists {
		deliveries = append(deliveries, authority.closePlayerLocked(
			presence.CharacterID, StateLost, EndReasonSessionEnded,
		)...)
	}
	presence.Ignored = cloneSet(presence.Ignored)
	authority.presences[presence.CharacterID] = presence
	return deliveries
}

// Disconnect removes one authenticated session and closes its exchange as
// LOST. A later duplicate disconnect is harmless.
func (authority *Authority) Disconnect(characterID uuid.UUID) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	var deliveries []Delivery
	deliveries = append(deliveries, authority.closePlayerLocked(
		characterID, StateLost, EndReasonSessionEnded,
	)...)
	delete(authority.presences, characterID)
	return deliveries
}

// UpdatePresence applies a world fact. Death and separation beyond five metres
// cancel an exchange immediately, matching the retail action handlers.
func (authority *Authority) UpdatePresence(change PresenceChange) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	var deliveries []Delivery
	presence, exists := authority.presences[change.CharacterID]
	if !exists {
		return deliveries
	}
	presence.Position = change.Position
	presence.Alive = change.Alive
	presence.Invisible = change.Invisible
	presence.Occupied = change.Occupied
	authority.presences[change.CharacterID] = presence

	current := authority.exchangeForLocked(change.CharacterID)
	if current == nil {
		return deliveries
	}
	if !change.Alive {
		return append(deliveries, authority.closeLocked(current, StateCanceled, EndReasonDeath)...)
	}
	left := authority.presences[current.inviter]
	right := authority.presences[current.invitee]
	if left.ZoneID != right.ZoneID ||
		left.Position.distanceSquared(right.Position) > MaxDistanceMetres*MaxDistanceMetres {
		return append(deliveries, authority.closeLocked(current, StateCanceled, EndReasonDistance)...)
	}
	return deliveries
}

// InventoryChanged removes any offered stack whose slot, item identity, or
// whole-stack count no longer matches storage. One removal resets both sides.
func (authority *Authority) InventoryChanged(
	ctx context.Context,
	characterID uuid.UUID,
) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	current := authority.exchangeForLocked(characterID)
	if current == nil || current.state != StateInProgress {
		return nil
	}
	own, _ := current.offers(characterID)
	holdings, err := authority.store.Load(ctx, characterID)
	if err != nil {
		return []Delivery{refusal(characterID, RefusalInternal)}
	}
	changed := false
	for index, offered := range own.items {
		if offered == nil {
			continue
		}
		stored, found := itemAt(holdings.Items, offered.BagSlot)
		if found && sameStack(stored, *offered) {
			continue
		}
		own.items[index] = nil
		changed = true
	}
	if !changed {
		return nil
	}
	current.resetConfirmations()
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

// InventoryMoved retargets offered source slots when storage proves the exact
// same stacks now occupy the destinations. A move that changed content or count
// removes the affected item and resets confirmations.
func (authority *Authority) InventoryMoved(
	ctx context.Context,
	characterID uuid.UUID,
	moves []SlotMove,
) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	current := authority.exchangeForLocked(characterID)
	if current == nil || current.state != StateInProgress {
		return nil
	}
	own, _ := current.offers(characterID)
	holdings, err := authority.store.Load(ctx, characterID)
	if err != nil {
		return []Delivery{refusal(characterID, RefusalInternal)}
	}
	bySource := make(map[int32]int32, len(moves))
	for _, move := range moves {
		bySource[move.From] = move.To
	}
	removed := false
	for index, offered := range own.items {
		if offered == nil {
			continue
		}
		destination, moved := bySource[offered.BagSlot]
		if !moved {
			continue
		}
		stored, found := itemAt(holdings.Items, destination)
		if !found || !sameStack(stored, *offered) {
			own.items[index] = nil
			removed = true
			continue
		}
		offered.BagSlot = destination
	}
	if removed {
		current.resetConfirmations()
		current.bump()
	}
	if !removed {
		// Bag slots are server-only in the retail replica. A pure retarget keeps
		// the offer and confirmations exactly as they were, so there is no client
		// replacement to send.
		return nil
	}
	return authority.broadcastLocked(current, nil, nil)
}

// MoneyChanged applies the retail wallet listener. A purse decrease clamps an
// over-large offer to the current balance and resets both confirmations. A
// purse increase does not alter the offer.
func (authority *Authority) MoneyChanged(
	ctx context.Context,
	characterID uuid.UUID,
) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	current := authority.exchangeForLocked(characterID)
	if current == nil || current.state != StateInProgress {
		return nil
	}
	own, _ := current.offers(characterID)
	holdings, err := authority.store.Load(ctx, characterID)
	if err != nil {
		return []Delivery{refusal(characterID, RefusalInternal)}
	}
	if own.money <= holdings.Money {
		return nil
	}
	own.money = holdings.Money
	current.resetConfirmations()
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

// Execute applies one typed command for the authenticated actor.
func (authority *Authority) Execute(
	ctx context.Context,
	actor uuid.UUID,
	command Command,
) []Delivery {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	var deliveries []Delivery
	if command == nil {
		return append(deliveries, refusal(actor, RefusalInvalidState))
	}

	var result []Delivery
	switch command := command.(type) {
	case Invite:
		result = authority.inviteLocked(actor, command)
	case Respond:
		result = authority.respondLocked(actor, command)
	case SetOfferItem:
		result = authority.setItemLocked(ctx, actor, command)
	case RemoveOfferItem:
		result = authority.removeItemLocked(actor, command)
	case SetMoney:
		result = authority.setMoneyLocked(ctx, actor, command)
	case SetPrimaryConfirmation:
		result = authority.setPrimaryLocked(actor, command)
	case SetFinalConfirmation:
		result = authority.setFinalLocked(ctx, actor, command)
	case Cancel:
		result = authority.cancelLocked(actor, command)
	default:
		result = []Delivery{refusal(actor, RefusalInvalidState)}
	}
	return append(deliveries, result...)
}

func (authority *Authority) inviteLocked(actor uuid.UUID, command Invite) []Delivery {
	actorPresence, actorPresent := authority.presences[actor]
	targetPresence, targetPresent := authority.presences[command.TargetCharacterID]
	switch {
	case !actorPresent:
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	case !targetPresent || actor == command.TargetCharacterID || targetPresence.Invisible:
		return []Delivery{refusal(actor, RefusalTargetNotFound)}
	case authority.exchangeForLocked(actor) != nil:
		return []Delivery{refusal(actor, RefusalActorBusy)}
	case authority.exchangeForLocked(command.TargetCharacterID) != nil:
		return []Delivery{refusal(actor, RefusalTargetBusy)}
	case actorPresence.Occupied:
		return []Delivery{refusal(actor, RefusalActorBusy)}
	case targetPresence.Occupied:
		return []Delivery{refusal(actor, RefusalTargetBusy)}
	case !actorPresence.Alive:
		return []Delivery{refusal(actor, RefusalActorDead)}
	case !targetPresence.Alive:
		return []Delivery{refusal(actor, RefusalTargetDead)}
	case actorPresence.ZoneID == "" || actorPresence.ZoneID != targetPresence.ZoneID ||
		actorPresence.Position.distanceSquared(targetPresence.Position) > MaxDistanceMetres*MaxDistanceMetres:
		return []Delivery{refusal(actor, RefusalTooFar)}
	case actorPresence.Invisible:
		return []Delivery{refusal(actor, RefusalActorInvisible)}
	case contains(targetPresence.Ignored, actor):
		return []Delivery{refusal(actor, RefusalTargetIgnoredActor)}
	case actorPresence.Faction == "" || actorPresence.Faction != targetPresence.Faction:
		return []Delivery{refusal(actor, RefusalNotFriendly)}
	}

	current := &exchange{
		id:       uuid.New(),
		revision: 1,
		state:    StateInvitation,
		inviter:  actor,
		invitee:  command.TargetCharacterID,
	}
	authority.active[current.id] = current
	authority.byPlayer[current.inviter] = current.id
	authority.byPlayer[current.invitee] = current.id
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) respondLocked(actor uuid.UUID, command Respond) []Delivery {
	current := authority.exchangeForLocked(actor)
	if current == nil || current.id != command.ExchangeID || actor != current.invitee {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	if current.state != StateInvitation {
		return []Delivery{refusal(actor, RefusalInvalidState)}
	}
	if !command.Accept {
		return authority.closeLocked(current, StateCanceled, EndReasonDeclined)
	}
	current.state = StateInProgress
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) setItemLocked(
	ctx context.Context,
	actor uuid.UUID,
	command SetOfferItem,
) []Delivery {
	current, own, _, rejection := authority.mutableOfferLocked(actor, command.ExchangeID)
	if rejection != RefusalNone {
		return []Delivery{refusal(actor, rejection)}
	}
	if command.OfferSlot >= OfferSlots {
		return []Delivery{refusal(actor, RefusalOfferSlotOutOfRange)}
	}
	if own.items[command.OfferSlot] != nil {
		return []Delivery{refusal(actor, RefusalOfferSlotUsed)}
	}
	holdings, err := authority.store.Load(ctx, actor)
	if err != nil {
		return []Delivery{refusal(actor, RefusalInternal)}
	}
	if command.BagSlot < 0 || command.BagSlot >= holdings.Capacity {
		return []Delivery{refusal(actor, RefusalBagSlotOutOfRange)}
	}
	item, found := itemAt(holdings.Items, command.BagSlot)
	if !found {
		return []Delivery{refusal(actor, RefusalItemNotFound)}
	}
	if item.Bound {
		return []Delivery{refusal(actor, RefusalItemBound)}
	}
	for _, offered := range own.items {
		if offered != nil && offered.BagSlot == command.BagSlot {
			return []Delivery{refusal(actor, RefusalItemAlreadyOffered)}
		}
	}
	copy := item
	own.items[command.OfferSlot] = &copy
	current.resetConfirmations()
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) removeItemLocked(actor uuid.UUID, command RemoveOfferItem) []Delivery {
	current, own, _, rejection := authority.mutableOfferLocked(actor, command.ExchangeID)
	if rejection != RefusalNone {
		return []Delivery{refusal(actor, rejection)}
	}
	if command.OfferSlot >= OfferSlots {
		return []Delivery{refusal(actor, RefusalOfferSlotOutOfRange)}
	}
	if own.items[command.OfferSlot] == nil {
		return nil
	}
	own.items[command.OfferSlot] = nil
	current.resetConfirmations()
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) setMoneyLocked(
	ctx context.Context,
	actor uuid.UUID,
	command SetMoney,
) []Delivery {
	current, own, _, rejection := authority.mutableOfferLocked(actor, command.ExchangeID)
	if rejection != RefusalNone {
		return []Delivery{refusal(actor, rejection)}
	}
	if command.Money < 0 {
		return []Delivery{refusal(actor, RefusalInvalidState)}
	}
	if command.Money == own.money {
		return nil
	}
	holdings, err := authority.store.Load(ctx, actor)
	if err != nil {
		return []Delivery{refusal(actor, RefusalInternal)}
	}
	if holdings.Money < command.Money {
		return []Delivery{refusal(actor, RefusalMoneyNotEnough)}
	}
	own.money = command.Money
	current.resetConfirmations()
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) setPrimaryLocked(
	actor uuid.UUID,
	command SetPrimaryConfirmation,
) []Delivery {
	current := authority.exchangeForLocked(actor)
	if current == nil || current.id != command.ExchangeID {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	if current.state != StateInProgress {
		return []Delivery{refusal(actor, RefusalInvalidState)}
	}
	own, _ := current.offers(actor)
	if own == nil {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	if !command.Confirmed {
		current.resetConfirmations()
	} else {
		own.primary = true
	}
	current.bump()
	return authority.broadcastLocked(current, nil, nil)
}

func (authority *Authority) setFinalLocked(
	ctx context.Context,
	actor uuid.UUID,
	command SetFinalConfirmation,
) []Delivery {
	current := authority.exchangeForLocked(actor)
	if current == nil || current.id != command.ExchangeID {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	if current.state != StateInProgress {
		return []Delivery{refusal(actor, RefusalInvalidState)}
	}
	own, _ := current.offers(actor)
	if own == nil {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	if !current.first.primary || !current.second.primary {
		return []Delivery{refusal(actor, RefusalPrimaryConfirmationRequired)}
	}
	own.final = command.Confirmed
	if !current.first.final || !current.second.final {
		current.bump()
		return authority.broadcastLocked(current, nil, nil)
	}
	return authority.commitLocked(ctx, current)
}

func (authority *Authority) cancelLocked(actor uuid.UUID, command Cancel) []Delivery {
	current := authority.exchangeForLocked(actor)
	if current == nil || current.id != command.ExchangeID {
		return []Delivery{refusal(actor, RefusalNotParticipant)}
	}
	return authority.closeLocked(current, StateCanceled, EndReasonCanceled)
}

func (authority *Authority) mutableOfferLocked(
	actor uuid.UUID,
	exchangeID uuid.UUID,
) (*exchange, *offer, *offer, Refusal) {
	current := authority.exchangeForLocked(actor)
	if current == nil || current.id != exchangeID {
		return nil, nil, nil, RefusalNotParticipant
	}
	if current.state != StateInProgress {
		return nil, nil, nil, RefusalInvalidState
	}
	own, other := current.offers(actor)
	if own == nil {
		return nil, nil, nil, RefusalNotParticipant
	}
	if own.final {
		return nil, nil, nil, RefusalInvalidState
	}
	return current, own, other, RefusalNone
}

func (authority *Authority) commitLocked(ctx context.Context, current *exchange) []Delivery {
	transfer := Transfer{
		FirstCharacterID:  current.inviter,
		SecondCharacterID: current.invitee,
		FirstItems:        current.first.snapshotItems(),
		SecondItems:       current.second.snapshotItems(),
		FirstMoney:        current.first.money,
		SecondMoney:       current.second.money,
	}
	result, err := authority.store.Transfer(ctx, transfer)
	switch {
	case errors.Is(err, ErrNoBagSpace):
		return authority.closeLocked(current, StateNoBagSpace, EndReasonNoBagSpace)
	case errors.Is(err, ErrHoldingsChanged):
		closed := authority.closeLocked(current, StateFailed, EndReasonCommitFailed)
		return append(closed, refusal(current.inviter, RefusalHoldingsChanged), refusal(current.invitee, RefusalHoldingsChanged))
	case err != nil:
		closed := authority.closeLocked(current, StateFailed, EndReasonCommitFailed)
		return append(closed, refusal(current.inviter, RefusalInternal), refusal(current.invitee, RefusalInternal))
	default:
		first := result.First
		second := result.Second
		return authority.closeLocked(current, StateCompleted, EndReasonCompleted,
			&first, &second)
	}
}

func (authority *Authority) closePlayerLocked(
	characterID uuid.UUID,
	state State,
	reason EndReason,
) []Delivery {
	current := authority.exchangeForLocked(characterID)
	if current == nil {
		return nil
	}
	return authority.closeLocked(current, state, reason)
}

func (authority *Authority) closeLocked(
	current *exchange,
	state State,
	reason EndReason,
	holdings ...*Holdings,
) []Delivery {
	current.state = state
	current.end = reason
	current.bump()
	var first, second *Holdings
	if len(holdings) > 0 {
		first = holdings[0]
	}
	if len(holdings) > 1 {
		second = holdings[1]
	}
	deliveries := authority.broadcastLocked(current, first, second)
	delete(authority.byPlayer, current.inviter)
	delete(authority.byPlayer, current.invitee)
	delete(authority.active, current.id)
	return deliveries
}

func (authority *Authority) broadcastLocked(
	current *exchange,
	firstHoldings *Holdings,
	secondHoldings *Holdings,
) []Delivery {
	first := authority.viewLocked(current, current.inviter)
	second := authority.viewLocked(current, current.invitee)
	return []Delivery{
		{Recipient: current.inviter, Event: Event{View: &first, Holdings: firstHoldings}},
		{Recipient: current.invitee, Event: Event{View: &second, Holdings: secondHoldings}},
	}
}

func (authority *Authority) viewLocked(current *exchange, self uuid.UUID) View {
	own, other := current.offers(self)
	otherID := current.invitee
	if self == current.invitee {
		otherID = current.inviter
	}
	otherName := authority.presences[otherID].Name
	return View{
		ExchangeID:           current.id,
		Revision:             current.revision,
		State:                current.state,
		InviterCharacterID:   current.inviter,
		SelfCharacterID:      self,
		OtherCharacterID:     otherID,
		OtherName:            otherName,
		SelfOffer:            own.view(),
		OtherOffer:           other.view(),
		ClientResponseWindow: ClientInvitationWindow,
		EndReason:            current.end,
	}
}

func (authority *Authority) exchangeForLocked(characterID uuid.UUID) *exchange {
	id, exists := authority.byPlayer[characterID]
	if !exists {
		return nil
	}
	return authority.active[id]
}

func (current *exchange) offers(characterID uuid.UUID) (*offer, *offer) {
	switch characterID {
	case current.inviter:
		return &current.first, &current.second
	case current.invitee:
		return &current.second, &current.first
	default:
		return nil, nil
	}
}

func (current *exchange) resetConfirmations() {
	current.first.primary = false
	current.first.final = false
	current.second.primary = false
	current.second.final = false
}

func (current *exchange) bump() {
	current.revision++
}

func (value offer) snapshotItems() []Item {
	items := make([]Item, 0, OfferSlots)
	for _, item := range value.items {
		if item != nil {
			items = append(items, *item)
		}
	}
	return items
}

func (value offer) view() OfferView {
	view := OfferView{
		Money:            value.money,
		PrimaryConfirmed: value.primary,
		FinalConfirmed:   value.final,
	}
	for index, item := range value.items {
		if item != nil {
			view.Items[index] = &ItemView{ItemID: item.ItemID, Count: item.Count}
		}
	}
	return view
}

func itemAt(items []Item, slot int32) (Item, bool) {
	for _, item := range items {
		if item.BagSlot == slot {
			return item, true
		}
	}
	return Item{}, false
}

func sameStack(left, right Item) bool {
	return left.ItemID == right.ItemID && left.Count == right.Count
}

func refusal(recipient uuid.UUID, reason Refusal) Delivery {
	return Delivery{Recipient: recipient, Event: Event{Refusal: reason}}
}

func contains(set map[uuid.UUID]struct{}, value uuid.UUID) bool {
	_, exists := set[value]
	return exists
}

func cloneSet(source map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	if source == nil {
		return nil
	}
	clone := make(map[uuid.UUID]struct{}, len(source))
	for value := range source {
		clone[value] = struct{}{}
	}
	return clone
}
