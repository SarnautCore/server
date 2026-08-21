package session

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
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
// EquipChanged take the lock through Zone.GameCommand, and MobKilled arrives from
// the kill fan-out that already holds it. That is the same discipline the
// quest module's logs live under, and it is what lets the driver hold no
// mutex of its own.
type ScriptDriver struct {
	logger              *slog.Logger
	zone                gametypes.Zone
	quests              *quests.Module
	source              QuestScriptSource
	evaluator           *script.Evaluator
	effects             *script.EffectRegistry
	combat              *combat.Module
	activeWarriorAction *activeWarriorAction
	warriorCombatants   map[uint64]WarriorCombatant
	// applyGuardUpdate is installed with combat. Keeping the host call as a
	// function makes rejection behavior testable without weakening combat's
	// public API.
	applyGuardUpdate func(gametypes.Tick, uint64, combat.GuardUpdate) error

	// tick is the tick the current evaluation runs in. It is set at every
	// entry point before the evaluator is invoked and is what the host
	// callbacks read; it is never read outside the zone lock.
	tick gametypes.Tick

	// attachments maps a bearer entity to the triggers materialized onto it.
	attachments map[uint64][]script.Attachment
	// pendingDetaches owns resumable cleanup for dead attachments and failed
	// activation compensation. The attachment is not live while cleanup is
	// pending, and completed effects are never run again on a retry.
	pendingDetaches      map[uint64][]script.AttachmentCleanup
	detachRetryScheduled map[uint64]bool
	// scopes holds the spawn-scoped attachments: mobWorld-wide prototypes that
	// materialize onto matching live mobs now and, for a scope that outlives
	// this moment, onto later tags. Materializing onto later *spawns* waits
	// for a world spawn hook that does not exist yet; see MobKilled's note.
	scopes []script.Attachment
	// tagged records TagMobForKill marks. The command's zone-wide meaning —
	// kill credit and loot following the tag — is not wired in M3; what the
	// mark does here is admit a mob into OnlyTagged spawn scopes.
	tagged map[uint64]bool
	// summons makes CommandSummon idempotent for the lifetime of this zone.
	// The key is the evaluator's stable execution key.
	summons map[string]uint64

	evaluations uint64
}

// NewScriptDriver wires the interpreter to one zone's quest module. The
// options gate everything: a driver built with Enabled false evaluates
// nothing, exactly as the evaluator itself refuses.
func NewScriptDriver(
	logger *slog.Logger,
	zone gametypes.Zone,
	questModule *quests.Module,
	source QuestScriptSource,
	options script.Options,
) *ScriptDriver {
	if logger == nil {
		logger = slog.Default()
	}
	driver := &ScriptDriver{
		logger:               logger,
		zone:                 zone,
		quests:               questModule,
		source:               source,
		attachments:          make(map[uint64][]script.Attachment),
		pendingDetaches:      make(map[uint64][]script.AttachmentCleanup),
		detachRetryScheduled: make(map[uint64]bool),
		tagged:               make(map[uint64]bool),
		summons:              make(map[string]uint64),
		warriorCombatants:    make(map[uint64]WarriorCombatant),
		effects:              script.NewEffectRegistry(),
	}
	driver.evaluator = script.New(scriptHost{driver: driver}, options)
	return driver
}

// BindCombat connects the driver's persistent-effect registry to the zone's
// combat path. It is called once while composing a shard, before sessions can
// activate scripts.
func (driver *ScriptDriver) BindCombat(module *combat.Module) {
	if driver == nil {
		return
	}
	driver.combat = module
	if module != nil {
		driver.applyGuardUpdate = module.ApplyGuardUpdate
		module.SetDamageEffectHost(driver)
		module.SetScriptActionHost(driver)
	} else {
		driver.applyGuardUpdate = nil
	}
}

