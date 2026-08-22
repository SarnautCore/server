package script

import (
	"context"
	"fmt"
)

// Scalers are where combat's numbers come from, and the data says what shape
// they have. Every fact in this file is quoted from the classic 1.1.02.0 tree,
// read-only, with the path named.
//
// The decisive observation is that the per-opcode scalers carry no fields at
// all. In Mechanics/Spells/AutoAttack/MeleeDamage.xdb the cooldown reads
//
//	<duration>1</duration>
//	<scaler type="gameMechanics.elements.scalers.WeaponSpeedScaler" />
//
// and the target impact reads
//
//	<Item type="gameMechanics.elements.impacts.ScaledPhysicalWeaponDamage">
//	  <canBeAvoided>true</canBeAvoided>
//	  <threatMultiplier>1</threatMultiplier>
//	  <scaler type="gameMechanics.elements.scalers.PhysicalScaler" />
//	  <avgDamage>8.75</avgDamage>
//	  <source>Mainhand</source>
//	</Item>
//
// RangedDamage.xdb is the same document with PhysicalRangedScaler, source
// Ranged, and the same avgDamage of 8.75.
//
// PhysicalScaler, PhysicalRangedScaler and TrivialScaler carry no fields at all,
// in every one of their 173, 40 and 1151 uses. WeaponSpeedScaler carries one
// optional source. The only scaler in the Warrior surface that carries a
// coefficient is LinearEffectScaler, with <coeff>0.5</coeff> in
// Warrior/Retreat/Buff01.xdb.
//
// So a scaler is not a formula stored in content. It is an opcode naming which
// composite character statistic multiplies a base magnitude that the enclosing
// node supplies. The reflection schema in Types/types.xml names the composition
// outright — PhysicalScaler is described as "Scaler 3in1: WeaponDamageBaseScaler
// -> StrengthDPSScaler -> WeaponSpeedBaseScaler", and PhysicalRangedScaler as
// the same chain over the ranged weapon — but every term in that chain is a
// character statistic the host owns. A cooldown of 1 scaled by weapon speed is
// "one swing"; an avgDamage of 8.75 scaled by PhysicalScaler is the auto-attack.
//
// That is why ADR 0036's amendment tiers scalers per opcode: the opcode is the
// entire content of the node, so a blanket "non-trivial scalers are refused"
// rule refuses the only three opcodes the Warrior kit has.
//
// The arithmetic itself stays out of this package. The host answers with a
// multiplier and the combat hook applies the result, which is ADR 0036's
// LifeGuard pre-commitment: if a scaler handler here ever wants internal/combat,
// the seam is wrong and the change stops.

// scaleQueries maps a scaler opcode to the typed question the host answers. The
// mapping lives here rather than in the host because ADR 0036 is explicit that
// the host never receives an opcode and never decides a coverage tier.
var scaleQueries = map[string]QueryKind{
	"PhysicalScaler":       QueryPhysicalScale,
	"PhysicalRangedScaler": QueryPhysicalRangedScale,
	"WeaponSpeedScaler":    QueryWeaponSpeedScale,
}

