package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/script"
)

const warriorActionGroup = "action-group.auto-attack-shared"

type warriorFixtureSource struct {
	emptyScriptSource
	action  WarriorAction
	actions map[string]WarriorAction
}

func (source warriorFixtureSource) WarriorAction(abilityID string) (WarriorAction, bool) {
	if source.actions != nil {
		action, ok := source.actions[abilityID]
		return action, ok
	}
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

func warriorRangedDamageImpact(key string, minimum, maximum int64) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: "ScaledPhysicalDamage", Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "minDamage", Value: script.Value{Kind: script.ValueDecimal, Mantissa: minimum, Scale: 2}},
			{Name: "maxDamage", Value: script.Value{Kind: script.ValueDecimal, Mantissa: maximum, Scale: 2}},
			{Name: "scaler", Value: script.Value{Kind: script.ValueNode, Node: &script.Node{
				Key: key + "/scaler", Family: script.FamilyScaler,
				Opcode: "PhysicalRangedScaler", Tier: script.TierImplemented,
			}}},
			{Name: "canBeAvoided", Value: script.Value{Kind: script.ValueBool, Bool: true}},
			{Name: "threatMultiplier", Value: script.Value{Kind: script.ValueInteger, Integer: 1}},
		},
	}
}

func warriorRemoteCondition(key string, distance int64) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyPredicate, Opcode: "PredicateRemote", Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "toLog", Value: script.Value{Kind: script.ValueBool}},
			{Name: "range", Value: script.Value{Kind: script.ValueInteger, Integer: distance}},
		},
	}
}

func warriorEquippedCondition(key string) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyPredicate, Opcode: "PredicateEquipped", Tier: script.TierImplemented,
		Fields: []script.Field{
			{Name: "toLog", Value: script.Value{Kind: script.ValueBool}},
			{Name: "dressType", Value: script.Value{Kind: script.ValueText, Text: "RANGED"}},
			{Name: "weaponRequired", Value: script.Value{Kind: script.ValueBool, Bool: true}},
		},
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
	if err := driver.SetWarriorCombatant(fixture.playerID, fixtureWarriorCombatant()); err != nil {
		t.Fatalf("SetWarriorCombatant() error = %v", err)
	}
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

func fixtureWarriorCombatant() WarriorCombatant {
	return WarriorCombatant{
		PhysicalScale: script.Decimal{Mantissa: 2}, PhysicalRangedScale: script.Decimal{Mantissa: 2},
		WeaponSpeedScale: map[string]script.Decimal{
			"Mainhand": {Mantissa: 1}, "Ranged": {Mantissa: 15, Scale: 1},
		},
		EquippedDressTypes: map[string]bool{"RANGED": true},
	}
}

func TestActionResourceCostUsesExtractedWeaponSpeedExactly(t *testing.T) {
	action := WarriorAction{
		ResourceCost:           script.Decimal{Mantissa: 30},
		ScaleCostByWeaponSpeed: true,
		ResourceSource:         "Ranged",
	}
	cost, err := actionResourceMilli(action, WarriorCombatant{WeaponSpeedScale: map[string]script.Decimal{
		"Ranged": {Mantissa: 15, Scale: 1},
	}})
	if err != nil || cost != 45_000 {
		t.Fatalf("scaled AimedShot cost = %d, %v, want 45000", cost, err)
	}
}

func TestActionCooldownUsesExtractedWeaponSpeedExactly(t *testing.T) {
	action := WarriorAction{
		Cooldown: time.Second, CooldownScalesByWeaponSpeed: true, CooldownSource: "Mainhand",
	}
	got, err := actionCooldown(action, WarriorCombatant{WeaponSpeedScale: map[string]script.Decimal{
		"Mainhand": {Mantissa: 25, Scale: 1},
	}})
	if err != nil || got != 2500*time.Millisecond {
		t.Fatalf("scaled auto-attack cooldown = %s, %v, want 2.5s", got, err)
	}
}

func TestExtractedSharedCooldownBlocksSwitchingActions(t *testing.T) {
	fixture := newEffectIntegrationFixtureFromPack(t, false, "demo-extended")
	abilityIDs := fixture.combat.Rules().AbilityIDs()
	if len(abilityIDs) < 2 {
		t.Fatalf("fixture has %d abilities, want at least two", len(abilityIDs))
	}
	group := "action-group.auto-attack-shared"
	actions := make(map[string]WarriorAction, 2)
	bindings := make([]combat.ActionBinding, 0, 2)
	for index, abilityID := range abilityIDs[:2] {
		actions[abilityID] = WarriorAction{
			AbilityID: abilityID, ActionGroupID: group,
			Cooldown: time.Second, CooldownGroupID: group,
			TargetImpacts: []*script.Node{warriorDamageImpact("shared/"+abilityID, 100)},
		}
		bindings = append(bindings, combat.ActionBinding{SlotIndex: uint32(index), AbilityID: abilityID})
	}
	driver := NewScriptDriver(nil, fixture.zone, nil, warriorFixtureSource{actions: actions}, script.Options{Enabled: true})
	driver.BindCombat(fixture.combat)
	if err := driver.SetWarriorCombatant(fixture.playerID, fixtureWarriorCombatant()); err != nil {
		t.Fatalf("SetWarriorCombatant() error = %v", err)
	}
	if err := fixture.combat.AdmitWithLoadout(fixture.playerID, combat.ActionLoadout{Bindings: bindings}); err != nil {
		t.Fatalf("AdmitWithLoadout() error = %v", err)
	}
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		caster, target := tick.Entity(fixture.playerID), tick.Entity(fixture.mobID)
		tick.MoveTo(caster, tick.Position(target).Add(gametypes.Vec3{X: 1}))
		return nil
	})
	if _, err := fixture.combat.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1); err != nil {
		t.Fatalf("first shared action error = %v", err)
	}
	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 1, 2); !errors.Is(err, combat.ErrOnCooldown) {
		t.Fatalf("switched shared action error = %v, want shared cooldown", err)
	}
}

