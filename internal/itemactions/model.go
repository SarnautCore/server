// Package itemactions owns authoritative inventory and equipment verbs.
//
// Protocol code supplies only retail-shaped slot and count arguments. Session
// code supplies the authenticated character and world entity. Static item
// rules come from the private native catalog through Catalog.
package itemactions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
)

const MaxMoveNoMore int32 = 999

// EquipmentSlot is the retail DressSlot ordinal. Ammo 17 is deliberately not
// admitted because the 1.1 character panel has no corresponding item verb.
type EquipmentSlot int16

const (
	EquipmentHelm           EquipmentSlot = 0
	EquipmentArmor          EquipmentSlot = 1
	EquipmentPants          EquipmentSlot = 2
	EquipmentBoots          EquipmentSlot = 3
	EquipmentMantle         EquipmentSlot = 4
	EquipmentGloves         EquipmentSlot = 5
	EquipmentBracers        EquipmentSlot = 6
	EquipmentBelt           EquipmentSlot = 7
	EquipmentRing1          EquipmentSlot = 8
	EquipmentRing2          EquipmentSlot = 9
	EquipmentEarrings       EquipmentSlot = 10
	EquipmentNecklace       EquipmentSlot = 11
	EquipmentCloak          EquipmentSlot = 12
	EquipmentShirt          EquipmentSlot = 13
	EquipmentMainhand       EquipmentSlot = 14
	EquipmentOffhand        EquipmentSlot = 15
	EquipmentRanged         EquipmentSlot = 16
	EquipmentTabard         EquipmentSlot = 18
	EquipmentTrinket        EquipmentSlot = 19
	EquipmentBag            EquipmentSlot = 20
	EquipmentDeathInsurance EquipmentSlot = 21
)

var regularEquipmentSlots = [...]EquipmentSlot{
	EquipmentHelm, EquipmentArmor, EquipmentPants, EquipmentBoots,
	EquipmentMantle, EquipmentGloves, EquipmentBracers, EquipmentBelt,
	EquipmentRing1, EquipmentRing2, EquipmentEarrings, EquipmentNecklace,
	EquipmentCloak, EquipmentShirt, EquipmentMainhand, EquipmentOffhand,
	EquipmentRanged, EquipmentTabard, EquipmentTrinket, EquipmentDeathInsurance,
}

func isRegularEquipmentSlot(slot EquipmentSlot) bool {
	for _, candidate := range regularEquipmentSlots {
		if candidate == slot {
			return true
		}
	}
	return false
}

// EquippedItem is one item instance outside the bag.
type EquippedItem struct {
	Slot EquipmentSlot
	Item inventory.Stack
}

// State is the complete persisted replacement one verb reads and writes.
// Revision is CharacterState.SaveSeq. Bag is separate from regular equipment
// because changing it also changes the authored inventory layout.
type State struct {
	Revision  int64
	Inventory []inventory.Stack
	Equipment []EquippedItem
	Bag       *inventory.Stack
	Layout    inventory.BagLayout
}

// Actor is created from an authenticated session. Protocol messages never
// carry either field.
type Actor struct {
	CharacterID uuid.UUID
	EntityID    uint64
}

// Repository owns the read, expected-revision check, and full replacement in
// one transaction. On ErrStaleRevision it returns the current complete state.
type Repository interface {
	Update(
		ctx context.Context,
		characterID uuid.UUID,
		expectedRevision int64,
		mutate func(State) (State, []EquipChanged, error),
	) (State, []EquipChanged, error)
}

// EquipChanged is queued after the state commit. RequestID is the idempotency
// key used by the script host when it turns this into EventEquipChanged.
type EquipChanged struct {
	RequestID   uint64
	CharacterID uuid.UUID
	EntityID    uint64
	Slot        EquipmentSlot
	TriggerSlot string
	Equipped    bool
	ItemID      string
}

// EquipEventSink accepts a committed event without blocking the item action.
// Implementations must deduplicate RequestID plus Slot plus Equipped.
type EquipEventSink interface {
	EnqueueEquipChanged(EquipChanged)
}

type LocationKind uint8

const (
	LocationDeposit LocationKind = iota
	LocationDress
	LocationItemBag
)

type ItemLocation struct {
	HasSlot bool
	Slot    int32
	Kind    LocationKind
}

type SplitCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	MoveNoMore       int32
	FromSlot         int32
	ToSlot           int32
}

type DropCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	Count            int32
	Slot             int32
}

type OpenBoxCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	BoxSlot          int32
	KeySlot          int32
}

type EquipCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	BagSlot          int32
	DressSlot        EquipmentSlot
}

type UnequipCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	// BagSlot is -1 for the retail automatic destination.
	BagSlot   int32
	DressSlot EquipmentSlot
}

type UseCommand struct {
	RequestID        uint64
	ExpectedRevision int64
	Location         ItemLocation
}

type Result struct {
	State  State
	Events []EquipChanged
}

// Cooldown is server-owned item-action state. The HUD adapter adds the
// committed revision and current slot before publishing it.
type Cooldown struct {
	InstanceID      uint64
	ProductActionID string
	Remaining       time.Duration
	Duration        time.Duration
}

// PreparedUse reserves a fully validated world action. Commit must not fail;
// Abort releases the reservation without changing world or cooldown state.
type PreparedUse interface {
	Resource() ActionResource
	Commit() Cooldown
	Abort()
}

// UseAuthority resolves the private native item action and owns its cooldown.
// Prepare changes no visible state, which lets persistent item consumption
// commit first without charging an action whose database transaction failed.
type UseAuthority interface {
	PrepareItemAction(
		ctx context.Context,
		entityID uint64,
		instanceID uint64,
		productActionID string,
		requestID uint64,
	) (PreparedUse, error)
}

var (
	ErrInvalidCommand      = errors.New("item actions: invalid command")
	ErrStaleRevision       = errors.New("item actions: stale revision")
	ErrEmptySlot           = errors.New("item actions: slot is empty")
	ErrSlotOutOfRange      = errors.New("item actions: slot is outside the current bag")
	ErrUnsupportedAction   = errors.New("item actions: item does not support that action")
	ErrIncompatibleSlot    = errors.New("item actions: item is incompatible with the equipment slot")
	ErrDestinationOccupied = errors.New("item actions: destination is occupied")
	ErrCursed              = errors.New("item actions: cursed item cannot be removed")
	ErrWrongKey            = errors.New("item actions: box key does not match")
	ErrBagWouldShrink      = errors.New("item actions: equipped bag would strand occupied slots")
)
