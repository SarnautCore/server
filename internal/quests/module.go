// Package quests runs the quest state machine of mechanics/quests.md against
// the definitions one content pack carries.
//
// Everything that varies per quest is read from the pack: the objectives and
// their limits and targets, the prerequisites, the required level, the starter
// and finisher, and the whole reward. What is in Go is the shape of the state
// machine and the two invented constants of section 3. Adding a second quest is
// a row in the pack and nothing here.
//
// The module reaches two sibling gameplay modules and imports neither. Kill
// credit arrives as [Kill], the value mechanics/combat.md rule 5.9.3 publishes,
// and the reward grant leaves through [Granter], which `internal/inventory`
// satisfies structurally over the plain values owned by the gameplay modules.
// tidiness: combat asking the quest log whether a kill counts, or the quest
// module reaching into a bag, is how two modules stop being separable, and
// `boundary_test.go` fails the build if either import appears.
//
// The module holds no lock of its own. Every quest log is read and written
// under the zone's, either from inside a tick or through world.Zone.Command,
// which is what makes "a kill and a turn-in cannot interleave" a property of
// the composition rather than of a mutex nobody can see.
package quests

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/pack"
)

// updateQueueSize bounds the backlog between the tick loop and the fan-out
// goroutine. A full queue drops rather than blocking: the tick is the thing
// that must not stall, which is the same trade combat and the save worker make.
const updateQueueSize = 256

// Kill is one mob death, as much of it as rule 5.4 needs.
//
// It is declared here and not taken from `internal/combat` on purpose. What
// this module needs is three fields; taking the combat value would take the
// import with it, and the loot table, the placement id and the corpse despawn
// tick that come with it are things a quest has no business knowing.
type Kill struct {
	KillerEntityID uint64
	// VictimContentID is what rule 5.4.3.1 matches an objective's targets
	// against. It is content identity and never reaches a client.
	VictimContentID string
	ServerTick      uint64
}

// Sink receives quest updates for one character's session.
type Sink interface {
	OfferQuestUpdate(Update)
}

// ObjectiveProgress is one counter of one instance, as a client sees it.
type ObjectiveProgress struct {
	Index      uint32
	Counter    int32
	Limit      int32
	ShowCount  bool
	CounterKey string
}

// Update is one quest changing state, or one refusal to change it.
//
// It is a domain value. The wire message it maps onto, QuestStateUpdate, is the
// session layer's business (ADR 0028).
type Update struct {
	QuestID    string
	State      State
	Refusal    Refusal
	Objectives []ObjectiveProgress

	// The grant a turn-in committed. Zero and empty on every other update,
	// including a refused turn-in: rule 5.7.6 leaves no partial grant to
	// report.
	Experience int64
	Money      int64
	Honor      int64
	Items      []ItemCount
}

// Character is what a session hands over at zone entry: the rows checkpoint L1
// loaded, the bag they have to be reconciled against, and the level the gates
// of rule 5.3 are evaluated at.
type Character struct {
	Level     uint32
	Quests    []QuestState
	Inventory []inventory.InventoryItem
}

// Module is the quest system for one zone.
type Module struct {
	logger  *slog.Logger
	zone    gametypes.Zone
	catalog Catalog
	granter Granter

	// logs and actors are read and written only under the zone lock, which is
	// what lets this module hold no mutex of its own.
	logs   map[uuid.UUID]*questLog
	actors map[uint64]uuid.UUID

	updates chan addressed
	dropped atomic.Uint64

	sinkMu sync.Mutex
	sinks  map[uuid.UUID]Sink
}

// questLog is one character's whole quest state.
type questLog struct {
	characterID uuid.UUID
	instances   map[string]*instance
	// held is how many units of each tracked item the character has, the input
	// to rule 5.5.3's recomputation. Only items some objective names are
	// counted: a bag of thirty things does not become thirty map entries.
	held  map[string]int32
	level uint32
}

// addressed is one update and the character it belongs to. Quest state is
// replicated to its owner and to nobody else, so the address travels with the
// payload rather than being inferred at delivery.
type addressed struct {
	characterID uuid.UUID
	update      Update
}

