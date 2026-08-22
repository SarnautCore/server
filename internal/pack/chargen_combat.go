package pack

import (
	"fmt"
	"math/big"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

func readStartingCombatStats(
	optionID string,
	row *contentv1.StartingCharacterStats,
) (*StartingCombatStats, error) {
	if row == nil || row.GetResource() == nil {
		return nil, nil
	}
	resource := row.GetResource()
	if resource.GetKind() == "" {
		return nil, fmt.Errorf("%w: chargen option %q has a starting resource with no kind",
			ErrMalformedTable, optionID)
	}
	initial, err := readChargenDecimal(optionID, "resource.initial", resource.GetInitial())
	if err != nil {
		return nil, err
	}
	maximum, err := readChargenDecimal(optionID, "resource.maximum", resource.GetMaximum())
	if err != nil {
		return nil, err
	}
	if decimalCompare(initial, maximum) > 0 || maximum.Mantissa <= 0 {
		return nil, fmt.Errorf("%w: chargen option %q has invalid resource bounds",
			ErrMalformedTable, optionID)
	}
	result := &StartingCombatStats{
		Resource: StartingResource{Kind: resource.GetKind(), Initial: initial, Maximum: maximum},
	}
	if result.Armor, err = readChargenDecimal(optionID, "armor", row.GetArmor()); err != nil {
		return nil, err
	}
	if result.HitDice, err = readChargenDecimal(optionID, "hit_dice", row.GetHitDice()); err != nil {
		return nil, err
	}
	if result.ManaDice, err = readChargenDecimal(optionID, "mana_dice", row.GetManaDice()); err != nil {
		return nil, err
	}
	if result.BaseStatValue, err = readChargenDecimal(optionID, "base_stat_value", row.GetBaseStatValue()); err != nil {
		return nil, err
	}
	if result.WeaponDPSDefault, err = readChargenDecimal(optionID, "weapon_dps_default", row.GetWeaponDpsDefault()); err != nil {
		return nil, err
	}
	if result.FairyScaler, err = readChargenDecimal(optionID, "fairy_scaler", row.GetFairyScaler()); err != nil {
		return nil, err
	}
	if result.Mainhand, err = readWeaponProfile(optionID, "mainhand", row.GetMainhand()); err != nil {
		return nil, err
	}
	if result.Ranged, err = readWeaponProfile(optionID, "ranged", row.GetRanged()); err != nil {
		return nil, err
	}
	result.Innate, err = readExactStats(optionID, "innate", row.GetInnate())
	if err != nil {
		return nil, err
	}
	result.Resistances, err = readExactStats(optionID, "resistances", row.GetResistances())
	if err != nil {
		return nil, err
	}
	if len(result.Innate) == 0 || result.BaseStatValue.Mantissa <= 0 ||
		result.WeaponDPSDefault.Mantissa <= 0 || result.FairyScaler.Mantissa <= 0 {
		return nil, fmt.Errorf("%w: chargen option %q has incomplete native combat stats",
			ErrMalformedTable, optionID)
	}
	return result, nil
}

func readWeaponProfile(optionID, field string, row *contentv1.WeaponProfile) (WeaponProfile, error) {
	if row == nil {
		return WeaponProfile{}, fmt.Errorf("%w: chargen option %q has no %s profile",
			ErrMalformedTable, optionID, field)
	}
	minimum, err := readChargenDecimal(optionID, field+".minimum_damage", row.GetMinimumDamage())
	if err != nil {
		return WeaponProfile{}, err
	}
	maximum, err := readChargenDecimal(optionID, field+".maximum_damage", row.GetMaximumDamage())
	if err != nil {
		return WeaponProfile{}, err
	}
	speed, err := readChargenDecimal(optionID, field+".speed_ms", row.GetSpeedMs())
	if err != nil {
		return WeaponProfile{}, err
	}
	if minimum.Mantissa < 0 || decimalCompare(minimum, maximum) > 0 || speed.Mantissa <= 0 {
		return WeaponProfile{}, fmt.Errorf("%w: chargen option %q has malformed %s profile",
			ErrMalformedTable, optionID, field)
	}
	return WeaponProfile{MinimumDamage: minimum, MaximumDamage: maximum, SpeedMS: speed}, nil
}

func readExactStats(
	optionID, field string,
	rows []*contentv1.ExactStatEntry,
) ([]ExactStatValue, error) {
	result := make([]ExactStatValue, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		if row.GetStat() == "" {
			return nil, fmt.Errorf("%w: chargen option %q has empty %s stat %d",
				ErrMalformedTable, optionID, field, index)
		}
		if _, duplicate := seen[row.GetStat()]; duplicate {
			return nil, fmt.Errorf("%w: chargen option %q repeats %s stat %q",
				ErrMalformedTable, optionID, field, row.GetStat())
		}
		value, err := readChargenDecimal(optionID, field+"."+row.GetStat(), row.GetValue())
		if err != nil {
			return nil, err
		}
		seen[row.GetStat()] = struct{}{}
		result = append(result, ExactStatValue{Stat: row.GetStat(), Value: value})
	}
	return result, nil
}

func readChargenDecimal(optionID, field string, value *contentv1.Decimal) (Decimal, error) {
	if value == nil || value.GetScale() < 0 || value.GetScale() > 18 {
		return Decimal{}, fmt.Errorf("%w: chargen option %q has malformed %s",
			ErrMalformedTable, optionID, field)
	}
	return Decimal{Mantissa: value.GetMantissa(), Scale: value.GetScale()}, nil
}

func decimalCompare(left, right Decimal) int {
	leftValue := new(big.Int).SetInt64(left.Mantissa)
	leftValue.Mul(leftValue, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(right.Scale)), nil))
	rightValue := new(big.Int).SetInt64(right.Mantissa)
	rightValue.Mul(rightValue, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(left.Scale)), nil))
	return leftValue.Cmp(rightValue)
}
