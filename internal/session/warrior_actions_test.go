package session

import (
	"errors"
	"sync"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
)

const warriorActionGroup = "action-group.auto-attack-shared"

type warriorFixtureSource struct {
	emptyScriptSource
	action WarriorAction
}

func (source warriorFixtureSource) WarriorAction(abilityID string) (WarriorAction, bool) {
	return source.action, abilityID == source.action.AbilityID
}

type recordingKillSink struct {
	mu    sync.Mutex
	kills []combat.Kill
}

func (sink *recordingKillSink) MobKilled(_ gametypes.Tick, kill combat.Kill) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.kills = append(sink.kills, kill)
}

func (sink *recordingKillSink) last() (combat.Kill, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.kills) == 0 {
		return combat.Kill{}, false
	}
	return sink.kills[len(sink.kills)-1], true
}

func warriorDamageImpact(key string, averageDamage int64) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: "ScaledPhysicalWeaponDamage",
		Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "avgDamage", Value: script.Value{Kind: script.ValueDecimal, Mantissa: averageDamage, Scale: 2}},
			{Name: "canBeAvoided", Value: script.Value{Kind: script.ValueBool, Bool: true}},
			{Name: "scaler", Value: script.Value{Kind: script.ValueNode, Node: &script.Node{
				Key: key + "/scaler", Family: script.FamilyScaler,
				Opcode: "PhysicalScaler", Tier: script.TierImplemented,
			}}},
			{Name: "source", Value: script.Value{Kind: script.ValueText, Text: "Mainhand"}},
			{Name: "threatMultiplier", Value: script.Value{Kind: script.ValueInteger, Integer: 1}},
		},
	}
}

func warriorSetTargetImpact(key string) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: "ImpactSetTarget", Tier: script.TierImplemented,
		Fields: []script.Field{{Name: "addresseeFinder", Value: script.Value{
			Kind: script.ValueNode,
			Node: &script.Node{
				Key: key + "/addresseeFinder", Family: script.FamilyFinder,
				Opcode: "AddresseeFinderCaster", Tier: script.TierImplemented,
			},
		}}},
	}
}

func bindWarriorAction(
	t *testing.T,
	fixture *effectIntegrationFixture,
	action WarriorAction,
	resource combat.ActionResource,
) *ScriptDriver {
	t.Helper()
	driver := NewScriptDriver(
		nil, fixture.zone, nil, warriorFixtureSource{action: action}, script.Options{Enabled: true},
	)
	driver.BindCombat(fixture.combat)
	if err := fixture.combat.AdmitWithLoadout(fixture.playerID, combat.ActionLoadout{
		Bindings: []combat.ActionBinding{{SlotIndex: 0, AbilityID: action.AbilityID}},
		Resource: resource,
	}); err != nil {
		t.Fatalf("AdmitWithLoadout() error = %v", err)
	}
	if _, err := fixture.combat.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	return driver
}

func TestActionResourceCostUsesExtractedWeaponSpeedExactly(t *testing.T) {
	action := WarriorAction{
		ResourceCost:           script.Decimal{Mantissa: 30},
		ScaleCostByWeaponSpeed: true,
		WeaponSpeedScale:       script.Decimal{Mantissa: 15, Scale: 1},
	}
	cost, err := actionResourceMilli(action)
	if err != nil || cost != 45_000 {
		t.Fatalf("scaled AimedShot cost = %d, %v, want 45000", cost, err)
	}
}

