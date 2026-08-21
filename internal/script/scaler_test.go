package script_test

import (
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

const enemyID = "mob.inst-league1.ruffian"

// meleeAutoAttack reproduces the targetImpacts of
// Mechanics/Spells/AutoAttack/MeleeDamage.xdb, which is the entire melee
// auto-attack: an avgDamage of 8.75 from the Mainhand, scaled by PhysicalScaler,
// avoidable, at a threat multiplier of 1.
func meleeAutoAttack(scalerOpcode, source string) *script.Node {
	return impact("autoattack/melee/targetImpacts[0]", "ScaledPhysicalWeaponDamage",
		field("avgDamage", decimal(875, 2)),
		field("canBeAvoided", script.Value{Kind: script.ValueBool, Bool: true}),
		field("scaler", node(scaler("autoattack/melee/targetImpacts[0]/scaler", scalerOpcode))),
		field("source", text(source)),
		field("threatMultiplier", integer(1)),
	)
}

// TestTheAutoAttackScalesItsAuthoredDamage is the Warrior damage path end to
// end. Both auto-attack documents are one ScaledPhysicalWeaponDamage in
// targetImpacts, so this handler and this scaler are every point of white damage
// a Warrior produces — and under ADR 0036 before its amendment, the first swing
// reached a refused node and failed the build.
//
// The magnitude assertion is exact and stays a decimal. 8.75 times a scale of 2
// is 17.5, not 17 and not 17.500001: the interpreter multiplies exact decimals
// and hands the result to the combat hook unrounded, because rounding is a
// combat decision.
func TestTheAutoAttackScalesItsAuthoredDamage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		opcode string
		source string
		// want is the rendered magnitude: authored 8.75 times the fake host's
		// scale for that opcode.
		want string
	}{
		{name: "melee, scaled by PhysicalScaler", opcode: "PhysicalScaler", source: "Mainhand", want: "17.5"},
		{name: "ranged, scaled by PhysicalRangedScaler", opcode: "PhysicalRangedScaler", source: "Ranged", want: "13.125"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			frame := newFrame()
			frame.SourceID = "spell.autoattack.melee"
			// The player swings at a mob: the caster is who scales, the
			// addressee is who is hit.
			frame.Addressee = enemyID
			frame.TargetID = enemyID

			if _, err := run(host, meleeAutoAttack(testCase.opcode, testCase.source), frame); err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}

			if len(host.trace) != 2 {
				t.Fatalf("host trace = %v, want a scale query then a damage command", host.trace)
			}
			// The scale query must ask about the caster and the caster's slot.
			// Asking the addressee would scale the player's swing by the rat's
			// weapon, which is the kind of mistake that looks like a balance
			// problem rather than a bug.
			if !strings.Contains(host.trace[0], playerID) ||
				!strings.Contains(host.trace[0], "slot="+testCase.source) {
				t.Errorf(
					"scale query = %q, want it to ask about %s in slot %s",
					host.trace[0], playerID, testCase.source,
				)
			}
			want := "apply damage " + testCase.want + " on " + enemyID +
				" avoidable=true threat=1 key=eval-1|autoattack/melee/targetImpacts[0]"
			if host.trace[1] != want {
				t.Errorf("damage command = %q, want %q", host.trace[1], want)
			}
		})
	}
}

// TestTrivialScalerNeedsNoHostRoundTrip pins the identity. TrivialScaler is
// fieldless in all 1151 of its uses and was the only scaler in ADR 0036's
// original implemented tier; it must stay free.
func TestTrivialScalerNeedsNoHostRoundTrip(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	frame := newFrame()
	frame.Addressee = enemyID

	node := impact("trivial", "ScaledPhysicalWeaponDamage",
		field("avgDamage", decimal(875, 2)),
		field("scaler", node(scaler("trivial/scaler", "TrivialScaler"))),
		field("source", text("Mainhand")),
	)

	if _, err := run(host, node, frame); err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	assertTrace(t, host.trace, []string{
		"apply damage 8.75 on " + enemyID +
			" avoidable=false threat=1 key=eval-1|trivial",
	})
}

// TestAnUntieredScalerIsRefusedRatherThanTreatedAsOne is the amendment's rule
// read back. A scaler now carries a tier per opcode, and an untiered one is
// refused — because the alternative, quietly scaling by 1, produces a damage
// number that is wrong in a way no test would notice.
func TestAnUntieredScalerIsRefusedRatherThanTreatedAsOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		tier   script.Tier
		reason string
	}{
		{name: "a refused scaler", tier: script.TierRefused, reason: "outside the M3 implemented tier"},
		{name: "an inert scaler", tier: script.TierInert, reason: "no scaler may be inert"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost()
			frame := newFrame()
			frame.Addressee = enemyID

			// LinerMultiplierScaler is spelled that way in the content, missing
			// its "a". It is in the amendment's implemented tier and this build
			// has no handler for it.
			unsupported := scaler("aimed/scaler", "LinerMultiplierScaler")
			unsupported.Tier = testCase.tier
			node := impact("aimed", "ScaledPhysicalWeaponDamage",
				field("avgDamage", decimal(875, 2)),
				field("scaler", node(unsupported)),
			)

			_, err := run(host, node, frame)
			if err == nil {
				t.Fatal("Evaluate() error = nil, want a refusal rather than a silent scale of 1")
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Errorf("error = %q, want it to say %q", err, testCase.reason)
			}
			for _, line := range host.trace {
				if strings.HasPrefix(line, "apply damage") {
					t.Errorf("host trace = %v, want no damage applied", host.trace)
				}
			}
		})
	}
}

// TestAddresseeFinderCasterNamesTheEntityTheNodeRefersTo covers the fourth
// addressee finder, at the site where it actually appears.
//
// Warrior/Entrapment/Spell01.xdb runs ImpactSetTarget from impactsOnAttach of a
// buff placed on the spell's target, and the finder names the caster. The finder
// is the only field on the node, so it has to be the value being set rather than
// the recipient: the victim's target becomes the warrior, which is a taunt, and
// Entrapment is a taunt.
func TestAddresseeFinderCasterNamesTheEntityTheNodeRefersTo(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	frame := newFrame()
	frame.Addressee = enemyID
	frame.TargetID = enemyID

	setTarget := impact("entrapment/impactsOnAttach[0]", "ImpactSetTarget",
		field("addresseeFinder", node(&script.Node{
			Key:    "entrapment/impactsOnAttach[0]/addresseeFinder",
			Family: script.FamilyFinder, Opcode: "AddresseeFinderCaster",
			Tier: script.TierImplemented,
		})),
	)

	if _, err := run(host, setTarget, frame); err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	assertTrace(t, host.trace, []string{
		"apply set-target " + enemyID + " -> " + playerID +
			" key=eval-1|entrapment/impactsOnAttach[0]",
	})
}
