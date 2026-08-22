// Package combat resolves ability use, damage, death and mob AI against the
// rules one content pack carries.
//
// Everything that varies per ability, per mob or per spawn slot is read from
// the pack: range, damage, cooldown, level, health multiplier, faction
// hostility, aggro radius, leash radius and respawn window. Adding a second
// ability or a second mob is a content change and nothing else. What is in Go
// is the shape of the formulae, which mechanics/combat.md section 3 states as
// constants and section 7.1 marks as the one placeholder it has not been able
// to source.
//
// The module holds no lock. Every mutation runs inside the zone's, either as a
// registered gametypes.System or through Zone.GameCommand.
package combat

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/SarnautCore/server/internal/gametypes"
)

// eventQueueSize bounds the backlog between the tick loop and the fan-out
// goroutine. A full queue drops rather than blocking: the tick is the thing
// that must not stall, which is the same trade the persistence save worker
// makes.
const eventQueueSize = 256

// Options tunes one combat module.
type Options struct {
	// Seed pins the zone spawn stream. Two runs with the same seed over the
	// same pack draw the same mob levels and the same respawn delays.
	Seed uint64
	// PlayerLevel is the level an entering character is admitted at.
	//
	// It is a seam, not a rule: a character's level is persisted state, and it
	// arrives from ADR 0031's shard.character_state once the auth handshake
	// carries a character id. Zero means level one.
	PlayerLevel uint32
	// PlayerFaction is the faction an entering character belongs to. It is the
	// same seam as PlayerLevel: chargen decides it (ADR 0032). Empty, or a
	// faction the pack does not describe, means the pack's first playable
	// faction in canonical-id order.
	PlayerFaction string
}

// Module is the combat system for one zone.
type Module struct {
	logger  *slog.Logger
	zone    gametypes.Zone
	rules   Rules
	level   uint32
	faction string

	// mobs, casters, stream and killSink are read and written only under the
	// zone lock.
	mobs     map[uint64]*mobState
	casters  map[uint64]*casterState
	stream   *spawnStream
	killSink KillSink
	// damageEffects is the optional session-owned adapter for persistent
	// script modifiers. It is installed and read only under the zone lock.
	damageEffects DamageEffectHost
	// scriptActions is the session-owned adapter that executes extracted
	// action trees. Combat still owns admission, targets, resource, cooldown,
	// damage, death, and events.
	scriptActions          ScriptActionHost
	scriptDamageEvents     map[uint64]scriptDamageReplay
	scriptTargetExecutions map[uint64]scriptTargetReplay

	events  chan Event
	dropped atomic.Uint64

	sinkMu sync.Mutex
	sinks  map[uint64]EventSink
}

// New builds the combat module for one zone and registers it as a per-tick
// system.
func New(logger *slog.Logger, zone gametypes.Zone, rules Rules, options Options) *Module {
	level := options.PlayerLevel
	if level == 0 {
		level = 1
	}
	faction := options.PlayerFaction
	if faction == "" || !rules.HasFaction(faction) {
		faction = rules.PlayerFaction()
	}
	module := &Module{
		logger:                 logger,
		zone:                   zone,
		rules:                  rules,
		level:                  level,
		faction:                faction,
		mobs:                   make(map[uint64]*mobState),
		casters:                make(map[uint64]*casterState),
		stream:                 newSpawnStream(options.Seed),
		events:                 make(chan Event, eventQueueSize),
		sinks:                  make(map[uint64]EventSink),
		scriptDamageEvents:     make(map[uint64]scriptDamageReplay),
		scriptTargetExecutions: make(map[uint64]scriptTargetReplay),
	}
	zone.GameAddSystem(module)
	return module
}

// Rules is the rule set this module resolves against.
func (module *Module) Rules() Rules { return module.rules }

// SetKillSink registers what to tell about a mob death, replacing whatever was
// there. Passing nil detaches.
//
// It is a setter rather than an Options field because the sink needs the zone
// and so does this module, and one of the two has to be built second. The write
// goes through Zone.Command so the field is published under the same lock the
// tick loop reads it under, rather than being raced into place while a kill is
// resolving.
func (module *Module) SetKillSink(sink KillSink) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		module.killSink = sink
		return nil
	})
}

