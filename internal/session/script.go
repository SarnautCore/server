package session

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/world"
)

// This file is the impact interpreter's session adapter: the one place
// `internal/script`'s host commands meet a zone's modules. It lives here for
// the same reason KillSink does — this is the package that already knows every
// module a zone is made of, and `internal/script` may import none of them
// (its boundary test enforces that module boundary), while `internal/quests` stays
// impact-ignorant and only ever sees a [quests.SpecialCredit].
//
// Everything in the driver is feature-flagged twice over: the evaluator
// refuses to run unless script.Options.Enabled is set, and a ZoneBinding with
// a nil Scripts field wires nothing at all, which is the default composition.

// QuestScriptSource provides the compiled script trees and their lookup maps.
// Production uses PackQuestScriptSource; tests may provide a smaller source.
type QuestScriptSource interface {
	// QuestActivation returns what accepting a quest evaluates: the
	// startImpacts in authored order, then the triggerAgents.
	QuestActivation(questID string) (QuestActivation, bool)
	// Trigger resolves a TriggerResource content row to its node tree.
	Trigger(ref script.Ref) (*script.Node, bool)
	// Counter maps a QuestCountId content row to the objective it advances.
	// The authored data binds them through the quest's `counters` list; the
	// extractor knows the pairing and the shard only needs the answer.
	Counter(ref script.Ref) (CounterBinding, bool)
	// SpawnTableMobs lists the mob content ids a spawn table stands up, which
	// is how ImpactFindSpawnTable's resolution finds live entities.
	SpawnTableMobs(ref script.Ref) []string
}

// QuestActivation is one quest's script surface.
type QuestActivation struct {
	StartImpacts  []*script.Node
	TriggerAgents []*script.Node
}

// CounterBinding names the objective one QuestCountId advances.
type CounterBinding struct {
	QuestID     string
	ObjectiveID string
	// ObjectiveIndex is the transitional runtime key until the M3-08 stable
	// objective id reaches internal/quests. The pack row retains both fields.
	ObjectiveIndex int
}

// Census returns the per-opcode counts reached by this zone's evaluator.
func (driver *ScriptDriver) Census() *script.Census {
	if driver == nil {
		return nil
	}
	return driver.evaluator.Census()
}

// ScriptDriver runs the impact interpreter for one zone.
//
// All of its mutable state — the attachment registry, the tag set, the current
// tick — is read and written only with the zone lock held: QuestActivated and
// EquipChanged take the lock through zone.Command, and MobKilled arrives from
// the kill fan-out that already holds it. That is the same discipline the
// quest module's logs live under, and it is what lets the driver hold no
// mutex of its own.
type ScriptDriver struct {
	logger    *slog.Logger
	zone      *world.Zone
	quests    *quests.Module
	source    QuestScriptSource
	evaluator *script.Evaluator
	effects   *script.EffectRegistry

	// tick is the tick the current evaluation runs in. It is set at every
	// entry point before the evaluator is invoked and is what the host
	// callbacks read; it is never read outside the zone lock.
	tick *world.Tick

	// attachments maps a bearer entity to the triggers materialized onto it.
	attachments map[uint64][]script.Attachment
	// scopes holds the spawn-scoped attachments: mobWorld-wide prototypes that
	// materialize onto matching live mobs now and, for a scope that outlives
	// this moment, onto later tags. Materializing onto later *spawns* waits
	// for a world spawn hook that does not exist yet; see MobKilled's note.
	scopes []script.Attachment
	// tagged records TagMobForKill marks. The command's zone-wide meaning —
	// kill credit and loot following the tag — is not wired in M3; what the
	// mark does here is admit a mob into OnlyTagged spawn scopes.
	tagged map[uint64]bool

	evaluations uint64
}

// NewScriptDriver wires the interpreter to one zone's quest module. The
// options gate everything: a driver built with Enabled false evaluates
// nothing, exactly as the evaluator itself refuses.
func NewScriptDriver(
	logger *slog.Logger,
	zone *world.Zone,
	questModule *quests.Module,
	source QuestScriptSource,
	options script.Options,
) *ScriptDriver {
	if logger == nil {
		logger = slog.Default()
	}
	driver := &ScriptDriver{
		logger:      logger,
		zone:        zone,
		quests:      questModule,
		source:      source,
		attachments: make(map[uint64][]script.Attachment),
		tagged:      make(map[uint64]bool),
		effects:     script.NewEffectRegistry(),
	}
	driver.evaluator = script.New(scriptHost{driver: driver}, options)
	return driver
}

