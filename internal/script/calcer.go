package script

import (
	"context"
	"fmt"
)

// calc evaluates a calcer or basic-element operand to an exact number.
//
// ADR 0036's amendment adds these two families for a concrete reason:
// HealthTrigger is in the implemented tier but its healthOn operand is
// gameMechanics.elements.calcers.FullHealthCalcer and its healthOff operand is
// gameMechanics.constructor.basicElements.FloatZero. Neither had a tier, both
// defaulted to refused, and the first rat death in quest 1-30 failed the build.
//
// Operands are numbers, not impacts, so they return a value rather than running.
func (evaluator *Evaluator) calc(ctx context.Context, node *Node, frame Frame) (amount, error) {
	if node == nil {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: frame.SourceID,
			Family: FamilyCalcer, Opcode: "",
			Reason: "a numeric operand is missing",
		}
	}

	tier := node.Tier
	if tier == TierInert && evaluator.options.StrictInert {
		tier = TierRefused
	}
	evaluator.census.record(tier, node.Opcode)
	if tier != TierImplemented {
		// An inert operand is not admissible even in principle: a threshold that
		// silently reads zero is a mob that dies at full health. The inert tier's
		// admission test is that omission cannot alter authoritative state, and
		// omitting a number always can.
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "a numeric operand must be implemented; no operand may be inert",
		}
	}

	switch node.Opcode {
	case "FullHealthCalcer":
		return evaluator.fullHealth(ctx, node, frame)

	case "FloatZero":
		// basicElements.FloatZero carries no children. It is the constant zero,
		// and it is HealthTrigger's healthOff operand in RatKiller.
		return amount{}, nil

	case "FloatData":
		// The other calcer in the bounded reach set at 15 uses; the three
		// VariableResource documents use it for their initial values.
		return literalAmount(node, frame, "value")

	default:
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no calcer handler registered",
		}
	}
}

// fullHealth is FullHealthCalcer: the addressee's full health times a
// multiplier. The multiplier is what makes it a threshold family rather than a
// constant — RatKiller uses multiplier 0, which is "dead", and the same node
// with a multiplier of one half would be "wounded".
//
// The host answers with full health and never with a threshold, so the tier
// table and not the host decides what the number means.
func (evaluator *Evaluator) fullHealth(ctx context.Context, node *Node, frame Frame) (amount, error) {
	multiplier := amount{mantissa: 1}
	if value, ok := node.Field("multiplier"); ok {
		parsed, parsedOK := amountFromValue(value)
		if !parsedOK {
			return amount{}, &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "field \"multiplier\" is not an exact number",
			}
		}
		multiplier = parsed
	}

	// A zero multiplier is the whole of shape B, and it needs no host round trip:
	// zero times any full health is zero. Skipping the query keeps a dying mob's
	// death test independent of whether the host can still answer for it.
	if multiplier.isZero() {
		return amount{}, nil
	}

	answer, err := evaluator.host.Query(ctx, Query{
		Kind: QueryMaxHealth, EntityID: frame.Addressee,
	})
	if err != nil {
		return amount{}, fmt.Errorf("query max health for %s: %w", frame.Addressee, err)
	}
	full, ok := amountFromValue(answer)
	if !ok {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the host answered max health with a non-numeric value",
		}
	}

	product, ok := full.mul(multiplier)
	if !ok {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: fmt.Sprintf("full health %s times multiplier %s is not representable", full, multiplier),
		}
	}
	return product, nil
}

// literalAmount reads a node's numeric field, defaulting to zero when the field
// is absent, and refusing when it is present but not a number.
func literalAmount(node *Node, frame Frame, name string) (amount, error) {
	value, ok := node.Field(name)
	if !ok {
		return amount{}, nil
	}
	parsed, parsedOK := amountFromValue(value)
	if !parsedOK {
		return amount{}, &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: fmt.Sprintf("field %q is not an exact number", name),
		}
	}
	return parsed, nil
}