// scale evaluates a scaler node to the multiplier it contributes, for the
// entity and equipment slot the enclosing node named.
func (evaluator *Evaluator) scale(
	ctx context.Context, node *Node, frame Frame, entity, slot string,
) (amount, error) {
	if node == nil {
		// A node with no scaler is unscaled, not zero. Treating a missing scaler
		// as zero would silently turn an impact into a no-op.
		return integerAmount(1), nil
	}

	run, err := evaluator.admit(node, frame, "scaler is outside the M3 implemented tier")
	if err != nil {
		return amount{}, err
	}
	if !run {
		// Same reasoning as a calcer operand: a number cannot be inert. Omitting
		// a multiplier changes the damage, and the inert tier's admission test is
		// that omission cannot alter authoritative state.
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "a scaler must be implemented; no scaler may be inert",
		}
	}

	if node.Opcode == "TrivialScaler" {
		// The identity. WarriorKania/Buff01.xdb and WarriorGibberling/Buff01.xdb
		// both use it as an empty element, and an identity needs no host call.
		return integerAmount(1), nil
	}

	kind, ok := scaleQueries[node.Opcode]
	if !ok {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no scaler handler registered",
		}
	}

	// WeaponSpeedScaler is the one per-opcode scaler with a field of its own: an
	// optional source naming which weapon's speed to read, defaulting to
	// Mainhand. Where it is present it wins over the enclosing node's slot,
	// because it is the more specific statement.
	if own, ok := node.Field("source"); ok && own.Kind == ValueText {
		slot = own.Text
	}
	if slot == "" {
		// The AttackSource enum has two members and Mainhand is its default.
		slot = "Mainhand"
	}

	answer, err := evaluator.host.Query(ctx, Query{Kind: kind, EntityID: entity, Slot: slot})
	if err != nil {
		return amount{}, fmt.Errorf("query scale for %s: %w", entity, err)
	}
	factor, ok := amountFromValue(answer)
	if !ok {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the host answered a scale query with a non-numeric value",
		}
	}
	return factor, nil
}

// evalScaledPhysicalWeaponDamage is the auto-attack. Both
// Mechanics/Spells/AutoAttack documents are one of these in targetImpacts, so
// this handler is every point of white damage a Warrior produces.
//
// The magnitude is avgDamage times the scaler's multiplier, computed with exact
// decimal arithmetic and handed to the host unrounded: rounding is a combat
// decision and belongs behind the combat hook.
//
// The caster is who swings and the addressee is who is hit, so the scale query
// asks about the caster's weapon while the damage command names the addressee.
// Getting that backwards would scale a player's swing by the rat's weapon.
func evalScaledPhysicalWeaponDamage(
	ctx context.Context, evaluator *Evaluator, node *Node, frame Frame,
) error {
	base, err := literalAmount(node, frame, "avgDamage")
	if err != nil {
		return err
	}

	// source names which weapon the damage comes from. It is an AttackSource,
	// whose whole enum is Mainhand and Ranged, and the scaler always agrees with
	// it: PhysicalScaler with Mainhand, PhysicalRangedScaler with Ranged.
	//
	// Note the spelling. AttackSource is Mainhand; the DressSlot enum that
	// EquipTrigger reads is MAINHAND, and it has thirty members including
	// TWOHANDED and DUALWIELD. The content uses two vocabularies for two related
	// ideas, and the host adapter is where they are reconciled, not here.
	slot := ""
	if value, ok := node.Field("source"); ok && value.Kind == ValueText {
		slot = value.Text
	}

	if frame.CasterID == "" {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the invocation names no caster to scale the weapon of",
		}
	}
	factor, err := evaluator.scale(ctx, first(node.Nodes("scaler")), frame, frame.CasterID, slot)
	if err != nil {
		return err
	}

	magnitude, ok := base.mul(factor)
	if !ok {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: fmt.Sprintf("damage %s times scale %s is not representable", base, factor),
		}
	}

	threat := integerAmount(1)
	if _, ok := node.Field("threatMultiplier"); ok {
		threat, err = literalAmount(node, frame, "threatMultiplier")
		if err != nil {
			return err
		}
	}

	canBeAvoided := false
	if value, ok := node.Field("canBeAvoided"); ok && value.Kind == ValueBool {
		canBeAvoided = value.Bool
	}

	err = evaluator.host.Apply(ctx, Command{
		Kind:             CommandDamage,
		EntityID:         frame.Addressee,
		Magnitude:        magnitude.decimal(),
		CanBeAvoided:     canBeAvoided,
		ThreatMultiplier: threat.decimal(),
		ExecutionKey:     frame.EvaluationID + "|" + node.Key,
	})
	if err != nil {
		return err
	}

	// ADR 0036 keeps a damage node's on-hit children in order, and MarkedImpact
	// is the one the tutorial and the Warrior kit both use.
	return evaluator.evalAll(ctx, node, "impactsOnHitTarget", frame)
}

func (value amount) decimal() Decimal {
	return Decimal{Mantissa: value.mantissa, Scale: value.scale}
}