// QuestActivated evaluates one quest's startImpacts and triggerAgents for the
// character that just accepted it. It is called from the session reader after
// the accept commits, and it takes the zone lock itself because evaluation
// reads and writes world state.
//
// A script error is logged, not returned: the accept has already committed and
// the session did nothing wrong. A refused node reaching a live zone is an
// operator problem — the census and the log line carry the row id — not a
// reason to kill the player's connection.
func (driver *ScriptDriver) QuestActivated(entityID uint64, questID string) {
	if driver == nil {
		return
	}
	activation, ok := driver.source.QuestActivation(questID)
	if !ok {
		return
	}
	_ = driver.zone.Command(func(tick *world.Tick) error {
		driver.tick = tick
		defer func() { driver.tick = nil }()

		driver.evaluations++
		actor := formatEntityID(entityID)
		frame := script.Frame{
			EvaluationID: fmt.Sprintf("%s|%d|%d", questID, entityID, driver.evaluations),
			ZoneID:       driver.zone.ID(),
			SourceID:     questID,
			CasterID:     actor,
			TargetID:     actor,
			Addressee:    actor,
		}
		for _, node := range activation.StartImpacts {
			if err := driver.evaluator.Evaluate(context.Background(), node, frame); err != nil {
				driver.logger.Warn("quest activation script failed",
					"quest_id", questID, "entity_id", entityID, "error", err)
			}
		}
		for _, agent := range activation.TriggerAgents {
			if err := driver.evaluator.Evaluate(context.Background(), agent, frame); err != nil {
				driver.logger.Warn("quest trigger agent failed",
					"quest_id", questID, "entity_id", entityID, "error", err)
			}
		}
		return nil
	})
}

// MobKilled implements combat.KillSink. It is the zone event that fires shape
// B's health triggers: the kill is delivered to every trigger attached to the
// victim as a health crossing from full to zero, with the killer as the cause.
//
// Two documented simplifications live here. First, the adapter has no damage
// event stream, so a HealthTrigger with a non-zero threshold (QuestCompl fires
// at a tenth of full health) fires at death rather than at the wounding —
// late, never spuriously, because any previous health above the threshold
// crosses it on the way to zero. Second, spawn scopes materialize onto the
// mobs alive at attach time (and onto later tags); a mob that respawns after
// the scope was created is not re-attached until a world spawn hook exists.
func (driver *ScriptDriver) MobKilled(tick *world.Tick, kill combat.Kill) {
	if driver == nil {
		return
	}
	held := driver.attachments[kill.VictimEntityID]
	if len(held) == 0 {
		return
	}
	driver.tick = tick
	defer func() { driver.tick = nil }()

	previous := int64(1)
	if victim := tick.Entity(kill.VictimEntityID); victim != nil && victim.MaxHealth > 0 {
		previous = int64(victim.MaxHealth)
	}
	event := script.Event{
		Kind:           script.EventHealthChanged,
		EntityID:       formatEntityID(kill.VictimEntityID),
		CauseID:        formatEntityID(kill.KillerEntityID),
		PreviousHealth: previous,
		Health:         0,
	}
	for _, attachment := range held {
		if err := driver.evaluator.Fire(context.Background(), attachment, event); err != nil {
			driver.logger.Warn("script trigger failed on kill",
				"victim_entity_id", kill.VictimEntityID, "trigger", attachment.TriggerRef.ID, "error", err)
		}
	}
	// Persistent effects come off in reverse trigger-attachment order; each
	// trigger then removes its own effects in reverse authored order.
	for index := len(held) - 1; index >= 0; index-- {
		if err := driver.evaluator.Detach(context.Background(), held[index]); err != nil {
			driver.logger.Warn("script trigger failed to detach on death",
				"victim_entity_id", kill.VictimEntityID,
				"trigger", held[index].TriggerRef.ID, "error", err)
		}
	}
	// The bearer is dead. Its attachments and its tag go with it: the corpse
	// fires nothing further, and the placement respawns as a new entity id.
	delete(driver.attachments, kill.VictimEntityID)
	delete(driver.tagged, kill.VictimEntityID)
}

// EquipChanged delivers shape A's event: the player equipped or removed an
// item in a slot, spelled the way the content spells it (MAINHAND, TWOHANDED).
// No module publishes it yet — M2 has bags and no equipment slots — so the
// method is the seam a future equipment module and today's tests share.
func (driver *ScriptDriver) EquipChanged(entityID uint64, slot string, equipped bool) {
	if driver == nil {
		return
	}
	_ = driver.zone.Command(func(tick *world.Tick) error {
		driver.tick = tick
		defer func() { driver.tick = nil }()

		event := script.Event{
			Kind:     script.EventEquipChanged,
			EntityID: formatEntityID(entityID),
			Slot:     slot,
			Equipped: equipped,
		}
		for _, attachment := range driver.attachments[entityID] {
			if err := driver.evaluator.Fire(context.Background(), attachment, event); err != nil {
				driver.logger.Warn("script trigger failed on equip",
					"entity_id", entityID, "trigger", attachment.TriggerRef.ID, "error", err)
			}
		}
		return nil
	})
}

