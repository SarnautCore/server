package itemactions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type TargetPolicy uint8

const (
	TargetSelf TargetPolicy = iota
	TargetCurrent
	TargetPoint
)

type ActionResourceKind uint8

const (
	ActionResourceNone ActionResourceKind = iota
	ActionResourceActiveItem
	ActionResourceItem
)

// ActionResource is an authored cost from the private native action row.
// ActiveItem consumes the instance that initiated use. Item consumes the
// named product item from the actor's bag. None is a non-consuming action.
type ActionResource struct {
	Kind   ActionResourceKind
	ItemID string
	Count  int32
}

// NativeAction is the server projection of the private action catalog. Ordered
// impacts remain behind WorldActionHost; this row owns targeting and cooldown.
type NativeAction struct {
	ActionID        string
	CooldownGroupID string
	Cooldown        time.Duration
	CastDuration    time.Duration
	TriggersGCD     bool
	GlobalCooldown  time.Duration
	Target          TargetPolicy
	NeedLineOfSight bool
	Aggro           bool
	Resource        ActionResource
	PlanID          string
}

type NativeActionCatalog interface {
	NativeAction(actionID string) (NativeAction, bool)
}

// PreparedWorldAction is a validated and reserved action. Commit must not fail
// and Abort must leave world state unchanged.
type PreparedWorldAction interface {
	Commit()
	Abort()
}

// WorldActionHost validates resources and target and reserves the ordered
// impacts without changing visible world state.
type WorldActionHost interface {
	PrepareItemAction(
		ctx context.Context,
		entityID uint64,
		action NativeAction,
		requestID uint64,
	) (PreparedWorldAction, error)
}

type Clock interface{ Now() time.Time }

type CooldownPhase uint8

const (
	CooldownStarted CooldownPhase = iota
	CooldownFinished
)

type CooldownEvent struct {
	EntityID        uint64
	InstanceID      uint64
	ProductActionID string
	Phase           CooldownPhase
	Duration        time.Duration
}

type CooldownEventSink interface{ EnqueueItemCooldown(CooldownEvent) }

var (
	ErrUnknownNativeAction = errors.New("item actions: native action is unknown")
	ErrActionOnCooldown    = errors.New("item actions: native action is on cooldown")
	ErrDuplicateUseRequest = errors.New("item actions: duplicate use request")
)

type cooldownKey struct {
	entityID uint64
	groupID  string
}

type activationKey struct {
	entityID   uint64
	instanceID uint64
	actionID   string
}

type activeCooldown struct {
	readyAt  time.Time
	duration time.Duration
}

type acceptedUse struct {
	requestID uint64
	result    Cooldown
}

// CooldownAuthority is the server-owned item-use gate. It shares cooldowns by
// the native group ID and emits one start and finish per accepted item use.
type CooldownAuthority struct {
	mu      sync.Mutex
	clock   Clock
	catalog NativeActionCatalog
	host    WorldActionHost
	sink    CooldownEventSink

	groups        map[cooldownKey]time.Time
	global        map[uint64]time.Time
	active        map[activationKey]activeCooldown
	lastAccepted  map[uint64]acceptedUse
	pendingGroup  map[cooldownKey]struct{}
	pendingGlobal map[uint64]struct{}
}

func NewCooldownAuthority(
	clock Clock,
	catalog NativeActionCatalog,
	host WorldActionHost,
	sink CooldownEventSink,
) (*CooldownAuthority, error) {
	switch {
	case clock == nil:
		return nil, errors.New("item actions: cooldown clock is required")
	case catalog == nil:
		return nil, errors.New("item actions: native action catalog is required")
	case host == nil:
		return nil, errors.New("item actions: world action host is required")
	}
	return &CooldownAuthority{
		clock: clock, catalog: catalog, host: host, sink: sink,
		groups: make(map[cooldownKey]time.Time), global: make(map[uint64]time.Time),
		active: make(map[activationKey]activeCooldown), lastAccepted: make(map[uint64]acceptedUse),
		pendingGroup: make(map[cooldownKey]struct{}), pendingGlobal: make(map[uint64]struct{}),
	}, nil
}

