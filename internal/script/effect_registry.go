package script

import (
	"fmt"
	"reflect"
	"sort"
	"time"
)

const guardRecheckInterval = 2 * time.Second

// EffectOwner carries the two runtime facts Guard requires. The session
// adapter derives them from the world without making this package import it.
type EffectOwner struct {
	Mob        bool
	CellPlaced bool
}

// EffectChange tells an adapter what changed outside the registry. The
// registry owns aggregation and idempotence; an aggro adapter owns the actual
// emitter, observer, noticed list, and mark counter.
type EffectChange struct {
	Changed bool
	// GuardActive is true while at least one guard radius remains.
	GuardActive bool
	// AggroMarkDelta is +1 on the first guard and -1 on the last removal.
	AggroMarkDelta int
	// RemoveAggroState is true only when the last guard goes away.
	RemoveAggroState bool
	ObserverRadius   Decimal
	NoticeTarget     bool
	// RecheckEvery is retail's nearby-creature aggro refresh cadence. It is
	// non-zero while GuardActive so an aggro adapter schedules one timer for the
	// shared guard state rather than one timer per radius.
	RecheckEvery time.Duration
}

type guardEntry struct {
	guard                Guard
	order                uint64
	lifecycleAttempt     uint64
	previousNoticeTarget bool
}

type modifierEntry struct {
	modifier         DamageModifier
	order            uint64
	lifecycleAttempt uint64
}

type guardOwnerState struct {
	entries      map[string]guardEntry
	noticeTarget bool
}

// EffectRegistry is the authoritative in-memory registry for persistent
// script effects. Apply is idempotent by EffectID and Modifiers returns a
// stable priority order. A durable adapter can persist the same commands.
type EffectRegistry struct {
	sequence  uint64
	guards    map[string]*guardOwnerState
	modifiers map[string]map[string]modifierEntry
}

func NewEffectRegistry() *EffectRegistry {
	return &EffectRegistry{
		guards: make(map[string]*guardOwnerState), modifiers: make(map[string]map[string]modifierEntry),
	}
}

// ApplyAtomic updates the registry and then applies the corresponding host
// change. If the host rejects that change, the registry is restored exactly,
// including attachment order. Replayed commands do not call apply because
// they changed no state and must not perturb an observer's timer.
func (registry *EffectRegistry) ApplyAtomic(
	command Command,
	owner EffectOwner,
	apply func(EffectChange) error,
) (EffectChange, error) {
	if registry == nil {
		return EffectChange{}, fmt.Errorf("script: nil effect registry")
	}
	before := registry.snapshot(command.EntityID, command.EffectID)
	change, err := registry.Apply(command, owner)
	if err != nil || !change.Changed || apply == nil {
		return change, err
	}
	if err := apply(change); err != nil {
		registry.restore(command.EntityID, command.EffectID, before)
		return EffectChange{}, err
	}
	return change, nil
}

type effectSnapshot struct {
	sequence uint64

	guardState        *guardOwnerState
	guardStateExists  bool
	guardEntry        guardEntry
	guardEntryExists  bool
	guardNoticeTarget bool

	modifierEntries      map[string]modifierEntry
	modifierEntriesExist bool
	modifierEntry        modifierEntry
	modifierEntryExists  bool
}

// snapshot records only the entry one command can mutate. ApplyAtomic used to
// clone every entity's registry for every effect, making a trigger with N
// effects pay for the whole live registry N times.
func (registry *EffectRegistry) snapshot(entityID, effectID string) effectSnapshot {
	snapshot := effectSnapshot{sequence: registry.sequence}
	if state, ok := registry.guards[entityID]; ok {
		snapshot.guardState = state
		snapshot.guardStateExists = true
		snapshot.guardNoticeTarget = state.noticeTarget
		snapshot.guardEntry, snapshot.guardEntryExists = state.entries[effectID]
	}
	if entries, ok := registry.modifiers[entityID]; ok {
		snapshot.modifierEntries = entries
		snapshot.modifierEntriesExist = true
		snapshot.modifierEntry, snapshot.modifierEntryExists = entries[effectID]
	}
	return snapshot
}