func TestAimedShotValidatesCasterAndFinishesAfterPreparation(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, PrepareDuration: 3 * (time.Second / 30),
		TriggersGlobalCooldown: true,
		CasterConditions: []*script.Node{
			warriorRemoteCondition("aimed/casterConditions[0]", 5),
			warriorEquippedCondition("aimed/casterConditions[1]"),
		},
		TargetImpacts: []*script.Node{warriorRangedDamageImpact("aimed/targetImpacts[0]", 1481, 1810)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{})

	before := fixture.health(t)
	start, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1)
	if err != nil || start.Damage != 0 || fixture.health(t) != before {
		t.Fatalf("AimedShot start = %+v, %v; health=%d want unchanged %d", start, err, fixture.health(t), before)
	}
	for range 2 {
		fixture.zone.Step()
	}
	if got := fixture.health(t); got != before {
		t.Fatalf("AimedShot resolved before prepare duration: health=%d want %d", got, before)
	}
	fixture.zone.Step()
	if got := fixture.health(t); got >= before || got < before-37 {
		t.Fatalf("AimedShot completion health=%d, want one ranged hit from %d", got, before)
	}
}

func TestAimedShotCasterConditionRefusesBeforeSpend(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID:        effectAbility,
		CasterConditions: []*script.Node{warriorRemoteCondition("aimed/remote", 8)},
		TargetImpacts:    []*script.Node{warriorRangedDamageImpact("aimed/damage", 1481, 1810)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{})
	before := fixture.health(t)
	event, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1)
	if !errors.Is(err, combat.ErrInvalidTarget) || event.Rejection != combat.RejectionInvalidTarget || fixture.health(t) != before {
		t.Fatalf("near AimedShot = %+v, %v health=%d, want condition refusal", event, err, fixture.health(t))
	}
}

func TestAimedShotRevalidatesRangeAtPreparationFinish(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, PrepareDuration: 2 * (time.Second / 30),
		TargetImpacts: []*script.Node{warriorRangedDamageImpact("aimed/revalidate", 1481, 1810)},
	}
	bindWarriorAction(t, fixture, action, combat.ActionResource{})
	before := fixture.health(t)
	if _, err := fixture.combat.ActivateSlot(fixture.playerID, 0, 1); err != nil {
		t.Fatalf("AimedShot start error = %v", err)
	}
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		caster := tick.Entity(fixture.playerID)
		tick.MoveTo(caster, tick.Position(caster).Add(gametypes.Vec3{X: 100}))
		return nil
	})
	for range 2 {
		fixture.zone.Step()
	}
	if got := fixture.health(t); got != before {
		t.Fatalf("out-of-range prepared AimedShot changed health to %d, want %d", got, before)
	}
}

func TestWarriorDamageExecutionKeyIsIdempotent(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID:     effectAbility,
		TargetImpacts: []*script.Node{warriorDamageImpact("idempotent/damage", 875)},
	}
	driver := bindWarriorAction(t, fixture, action, combat.ActionResource{})
	profile, ok := driver.Profile(fixture.playerID, action.AbilityID)
	if !ok {
		t.Fatal("Profile() did not resolve fixture action")
	}
	invocation := combat.ScriptActionInvocation{
		CasterID: fixture.playerID, TargetID: fixture.mobID, AbilityID: action.AbilityID,
		ActivationOrdinal: 7, DefinitionDigest: profile.DefinitionDigest,
	}
	before := fixture.health(t)
	err := fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		for range 2 {
			if _, err := driver.ExecuteAction(tick, invocation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replayed ExecuteAction() error = %v", err)
	}
	if got := fixture.health(t); got != before-18 {
		t.Fatalf("replayed damage changed health to %d, want one hit %d", got, before-18)
	}

	fixture.combat.Release(fixture.playerID)
	if err := fixture.combat.AdmitWithLoadout(fixture.playerID, combat.ActionLoadout{
		Bindings: []combat.ActionBinding{{SlotIndex: 0, AbilityID: action.AbilityID}},
	}); err != nil {
		t.Fatalf("re-admit caster: %v", err)
	}
	if _, err := fixture.combat.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("reselect target: %v", err)
	}
	before = fixture.health(t)
	err = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		_, executeErr := driver.ExecuteAction(tick, invocation)
		return executeErr
	})
	if err != nil {
		t.Fatalf("reused ordinal ExecuteAction() error = %v", err)
	}
	if got := fixture.health(t); got != before-18 {
		t.Fatalf("reused ordinal damage changed health to %d, want one fresh hit %d", got, before-18)
	}
}

func TestExtractedWarriorActionOwnsDamageResourceCooldownAndEvent(t *testing.T) {
	fixture := newEffectIntegrationFixture(t)
	action := WarriorAction{
		AbilityID: effectAbility, ActionGroupID: warriorActionGroup,
		TriggersGlobalCooldown: true,
		ResourceKind:           "Energy", ResourceCost: script.Decimal{Mantissa: 125, Scale: 1},
		TargetImpacts: []*script.Node{warriorDamageImpact("auto-attack/targetImpacts[0]", 875)},
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
		TargetImpacts: []*script.Node{warriorDamageImpact("range/targetImpacts[0]", 875)},
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
		TargetImpacts: []*script.Node{warriorSetTargetImpact("entrapment/targetImpacts[0]")},
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
		TargetImpacts: []*script.Node{warriorDamageImpact("lethal/targetImpacts[0]", 87500)},
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