// Populate spawns every NPC the pack resolved, drawing each mob's level from
// the zone spawn stream in placement order.
func (module *Module) Populate(spawns []gametypes.NPCSpawn) error {
	for _, spawn := range spawns {
		mob, ok := module.rules.Mob(spawn.MobID)
		if !ok {
			return fmt.Errorf("spawn %q names mob %q, which the rules do not describe",
				spawn.PlacementID, spawn.MobID)
		}
		var entityID uint64
		if err := module.zone.GameCommand(func(tick gametypes.Tick) error {
			level := module.stream.level(mob.LevelMin, mob.LevelMax)
			maxHealth := MaxHealth(level, mob.HPMod)
			entityID = tick.SpawnNPC(gametypes.NPCSpec{
				ContentID:   mob.ID,
				NameKey:     mob.NameKey,
				PlacementID: spawn.PlacementID,
				Faction:     mob.FactionID,
				Level:       level,
				MaxHealth:   maxHealth,
				Position:    gametypes.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z},
				Heading:     spawn.Heading,
			}).ID
			module.retireScriptReplays(entityID)
			module.mobs[entityID] = module.newMobState(mob, spawn)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Admit gives a joined player its combat identity and the legacy content-pack
// action list. New session code should call AdmitWithActions so character
// content, rather than the complete pack, decides what the player knows.
func (module *Module) Admit(entityID uint64) error {
	abilityIDs := module.rules.AbilityIDs()
	bindings := make([]ActionBinding, 0, min(len(abilityIDs), ActionBarSlotCount))
	for index, abilityID := range abilityIDs {
		if index == ActionBarSlotCount {
			break
		}
		bindings = append(bindings, ActionBinding{SlotIndex: uint32(index), AbilityID: abilityID})
	}
	return module.admit(entityID, abilityIDs, bindings)
}

// AdmitWithActions gives a joined player only the abilities and slot bindings
// authored for that character. Every binding is validated before the player
// entity or caster state changes.
func (module *Module) AdmitWithActions(entityID uint64, bindings []ActionBinding) error {
	return module.AdmitWithLoadout(entityID, ActionLoadout{Bindings: bindings})
}

// AdmitWithLoadout admits authored slots and the character's authoritative
// action resource as one validated change.
func (module *Module) AdmitWithLoadout(entityID uint64, loadout ActionLoadout) error {
	abilities, actionBar, err := module.validateActionBindings(loadout.Bindings)
	if err != nil {
		return err
	}
	resource, err := validateActionResource(loadout.Resource)
	if err != nil {
		return err
	}
	return module.admitValidated(entityID, abilities, actionBar, resource)
}

func (module *Module) admit(entityID uint64, abilities []string, bindings []ActionBinding) error {
	validatedAbilities, actionBar, err := module.validateActionBindings(bindings)
	if err != nil {
		return err
	}
	// Legacy Admit deliberately preserves direct UseAbility access to every
	// pack ability, including any beyond the 36 visible slots.
	validatedAbilities = append(validatedAbilities[:0], abilities...)
	return module.admitValidated(entityID, validatedAbilities, actionBar, actionResource{})
}

func (module *Module) admitValidated(
	entityID uint64,
	abilities []string,
	actionBar [ActionBarSlotCount]string,
	resource actionResource,
) error {
	return module.zone.GameCommand(func(tick gametypes.Tick) error {
		entity := tick.Entity(entityID)
		if entity == nil || entity.Kind != gametypes.EntityKindPlayer {
			return fmt.Errorf("admit entity %d: %w", entityID, gametypes.ErrUnknownEntity)
		}
		entity.Faction = module.faction
		entity.Level = module.level
		entity.MaxHealth = MaxHealth(module.level, defaultHPMod)
		entity.Health = entity.MaxHealth
		entity.Alive = true
		module.retireScriptReplays(entityID)
		module.casters[entityID] = &casterState{
			abilities: append([]string(nil), abilities...),
			actionBar: actionBar,
			resource:  resource,
		}
		return nil
	})
}

// Release drops the combat state of a departed player.
func (module *Module) Release(entityID uint64) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		delete(module.casters, entityID)
		module.retireScriptReplays(entityID)
		return nil
	})
	module.Unsubscribe(entityID)
}

// Subscribe starts combat-event delivery for one session.
func (module *Module) Subscribe(entityID uint64, sink EventSink) {
	module.sinkMu.Lock()
	defer module.sinkMu.Unlock()
	module.sinks[entityID] = sink
}

// Unsubscribe stops combat-event delivery for one session.
func (module *Module) Unsubscribe(entityID uint64) {
	module.sinkMu.Lock()
	defer module.sinkMu.Unlock()
	delete(module.sinks, entityID)
}

// Run fans combat events out to subscribed sessions until the context ends.
//
// It exists so that publishing an event never happens on the tick goroutine
// with the zone lock held: a slow session would otherwise stall the
// simulation.
func (module *Module) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if dropped := module.dropped.Load(); dropped > 0 {
				module.logger.WarnContext(ctx, "combat events were dropped",
					"zone_id", module.zone.ID(), "dropped", dropped)
			}
			return
		case event := <-module.events:
			module.deliver(event)
		}
	}
}

func (module *Module) deliver(event Event) {
	module.sinkMu.Lock()
	targets := make([]EventSink, 0, len(module.sinks))
	if event.PrivateTo != 0 {
		if sink, ok := module.sinks[event.PrivateTo]; ok {
			targets = append(targets, sink)
		}
	} else {
		for _, sink := range module.sinks {
			targets = append(targets, sink)
		}
	}
	module.sinkMu.Unlock()

	for _, sink := range targets {
		sink.OfferCombatEvent(event)
	}
}

// publish queues an event. It is called with the zone lock held, so it must
// never block, which means a full queue has to lose something.
//
// What it loses is never a death. A refusal or a hit is one frame among many
// and a client that misses one is corrected by the next snapshot; a death
// happens once, and nothing later carries it again. So an ability event on a
// full queue is dropped, and a death displaces the oldest thing in the queue
// instead.
func (module *Module) publish(event Event) {
	select {
	case module.events <- event:
		return
	default:
	}
	if event.Kind != EventKindDeath {
		module.dropped.Add(1)
		return
	}
	select {
	case <-module.events:
		module.dropped.Add(1)
	default:
	}
	select {
	case module.events <- event:
	default:
		module.dropped.Add(1)
	}
}

// casterState is one entity's ability bookkeeping, in ticks.
type casterState struct {
	abilities     []string
	actionBar     [ActionBarSlotCount]string
	selected      uint64
	gcdReadyTick  uint64
	readyTick     map[string]uint64
	castReadyTick uint64
	actionOrdinal uint64
	lastSeq       uint64
	hasSeq        bool
	resource      actionResource
}

func (state *casterState) knows(abilityID string) bool {
	for _, known := range state.abilities {
		if known == abilityID {
			return true
		}
	}
	return false
}

// defaultAbility is what an AbilityUse with no ability id selects: the first
// ability the caster knows, in canonical-id order.
func (state *casterState) defaultAbility() string {
	if len(state.abilities) == 0 {
		return ""
	}
	return state.abilities[0]
}
