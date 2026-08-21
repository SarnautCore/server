package session

import (
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/world"
)

const (
	effectTargetMob = "mob.paper-harbor.tide-crab"
	effectAbility   = "ability.melee.harbor-cleave"
)

type emptyScriptSource struct{}

type effectDiscardSnapshots struct{}

func (effectDiscardSnapshots) OfferSnapshot(world.Snapshot) {}

func (emptyScriptSource) QuestActivation(string) (QuestActivation, bool) {
	return QuestActivation{}, false
}

func (emptyScriptSource) Trigger(script.Ref) (*script.Node, bool) { return nil, false }

func (emptyScriptSource) Counter(script.Ref) (CounterBinding, bool) {
	return CounterBinding{}, false
}

func (emptyScriptSource) SpawnTableMobs(script.Ref) []string { return nil }

type effectIntegrationFixture struct {
	zone        *world.Zone
	combat      *combat.Module
	driver      *ScriptDriver
	playerID    uint64
	mobID       uint64
	guardRadius float32
}

func newEffectIntegrationFixture(t *testing.T) *effectIntegrationFixture {
	return newEffectIntegrationFixtureWithGuardRange(t, false)
}

func newEffectIntegrationFixtureWithGuardRange(
	t *testing.T,
	guardRange bool,
) *effectIntegrationFixture {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	var anchor world.Vec3
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID == effectTargetMob {
			anchor = world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}
			break
		}
	}
	playerOffset := float32(6)
	guardRadius := float32(2)
	if guardRange {
		mob, mobOK := content.Mob(effectTargetMob)
		var stopRange float32
		if mobOK {
			for _, abilityID := range mob.AbilityIDs {
				ability, ok := content.Ability(abilityID)
				if !ok || ability.RangeM <= 0 {
					continue
				}
				if stopRange == 0 || ability.RangeM < stopRange {
					stopRange = ability.RangeM
				}
			}
		}
		if !mobOK || mob.AggroRadiusM <= stopRange {
			t.Fatalf("fixture has no observable Guard range between stop %.1f and aggro %.1f",
				stopRange, mob.AggroRadiusM)
		}
		playerOffset = (mob.AggroRadiusM + stopRange) / 2
		guardRadius = stopRange
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "EffectIntegration",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
		PlayerSpawn:      anchor.Add(world.Vec3{X: playerOffset}),
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("combat.RulesFromPack() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})
	if err := combatModule.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("combat.Populate() error = %v", err)
	}
	playerID, _ := zone.Join()
	if err := combatModule.Admit(playerID); err != nil {
		t.Fatalf("combat.Admit() error = %v", err)
	}
	if err := zone.Subscribe(playerID, effectDiscardSnapshots{}); err != nil {
		t.Fatalf("zone.Subscribe() error = %v", err)
	}
	var mobID uint64
	_ = zone.Command(func(tick *world.Tick) error {
		tick.Each(func(entity *world.Entity) bool {
			if entity.ContentID == effectTargetMob {
				mobID = entity.ID
				return false
			}
			return true
		})
		return nil
	})
	if mobID == 0 {
		t.Fatal("fixture pack spawned no target mob")
	}
	driver := NewScriptDriver(
		slog.New(slog.DiscardHandler), zone, nil, emptyScriptSource{}, script.Options{Enabled: true},
	)
	driver.BindCombat(combatModule)
	return &effectIntegrationFixture{
		zone: zone, combat: combatModule, driver: driver, playerID: playerID, mobID: mobID,
		guardRadius: guardRadius,
	}
}

func (fixture *effectIntegrationFixture) apply(t *testing.T, command script.Command) error {
	t.Helper()
	return fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		return (scriptHost{driver: fixture.driver}).Apply(t.Context(), command)
	})
}

func (fixture *effectIntegrationFixture) health(t *testing.T) int32 {
	t.Helper()
	var health int32
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		health = tick.Entity(fixture.mobID).Health
		return nil
	})
	return health
}

func (fixture *effectIntegrationFixture) mobPosition(t *testing.T) world.Vec3 {
	t.Helper()
	var position world.Vec3
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		position = tick.Entity(fixture.mobID).Position()
		return nil
	})
	return position
}

