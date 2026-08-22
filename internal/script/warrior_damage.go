package script

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

// evalScaledPhysicalDamage resolves the ranged min/max damage used by the
// level-one Warrior AimedShot. It draws on the exact authored decimal grid and
// uses ADR 0036's replay-stable SHA-256 inputs instead of process randomness.
func evalScaledPhysicalDamage(
	ctx context.Context,
	evaluator *Evaluator,
	node *Node,
	frame Frame,
) error {
	minimum, err := literalAmount(node, frame, "minDamage")
	if err != nil {
		return err
	}
	maximum, err := literalAmount(node, frame, "maxDamage")
	if err != nil {
		return err
	}
	minimum, maximum, ok := align(minimum, maximum)
	if !ok || minimum.mantissa < 0 || maximum.mantissa < minimum.mantissa {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "minDamage and maxDamage do not form a non-negative representable range",
		}
	}
	rolled, ok := deterministicDamageAmount(frame, node.Key, minimum, maximum)
	if !ok {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "damage range is too wide to sample exactly",
		}
	}
	if frame.CasterID == "" {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the invocation names no caster to scale ranged damage for",
		}
	}
	factor, err := evaluator.scale(ctx, first(node.Nodes("scaler")), frame, frame.CasterID, "Ranged")
	if err != nil {
		return err
	}
	magnitude, ok := rolled.mul(factor)
	if !ok {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: fmt.Sprintf("damage %s times scale %s is not representable", rolled, factor),
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
	if err := evaluator.host.Apply(ctx, Command{
		Kind:             CommandDamage,
		EntityID:         frame.Addressee,
		Magnitude:        magnitude.decimal(),
		CanBeAvoided:     canBeAvoided,
		ThreatMultiplier: threat.decimal(),
		ExecutionKey:     frame.EvaluationID + "|" + node.Key,
	}); err != nil {
		return err
	}
	return evaluator.evalAll(ctx, node, "impactsOnHitTarget", frame)
}

func deterministicDamageAmount(
	frame Frame,
	nodeKey string,
	minimum amount,
	maximum amount,
) (amount, bool) {
	span := maximum.mantissa - minimum.mantissa
	if span < 0 || span == math.MaxInt64 {
		return amount{}, false
	}
	hasher := sha256.New()
	writeDamageHashPart(hasher, frame.PackID)
	writeDamageHashPart(hasher, frame.EvaluationID)
	writeDamageHashPart(hasher, nodeKey)
	var ordinal [8]byte
	binary.BigEndian.PutUint64(ordinal[:], frame.ActivationOrdinal)
	_, _ = hasher.Write(ordinal[:])
	sum := hasher.Sum(nil)
	draw := binary.BigEndian.Uint64(sum[:8])
	width := uint64(span) + 1
	return amount{mantissa: minimum.mantissa + int64(draw%width), scale: minimum.scale}, true
}

type damageHashWriter interface {
	Write([]byte) (int, error)
}

func writeDamageHashPart(writer damageHashWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}