// New builds the quest module for one zone.
func New(logger *slog.Logger, zone gametypes.Zone, catalog Catalog, granter Granter) *Module {
	if logger == nil {
		logger = slog.Default()
	}
	module := &Module{
		logger:  logger,
		zone:    zone,
		catalog: catalog,
		granter: granter,
		logs:    make(map[uuid.UUID]*questLog),
		actors:  make(map[uint64]uuid.UUID),
		updates: make(chan addressed, updateQueueSize),
		sinks:   make(map[uuid.UUID]Sink),
	}
	for _, edge := range catalog.UnresolvedPrerequisites() {
		logger.Warn("quest waits on a prerequisite this pack does not carry", "edge", edge)
	}
	return module
}

// Catalog is the definition set this module plays.
func (module *Module) Catalog() Catalog { return module.catalog }

// Admit loads one character's quest log and binds it to a world entity.
//
// The counters of every `count-item` objective are recomputed from the bag
// before anything else happens (rule 5.5.3). They have to be: the character may
// have sold the tracked item in a shop this shard has never heard of, and a
// counter restored from a row would say `completable` about a bag that is
// empty.
func (module *Module) Admit(entityID uint64, characterID uuid.UUID, loaded Character) error {
	return module.zone.GameCommand(func(gametypes.Tick) error {
		log := &questLog{
			characterID: characterID,
			instances:   make(map[string]*instance, len(loaded.Quests)),
			held:        make(map[string]int32),
			level:       loaded.Level,
		}
		for _, row := range loaded.Quests {
			definition, ok := module.catalog.Definition(row.QuestID)
			if !ok {
				// A row for a quest this pack no longer carries. It is kept out
				// of the log rather than dropped from storage: content that came
				// back would find the player's progress intact, and a shard is
				// not the place to decide a quest no longer exists.
				module.logger.Warn("stored quest is not in the content pack",
					"character_id", characterID.String(), "quest_id", row.QuestID)
				continue
			}
			held, err := instanceFromRow(row, definition)
			if err != nil {
				return err
			}
			log.instances[held.questID] = held
		}
		module.trackItems(log, loaded.Inventory)
		module.recountItems(log)
		module.logs[characterID] = log
		module.actors[entityID] = characterID
		return nil
	})
}

// Release drops a departed character's log and its entity mapping.
//
// It must be armed so that it runs *after* the session's final checkpoint: the
// checkpoint reads this log, and a log released first would persist an empty
// quest set over a real one.
func (module *Module) Release(entityID uint64) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		characterID, ok := module.actors[entityID]
		if !ok {
			return nil
		}
		delete(module.actors, entityID)
		delete(module.logs, characterID)
		return nil
	})
}

// Subscribe starts quest-update delivery for one session.
//
// It is keyed on the character and not on the entity or the session, because a
// quest log belongs to the character: rule 5.7.5's re-evaluation and rule 5.4's
// credit both address one, and both outlive any particular entity id.
func (module *Module) Subscribe(characterID uuid.UUID, sink Sink) {
	module.sinkMu.Lock()
	defer module.sinkMu.Unlock()
	module.sinks[characterID] = sink
}

// Unsubscribe stops quest-update delivery for one session.
func (module *Module) Unsubscribe(characterID uuid.UUID) {
	module.sinkMu.Lock()
	defer module.sinkMu.Unlock()
	delete(module.sinks, characterID)
}

// Run fans quest updates out to subscribed sessions until the context ends.
//
// It exists so that publishing never happens on the tick goroutine with the
// zone lock held: a slow session would otherwise stall the simulation.
func (module *Module) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if dropped := module.dropped.Load(); dropped > 0 {
				module.logger.Warn("quest updates were dropped",
					"zone_id", module.zone.ID(), "dropped", dropped)
			}
			return
		case queued := <-module.updates:
			module.deliver(queued)
		}
	}
}

