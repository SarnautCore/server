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
	"time"

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
	// PlayerFaction is the faction an entering character belongs to. It is the
	// same seam as PlayerLevel: chargen decides it (ADR 0032). Empty, or a
	// faction the pack does not describe, means the pack's first playable
	// faction in canonical-id order.
	PlayerFaction string
	// PlayerLifecycle is authored native content. Nil is allowed while a zone
	// has no players, but admitting a persisted dead player or resolving a
	// player death fails closed without it.
	PlayerLifecycle *PlayerLifecycleRules
}

// PlayerLifecycleRules are the private native values used after a player
// death. They are injected by composition; combat carries no fallback timing.
type PlayerLifecycleRules struct {
	RespawnDelay         time.Duration
	ResurrectionSickness time.Duration
}

func (rules PlayerLifecycleRules) valid() bool {
	return rules.RespawnDelay > 0 && rules.ResurrectionSickness > 0
}

// PlayerAdmission is the persisted and content-authored combat identity of one
// character. A caller must provide every field; admission does not synthesize
// level, health, or a loadout.
type PlayerAdmission struct {
	Level                         uint32
	Experience                    int64
	Health                        int32
	MaxHealth                     int32
	AbilityIDs                    []string
	ResurrectionSicknessRemaining time.Duration
}

// Module is the combat system for one zone.
type Module struct {
	logger    *slog.Logger
	zone      gametypes.Zone
	rules     Rules
	faction   string
	lifecycle *PlayerLifecycleRules

	// mobs, casters, stream and killSink are read and written only under the
	// zone lock.
	mobs     map[uint64]*mobState
	players  map[uint64]*playerState
	casters  map[uint64]*casterState
	stream   *spawnStream
	killSink KillSink
	// damageEffects is the optional session-owned adapter for persistent
	// script modifiers. It is installed and read only under the zone lock.
	damageEffects DamageEffectHost

	events  chan Event
	dropped atomic.Uint64

	sinkMu sync.Mutex
	sinks  map[uint64]EventSink
}

// New builds the combat module for one zone and registers it as a per-tick
// system.
func New(logger *slog.Logger, zone gametypes.Zone, rules Rules, options Options) *Module {
	faction := options.PlayerFaction
	if faction == "" || !rules.HasFaction(faction) {
		faction = rules.PlayerFaction()
	}
	module := &Module{
		logger:    logger,
		zone:      zone,
		rules:     rules,
		faction:   faction,
		lifecycle: options.PlayerLifecycle,
		mobs:      make(map[uint64]*mobState),
		players:   make(map[uint64]*playerState),
		casters:   make(map[uint64]*casterState),
		stream:    newSpawnStream(options.Seed),
		events:    make(chan Event, eventQueueSize),
		sinks:     make(map[uint64]EventSink),
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
			module.mobs[entityID] = module.newMobState(mob, spawn)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Admit gives a joined player its combat identity, from the pack.
func (module *Module) Admit(entityID uint64, admission PlayerAdmission) error {
	return module.zone.GameCommand(func(tick gametypes.Tick) error {
		entity := tick.Entity(entityID)
		if entity == nil || entity.Kind != gametypes.EntityKindPlayer {
			return fmt.Errorf("admit entity %d: %w", entityID, gametypes.ErrUnknownEntity)
		}
		if admission.Level == 0 || admission.Experience < 0 {
			return fmt.Errorf("admit entity %d: invalid loaded progression", entityID)
		}
		if admission.MaxHealth <= 0 || admission.Health < 0 || admission.Health > admission.MaxHealth {
			return fmt.Errorf("admit entity %d: loaded health %d is outside 0..%d",
				entityID, admission.Health, admission.MaxHealth)
		}
		if admission.ResurrectionSicknessRemaining < 0 {
			return fmt.Errorf("admit entity %d: loaded resurrection sickness is negative", entityID)
		}
		if len(admission.AbilityIDs) == 0 {
			return fmt.Errorf("admit entity %d: authored ability loadout is empty", entityID)
		}
		abilities := append([]string(nil), admission.AbilityIDs...)
		for _, abilityID := range abilities {
			if _, ok := module.rules.Ability(abilityID); !ok {
				return fmt.Errorf("admit entity %d: authored ability %q is absent", entityID, abilityID)
			}
		}
		if admission.Health == 0 && (module.lifecycle == nil || !module.lifecycle.valid()) {
			return fmt.Errorf("admit entity %d: dead load has no authored player lifecycle", entityID)
		}

		entity.Faction = module.faction
		entity.Level = admission.Level
		entity.MaxHealth = admission.MaxHealth
		entity.Health = admission.Health
		entity.Alive = admission.Health > 0
		module.casters[entityID] = &casterState{abilities: abilities}
		module.players[entityID] = &playerState{
			anchor: tick.Position(entity), anchorYaw: entity.Heading,
			phase: playerAlive,
		}
		if !entity.Alive {
			module.players[entityID].phase = playerDead
			module.schedulePlayerRespawn(tick, entity)
		} else if admission.ResurrectionSicknessRemaining > 0 {
			module.resumePlayerSickness(tick, entity, admission.ResurrectionSicknessRemaining)
		}
		return nil
	})
}

// Release drops the combat state of a departed player.
func (module *Module) Release(entityID uint64) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		delete(module.casters, entityID)
		delete(module.players, entityID)
		return nil
	})
	module.Unsubscribe(entityID)
}

// ApplyPlayerProgression projects a level that has already committed off the
// tick path. It changes simulation state only; persistence and event ordering
// belong to the progression service and session adapter.
func (module *Module) ApplyPlayerProgression(entityID uint64, level uint32) error {
	return module.zone.GameCommand(func(tick gametypes.Tick) error {
		entity := tick.Entity(entityID)
		if entity == nil || entity.Kind != gametypes.EntityKindPlayer {
			return fmt.Errorf("project progression entity %d: %w", entityID, gametypes.ErrUnknownEntity)
		}
		if _, ok := module.players[entityID]; !ok {
			return fmt.Errorf("project progression entity %d: no player combat state", entityID)
		}
		if level == 0 || level < entity.Level {
			return fmt.Errorf("project progression entity %d: level %d cannot replace %d",
				entityID, level, entity.Level)
		}
		entity.Level = level
		return nil
	})
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
	if event.Kind != EventKindDeath && event.Kind != EventKindPlayerDeath &&
		event.Kind != EventKindPlayerRespawn && event.Kind != EventKindResurrectionSicknessExpired {
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
	abilities    []string
	gcdReadyTick uint64
	readyTick    map[string]uint64
	lastSeq      uint64
	hasSeq       bool
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