func damageModifier(
	effectID string,
	entityID uint64,
	direction script.DamageDirection,
	coefficient script.Decimal,
) script.Command {
	return script.Command{
		Kind:     script.CommandAttachDamageModifier,
		EntityID: formatEntityID(entityID),
		EffectID: effectID,
		DamageModifier: &script.DamageModifier{
			Direction: direction, Priority: script.DamagePriorityScaleAll,
			Scaler: script.LinearScaler{Coefficient: coefficient}, StackCount: 1,
		},
	}
}

func TestCombatFoldsOutgoingThenIncomingPersistentDamage(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	if err := fixture.apply(t, damageModifier(
		"outgoing-half-more", fixture.playerID, script.DamageOutgoing,
		script.Decimal{Mantissa: 5, Scale: 1},
	)); err != nil {
		t.Fatalf("attach outgoing modifier: %v", err)
	}
	if err := fixture.apply(t, damageModifier(
		"incoming-half", fixture.mobID, script.DamageIncoming,
		script.Decimal{Mantissa: -5, Scale: 1},
	)); err != nil {
		t.Fatalf("attach incoming modifier: %v", err)
	}

	event, err := fixture.combat.UseAbility(fixture.playerID, combat.AbilityRequest{
		Seq: 1, TargetID: fixture.mobID, AbilityID: effectAbility,
	})
	if err != nil {
		t.Fatalf("UseAbility() error = %v", err)
	}
	if event.Damage != 15 {
		t.Fatalf("scaled damage = %d, want round(20 * 1.5 * 0.5) = 15", event.Damage)
	}
}

func TestPersistentEffectRequiresCombatBeforeItMutatesTheRegistry(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	driver := NewScriptDriver(
		slog.New(slog.DiscardHandler), fixture.zone, nil, emptyScriptSource{},
		script.Options{Enabled: true},
	)
	command := damageModifier(
		"bind-before-mutate", fixture.playerID, script.DamageOutgoing,
		script.Decimal{Mantissa: 5, Scale: 1},
	)
	apply := func() error {
		return fixture.zone.GameCommand(func(tick gametypes.Tick) error {
			driver.tick = tick
			defer func() { driver.tick = nil }()
			return (scriptHost{driver: driver}).Apply(t.Context(), command)
		})
	}
	if err := apply(); err == nil || !strings.Contains(err.Error(), "has no combat host") {
		t.Fatalf("unbound Apply() error = %v, want missing combat host", err)
	}
	driver.BindCombat(fixture.combat)
	if err := apply(); err != nil {
		t.Fatalf("Apply() after BindCombat = %v; unbound call retained registry state", err)
	}
	event, err := fixture.combat.UseAbility(fixture.playerID, combat.AbilityRequest{
		Seq: 1, TargetID: fixture.mobID, AbilityID: effectAbility,
	})
	if err != nil || event.Damage != 30 {
		t.Fatalf("damage after bound retry = %d, %v, want 30", event.Damage, err)
	}
}

func TestDamageEffectFailureMutatesNeitherHealthCooldownNorSequence(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	command := damageModifier(
		"predicate-needs-unavailable-class", fixture.mobID, script.DamageIncoming,
		script.Decimal{Mantissa: -5, Scale: 1},
	)
	command.DamageModifier.AttackerPredicates = []*script.Node{{
		Key: "condition/class", Family: script.FamilyPredicate,
		Opcode: "PredicateCharacterClass", Tier: script.TierImplemented,
		Fields: []script.Field{{Name: "characterClass", Value: script.Value{
			Kind: script.ValueRef, Ref: script.Ref{ID: "class.fixture"},
		}}},
	}}
	if err := fixture.apply(t, command); err != nil {
		t.Fatalf("attach predicate modifier: %v", err)
	}
	before := fixture.health(t)
	request := combat.AbilityRequest{Seq: 7, TargetID: fixture.mobID, AbilityID: effectAbility}
	if _, err := fixture.combat.UseAbility(fixture.playerID, request); err == nil ||
		!strings.Contains(err.Error(), "answers no query") {
		t.Fatalf("UseAbility() error = %v, want explicit unsupported-query failure", err)
	}
	if after := fixture.health(t); after != before {
		t.Fatalf("health after failed scaling = %d, want unchanged %d", after, before)
	}
	if err := fixture.apply(t, script.Command{
		Kind: script.CommandDetachDamageModifier, EntityID: formatEntityID(fixture.mobID),
		EffectID: command.EffectID,
	}); err != nil {
		t.Fatalf("detach predicate modifier: %v", err)
	}
	event, err := fixture.combat.UseAbility(fixture.playerID, request)
	if err != nil {
		t.Fatalf("retry same sequence after effect failure: %v", err)
	}
	if event.Damage != 20 {
		t.Fatalf("unmodified retry damage = %d, want 20", event.Damage)
	}
}

