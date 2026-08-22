package pack

import (
	"fmt"
	"math"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const tableNativeActions = "native-actions"

// Decimal is an exact native-content number.
type Decimal struct {
	Mantissa int64
	Scale    int32
}

func (value Decimal) Float64() (float64, bool) {
	if value.Scale < 0 || value.Scale > 18 {
		return 0, false
	}
	divisor := math.Pow10(int(value.Scale))
	result := float64(value.Mantissa) / divisor
	return result, !math.IsInf(result, 0) && !math.IsNaN(result)
}

type NativeActionCooldown struct {
	Duration time.Duration
	GroupID  string
	Scaler   string
	Base     Decimal
}

type NativeActionResource struct {
	Kind               string
	Cost               Decimal
	ScaleByWeaponSpeed bool
	Source             string
}

// NativeAction is one source-free action row from the compiled product pack.
type NativeAction struct {
	ID               string
	TargetPolicy     string
	RangeM           Decimal
	CastDuration     time.Duration
	ChannelDuration  time.Duration
	PrepareDuration  time.Duration
	RequiresLOS      bool
	IsAggro          bool
	TriggersGCD      bool
	IgnoresGCD       bool
	ActionGroupID    string
	Cooldown         *NativeActionCooldown
	Resource         *NativeActionResource
	TargetImpacts    []ScriptNode
	CasterConditions []ScriptNode
}

func (p *Pack) NativeAction(id string) (NativeAction, bool) {
	action, ok := p.nativeActions[id]
	return cloneNativeAction(action), ok
}

func (p *Pack) NativeActions() []NativeAction {
	ids := make([]string, 0, len(p.nativeActions))
	for id := range p.nativeActions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	actions := make([]NativeAction, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, cloneNativeAction(p.nativeActions[id]))
	}
	return actions
}

func cloneNativeAction(action NativeAction) NativeAction {
	result := action
	result.TargetImpacts = append([]ScriptNode(nil), action.TargetImpacts...)
	result.CasterConditions = append([]ScriptNode(nil), action.CasterConditions...)
	if action.Cooldown != nil {
		cooldown := *action.Cooldown
		result.Cooldown = &cooldown
	}
	if action.Resource != nil {
		resource := *action.Resource
		result.Resource = &resource
	}
	return result
}

func readNativeActions(tables map[string]*table) (map[string]NativeAction, error) {
	loaded, ok := tables[tableNativeActions]
	if !ok {
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_NATIVE_ACTION; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf("%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableNativeActions, contentv1.RowType(loaded.rowTypeID), want)
	}
	actions := make(map[string]NativeAction, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.NativeAction
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode native action row: %w", ErrMalformedTable, err)
		}
		if row.GetId() == "" || row.GetTargetPolicy() == "" {
			return nil, fmt.Errorf("%w: native action has id %q and target policy %q",
				ErrMalformedTable, row.GetId(), row.GetTargetPolicy())
		}
		if _, duplicate := actions[row.GetId()]; duplicate {
			return nil, fmt.Errorf("%w: table %q repeats id %q",
				ErrMalformedTable, tableNativeActions, row.GetId())
		}
		rangeM, err := readNonNegativeDecimal(row.GetId(), "range_m", row.GetRangeM())
		if err != nil {
			return nil, err
		}
		action := NativeAction{
			ID: row.GetId(), TargetPolicy: row.GetTargetPolicy(), RangeM: rangeM,
			CastDuration:    time.Duration(row.GetCastDurationMs()) * time.Millisecond,
			ChannelDuration: time.Duration(row.GetChannelDurationMs()) * time.Millisecond,
			PrepareDuration: time.Duration(row.GetPrepareDurationMs()) * time.Millisecond,
			RequiresLOS:     row.GetRequiresLos(), IsAggro: row.GetIsAggro(),
			TriggersGCD: row.GetTriggersGcd(), IgnoresGCD: row.GetIgnoresGcd(),
			ActionGroupID: row.GetActionGroupId(),
		}
		if cooldown := row.GetCooldown(); cooldown != nil {
			base, err := readNonNegativeDecimal(row.GetId(), "cooldown.base", cooldown.GetBase())
			if err != nil {
				return nil, err
			}
			action.Cooldown = &NativeActionCooldown{
				Duration: time.Duration(cooldown.GetDurationMs()) * time.Millisecond,
				GroupID:  cooldown.GetGroupId(), Scaler: cooldown.GetScaler(), Base: base,
			}
		}
		if resource := row.GetResource(); resource != nil {
			cost, err := readNonNegativeDecimal(row.GetId(), "resource.cost", resource.GetCost())
			if err != nil {
				return nil, err
			}
			if resource.GetKind() == "" {
				return nil, fmt.Errorf("%w: native action %q has a resource cost with no kind",
					ErrMalformedTable, row.GetId())
			}
			action.Resource = &NativeActionResource{
				Kind: resource.GetKind(), Cost: cost,
				ScaleByWeaponSpeed: resource.GetScaleByWeaponSpeed(), Source: resource.GetSource(),
			}
		}
		for index, node := range row.GetTargetImpacts() {
			converted, err := readScriptNode(row.GetId(), fmt.Sprintf("target_impacts[%d]", index), node)
			if err != nil {
				return nil, err
			}
			action.TargetImpacts = append(action.TargetImpacts, converted)
		}
		for index, node := range row.GetCasterConditions() {
			converted, err := readScriptNode(row.GetId(), fmt.Sprintf("caster_conditions[%d]", index), node)
			if err != nil {
				return nil, err
			}
			action.CasterConditions = append(action.CasterConditions, converted)
		}
		if len(action.TargetImpacts) == 0 {
			return nil, fmt.Errorf("%w: native action %q has no target impacts",
				ErrMalformedTable, row.GetId())
		}
		actions[action.ID] = action
	}
	return actions, nil
}

func readNonNegativeDecimal(rowID, field string, value *contentv1.Decimal) (Decimal, error) {
	if value == nil || value.GetMantissa() < 0 || value.GetScale() < 0 || value.GetScale() > 18 {
		return Decimal{}, fmt.Errorf("%w: native action %q has malformed %s",
			ErrMalformedTable, rowID, field)
	}
	return Decimal{Mantissa: value.GetMantissa(), Scale: value.GetScale()}, nil
}