// materialize turns one attachment into a live registry entry on one bearer,
// loading the trigger row if the command carried only the reference.
func (driver *ScriptDriver) materialize(attachment script.Attachment, entityID uint64) {
	if attachment.Trigger == nil {
		document, ok := driver.source.Trigger(attachment.TriggerRef)
		if !ok {
			driver.logger.Warn("attach names a trigger this source does not carry",
				"trigger", attachment.TriggerRef.ID)
			return
		}
		attachment.Trigger = document
	}
	attachment.EntityID = formatEntityID(entityID)
	for _, existing := range driver.attachments[entityID] {
		if existing.ID == attachment.ID {
			return
		}
	}
	if err := driver.evaluator.ActivateAttachment(context.Background(), attachment); err != nil {
		driver.logger.Warn("attach trigger effects failed",
			"trigger", attachment.TriggerRef.ID, "entity_id", entityID, "error", err)
		return
	}
	driver.attachments[entityID] = append(driver.attachments[entityID], attachment)
}

// materializeScope applies one spawn scope to every live mob it covers.
func (driver *ScriptDriver) materializeScope(scope script.Attachment) {
	driver.tick.Each(func(entity *world.Entity) bool {
		if entity.Kind == world.EntityKindNPC && entity.Alive && entity.ContentID == scope.MobWorld.ID {
			if !scope.OnlyTagged || driver.tagged[entity.ID] {
				driver.materialize(scope, entity.ID)
			}
		}
		return true
	})
}

// scriptHost adapts the driver to script.Host. It is a separate type so the
// host surface — Apply, Query, Resolve, Enqueue — does not become public API
// on the driver.
type scriptHost struct {
	driver *ScriptDriver
}

// Now is the zone's tick clock, not the wall clock: tick number times tick
// interval. Deferred due times computed from it survive a replay identically,
// which is the property ADR 0036 wants from the deferred queue, and Enqueue
// schedules against the same clock.
func (host scriptHost) Now() time.Time {
	tick := host.driver.tick
	return time.UnixMilli(int64(tick.Number()) * tick.Interval().Milliseconds())
}

func (host scriptHost) Query(_ context.Context, query script.Query) (script.Value, error) {
	switch query.Kind {
	case script.QueryMaxHealth:
		entityID, err := parseEntityID(query.EntityID)
		if err != nil {
			return script.Value{}, err
		}
		entity := host.driver.tick.Entity(entityID)
		if entity == nil {
			return script.Value{}, fmt.Errorf("session: entity %d is not in the zone", entityID)
		}
		return script.Value{Kind: script.ValueInteger, Integer: int64(entity.MaxHealth)}, nil
	case script.QueryIsAvatar:
		entityID, err := parseEntityID(query.EntityID)
		if err != nil {
			return script.Value{}, err
		}
		entity := host.driver.tick.Entity(entityID)
		return script.Value{
			Kind: script.ValueBool, Bool: entity != nil && entity.Kind == world.EntityKindPlayer,
		}, nil
	default:
		// Class, race, quest-status and item queries wait for the quest-tree
		// callers that need them; answering them wrongly here would be worse
		// than refusing.
		return script.Value{}, fmt.Errorf("session: the script adapter answers no query of kind %d yet", query.Kind)
	}
}

// destinationSource is the optional pack adapter for absolute map locators.
// Quest trees that never evaluate a destination do not need to provide it.
type destinationSource interface {
	LocateDestination(script.Ref, string) (script.Position, bool)
}

func (host scriptHost) Locate(_ context.Context, request script.DestinationRequest) (script.Destination, error) {
	source, ok := host.driver.source.(destinationSource)
	if !ok {
		return script.Destination{}, fmt.Errorf(
			"session: script source carries no absolute map-locator index for %s/%s",
			request.Map.ID, request.ScriptID,
		)
	}
	position, ok := source.LocateDestination(request.Map, request.ScriptID)
	if !ok {
		return script.Destination{}, fmt.Errorf(
			"session: map locator %s/%s is absent", request.Map.ID, request.ScriptID,
		)
	}
	return script.Destination{Map: request.Map, Position: position}, nil
}

// Resolve serves ImpactFindSpawnTable: the source names the table's mob kinds,
// the registry supplies the living. Ids return in bytewise order, as the
// evaluator requires for deterministic iteration.
func (host scriptHost) Resolve(_ context.Context, request script.ResolveRequest) ([]string, error) {
	if request.Finder != "ImpactFindSpawnTable" {
		return nil, fmt.Errorf("session: the script adapter resolves no finder %q yet", request.Finder)
	}
	members := make(map[string]bool)
	for _, mobID := range host.driver.source.SpawnTableMobs(request.Ref) {
		members[mobID] = true
	}
	var found []string
	host.driver.tick.Each(func(entity *world.Entity) bool {
		if entity.Kind == world.EntityKindNPC && entity.Alive && members[entity.ContentID] {
			found = append(found, formatEntityID(entity.ID))
		}
		return true
	})
	sort.Strings(found)
	return found, nil
}