func TestGuardAttachAndLastDetachDriveMobAggroObserver(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixtureWithGuardRange(t, true)
	origin := fixture.mobPosition(t)
	guardRadius, err := decimalFromFloat32(fixture.guardRadius)
	if err != nil {
		t.Fatalf("guard radius conversion: %v", err)
	}
	guard := script.Command{
		Kind: script.CommandAttachGuard, EntityID: formatEntityID(fixture.mobID), EffectID: "guard-near",
		Guard: &script.Guard{Radius: guardRadius, NoticeTarget: true},
	}
	if err := fixture.apply(t, guard); err != nil {
		t.Fatalf("attach Guard: %v", err)
	}
	for range 5 {
		fixture.zone.Step()
	}
	if got := fixture.mobPosition(t); got != origin {
		t.Fatalf("mob moved under two-metre Guard observer: got %v, origin %v", got, origin)
	}
	if err := fixture.apply(t, script.Command{
		Kind: script.CommandDetachGuard, EntityID: guard.EntityID, EffectID: guard.EffectID,
	}); err != nil {
		t.Fatalf("detach last Guard: %v", err)
	}
	for range 20 {
		fixture.zone.Step()
	}
	if got := fixture.mobPosition(t); got == origin {
		t.Fatalf("mob stayed at %v after last Guard detach restored authored aggro", got)
	}
}

func TestPartialAttachmentFailureRollsBackLiveEffectsAndMetadata(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	attachment := script.Attachment{
		ID: "partial-attach", EntityID: formatEntityID(fixture.mobID),
		TriggerRef: script.Ref{ID: "trigger.partial-attach"},
		Trigger: &script.Node{
			Key: "trigger.partial-attach", Family: script.FamilyTrigger,
			Opcode: "TriggerResource", Tier: script.TierImplemented,
			Fields: []script.Field{{Name: "effects", Value: script.Value{
				Kind: script.ValueList,
				List: []script.Value{
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/first-guard", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
						Fields: []script.Field{{Name: "scanRadius", Value: script.Value{
							Kind: script.ValueInteger, Integer: 10,
						}}},
					}},
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/second-guard", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
						Fields: []script.Field{{Name: "scanRadius", Value: script.Value{
							Kind: script.ValueInteger, Integer: 20,
						}}},
					}},
				},
			}}},
		},
		Frame: script.Frame{
			EvaluationID: "partial-attach-eval", SourceID: "quest.partial-attach",
			ZoneID: fixture.zone.ID(), CasterID: formatEntityID(fixture.playerID),
		},
	}

	rejected := errors.New("injected second Guard rejection")
	originalApply := fixture.driver.applyGuardUpdate
	applyCalls := 0
	fixture.driver.applyGuardUpdate = func(
		tick gametypes.Tick, entityID uint64, update combat.GuardUpdate,
	) error {
		applyCalls++
		if applyCalls == 2 {
			return rejected
		}
		return originalApply(tick, entityID, update)
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(attachment, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("materialize attachment: %v", err)
	}

	if applyCalls != 3 {
		t.Fatalf("Guard host calls = %d, want first attach, rejected second attach, rollback first", applyCalls)
	}
	if len(fixture.driver.attachments[fixture.mobID]) != 0 {
		t.Fatalf("failed activation published attachment metadata")
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); state.GuardActive {
		t.Fatalf("failed activation retained live Guard state %#v", state)
	}
}

