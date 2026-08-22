package script

import (
	"fmt"
	"math"
)

// amount is an exact base-10 decimal: mantissa * 10^-scale. It is the arithmetic
// type for every number that reaches authoritative state — calcer thresholds,
// scaler magnitudes, damage — and it is deliberately not a float.
//
// ADR 0036 fixes this: "Decimal is an exact signed mantissa plus a base-10
// scale", and probability thresholds "are compared with integer arithmetic so
// that a restart makes the same choice". A health threshold and a damage number
// have the same requirement. FullHealthCalcer(multiplier=0) must compare equal to
// zero on every machine that ever replays a rat's death, and a float multiplier
// of 0.35 that rounds one way on a fresh evaluation and another way on a deferred
// replay is a silent divergence in authoritative state.
type amount struct {
	mantissa int64
	scale    int32
}

// maxScale bounds the scale so that repeated multiplication cannot walk off into
// an unrepresentable power of ten. Nothing in the content needs more: the widest
// authored decimal in the Warrior surface is four fractional digits.
const maxScale = 9

var powersOfTen = [maxScale + 1]int64{
	1, 10, 100, 1_000, 10_000, 100_000,
	1_000_000, 10_000_000, 100_000_000, 1_000_000_000,
}

func integerAmount(value int64) amount { return amount{mantissa: value} }

// amountFromValue reads a ScriptValue as an exact decimal. Integers, decimals and
// durations are all numbers; anything else is a content error the caller reports
// as a refusal, because guessing at a malformed operand is how a wrong damage
// number gets into the world quietly.
func amountFromValue(value Value) (amount, bool) {
	switch value.Kind {
	case ValueInteger:
		return integerAmount(value.Integer), true
	case ValueDecimal:
		if value.Scale < 0 || value.Scale > maxScale {
			return amount{}, false
		}
		return amount{mantissa: value.Mantissa, scale: value.Scale}, true
	case ValueDurationMS:
		if value.DurationMS > math.MaxInt64 {
			return amount{}, false
		}
		return integerAmount(int64(value.DurationMS)), true
	default:
		return amount{}, false
	}
}

// rescale raises an amount to a wider scale. It reports failure rather than
// overflowing, so a nonsense operand becomes a named refusal instead of a
// wrapped-around damage number.
func (value amount) rescale(scale int32) (amount, bool) {
	if scale < value.scale || scale > maxScale {
		return amount{}, false
	}
	factor := powersOfTen[scale-value.scale]
	scaled := value.mantissa * factor
	if factor != 0 && scaled/factor != value.mantissa {
		return amount{}, false
	}
	return amount{mantissa: scaled, scale: scale}, true
}

// align brings two amounts to a common scale so they can be added or compared.
func align(left, right amount) (amount, amount, bool) {
	scale := left.scale
	if right.scale > scale {
		scale = right.scale
	}
	alignedLeft, leftOK := left.rescale(scale)
	alignedRight, rightOK := right.rescale(scale)
	return alignedLeft, alignedRight, leftOK && rightOK
}

// mul multiplies two exact decimals. The product's scale is the sum of the
// operands' scales, then trailing zeroes are dropped so that a chain of scalers
// does not run the scale up to the cap on its own.
func (value amount) mul(other amount) (amount, bool) {
	if value.mantissa == 0 || other.mantissa == 0 {
		return amount{}, true
	}
	product := value.mantissa * other.mantissa
	if product/other.mantissa != value.mantissa {
		return amount{}, false
	}
	result := amount{mantissa: product, scale: value.scale + other.scale}.trim()
	if result.scale > maxScale {
		return amount{}, false
	}
	return result, true
}

// trim removes trailing decimal zeroes without changing the value, which keeps
// the scale of a long multiplication chain bounded.
func (value amount) trim() amount {
	for value.scale > 0 && value.mantissa%10 == 0 {
		value.mantissa /= 10
		value.scale--
	}
	if value.mantissa == 0 {
		value.scale = 0
	}
	return value
}

// compare returns -1, 0 or 1. It reports an error rather than a wrong answer when
// the operands cannot be aligned, because a health threshold that silently
// compares wrong is a mob that never dies or one that dies at full health.
func (value amount) compare(other amount) (int, bool) {
	left, right, ok := align(value, other)
	if !ok {
		return 0, false
	}
	switch {
	case left.mantissa < right.mantissa:
		return -1, true
	case left.mantissa > right.mantissa:
		return 1, true
	default:
		return 0, true
	}
}

func (value amount) isZero() bool { return value.mantissa == 0 }

func (value amount) String() string {
	if value.scale == 0 {
		return fmt.Sprintf("%d", value.mantissa)
	}
	divisor := powersOfTen[value.scale]
	whole := value.mantissa / divisor
	fraction := value.mantissa % divisor
	if fraction < 0 {
		fraction = -fraction
	}
	sign := ""
	if value.mantissa < 0 && whole == 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, value.scale, fraction)
}
