// Package trade owns the server-authoritative two-player item exchange.
//
// Callers authenticate the actor before crossing this package's interface. The
// actor id is never accepted from a command payload. The module snapshots whole
// bag stacks, resets both confirmations after every offer mutation, and asks
// one Store operation to commit both bags and both purses.
package trade

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	OfferSlots             = 5
	ClientInvitationWindow = 30 * time.Second
	MaxDistanceMetres      = float32(5)
)

// State preserves the retail 1.1 exchange-state order. Values are explicit so
// persisted diagnostics and protocol adapters cannot silently renumber them.
type State uint8

const (
	StateInvitation State = 0
	StateInProgress State = 1
	StateCompleted  State = 2
	StateCanceled   State = 3
	StateFailed     State = 4
	StateNoBagSpace State = 5
	StateLost       State = 6
)

// Refusal is an ordinary gameplay answer. It never closes the client session.
type Refusal uint8

const (
	RefusalNone Refusal = iota
	RefusalTargetNotFound
	RefusalActorBusy
	RefusalTargetBusy
	RefusalTooFar
	RefusalActorDead
	RefusalTargetDead
	RefusalActorInvisible
	RefusalTargetIgnoredActor
	RefusalNotFriendly
	RefusalInvalidState
	RefusalNotParticipant
	RefusalOfferSlotOutOfRange
	RefusalBagSlotOutOfRange
	RefusalItemNotFound
	RefusalItemAlreadyOffered
	RefusalOfferSlotUsed
	RefusalItemBound
	RefusalMoneyNotEnough
	RefusalPrimaryConfirmationRequired
	RefusalHoldingsChanged
	RefusalInternal
)

// EndReason says why a terminal state was reached without making clients infer
// it from the last command they happened to send.
type EndReason uint8

const (
	EndReasonNone EndReason = iota
	EndReasonDeclined
	EndReasonCanceled
	EndReasonDistance
	EndReasonDeath
	EndReasonSessionEnded
	EndReasonCommitFailed
	EndReasonNoBagSpace
	EndReasonCompleted
)

type Vec3 struct {
	X float32
	Y float32
	Z float32
}

func (left Vec3) distanceSquared(right Vec3) float32 {
	x := left.X - right.X
	y := left.Y - right.Y
	z := left.Z - right.Z
	return x*x + y*y + z*z
}

// Presence is server-owned live state. Ignore lists and visibility never come
// from a trade request.
type Presence struct {
	CharacterID uuid.UUID
	EntityID    uint64
	Name        string
	ZoneID      string
	Position    Vec3
	Faction     string
	Alive       bool
	Invisible   bool
	Occupied    bool
	Ignored     map[uuid.UUID]struct{}
}

// PresenceChange carries only facts that can cancel an active exchange.
type PresenceChange struct {
	CharacterID uuid.UUID
	Position    Vec3
	Alive       bool
	Invisible   bool
	Occupied    bool
}

// Item is the exact whole stack captured when a player offers a bag slot.
// Bound is server metadata and is never accepted from the client.
type Item struct {
	BagSlot int32
	ItemID  string
	Count   int32
	Bound   bool
}

type Holdings struct {
	Items    []Item
	Money    int64
	Capacity int32
	SaveSeq  int64
}

// Transfer asks the store to revalidate and exchange both offers atomically.
type Transfer struct {
	FirstCharacterID  uuid.UUID
	SecondCharacterID uuid.UUID
	FirstItems        []Item
	SecondItems       []Item
	FirstMoney        int64
	SecondMoney       int64
}

type TransferResult struct {
	First  Holdings
	Second Holdings
}

var (
	ErrNoBagSpace      = errors.New("trade: no bag space")
	ErrHoldingsChanged = errors.New("trade: offered holdings changed")
)

// Store is the transaction seam. Load authenticates bag-slot snapshots;
// Transfer rechecks them and commits both inventories and both purses as one
// operation. Production and in-memory implementations must share this contract.
type Store interface {
	Load(ctx context.Context, characterID uuid.UUID) (Holdings, error)
	Transfer(ctx context.Context, transfer Transfer) (TransferResult, error)
}

// Command is a closed set. Protocol adapters map their versioned oneof onto
// these values after deriving the actor from the authenticated session.
type Command interface{ tradeCommand() }

type Invite struct{ TargetCharacterID uuid.UUID }
type Respond struct {
	ExchangeID uuid.UUID
	Accept     bool
}
type SetOfferItem struct {
	ExchangeID uuid.UUID
	OfferSlot  uint32
	BagSlot    int32
}
type RemoveOfferItem struct {
	ExchangeID uuid.UUID
	OfferSlot  uint32
}
type SetMoney struct {
	ExchangeID uuid.UUID
	Money      int64
}
type SetPrimaryConfirmation struct {
	ExchangeID uuid.UUID
	Confirmed  bool
}
type SetFinalConfirmation struct {
	ExchangeID uuid.UUID
	Confirmed  bool
}
type Cancel struct{ ExchangeID uuid.UUID }

// SlotMove is a server-observed bag relocation. It is not a client command.
// Moving the same stack retargets an offered item without resetting either
// side's confirmations.
type SlotMove struct {
	From int32
	To   int32
}

func (Invite) tradeCommand()                 {}
func (Respond) tradeCommand()                {}
func (SetOfferItem) tradeCommand()           {}
func (RemoveOfferItem) tradeCommand()        {}
func (SetMoney) tradeCommand()               {}
func (SetPrimaryConfirmation) tradeCommand() {}
func (SetFinalConfirmation) tradeCommand()   {}
func (Cancel) tradeCommand()                 {}

// OfferView is a fixed five-slot replacement. A nil item means an empty offer
// slot. It deliberately does not expose the source bag slot to the other side.
type OfferView struct {
	Items            [OfferSlots]*ItemView
	Money            int64
	PrimaryConfirmed bool
	FinalConfirmed   bool
}

type ItemView struct {
	ItemID string
	Count  int32
}

type View struct {
	ExchangeID           uuid.UUID
	Revision             uint64
	State                State
	InviterCharacterID   uuid.UUID
	SelfCharacterID      uuid.UUID
	OtherCharacterID     uuid.UUID
	OtherName            string
	SelfOffer            OfferView
	OtherOffer           OfferView
	ClientResponseWindow time.Duration
	EndReason            EndReason
}

type Event struct {
	View     *View
	Refusal  Refusal
	Holdings *Holdings
}

type Delivery struct {
	Recipient uuid.UUID
	Event     Event
}