func TestFailedActivationRollbackRetainsMetadataUntilRetryCompletes(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	attachment := script.Attachment{
		ID: "rollback-retry", EntityID: formatEntityID(fixture.mobID),
		TriggerRef: script.Ref{ID: "trigger.rollback-retry"},
		Trigger: &script.Node{
			Key: "trigger.rollback-retry", Family: script.FamilyTrigger,
			Opcode: "TriggerResource", Tier: script.TierImplemented,
			Fields: []script.Field{{Name: "effects", Value: script.Value{
				Kind: script.ValueList,
				List: []script.Value{
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/first", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
					}},
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/second", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
					}},
				},
			}}},
		},
		Frame: script.Frame{
			EvaluationID: "rollback-retry-eval", SourceID: "quest.rollback-retry",
			ZoneID: fixture.zone.ID(), CasterID: formatEntityID(fixture.playerID),
		},
	}

	originalApply := fixture.driver.applyGuardUpdate
	applyCalls := 0
	fixture.driver.applyGuardUpdate = func(
		tick gametypes.Tick, entityID uint64, update combat.GuardUpdate,
	) error {
		applyCalls++
		if applyCalls == 2 || applyCalls == 3 {
			return errors.New("injected activation or rollback rejection")
		}
		return originalApply(tick, entityID, update)
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(attachment, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("materialize attachment: %v", err)
	}
	if len(fixture.driver.attachments[fixture.mobID]) != 0 ||
		len(fixture.driver.pendingDetaches[fixture.mobID]) != 1 {
		t.Fatalf(
			"failed compensation state: live=%d pending=%d",
			len(fixture.driver.attachments[fixture.mobID]),
			len(fixture.driver.pendingDetaches[fixture.mobID]),
		)
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); !state.GuardActive {
		t.Fatal("failed compensation lost the live state its retry must remove")
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(attachment, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("replayed materialize while cleanup pending: %v", err)
	}
	if applyCalls != 3 || len(fixture.driver.attachments[fixture.mobID]) != 0 ||
		len(fixture.driver.pendingDetaches[fixture.mobID]) != 1 {
		t.Fatalf(
			"pending cleanup admitted a replay: calls=%d live=%d pending=%d",
			applyCalls, len(fixture.driver.attachments[fixture.mobID]),
			len(fixture.driver.pendingDetaches[fixture.mobID]),
		)
	}

	fixture.zone.Step()
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 0 {
		t.Fatal("successful compensation retry retained metadata")
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); state.GuardActive {
		t.Fatalf("successful compensation retry retained Guard state %#v", state)
	}
}

func TestFailedActivationRollbackRetryPreservesReplayedEarlierEffect(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	attachment := script.Attachment{
		ID: "rollback-replay", EntityID: formatEntityID(fixture.mobID),
		TriggerRef: script.Ref{ID: "trigger.rollback-replay"},
		Trigger: &script.Node{
			Key: "trigger.rollback-replay", Family: script.FamilyTrigger,
			Opcode: "TriggerResource", Tier: script.TierImplemented,
			Fields: []script.Field{{Name: "effects", Value: script.Value{
				Kind: script.ValueList,
				List: []script.Value{
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/preexisting", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
					}},
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/new", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
					}},
					{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/rejected", Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
					}},
				},
			}}},
		},
		Frame: script.Frame{
			EvaluationID: "rollback-replay-eval", SourceID: "quest.rollback-replay",
			ZoneID: fixture.zone.ID(), CasterID: formatEntityID(fixture.playerID),
		},
	}
	preexistingID := attachment.ID + "|effects/preexisting"
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		return (scriptHost{driver: fixture.driver}).Apply(t.Context(), script.Command{
			Kind: script.CommandAttachGuard, EntityID: attachment.EntityID,
			EffectID: preexistingID, Guard: &script.Guard{
				Radius: script.Decimal{Mantissa: 425, Scale: 1},
			}, LifecycleAttempt: 99,
		})
	}); err != nil {
		t.Fatalf("seed pre-existing Guard: %v", err)
	}

	originalApply := fixture.driver.applyGuardUpdate
	applyCalls := 0
	fixture.driver.applyGuardUpdate = func(
		tick gametypes.Tick, entityID uint64, update combat.GuardUpdate,
	) error {
		applyCalls++
		if applyCalls == 2 || applyCalls == 3 {
			return errors.New("injected activation or rollback rejection")
		}
		return originalApply(tick, entityID, update)
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(attachment, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("materialize attachment: %v", err)
	}
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 1 {
		t.Fatal("failed compensation did not retain retry metadata")
	}

	fixture.zone.Step()
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); !state.GuardActive {
		t.Fatal("compensation retry removed the Guard replayed from an earlier attempt")
	}
}