func (registry *EffectRegistry) restore(entityID, effectID string, snapshot effectSnapshot) {
	registry.sequence = snapshot.sequence
	if !snapshot.guardStateExists {
		delete(registry.guards, entityID)
	} else {
		registry.guards[entityID] = snapshot.guardState
		snapshot.guardState.noticeTarget = snapshot.guardNoticeTarget
		if snapshot.guardEntryExists {
			snapshot.guardState.entries[effectID] = snapshot.guardEntry
		} else {
			delete(snapshot.guardState.entries, effectID)
		}
	}
	if !snapshot.modifierEntriesExist {
		delete(registry.modifiers, entityID)
	} else {
		registry.modifiers[entityID] = snapshot.modifierEntries
		if snapshot.modifierEntryExists {
			snapshot.modifierEntries[effectID] = snapshot.modifierEntry
		} else {
			delete(snapshot.modifierEntries, effectID)
		}
	}
}

// Apply registers or removes one typed persistent-effect command.
func (registry *EffectRegistry) Apply(command Command, owner EffectOwner) (EffectChange, error) {
	if registry == nil {
		return EffectChange{}, fmt.Errorf("script: nil effect registry")
	}
	if command.EntityID == "" || command.EffectID == "" {
		return EffectChange{}, fmt.Errorf("script: persistent effect command has no entity or effect id")
	}
	switch command.Kind {
	case CommandAttachGuard:
		if command.Guard == nil {
			return EffectChange{}, fmt.Errorf("script: attach guard %s carries no guard", command.EffectID)
		}
		if command.Guard.NoticeTarget {
			return EffectChange{}, fmt.Errorf("script: guard %s noticeTarget=true is not implemented", command.EffectID)
		}
		if !owner.Mob || !owner.CellPlaced {
			return EffectChange{}, fmt.Errorf(
				"script: guard %s requires a cell-placed mob owner", command.EffectID,
			)
		}
		return registry.attachGuard(
			command.EntityID, command.EffectID, *command.Guard, command.LifecycleAttempt,
		)

	case CommandDetachGuard:
		return registry.detachGuard(
			command.EntityID, command.EffectID, command.Rollback, command.LifecycleAttempt,
		)

	case CommandAttachDamageModifier:
		if command.DamageModifier == nil {
			return EffectChange{}, fmt.Errorf("script: attach modifier %s carries no modifier", command.EffectID)
		}
		return registry.attachModifier(
			command.EntityID, command.EffectID, *command.DamageModifier, command.LifecycleAttempt,
		)

	case CommandDetachDamageModifier:
		return registry.detachModifier(
			command.EntityID, command.EffectID, command.Rollback, command.LifecycleAttempt,
		), nil

	default:
		return EffectChange{}, fmt.Errorf("script: command kind %d is not a persistent effect", command.Kind)
	}
}

func (registry *EffectRegistry) attachGuard(
	entityID, effectID string, guard Guard, lifecycleAttempt uint64,
) (EffectChange, error) {
	radius, ok := decimalAmount(guard.Radius)
	if !ok {
		return EffectChange{}, fmt.Errorf("script: guard %s radius is not representable", effectID)
	}
	if comparison, _ := radius.compare(integerAmount(0)); comparison < 0 {
		return EffectChange{}, fmt.Errorf("script: guard %s radius is negative", effectID)
	}
	state := registry.guards[entityID]
	if state == nil {
		state = &guardOwnerState{entries: make(map[string]guardEntry)}
		registry.guards[entityID] = state
	}
	if existing, exists := state.entries[effectID]; exists {
		if existing.guard != guard {
			return EffectChange{}, fmt.Errorf("script: replayed guard %s changed its payload", effectID)
		}
		return registry.guardChange(entityID, Decimal{}, false), nil
	}
	registry.sequence++
	state.entries[effectID] = guardEntry{
		guard: guard, order: registry.sequence, lifecycleAttempt: lifecycleAttempt,
		previousNoticeTarget: state.noticeTarget,
	}
	// Retail stores noticeTarget on the shared GuardPart. A later attach wins;
	// removing it does not restore an earlier value.
	state.noticeTarget = guard.NoticeTarget
	change := registry.guardChange(entityID, Decimal{}, false)
	change.Changed = true
	if len(state.entries) == 1 {
		change.AggroMarkDelta = 1
	}
	return change, nil
}

func (registry *EffectRegistry) detachGuard(
	entityID, effectID string, rollback bool, lifecycleAttempt uint64,
) (EffectChange, error) {
	state := registry.guards[entityID]
	if state == nil {
		return EffectChange{}, nil
	}
	entry, exists := state.entries[effectID]
	if !exists {
		return registry.guardChange(entityID, Decimal{}, false), nil
	}
	if rollback && entry.lifecycleAttempt != lifecycleAttempt {
		return registry.guardChange(entityID, Decimal{}, false), nil
	}
	delete(state.entries, effectID)
	if rollback {
		state.noticeTarget = entry.previousNoticeTarget
		if entry.order == registry.sequence {
			registry.sequence--
		}
	}
	if len(state.entries) == 0 {
		delete(registry.guards, entityID)
		return EffectChange{
			Changed: true, AggroMarkDelta: -1, RemoveAggroState: true,
		}, nil
	}
	change := registry.guardChange(entityID, Decimal{}, false)
	change.Changed = true
	return change, nil
}

