package pack

import (
	"errors"
	"testing"
	"time"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const fixtureNativeActionID = "action.fixture.auto-attack"

func TestPackReadsNativeActionsWithoutExposingStoredNodes(t *testing.T) {
	t.Parallel()
	directory := copyFixture(t)
	action := fixtureNativeAction()
	replaceCompiledTable(t, directory, tableNativeActions, contentv1.RowType_ROW_TYPE_NATIVE_ACTION,
		[]compiledRow{{id: action.GetId(), message: action}})
	reseal(t, directory)

	content, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loaded, ok := content.NativeAction(fixtureNativeActionID)
	if !ok || loaded.RangeM != (Decimal{Mantissa: 5}) ||
		loaded.Cooldown == nil || loaded.Cooldown.Duration != 2*time.Second ||
		loaded.Resource == nil || loaded.Resource.Cost != (Decimal{Mantissa: 125, Scale: 1}) ||
		len(loaded.TargetImpacts) != 1 {
		t.Fatalf("NativeAction() = %#v, %v", loaded, ok)
	}
	loaded.TargetImpacts[0].Opcode = "scribbled-over"
	fresh, _ := content.NativeAction(fixtureNativeActionID)
	if fresh.TargetImpacts[0].Opcode == "scribbled-over" {
		t.Fatal("NativeAction() exposed the stored impact slice")
	}
}

func TestPackRejectsMalformedNativeActions(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*contentv1.NativeAction){
		"missing range": func(action *contentv1.NativeAction) { action.RangeM = nil },
		"no impacts":    func(action *contentv1.NativeAction) { action.TargetImpacts = nil },
		"negative resource": func(action *contentv1.NativeAction) {
			action.Resource.Cost.Mantissa = -1
		},
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := copyFixture(t)
			action := fixtureNativeAction()
			mutate(action)
			replaceCompiledTable(t, directory, tableNativeActions, contentv1.RowType_ROW_TYPE_NATIVE_ACTION,
				[]compiledRow{{id: action.GetId(), message: action}})
			reseal(t, directory)
			if _, err := Load(directory, Options{}); !errors.Is(err, ErrMalformedTable) {
				t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
			}
		})
	}
}

func fixtureNativeAction() *contentv1.NativeAction {
	return &contentv1.NativeAction{
		Id: fixtureNativeActionID, TargetPolicy: "current-target",
		RangeM: &contentv1.Decimal{Mantissa: 5}, RequiresLos: true,
		IsAggro: true, TriggersGcd: true, IgnoresGcd: true,
		ActionGroupId: "action-group.fixture.auto-attack",
		Cooldown: &contentv1.ActionCooldown{
			DurationMs: 2000, GroupId: "action-group.fixture.auto-attack",
			Scaler: "weapon-speed", Base: &contentv1.Decimal{Mantissa: 1},
		},
		Resource: &contentv1.ActionResource{
			Kind: "energy", Cost: &contentv1.Decimal{Mantissa: 125, Scale: 1},
			ScaleByWeaponSpeed: true, Source: "mainhand",
		},
		TargetImpacts: []*contentv1.ScriptNode{{
			NodeKey: fixtureNativeActionID + "/targetImpacts[0]",
			Family:  "impact", Opcode: "ScaledPhysicalWeaponDamage",
			Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
			Fields: []*contentv1.ScriptField{
				{Name: "avgDamage", Value: decimalValue(875, 2)},
				{Name: "canBeAvoided", Value: boolValue(true)},
				{Name: "scaler", Value: nodeValue(&contentv1.ScriptNode{
					NodeKey: fixtureNativeActionID + "/targetImpacts[0]/scaler",
					Family:  "scaler", Opcode: "PhysicalScaler",
					Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
				})},
				{Name: "source", Value: textValue("Mainhand")},
				{Name: "threatMultiplier", Value: integerValue(1)},
			},
		}},
	}
}

func fixtureStartingCombatStats() *contentv1.StartingCharacterStats {
	return &contentv1.StartingCharacterStats{
		Health: 146, MaxHealth: 146,
		Resource: &contentv1.StartingResource{
			Kind: "energy", Initial: &contentv1.Decimal{Mantissa: 100},
			Maximum: &contentv1.Decimal{Mantissa: 100},
		},
		Innate: []*contentv1.ExactStatEntry{{
			Stat: "strength", Value: &contentv1.Decimal{Mantissa: 20},
		}},
		Armor:            &contentv1.Decimal{},
		HitDice:          &contentv1.Decimal{Mantissa: 1},
		ManaDice:         &contentv1.Decimal{Mantissa: 1},
		BaseStatValue:    &contentv1.Decimal{Mantissa: 20},
		WeaponDpsDefault: &contentv1.Decimal{Mantissa: 1},
		FairyScaler:      &contentv1.Decimal{Mantissa: 1},
		Mainhand: &contentv1.WeaponProfile{
			MinimumDamage: &contentv1.Decimal{Mantissa: 1},
			MaximumDamage: &contentv1.Decimal{Mantissa: 1},
			SpeedMs:       &contentv1.Decimal{Mantissa: 2000},
		},
		Ranged: &contentv1.WeaponProfile{
			MinimumDamage: &contentv1.Decimal{Mantissa: 1},
			MaximumDamage: &contentv1.Decimal{Mantissa: 1},
			SpeedMs:       &contentv1.Decimal{Mantissa: 2000},
		},
	}
}

func decimalValue(mantissa int64, scale int32) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Decimal{
		Decimal: &contentv1.Decimal{Mantissa: mantissa, Scale: scale},
	}}
}

func boolValue(value bool) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Boolean{Boolean: value}}
}
