package script

import "testing"

func TestDeterministicDamageAmountReplaysAndStaysInAuthoredRange(t *testing.T) {
	frame := Frame{
		PackID: "pack.inst-league1", EvaluationID: "action|17|action.warrior.aimed-shot|9",
		ActivationOrdinal: 4,
	}
	minimum := amount{mantissa: 1481, scale: 2}
	maximum := amount{mantissa: 1810, scale: 2}
	first, ok := deterministicDamageAmount(frame, "aimed-shot/targetImpacts[0]", minimum, maximum)
	if !ok {
		t.Fatal("deterministicDamageAmount() refused an authored range")
	}
	second, ok := deterministicDamageAmount(frame, "aimed-shot/targetImpacts[0]", minimum, maximum)
	if !ok || first != second {
		t.Fatalf("replayed damage = %+v, %v, want %+v", second, ok, first)
	}
	if first.mantissa < minimum.mantissa || first.mantissa > maximum.mantissa || first.scale != 2 {
		t.Fatalf("sampled damage = %+v, want exact [14.81,18.10] grid", first)
	}

	frame.ActivationOrdinal++
	third, ok := deterministicDamageAmount(frame, "aimed-shot/targetImpacts[0]", minimum, maximum)
	if !ok || third == first {
		t.Fatalf("next activation damage = %+v, %v, want a distinct deterministic draw", third, ok)
	}
}