func TestExtractedWarriorActionOwnsDamageResourceCooldownAndEvent(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, ActionGroupID: warriorActionGroup,
		ResourceKind: "Energy", ResourceCost: script.Decimal{Mantissa: 125, Scale: 1},
		PhysicalScale:       script.Decimal{Mantissa: 2},
		PhysicalRangedScale: script.Decimal{Mantissa: 2},
		WeaponSpeedScale:    script.Decimal{Mantissa: 1},
		TargetImpacts:       []*script.Node{warriorDamageImpact("auto-attack/targetImpacts[0]", 875)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{
		Kind: "Energy", CurrentMilli: 25_000, MaximumMilli: 25_000,
	})

	before := fixture.health(t)
	event, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1)
	if err != nil {
		t.Fatalf("ActivateSlot() error = %v", err)
	}
	if event.AbilityID != effectAbility || event.ActionGroupID != warriorActionGroup || event.Damage != 18 {
		t.Fatalf("action event = %+v, want extracted id/group and round(8.75*2)=18", event)
	}
	if after := fixture.health(t); after != before-18 {
		t.Fatalf("target health = %d, want %d", after, before-18)
	}
	resource, err := fixture.combat.ActionResourceState(fixture.playerID)
	if err != nil || resource.CurrentMilli != 12_500 {
		t.Fatalf("resource after action = %+v, %v, want 12500", resource, err)
	}

	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 2); !errors.Is(err, combat.ErrOnCooldown) {
		t.Fatalf("immediate replay error = %v, want cooldown refusal", err)
	}
	resource, _ = fixture.combat.ActionResourceState(fixture.playerID)
	if resource.CurrentMilli != 12_500 {
		t.Fatalf("cooldown refusal consumed resource: %+v", resource)
	}

	for range 30 {
		fixture.zone.Step()
	}
	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 3); err != nil {
		t.Fatalf("second paid action error = %v", err)
	}
	for range 30 {
		fixture.zone.Step()
	}
	healthBeforeRefusal := fixture.health(t)
	event, err = fixture.combat.ActivateSlot(fixture.playerID, 0, 4)
	if !errors.Is(err, combat.ErrNoResource) || event.Rejection != combat.RejectionNoResource {
		t.Fatalf("empty resource action = %+v, %v, want no-resource refusal", event, err)
	}
	if after := fixture.health(t); after != healthBeforeRefusal {
		t.Fatalf("resource refusal changed health from %d to %d", healthBeforeRefusal, after)
	}
	bar, err := fixture.combat.ActionBar(fixture.playerID)
	if err != nil || bar.Slots[0].UnavailableReason != combat.ActionUnavailableNoResource {
		t.Fatalf("action bar after resource exhaustion = %+v, %v", bar.Slots[0], err)
	}
}

func TestWarriorRangeRefusalMutatesNeitherResourceNorTarget(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, ActionGroupID: warriorActionGroup,
		ResourceKind: "Energy", ResourceCost: script.Decimal{Mantissa: 125, Scale: 1},
		PhysicalScale:       script.Decimal{Mantissa: 2},
		PhysicalRangedScale: script.Decimal{Mantissa: 2},
		WeaponSpeedScale:    script.Decimal{Mantissa: 1},
		TargetImpacts:       []*script.Node{warriorDamageImpact("range/targetImpacts[0]", 875)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{
		Kind: "Energy", CurrentMilli: 25_000, MaximumMilli: 25_000,
	})
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		caster := tick.Entity(fixture.playerID)
		tick.MoveTo(caster, tick.Position(caster).Add(gametypes.Vec3{X: 100}))
		return nil
	})

	before := fixture.health(t)
	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1); !errors.Is(err, combat.ErrOutOfRange) {
		t.Fatalf("out-of-range activation error = %v, want ErrOutOfRange", err)
	}
	resource, _ := fixture.combat.ActionResourceState(fixture.playerID)
	if resource.CurrentMilli != 25_000 || fixture.health(t) != before {
		t.Fatalf("range refusal mutated resource or health: resource=%+v health=%d want=%d", resource, fixture.health(t), before)
	}
}

func TestImpactSetTargetMutatesAuthoritativeMobTarget(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, ActionGroupID: "action-group.warrior.entrapment",
		PhysicalScale:       script.Decimal{Mantissa: 1},
		PhysicalRangedScale: script.Decimal{Mantissa: 1},
		WeaponSpeedScale:    script.Decimal{Mantissa: 1},
		TargetImpacts:       []*script.Node{warriorSetTargetImpact("entrapment/targetImpacts[0]")},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{})

	event, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1)
	if err != nil || event.ActionGroupID != action.ActionGroupID || event.Damage != 0 {
		t.Fatalf("target-control action = %+v, %v", event, err)
	}
	selected, err := fixture.combat.SelectedTarget(fixture.mobID)
	if err != nil || selected != fixture.playerID {
		t.Fatalf("mob authoritative target = %d, %v, want player %d", selected, err, fixture.playerID)
	}
}

func TestLethalWarriorDamagePublishesKillForQuestAndEventSinks(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, ActionGroupID: warriorActionGroup,
		PhysicalScale:       script.Decimal{Mantissa: 100},
		PhysicalRangedScale: script.Decimal{Mantissa: 100},
		WeaponSpeedScale:    script.Decimal{Mantissa: 1},
		TargetImpacts:       []*script.Node{warriorDamageImpact("lethal/targetImpacts[0]", 875)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{})
	kills := &recordingKillSink{}
	fixture.combat.SetKillSink(kills)

	event, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1)
	if err != nil || !event.KillingBlow {
		t.Fatalf("lethal action = %+v, %v", event, err)
	}
	kill, ok := kills.last()
	if !ok || kill.KillerEntityID != fixture.playerID || kill.VictimEntityID != fixture.mobID ||
		kill.VictimContentID != effectTargetMob {
		t.Fatalf("kill sink event = %+v, %v", kill, ok)
	}
}