func (module *Module) deliver(queued addressed) {
	module.sinkMu.Lock()
	sink := module.sinks[queued.characterID]
	module.sinkMu.Unlock()
	if sink == nil {
		return
	}
	sink.OfferQuestUpdate(queued.update)
}

// publish queues one update. It is called with the zone lock held, so it must
// never block, which means a full queue has to lose something.
func (module *Module) publish(characterID uuid.UUID, update Update) {
	select {
	case module.updates <- addressed{characterID: characterID, update: update}:
	default:
		module.dropped.Add(1)
	}
}

// CreditKill is rule 5.4: one mob death turned into objective progress.
//
// It is called from inside the tick that caused the death, with the zone lock
// held, so it does exactly what a world.System may do — mutate through state
// this lock already covers, and nothing that blocks.
//
// Credit goes to the killer and to nobody else (rule 5.4.2). One kill
// increments at most one counter per objective, and may increment counters in
// several quests, which is intended (rule 5.4.4). The state transition is
// evaluated once per quest after all of that quest's counters have moved (rule
// 5.4.5), so a kill that satisfies the last two objectives at once reports one
// completion rather than an intermediate state that was never true.
func (module *Module) CreditKill(_ gametypes.Tick, kill Kill) {
	characterID, ok := module.actors[kill.KillerEntityID]
	if !ok {
		return
	}
	log, ok := module.logs[characterID]
	if !ok {
		return
	}
	for _, questID := range sortedIDs(log.instances) {
		held := log.instances[questID]
		if !held.state.Active() {
			continue
		}
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			continue
		}
		var moved bool
		for index, objective := range definition.Objectives {
			if objective.Kind != pack.QuestObjectiveCountKill {
				continue
			}
			if index >= len(held.counters) || held.counters[index] >= objective.Limit {
				// Rule 5.4.3.2: already satisfied. Over-counting would make the
				// regression check of T10 ambiguous.
				continue
			}
			if !matches(objective.TargetIDs, kill.VictimContentID) {
				continue
			}
			held.counters[index]++
			moved = true
		}
		if !moved {
			continue
		}
		held.state = progressState(definition, held.counters)
		module.publish(characterID, module.updateFor(definition, held))
	}
}

// InventoryChanged is rule 5.5.3: a `count-item` counter is recomputed from
// what the character holds, never incremented.
//
// It is what makes that kind the only one that can regress (rule 5.5.4): a
// dropped or sold item lowers the counter and can move a `completable` instance
// back to `in-progress`, which is transition T10.
func (module *Module) InventoryChanged(characterID uuid.UUID, items []inventory.InventoryItem) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		module.trackItems(log, items)
		for _, update := range module.recountItems(log) {
			module.publish(characterID, update)
		}
		return nil
	})
}

// trackItems refreshes the held-item tally from a whole bag.
//
// Only items some objective names are counted. The bag is the authority and the
// tally is derived from it every time, so an item that left the bag between two
// calls disappears from the tally rather than lingering as a stale count.
func (module *Module) trackItems(log *questLog, items []inventory.InventoryItem) {
	tracked := make(map[string]struct{})
	for questID := range log.instances {
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			continue
		}
		for _, objective := range definition.Objectives {
			if objective.Kind != pack.QuestObjectiveCountItem {
				continue
			}
			for _, target := range objective.TargetIDs {
				tracked[target] = struct{}{}
			}
		}
	}
	held := make(map[string]int32, len(tracked))
	for _, item := range items {
		if _, ok := tracked[item.ItemID]; !ok {
			continue
		}
		held[item.ItemID] += item.Quantity
	}
	log.held = held
}