func (authority *CooldownAuthority) PrepareItemAction(
	ctx context.Context,
	entityID uint64,
	instanceID uint64,
	productActionID string,
	requestID uint64,
) (PreparedUse, error) {
	if entityID == 0 || instanceID == 0 || productActionID == "" || requestID == 0 {
		return nil, fmt.Errorf("%w: incomplete item-use identity", ErrInvalidCommand)
	}
	action, ok := authority.catalog.NativeAction(productActionID)
	if !ok || action.ActionID != productActionID {
		return nil, fmt.Errorf("%w: %q", ErrUnknownNativeAction, productActionID)
	}
	if err := validateNativeAction(action); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknownNativeAction, err)
	}
	groupID := action.CooldownGroupID
	if groupID == "" {
		groupID = action.ActionID
	}
	key := cooldownKey{entityID: entityID, groupID: groupID}

	authority.mu.Lock()
	now := authority.clock.Now()
	if accepted, seen := authority.lastAccepted[entityID]; seen && requestID <= accepted.requestID {
		authority.mu.Unlock()
		return nil, ErrDuplicateUseRequest
	}
	if _, pending := authority.pendingGroup[key]; pending {
		authority.mu.Unlock()
		return nil, ErrActionOnCooldown
	}
	if action.TriggersGCD {
		if _, pending := authority.pendingGlobal[entityID]; pending {
			authority.mu.Unlock()
			return nil, ErrActionOnCooldown
		}
	}
	readyAt := authority.groups[key]
	if action.TriggersGCD && authority.global[entityID].After(readyAt) {
		readyAt = authority.global[entityID]
	}
	if readyAt.After(now) {
		authority.mu.Unlock()
		return nil, ErrActionOnCooldown
	}
	authority.pendingGroup[key] = struct{}{}
	if action.TriggersGCD {
		authority.pendingGlobal[entityID] = struct{}{}
	}
	authority.mu.Unlock()

	world, err := authority.host.PrepareItemAction(ctx, entityID, action, requestID)
	if err != nil {
		authority.releaseReservation(key, entityID, action.TriggersGCD)
		return nil, err
	}
	if world == nil {
		authority.releaseReservation(key, entityID, action.TriggersGCD)
		return nil, errors.New("item actions: world host returned a nil prepared action")
	}
	return &preparedCooldown{
		authority: authority, world: world, key: key, entityID: entityID,
		instanceID: instanceID, action: action, requestID: requestID,
	}, nil
}

type preparedCooldown struct {
	mu         sync.Mutex
	authority  *CooldownAuthority
	world      PreparedWorldAction
	key        cooldownKey
	entityID   uint64
	instanceID uint64
	action     NativeAction
	requestID  uint64
	done       bool
	result     Cooldown
}

func (prepared *preparedCooldown) Commit() Cooldown {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.done {
		return prepared.result
	}
	prepared.world.Commit()
	prepared.result = prepared.authority.commitPrepared(prepared)
	prepared.done = true
	return prepared.result
}

func (prepared *preparedCooldown) Resource() ActionResource { return prepared.action.Resource }

func (prepared *preparedCooldown) Abort() {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.done {
		return
	}
	prepared.world.Abort()
	prepared.authority.releaseReservation(prepared.key, prepared.entityID, prepared.action.TriggersGCD)
	prepared.done = true
}

func (authority *CooldownAuthority) commitPrepared(prepared *preparedCooldown) Cooldown {
	authority.mu.Lock()
	now := authority.clock.Now()
	action := prepared.action
	result := Cooldown{InstanceID: prepared.instanceID, ProductActionID: action.ActionID,
		Remaining: action.Cooldown, Duration: action.Cooldown}
	authority.lastAccepted[prepared.entityID] = acceptedUse{requestID: prepared.requestID, result: result}
	delete(authority.pendingGroup, prepared.key)
	if action.TriggersGCD {
		delete(authority.pendingGlobal, prepared.entityID)
	}
	events := []CooldownEvent{{
		EntityID: prepared.entityID, InstanceID: prepared.instanceID, ProductActionID: action.ActionID,
		Phase: CooldownStarted, Duration: action.Cooldown,
	}}
	if action.Cooldown > 0 {
		readyAt := now.Add(action.Cooldown)
		authority.groups[prepared.key] = readyAt
		authority.active[activationKey{entityID: prepared.entityID, instanceID: prepared.instanceID, actionID: action.ActionID}] = activeCooldown{
			readyAt: readyAt, duration: action.Cooldown,
		}
	} else {
		events = append(events, CooldownEvent{
			EntityID: prepared.entityID, InstanceID: prepared.instanceID, ProductActionID: action.ActionID,
			Phase: CooldownFinished,
		})
	}
	if action.TriggersGCD {
		authority.global[prepared.entityID] = now.Add(action.GlobalCooldown)
	}
	authority.mu.Unlock()
	authority.offer(events)
	return result
}

func (authority *CooldownAuthority) releaseReservation(key cooldownKey, entityID uint64, global bool) {
	authority.mu.Lock()
	delete(authority.pendingGroup, key)
	if global {
		delete(authority.pendingGlobal, entityID)
	}
	authority.mu.Unlock()
}

// Advance emits finish events for cooldowns whose server deadline passed.
// Session code calls it from its existing tick cadence.
func (authority *CooldownAuthority) Advance() []CooldownEvent {
	authority.mu.Lock()
	now := authority.clock.Now()
	finished := make([]CooldownEvent, 0)
	for key, cooldown := range authority.active {
		if cooldown.readyAt.After(now) {
			continue
		}
		finished = append(finished, CooldownEvent{
			EntityID: key.entityID, InstanceID: key.instanceID, ProductActionID: key.actionID,
			Phase: CooldownFinished, Duration: cooldown.duration,
		})
		delete(authority.active, key)
	}
	authority.mu.Unlock()
	authority.offer(finished)
	return finished
}

func (authority *CooldownAuthority) offer(events []CooldownEvent) {
	if authority.sink == nil {
		return
	}
	for _, event := range events {
		authority.sink.EnqueueItemCooldown(event)
	}
}