// ScaleDamage implements combat.DamageEffectHost. Outgoing effects fold
// before incoming effects, and combat performs no mutation unless both folds
// succeed.
func (driver *ScriptDriver) ScaleDamage(
	tick gametypes.Tick,
	request combat.DamageEffectRequest,
) (int32, error) {
	if driver == nil || tick == nil {
		return request.Magnitude, fmt.Errorf("session: damage effect host has no active tick")
	}
	previous := driver.tick
	driver.tick = tick
	defer func() { driver.tick = previous }()

	magnitude := script.Decimal{Mantissa: int64(request.Magnitude)}
	steps := []struct {
		owner     uint64
		offender  uint64
		direction script.DamageDirection
	}{
		{owner: request.CasterID, offender: request.TargetID, direction: script.DamageOutgoing},
		{owner: request.TargetID, offender: request.CasterID, direction: script.DamageIncoming},
	}
	for _, step := range steps {
		owner := formatEntityID(step.owner)
		resolved := tick.Entity(step.offender) != nil
		var err error
		magnitude, err = driver.evaluator.ScaleDamage(context.Background(), script.DamageEvent{
			Magnitude:        magnitude,
			OwnerID:          owner,
			OffenderID:       formatEntityID(step.offender),
			OffenderResolved: resolved,
			HasActiveAction:  request.AbilityID != "",
			// Compiled ability rows do not yet carry the retail action group.
			// Empty deliberately leaves a grouped output modifier unmatched.
			ActionGroup: script.Ref{ID: request.ActionGroupID, RowType: "action-group"},
		}, driver.effects.Modifiers(owner, step.direction))
		if err != nil {
			return request.Magnitude, fmt.Errorf("session: scale damage for entity %d: %w", step.owner, err)
		}
	}
	return roundedDamage(magnitude)
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
	_ = driver.zone.GameCommand(func(tick gametypes.Tick) error {
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
func (driver *ScriptDriver) MobKilled(tick gametypes.Tick, kill combat.Kill) {
	if driver == nil {
		return
	}
	held := driver.attachments[kill.VictimEntityID]
	if len(held) == 0 {
		return
	}
	previousTick := driver.tick
	driver.tick = tick
	defer func() { driver.tick = previousTick }()

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
	// Stop publishing these attachments before cleanup. A failed detach must
	// remain retryable, but it must not make a replayed death fire the trigger a
	// second time.
	delete(driver.attachments, kill.VictimEntityID)
	for _, attachment := range held {
		cleanup, err := driver.evaluator.BeginAttachmentDetach(attachment)
		if err != nil {
			driver.logger.Warn("script trigger cleanup could not start",
				"entity_id", kill.VictimEntityID,
				"trigger", attachment.TriggerRef.ID, "error", err)
			continue
		}
		driver.retainPendingDetach(kill.VictimEntityID, cleanup)
	}
	driver.retryPendingDetaches(tick, kill.VictimEntityID)
	// The tag is event-admission state rather than effect-lifetime state.
	delete(driver.tagged, kill.VictimEntityID)
}

func (driver *ScriptDriver) retainPendingDetach(
	entityID uint64, cleanup script.AttachmentCleanup,
) {
	for _, existing := range driver.pendingDetaches[entityID] {
		if existing.AttachmentID() == cleanup.AttachmentID() {
			return
		}
	}
	driver.pendingDetaches[entityID] = append(driver.pendingDetaches[entityID], cleanup)
}

// retryPendingDetaches removes unpublished attachments in exact reverse order.
// Successful entries disappear from the retry set. Failed entries retain the
// full attachment document and retry on the next simulation tick.
func (driver *ScriptDriver) retryPendingDetaches(tick gametypes.Tick, entityID uint64) {
	pending := driver.pendingDetaches[entityID]
	if len(pending) == 0 {
		delete(driver.pendingDetaches, entityID)
		delete(driver.detachRetryScheduled, entityID)
		return
	}

	for len(pending) > 0 {
		index := len(pending) - 1
		if err := driver.evaluator.ContinueAttachmentCleanup(
			context.Background(), &pending[index],
		); err != nil {
			driver.logger.Warn("script trigger cleanup failed",
				"entity_id", entityID,
				"attachment", pending[index].AttachmentID(), "error", err)
			driver.pendingDetaches[entityID] = pending
			driver.schedulePendingDetachRetry(tick, entityID)
			return
		}
		pending = pending[:index]
	}
	delete(driver.pendingDetaches, entityID)
	delete(driver.detachRetryScheduled, entityID)
}

func (driver *ScriptDriver) schedulePendingDetachRetry(tick gametypes.Tick, entityID uint64) {
	if driver.detachRetryScheduled[entityID] {
		return
	}
	driver.detachRetryScheduled[entityID] = true
	tick.After(1, func(later gametypes.Tick) {
		delete(driver.detachRetryScheduled, entityID)
		driver.tick = later
		defer func() { driver.tick = nil }()
		driver.retryPendingDetaches(later, entityID)
	})
}

// EquipChanged delivers shape A's event: the player equipped or removed an
// item in a slot, spelled the way the content spells it (MAINHAND, TWOHANDED).
// No module publishes it yet — M2 has bags and no equipment slots — so the
// method is the seam a future equipment module and today's tests share.
func (driver *ScriptDriver) EquipChanged(entityID uint64, slot string, equipped bool) {
	if driver == nil {
		return
	}
	_ = driver.zone.GameCommand(func(tick gametypes.Tick) error {
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
	for _, cleanup := range driver.pendingDetaches[entityID] {
		if cleanup.AttachmentID() == attachment.ID {
			return
		}
	}
	if err := driver.evaluator.ActivateAttachment(context.Background(), attachment); err != nil {
		driver.logger.Warn("attach trigger effects failed",
			"trigger", attachment.TriggerRef.ID, "entity_id", entityID, "error", err)
		if cleanup, ok := script.AttachmentRollbackCleanup(err); ok {
			driver.retainPendingDetach(entityID, cleanup)
			driver.schedulePendingDetachRetry(driver.tick, entityID)
		}
		return
	}
	driver.attachments[entityID] = append(driver.attachments[entityID], attachment)
}

// materializeScope applies one spawn scope to every live mob it covers.
func (driver *ScriptDriver) materializeScope(scope script.Attachment) {
	driver.tick.Each(func(entity *gametypes.EntityData) bool {
		if entity.Kind == gametypes.EntityKindNPC && entity.Alive && entity.ContentID == scope.MobWorld.ID {
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
	case script.QueryPhysicalScale, script.QueryPhysicalRangedScale, script.QueryWeaponSpeedScale:
		return host.driver.activeScale(query)
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
			Kind: script.ValueBool, Bool: entity != nil && entity.Kind == gametypes.EntityKindPlayer,
		}, nil
	case script.QueryDistance:
		entityID, err := parseEntityID(query.EntityID)
		if err != nil {
			return script.Value{}, err
		}
		otherID, err := parseEntityID(query.OtherEntityID)
		if err != nil {
			return script.Value{}, err
		}
		entity, other := host.driver.tick.Entity(entityID), host.driver.tick.Entity(otherID)
		if entity == nil || other == nil {
			return script.Value{}, gametypes.ErrUnknownEntity
		}
		millimetres := math.Round(float64(gametypes.Distance(
			host.driver.tick.Position(entity), host.driver.tick.Position(other),
		)) * 1000)
		if millimetres > math.MaxInt64 {
			return script.Value{}, fmt.Errorf("session: entity distance exceeds exact script range")
		}
		return script.Value{Kind: script.ValueDecimal, Mantissa: int64(millimetres), Scale: 3}, nil
	case script.QueryEquipped:
		entityID, err := parseEntityID(query.EntityID)
		if err != nil {
			return script.Value{}, err
		}
		actor, ok := host.driver.warriorCombatants[entityID]
		return script.Value{Kind: script.ValueBool, Bool: ok && actor.EquippedDressTypes[query.Slot]}, nil
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

type destinationIndexSource interface {
	HasDestinationIndex() bool
}

// summonSource resolves the mob row named by ImpactSummon. It stays optional
// so script sources that never evaluate a summon do not fabricate mob data.
type summonSource interface {
	SummonMob(script.Ref) (gametypes.Mob, bool)
}

func (host scriptHost) Locate(_ context.Context, request script.DestinationRequest) (script.Destination, error) {
	if strings.HasPrefix(request.Map.ID, "ext.") {
		return script.Destination{}, fmt.Errorf(
			"session: map locator uses non-product map id %q", request.Map.ID,
		)
	}
	source, ok := host.driver.source.(destinationSource)
	if !ok {
		return script.Destination{}, fmt.Errorf(
			"session: script source carries no absolute map-locator index for %s/%s",
			request.Map.ID, request.ScriptID,
		)
	}
	if indexed, ok := host.driver.source.(destinationIndexSource); ok && !indexed.HasDestinationIndex() {
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
	host.driver.tick.Each(func(entity *gametypes.EntityData) bool {
		if entity.Kind == gametypes.EntityKindNPC && entity.Alive && members[entity.ContentID] {
			found = append(found, formatEntityID(entity.ID))
		}
		return true
	})
	sort.Strings(found)
	return found, nil
}

func (host scriptHost) Apply(ctx context.Context, command script.Command) error {
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

	case script.CommandSummon:
		if command.Summon == nil {
			return fmt.Errorf("session: summon command has no payload")
		}
		if driver.combat == nil {
			return fmt.Errorf("session: summon %s has no combat host", command.Summon.Object.ID)
		}
		if existingID := driver.summons[command.ExecutionKey]; existingID != 0 {
			if driver.tick.Entity(existingID) != nil {
				return nil
			}
			delete(driver.summons, command.ExecutionKey)
		}
		source, ok := driver.source.(summonSource)
		if !ok {
			return fmt.Errorf("session: script source carries no summon-mob index for %s", command.Summon.Object.ID)
		}
		mob, ok := source.SummonMob(command.Summon.Object)
		if !ok || mob.ID != command.Summon.Object.ID {
			return fmt.Errorf("session: summon mob %s is absent", command.Summon.Object.ID)
		}
		heading, err := decimalFloat32(command.Summon.Destination.Yaw)
		if err != nil {
			return fmt.Errorf("session: summon %s yaw: %w", mob.ID, err)
		}
		position := gametypes.Vec3{
			X: command.Summon.Destination.Position.X,
			Y: command.Summon.Destination.Position.Y,
			Z: command.Summon.Destination.Position.Z,
		}
		entity, err := driver.combat.Summon(
			driver.tick, mob, "script-summon|"+command.ExecutionKey, position, heading,
		)
		if err != nil {
			return fmt.Errorf("session: summon %s: %w", mob.ID, err)
		}
		childFrame := command.Summon.Frame
		childFrame.Addressee = formatEntityID(entity.ID)
		for _, child := range command.Summon.Impacts {
			if err := driver.evaluator.Evaluate(ctx, child, childFrame); err != nil {
				if rollbackErr := driver.combat.DismissSummon(driver.tick, entity.ID); rollbackErr != nil {
					return fmt.Errorf("session: summon child %s failed: %w; rollback failed: %w",
						child.Key, err, rollbackErr)
				}
				return fmt.Errorf("session: summon child %s failed: %w", child.Key, err)
			}
		}
		driver.summons[command.ExecutionKey] = entity.ID
		return nil

	case script.CommandTurnMob:
		if driver.combat == nil {
			return fmt.Errorf("session: turn mob %s has no combat host", command.EntityID)
		}
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		return driver.combat.TurnMob(driver.tick, entityID, gametypes.Vec3{
			X: command.Destination.Position.X,
			Y: command.Destination.Position.Y,
			Z: command.Destination.Position.Z,
		})

	case script.CommandDamage:
		active := driver.activeWarriorAction
		if driver.combat == nil || active == nil {
			return fmt.Errorf("session: damage command has no active Warrior action")
		}
		targetID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		damage, err := roundedDamage(command.Magnitude)
		if err != nil {
			return fmt.Errorf("session: round Warrior damage: %w", err)
		}
		threat, err := decimalFloat64(command.ThreatMultiplier)
		if err != nil {
			return fmt.Errorf("session: Warrior threat multiplier: %w", err)
		}
		event, err := driver.combat.ApplyScriptDamage(driver.tick, combat.ScriptDamageRequest{
			CasterID: active.invocation.CasterID, TargetID: targetID,
			AbilityID: active.invocation.AbilityID, ActionGroupID: active.invocation.ActionGroupID,
			Damage: damage, ThreatMultiplier: threat, CanBeAvoided: command.CanBeAvoided,
			ExecutionKey: command.ExecutionKey,
		})
		if err != nil {
			return err
		}
		active.event = event
		active.damaged = true
		active.mutated = true
		return nil

	case script.CommandSetTarget:
		if driver.combat == nil {
			return fmt.Errorf("session: target command has no combat host")
		}
		actorID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		targetID, err := parseEntityID(command.TargetID)
		if err != nil {
			return err
		}
		if err := driver.combat.SetScriptTarget(driver.tick, actorID, targetID, command.ExecutionKey); err != nil {
			return err
		}
		if driver.activeWarriorAction != nil {
			driver.activeWarriorAction.mutated = true
		}
		return nil

	case script.CommandAttachGuard, script.CommandDetachGuard,
		script.CommandAttachDamageModifier, script.CommandDetachDamageModifier:
		entityID, err := parseEntityID(command.EntityID)
		if err != nil {
			return err
		}
		entity := driver.tick.Entity(entityID)
		if driver.combat == nil || driver.applyGuardUpdate == nil {
			return fmt.Errorf("session: persistent effect %s has no combat host", command.EffectID)
		}
		if entity == nil {
			return fmt.Errorf("session: persistent effect owner %d is not in the zone", entityID)
		}
		var sightRadius script.Decimal
		if command.Kind == script.CommandAttachGuard || command.Kind == script.CommandDetachGuard {
			mob, ok := driver.combat.Rules().Mob(entity.ContentID)
			if !ok {
				return fmt.Errorf("session: guard owner %d has no combat mob record", entityID)
			}
			sightRadius, err = decimalFromFloat32(mob.AggroRadiusM)
			if err != nil {
				return fmt.Errorf("session: guard owner %d sight radius: %w", entityID, err)
			}
		}
		_, err = driver.effects.ApplyAtomic(command, script.EffectOwner{
			Mob: entity.Kind == gametypes.EntityKindNPC,
			// An entity in this tick's spatial registry is cell-placed.
			CellPlaced: entity != nil,
		}, func(change script.EffectChange) error {
			if command.Kind != script.CommandAttachGuard && command.Kind != script.CommandDetachGuard {
				return nil
			}
			state := driver.effects.GuardState(command.EntityID, sightRadius)
			if change.RemoveAggroState {
				state = change
			}
			radius, err := decimalFloat32(state.ObserverRadius)
			if err != nil {
				return fmt.Errorf("session: guard owner %d radius: %w", entityID, err)
			}
			return driver.applyGuardUpdate(driver.tick, entityID, combat.GuardUpdate{
				Active:           state.GuardActive,
				ObserverRadius:   radius,
				NoticeTarget:     state.NoticeTarget,
				RecheckEvery:     state.RecheckEvery,
				AggroMarkDelta:   change.AggroMarkDelta,
				RemoveAggroState: change.RemoveAggroState,
			})
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
	tick.After(delayTicks, func(later gametypes.Tick) {
		driver.tick = later
		defer func() { driver.tick = nil }()
		if err := driver.evaluator.Evaluate(context.Background(), deferred.Node, deferred.Frame); err != nil {
			driver.logger.Warn("deferred script impact failed",
				"node", deferred.Node.Key, "error", err)
		}
	})
	return nil
}

var _ combat.KillSink = (*ScriptDriver)(nil)

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

func decimalFromFloat32(value float32) (script.Decimal, error) {
	if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
		return script.Decimal{}, fmt.Errorf("float32 %v is not a finite decimal", value)
	}
	text := strconv.FormatFloat(float64(value), 'f', -1, 32)
	point := -1
	for index, character := range text {
		if character == '.' {
			point = index
			break
		}
	}
	scale := int32(0)
	digits := text
	if point >= 0 {
		scale = int32(len(text) - point - 1)
		digits = text[:point] + text[point+1:]
	}
	mantissa, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return script.Decimal{}, fmt.Errorf("float32 %v is not an int64-backed decimal: %w", value, err)
	}
	return script.Decimal{Mantissa: mantissa, Scale: scale}, nil
}

func decimalFloat32(value script.Decimal) (float32, error) {
	factor := math.Pow10(int(value.Scale))
	result := float64(value.Mantissa) / factor
	if math.IsNaN(result) || math.IsInf(result, 0) || result > math.MaxFloat32 || result < -math.MaxFloat32 {
		return 0, fmt.Errorf("decimal %dE-%d is not representable", value.Mantissa, value.Scale)
	}
	return float32(result), nil
}

func decimalFloat64(value script.Decimal) (float64, error) {
	if value.Scale < 0 || value.Scale > 9 {
		return 0, fmt.Errorf("decimal %dE-%d has unsupported scale", value.Mantissa, value.Scale)
	}
	result := float64(value.Mantissa) / math.Pow10(int(value.Scale))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, fmt.Errorf("decimal %dE-%d is not representable", value.Mantissa, value.Scale)
	}
	return result, nil
}

func roundedDamage(value script.Decimal) (int32, error) {
	if value.Mantissa <= 0 {
		return 0, nil
	}
	if value.Scale < 0 || value.Scale > 9 {
		return 0, fmt.Errorf("session: scaled damage has unsupported decimal scale %d", value.Scale)
	}
	divisor := int64(1)
	for range value.Scale {
		divisor *= 10
	}
	rounded := value.Mantissa / divisor
	if remainder := value.Mantissa % divisor; remainder*2 >= divisor {
		rounded++
	}
	if rounded > math.MaxInt32 {
		return 0, fmt.Errorf("session: scaled damage %d exceeds int32", rounded)
	}
	return int32(rounded), nil
}