// recountItems applies the tally to every active instance and reports the ones
// that changed state or counter.
func (module *Module) recountItems(log *questLog) []Update {
	var updates []Update
	for _, questID := range sortedIDs(log.instances) {
		held := log.instances[questID]
		if !held.state.Active() && held.state != StateCompletable {
			continue
		}
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			continue
		}
		var moved bool
		for index, objective := range definition.Objectives {
			if objective.Kind != pack.QuestObjectiveCountItem || index >= len(held.counters) {
				continue
			}
			counted := int32(0)
			for _, target := range objective.TargetIDs {
				counted += log.held[target]
			}
			if counted > objective.Limit {
				counted = objective.Limit
			}
			if counted != held.counters[index] {
				held.counters[index] = counted
				moved = true
			}
		}
		if !moved {
			continue
		}
		held.state = progressState(definition, held.counters)
		updates = append(updates, module.updateFor(definition, held))
	}
	return updates
}

// Rows is one character's quest log as a checkpoint persists it.
//
// It returns nil for a character this module does not hold, which is the
// ordinary answer once the session has been released; a caller that treated nil
// as "no quests" and wrote it would erase a log, so the session keeps its
// loaded rows and only replaces them when this answers non-nil.
func (module *Module) Rows(characterID uuid.UUID) []QuestState {
	var rows []QuestState
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		rows = make([]QuestState, 0, len(log.instances))
		for _, questID := range sortedIDs(log.instances) {
			row, err := log.instances[questID].row()
			if err != nil {
				module.logger.Error("encode quest row",
					"character_id", characterID.String(), "quest_id", questID, "error", err)
				continue
			}
			rows = append(rows, row)
		}
		return nil
	})
	return rows
}

// Log is one character's journal: every quest they hold, in canonical-id order.
//
// It is what a session sends on zone entry, and it deliberately stops there.
// What the catalog would *offer* is a property of standing in front of an NPC,
// not of logging in: rule 5.3 evaluates gates on demand when a player interacts
// with a starter, and a login that enumerated every offer in the zone would be
// answering a question nobody asked with a frame per quest.
func (module *Module) Log(characterID uuid.UUID) []Update {
	var updates []Update
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		for _, questID := range sortedIDs(log.instances) {
			definition, found := module.catalog.Definition(questID)
			if !found {
				continue
			}
			held := log.instances[questID]
			if !held.state.Held() {
				// A stored `offered` row — which is what a chargen starting
				// quest materializes as — or an `abandoned` one. Neither is an
				// instance (rule 5.1), so neither belongs in a journal.
				continue
			}
			updates = append(updates, module.updateFor(definition, held))
		}
		return nil
	})
	return updates
}

// gate is rule 5.3, evaluated for one character against one definition.
func (module *Module) gate(log *questLog, definition pack.Quest) State {
	if held, ok := log.instances[definition.ID]; ok && held.state.Held() {
		// Rule 5.3.1. An `abandoned` instance is the one exception: T16 lets
		// the quest be taken again from scratch.
		return StateUnavailable
	}
	if definition.RequiredLevel > 0 && log.level < definition.RequiredLevel {
		return StateUnavailable
	}
	for _, prerequisite := range definition.Prerequisites {
		// Rule 5.3.4: conjunctive. All must pass.
		done, ok := log.instances[prerequisite.QuestID]
		if !ok || done.state != StateTurnedIn {
			return StateUnavailable
		}
	}
	return StateOffered
}

// updateFor renders one instance for a client. Objectives marked `internal` are
// evaluated and not shown (rule 5.5.7).
func (module *Module) updateFor(definition pack.Quest, held *instance) Update {
	update := Update{QuestID: held.questID, State: held.state}
	for index, objective := range definition.Objectives {
		if objective.Internal || index >= len(held.counters) {
			continue
		}
		update.Objectives = append(update.Objectives, ObjectiveProgress{
			Index:      uint32(index),
			Counter:    held.counters[index],
			Limit:      objective.Limit,
			ShowCount:  objective.ShowCount,
			CounterKey: objective.CounterKey,
		})
	}
	return update
}

// matches reports whether one content id is among an objective's targets. Any
// one of them counts (rule 5.5.1).
func matches(targets []string, contentID string) bool {
	for _, target := range targets {
		if target == contentID {
			return true
		}
	}
	return false
}

func sortedIDs(instances map[string]*instance) []string {
	ids := make([]string, 0, len(instances))
	for id := range instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