func (host scriptHost) Apply(_ context.Context, command script.Command) error {
	driver := host.driver
	switch command.Kind {
	case script.CommandAttachTrigger:
		attachment := *command.Attachment
		if attachment.EntityID != "" {
			entityID, err := parseEntityID(attachment.EntityID)
			if err != nil {
				return err
			}
			driver.materialize(attachment, entityID)
			return nil
		}
		// A spawn scope covers the matching mobs alive now and, when
		// OnlyTagged, the ones tagged later; it is kept so the tag path can
		// consult it.
		driver.scopes = append(driver.scopes, attachment)
		driver.materializeScope(attachment)
		return nil

	case script.CommandDetachTrigger:
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		kept := driver.attachments[entityID][:0]
		for _, attachment := range driver.attachments[entityID] {
			if attachment.ID != command.Attachment.ID {
				kept = append(kept, attachment)
			}
		}
		if len(kept) == 0 {
			delete(driver.attachments, entityID)
		} else {
			driver.attachments[entityID] = kept
		}
		return nil

	case script.CommandTagMobForKill:
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		driver.tagged[entityID] = true
		// The mark's zone-wide meaning — credit and loot following the tag —
		// is not wired in M3 and stays a documented no-op. What it does wire
		// is admission into OnlyTagged spawn scopes, which is what quest 2-10
		// needs from it.
		entity := driver.tick.Entity(entityID)
		for _, scope := range driver.scopes {
			if scope.OnlyTagged && entity != nil && entity.Alive && entity.ContentID == scope.MobWorld.ID {
				driver.materialize(scope, entityID)
			}
		}
		return nil

	case script.CommandIncreaseQuestCount:
		binding, ok := driver.source.Counter(command.Ref)
		if !ok {
			return fmt.Errorf("session: count id %s is bound to no objective", command.Ref.ID)
		}
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		driver.quests.CreditSpecial(driver.tick, quests.SpecialCredit{
			CharacterEntityID: entityID,
			QuestID:           binding.QuestID,
			ObjectiveIndex:    binding.ObjectiveIndex,
			Delta:             int32(command.Count),
		})
		return nil

	case script.CommandDamage, script.CommandSetTarget:
		// Neither is reached by the quest trees this adapter serves; both
		// belong to the spell callers, which are not wired. A documented no-op
		// with a log line beats a silent one.
		driver.logger.Warn("script command has no zone wiring yet",
			"kind", uint8(command.Kind), "entity", command.EntityID)
		return nil

	case script.CommandAttachGuard, script.CommandDetachGuard,
		script.CommandAttachDamageModifier, script.CommandDetachDamageModifier:
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		entity := driver.tick.Entity(entityID)
		_, err = driver.effects.Apply(command, script.EffectOwner{
			Mob: entity != nil && entity.Kind == world.EntityKindNPC,
			// An entity in this tick's spatial registry is cell-placed.
			CellPlaced: entity != nil,
		})
		return err

	default:
		return fmt.Errorf("session: the script adapter applies no command of kind %d yet", command.Kind)
	}
}

// Enqueue schedules a deferred impact on the zone's timing wheel, converting
// the due time from the tick clock back into ticks. The queue is in-memory:
// ADR 0036 wants the deferred queue persisted, and until the storage row
// exists a shard restart drops pending deferred work. That is a known gap of
// the flag-on path, not of the default composition.
func (host scriptHost) Enqueue(_ context.Context, deferred script.Deferred) error {
	driver := host.driver
	tick := driver.tick
	intervalMS := tick.Interval().Milliseconds()
	if intervalMS <= 0 {
		return fmt.Errorf("session: zone tick interval %s cannot schedule deferred work", tick.Interval())
	}
	nowMS := int64(tick.Number()) * intervalMS
	var delayTicks uint64
	if due := int64(deferred.DueAtMS); due > nowMS {
		delayTicks = uint64((due - nowMS + intervalMS - 1) / intervalMS)
	}
	tick.After(delayTicks, func(later *world.Tick) {
		driver.tick = later
		defer func() { driver.tick = nil }()
		if err := driver.evaluator.Evaluate(context.Background(), deferred.Node, deferred.Frame); err != nil {
			driver.logger.Warn("deferred script impact failed",
				"node", deferred.Node.Key, "error", err)
		}
	})
	return nil
}

func formatEntityID(entityID uint64) string {
	return strconv.FormatUint(entityID, 10)
}

func parseEntityID(value string) (uint64, error) {
	entityID, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("session: %q is not an entity id: %w", value, err)
	}
	return entityID, nil
}