func TestFailedDeathDetachRetainsMetadataAndRetriesWithoutRefiring(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	attachment := script.Attachment{
		ID: "death-retry", EntityID: formatEntityID(fixture.mobID),
		TriggerRef: script.Ref{ID: "trigger.death-retry"},
		Trigger: &script.Node{
			Key: "trigger.death-retry", Family: script.FamilyTrigger,
			Opcode: "TriggerResource", Tier: script.TierImplemented,
			Fields: []script.Field{{Name: "effects", Value: script.Value{
				Kind: script.ValueList,
				List: []script.Value{{Kind: script.ValueNode, Node: &script.Node{
					Key: "effects/guard", Family: script.FamilyEffect,
					Opcode: "Guard", Tier: script.TierImplemented,
				}}},
			}}},
		},
		Frame: script.Frame{
			EvaluationID: "death-retry-eval", SourceID: "quest.death-retry",
			ZoneID: fixture.zone.ID(), CasterID: formatEntityID(fixture.playerID),
		},
	}

	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(attachment, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("materialize attachment: %v", err)
	}
	if len(fixture.driver.attachments[fixture.mobID]) != 1 {
		t.Fatalf("live attachments = %d, want 1", len(fixture.driver.attachments[fixture.mobID]))
	}

	rejected := errors.New("injected first death detach rejection")
	originalApply := fixture.driver.applyGuardUpdate
	detachAttempts := 0
	fixture.driver.applyGuardUpdate = func(
		tick gametypes.Tick, entityID uint64, update combat.GuardUpdate,
	) error {
		if update.RemoveAggroState {
			detachAttempts++
			if detachAttempts == 1 {
				return rejected
			}
		}
		return originalApply(tick, entityID, update)
	}

	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.MobKilled(tick, combat.Kill{
			VictimEntityID: fixture.mobID, KillerEntityID: fixture.playerID,
			VictimContentID: effectTargetMob, DeathTick: tick.Number(),
		})
		return nil
	}); err != nil {
		t.Fatalf("first death cleanup: %v", err)
	}
	if len(fixture.driver.attachments[fixture.mobID]) != 0 {
		t.Fatalf("failed cleanup left a live event attachment")
	}
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 1 {
		t.Fatalf("pending detaches = %d, want retry metadata", len(fixture.driver.pendingDetaches[fixture.mobID]))
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); !state.GuardActive {
		t.Fatal("failed host detach dropped retryable Guard registry state")
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.MobKilled(tick, combat.Kill{
			VictimEntityID: fixture.mobID, KillerEntityID: fixture.playerID,
			VictimContentID: effectTargetMob, DeathTick: tick.Number(),
		})
		return nil
	}); err != nil {
		t.Fatalf("replayed death: %v", err)
	}
	if detachAttempts != 1 || len(fixture.driver.pendingDetaches[fixture.mobID]) != 1 {
		t.Fatalf(
			"replayed death changed cleanup state: attempts=%d pending=%d",
			detachAttempts, len(fixture.driver.pendingDetaches[fixture.mobID]),
		)
	}

	fixture.zone.Step()
	if detachAttempts != 2 {
		t.Fatalf("death detach attempts = %d, want failed attempt plus one retry", detachAttempts)
	}
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 0 {
		t.Fatalf("successful retry retained pending metadata")
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); state.GuardActive {
		t.Fatalf("successful retry retained Guard state %#v", state)
	}
}