// GuardState returns the aggregated observer state. The active radius is the
// smaller of sightRadius and the largest attached guard radius.
func (registry *EffectRegistry) GuardState(entityID string, sightRadius Decimal) EffectChange {
	return registry.guardChange(entityID, sightRadius, true)
}

func (registry *EffectRegistry) guardChange(entityID string, sightRadius Decimal, capAtSight bool) EffectChange {
	state := registry.guards[entityID]
	if state == nil || len(state.entries) == 0 {
		return EffectChange{}
	}
	var maximum amount
	first := true
	for _, entry := range state.entries {
		radius, ok := decimalAmount(entry.guard.Radius)
		if !ok {
			continue
		}
		if first {
			maximum, first = radius, false
			continue
		}
		if comparison, ok := radius.compare(maximum); ok && comparison > 0 {
			maximum = radius
		}
	}
	active := maximum
	if sight, ok := decimalAmount(sightRadius); capAtSight && ok {
		if comparison, comparable := sight.compare(active); comparable && comparison < 0 {
			active = sight
		}
	}
	return EffectChange{
		GuardActive: true, ObserverRadius: active.decimal(), NoticeTarget: state.noticeTarget,
		RecheckEvery: guardRecheckInterval,
	}
}

func (registry *EffectRegistry) attachModifier(
	entityID, effectID string, modifier DamageModifier, lifecycleAttempt uint64,
) (EffectChange, error) {
	modifier.EntityID = entityID
	modifier.EffectID = effectID
	if modifier.StackCount <= 0 {
		return EffectChange{}, fmt.Errorf("script: modifier %s has non-positive stack count", effectID)
	}
	if modifier.Direction != DamageIncoming && modifier.Direction != DamageOutgoing {
		return EffectChange{}, fmt.Errorf("script: modifier %s has no direction", effectID)
	}
	if modifier.Priority != DamagePriorityScaleAll {
		return EffectChange{}, fmt.Errorf("script: modifier %s has unsupported priority %d", effectID, modifier.Priority)
	}
	entries := registry.modifiers[entityID]
	if entries == nil {
		entries = make(map[string]modifierEntry)
		registry.modifiers[entityID] = entries
	}
	if existing, exists := entries[effectID]; exists {
		if !reflect.DeepEqual(existing.modifier, modifier) {
			return EffectChange{}, fmt.Errorf("script: replayed modifier %s changed its payload", effectID)
		}
		return EffectChange{}, nil
	}
	registry.sequence++
	entries[effectID] = modifierEntry{
		modifier: modifier, order: registry.sequence, lifecycleAttempt: lifecycleAttempt,
	}
	return EffectChange{Changed: true}, nil
}

func (registry *EffectRegistry) detachModifier(
	entityID, effectID string, rollback bool, lifecycleAttempt uint64,
) EffectChange {
	entries := registry.modifiers[entityID]
	if entries == nil {
		return EffectChange{}
	}
	entry, exists := entries[effectID]
	if !exists {
		return EffectChange{}
	}
	if rollback && entry.lifecycleAttempt != lifecycleAttempt {
		return EffectChange{}
	}
	delete(entries, effectID)
	if rollback && entry.order == registry.sequence {
		registry.sequence--
	}
	if len(entries) == 0 {
		delete(registry.modifiers, entityID)
	}
	return EffectChange{Changed: true}
}

// Modifiers returns one owner's direction-specific modifiers in ascending
// priority and stable attachment order.
func (registry *EffectRegistry) Modifiers(entityID string, direction DamageDirection) []DamageModifier {
	entries := registry.modifiers[entityID]
	ordered := make([]modifierEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.modifier.Direction == direction {
			ordered = append(ordered, entry)
		}
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].modifier.Priority == ordered[right].modifier.Priority {
			return ordered[left].order < ordered[right].order
		}
		return ordered[left].modifier.Priority < ordered[right].modifier.Priority
	})
	result := make([]DamageModifier, 0, len(ordered))
	for _, entry := range ordered {
		result = append(result, entry.modifier)
	}
	return result
}
