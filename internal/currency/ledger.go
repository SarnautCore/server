// Package currency owns durable per-character balances for the two authored
// alternative currencies used by paid retail chat channels.
package currency

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// Resource is the product identity baked from the private gameplay pack. Both
// fields must match. Checking the pair prevents a stale or forged caller from
// spending a different resource that happens to reuse one field.
type Resource struct {
	ResourceID uint32
	SysName    string
}

const (
	ZoneChatSpecialResourceID uint32 = 455213063
	ZoneChatSpecialSysName           = "zone_chat_special"
	WorldChatResourceID       uint32 = 455213071
	WorldChatSysName                 = "world_chat"
)

var (
	ErrUnknownResource  = errors.New("currency: unknown resource")
	ErrInvalidCharacter = errors.New("currency: character id is required")
	ErrInvalidAmount    = errors.New("currency: paid chat spends exactly one unit")
	ErrInvalidCredit    = errors.New("currency: credit amount must be positive")
	ErrBalanceOverflow  = errors.New("currency: balance exceeds durable range")
)

// MaxBalance is PostgreSQL bigint's positive range. The public API uses uint64
// to match the chat authorization seam, but the durable representation never
// accepts a value PostgreSQL cannot encode.
const MaxBalance = uint64(math.MaxInt64)

type store interface {
	Balance(context.Context, uuid.UUID, uint32) (uint64, error)
	Credit(context.Context, uuid.UUID, uint32, uint64) (uint64, error)
	DebitOne(context.Context, uuid.UUID, uint32) (bool, error)
}

// Ledger validates authored identities and keeps storage details out of chat.
type Ledger struct {
	store store
}

func newLedger(storage store) *Ledger {
	return &Ledger{store: storage}
}

// ResourceFromProductIdentity accepts only the two exact id and sysName pairs
// baked from the private pack. Composition adapters use it to translate their
// local product-identity type without teaching the ledger about chat.
func ResourceFromProductIdentity(resourceID uint32, sysName string) (Resource, error) {
	resource := Resource{ResourceID: resourceID, SysName: sysName}
	if !supported(resource) {
		return Resource{}, fmt.Errorf("%w: id %d sys_name %q", ErrUnknownResource, resourceID, sysName)
	}
	return resource, nil
}

// ZoneChatSpecialResource returns the immutable authored identity charged by
// zone special chat.
func ZoneChatSpecialResource() Resource {
	return Resource{ResourceID: ZoneChatSpecialResourceID, SysName: ZoneChatSpecialSysName}
}

// WorldChatResource returns the immutable authored identity charged by world
// chat.
func WorldChatResource() Resource {
	return Resource{ResourceID: WorldChatResourceID, SysName: WorldChatSysName}
}

// Balance returns the durable balance for one supported resource. A missing
// row is a zero balance.
func (ledger *Ledger) Balance(
	ctx context.Context,
	characterID uuid.UUID,
	resource Resource,
) (uint64, error) {
	if err := validate(characterID, resource); err != nil {
		return 0, err
	}
	return ledger.store.Balance(ctx, characterID, resource.ResourceID)
}

// Credit atomically adds amount and returns the committed balance. Content
// reward code may use this method to fund paid chat without learning the table
// or update rules.
func (ledger *Ledger) Credit(
	ctx context.Context,
	characterID uuid.UUID,
	resource Resource,
	amount uint64,
) (uint64, error) {
	if err := validate(characterID, resource); err != nil {
		return 0, err
	}
	if amount == 0 || amount > MaxBalance {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidCredit, amount)
	}
	return ledger.store.Credit(ctx, characterID, resource.ResourceID, amount)
}

// Spend atomically debits the one-unit authored paid-chat cost. It returns
// false without changing state when the balance is empty. Unknown resources,
// mismatched identities, and any amount other than one fail closed with an
// error so a bad composition cannot masquerade as an ordinary empty purse.
func (ledger *Ledger) Spend(
	ctx context.Context,
	characterID uuid.UUID,
	resource Resource,
	amount uint64,
) (bool, error) {
	if err := validate(characterID, resource); err != nil {
		return false, err
	}
	if amount != 1 {
		return false, fmt.Errorf("%w: got %d", ErrInvalidAmount, amount)
	}
	return ledger.store.DebitOne(ctx, characterID, resource.ResourceID)
}

// SpendProductIdentity is the composition adapter for callers whose local
// currency type carries the baked id and sysName pair. Validation and debit
// remain one ledger operation from the caller's point of view.
func (ledger *Ledger) SpendProductIdentity(
	ctx context.Context,
	characterID uuid.UUID,
	resourceID uint32,
	sysName string,
	amount uint64,
) (bool, error) {
	resource, err := ResourceFromProductIdentity(resourceID, sysName)
	if err != nil {
		return false, err
	}
	return ledger.Spend(ctx, characterID, resource, amount)
}

func validate(characterID uuid.UUID, resource Resource) error {
	if characterID == uuid.Nil {
		return ErrInvalidCharacter
	}
	if !supported(resource) {
		return fmt.Errorf("%w: id %d sys_name %q", ErrUnknownResource, resource.ResourceID, resource.SysName)
	}
	return nil
}

func supported(resource Resource) bool {
	return resource == (Resource{
		ResourceID: ZoneChatSpecialResourceID,
		SysName:    ZoneChatSpecialSysName,
	}) || resource == (Resource{
		ResourceID: WorldChatResourceID,
		SysName:    WorldChatSysName,
	})
}
