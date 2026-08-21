package session

import (
	"log/slog"
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
	guard := script.Command{
		Kind: script.CommandAttachGuard, EntityID: formatEntityID(fixture.mobID), EffectID: "guard-near",
		Guard: &script.Guard{Radius: decimalFromFloat32(fixture.guardRadius), NoticeTarget: true},
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

var _ combat.DamageEffectHost = (*ScriptDriver)(nil)
var _ QuestScriptSource = emptyScriptSource{}