func TestDeathDetachFailurePreservesReverseAttachmentOrder(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	attachment := func(id string, radius int64) script.Attachment {
		return script.Attachment{
			ID: id, EntityID: formatEntityID(fixture.mobID),
			TriggerRef: script.Ref{ID: "trigger." + id},
			Trigger: &script.Node{
				Key: "trigger." + id, Family: script.FamilyTrigger,
				Opcode: "TriggerResource", Tier: script.TierImplemented,
				Fields: []script.Field{{Name: "effects", Value: script.Value{
					Kind: script.ValueList,
					List: []script.Value{{Kind: script.ValueNode, Node: &script.Node{
						Key: "effects/guard-" + id, Family: script.FamilyEffect,
						Opcode: "Guard", Tier: script.TierImplemented,
						Fields: []script.Field{{Name: "scanRadius", Value: script.Value{
							Kind: script.ValueInteger, Integer: radius,
						}}},
					}}},
				}}},
			},
			Frame: script.Frame{
				EvaluationID: id + "-eval", SourceID: "quest." + id,
				ZoneID: fixture.zone.ID(), CasterID: formatEntityID(fixture.playerID),
			},
		}
	}
	first := attachment("first", 10)
	second := attachment("second", 20)
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.tick = tick
		defer func() { fixture.driver.tick = nil }()
		fixture.driver.materialize(first, fixture.mobID)
		fixture.driver.materialize(second, fixture.mobID)
		return nil
	}); err != nil {
		t.Fatalf("materialize attachments: %v", err)
	}

	originalApply := fixture.driver.applyGuardUpdate
	detachCalls := 0
	fixture.driver.applyGuardUpdate = func(
		tick gametypes.Tick, entityID uint64, update combat.GuardUpdate,
	) error {
		detachCalls++
		if detachCalls == 1 {
			return errors.New("injected later-attachment detach rejection")
		}
		return originalApply(tick, entityID, update)
	}
	if err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.driver.MobKilled(tick, combat.Kill{
			VictimEntityID: fixture.mobID, KillerEntityID: fixture.playerID,
			VictimContentID: effectTargetMob, DeathTick: tick.Number(),
		})
		return nil
	}); err != nil {
		t.Fatalf("death cleanup: %v", err)
	}
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 2 {
		t.Fatalf(
			"pending detaches = %d, want both attachments until the later one cleans up",
			len(fixture.driver.pendingDetaches[fixture.mobID]),
		)
	}
	if state := fixture.driver.effects.GuardState(
		formatEntityID(fixture.mobID), script.Decimal{Mantissa: 100},
	); state.ObserverRadius != (script.Decimal{Mantissa: 20}) {
		t.Fatalf("failed reverse cleanup changed Guard order/state: %#v", state)
	}

	fixture.zone.Step()
	if len(fixture.driver.pendingDetaches[fixture.mobID]) != 0 {
		t.Fatal("successful ordered retry retained cleanup metadata")
	}
}

func TestDestinationLocatorWithoutPackIndexFailsExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newEffectIntegrationFixture(t)
	_, err := (scriptHost{driver: fixture.driver}).Locate(t.Context(), script.DestinationRequest{
		Map: script.Ref{ID: "map.fixture"}, ScriptID: "MissingLocator",
	})
	if err == nil || !strings.Contains(err.Error(), "carries no absolute map-locator index") {
		t.Fatalf("Locate() error = %v, want missing-index failure", err)
	}
}

func TestScaledDamageRoundsHalfUpWithExactDecimalArithmetic(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		value script.Decimal
		want  int32
	}{
		{name: "below half", value: script.Decimal{Mantissa: 204, Scale: 1}, want: 20},
		{name: "at half", value: script.Decimal{Mantissa: 205, Scale: 1}, want: 21},
		{name: "zero floor", value: script.Decimal{Mantissa: -1, Scale: 1}, want: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := roundedDamage(testCase.value)
			if err != nil || got != testCase.want {
				t.Fatalf("roundedDamage(%#v) = %d, %v, want %d", testCase.value, got, err, testCase.want)
			}
		})
	}
}

func TestFloat32DecimalConversionRejectsNonFiniteAndOverflow(t *testing.T) {
	t.Parallel()
	for _, value := range []float32{
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), math.MaxFloat32,
	} {
		if _, err := decimalFromFloat32(value); err == nil {
			t.Fatalf("decimalFromFloat32(%v) succeeded, want explicit conversion error", value)
		}
	}
	got, err := decimalFromFloat32(42.5)
	if err != nil || got != (script.Decimal{Mantissa: 425, Scale: 1}) {
		t.Fatalf("decimalFromFloat32(42.5) = %#v, %v", got, err)
	}
}

var _ combat.DamageEffectHost = (*ScriptDriver)(nil)
var _ QuestScriptSource = emptyScriptSource{}
