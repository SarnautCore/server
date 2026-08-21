package script

import (
	"context"
	"fmt"
)

// An addressee finder names whom a node applies to, as a field on that node
// rather than as an impact of its own. In Mechanics/Spells/Warrior it appears
// twice, both times under the field name addresseeFinder and both times as an
// empty element:
//
//	Warrior/Entrapment/Spell01.xdb, inside an ImpactSetTarget:
//	  <addresseeFinder type="gameMechanics.elements.addresseeFinders.AddresseeFinderCaster" />
//	Warrior/Entrapment/Buff01.xdb, inside an AbonentLostWatcher effect:
//	  <addresseeFinder type="gameMechanics.elements.addresseeFinders.AddresseeFinderCaster" />
//
// ADR 0036 counted three finders, which is right for the 570-document tutorial
// scope and stops being right the moment spells land; the amendment adds this
// fourth one.

// resolveFinder resolves a node's addresseeFinder field to an entity.
//
// The field names an entity the node refers to, not the entity the node applies
// to. Both Warrior sites read that way: ImpactSetTarget carries the finder as
// its only field, so the finder has to be the value being set rather than the
// recipient — an impact whose sole field said "apply to the caster" would have
// nothing left to say what to set. AbonentLostWatcher reads the same, naming
// whose disconnection to watch for.
//
// A node with no finder falls back to the frame's addressee, which is how the
// data spells the common case: the field is absent far more often than present.
func (evaluator *Evaluator) resolveFinder(ctx context.Context, node *Node, frame Frame) (string, error) {
	finder := first(node.Nodes("addresseeFinder"))
	if finder == nil {
		return frame.Addressee, nil
	}
	return evaluator.resolveFinderNode(ctx, finder, frame)
}

func (evaluator *Evaluator) resolveFinderNode(
	ctx context.Context, finder *Node, frame Frame,
) (string, error) {

	run, err := evaluator.admit(finder, frame, "addressee finder is outside the M3 implemented tier")
	if err != nil {
		return "", err
	}
	if !run {
		// An inert finder would silently redirect a node onto the wrong entity,
		// which is the one thing a finder exists to get right.
		return "", &RefusedError{
			SourceID: frame.SourceID, NodeKey: finder.Key,
			Family: finder.Family, Opcode: finder.Opcode,
			Reason: "an addressee finder must be implemented; no finder may be inert",
		}
	}

	var found string
	switch finder.Opcode {
	case "AddresseeFinderCaster":
		found = frame.CasterID
	case "AddresseeFinderSelf":
		found = frame.Addressee
	case "AddresseeFinderTarget":
		found = frame.TargetID
	case "AddresseeFinderSingleMob":
		locator, err := requiredLocator(finder, frame, "mob")
		if err != nil {
			return "", err
		}
		entities, err := evaluator.host.Resolve(ctx, ResolveRequest{
			Finder: finder.Opcode, Frame: frame, Locator: &locator,
		})
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", finder.Opcode, err)
		}
		if len(entities) != 1 {
			return "", &RefusedError{
				SourceID: frame.SourceID, NodeKey: finder.Key,
				Family: finder.Family, Opcode: finder.Opcode,
				Reason: fmt.Sprintf("expected exactly one live entity, found %d", len(entities)),
			}
		}
		found = entities[0]
	default:
		return "", &RefusedError{
			SourceID: frame.SourceID, NodeKey: finder.Key,
			Family: finder.Family, Opcode: finder.Opcode,
			Reason: "no addressee finder handler registered",
		}
	}

	if found == "" {
		return "", &RefusedError{
			SourceID: frame.SourceID, NodeKey: finder.Key,
			Family: finder.Family, Opcode: finder.Opcode,
			Reason: fmt.Sprintf("the invocation names no entity for %s to find", finder.Opcode),
		}
	}
	return found, nil
}

// evalImpactSetTarget points the addressee's current target at the entity the
// finder names. It is the site where AddresseeFinderCaster actually appears,
// which is why it is implemented alongside the finder rather than left to the
// round that does the rest of the Warrior kit: a finder with no caller would be
// untested code.
//
// Warrior/Entrapment/Spell01.xdb runs it from impactsOnAttach of a buff placed
// on the spell's target, with the finder naming the caster — so the victim's
// target becomes the warrior. That is a taunt, which is what Entrapment is.
//
// The direction is inferred rather than quoted, and it is inferred from the node
// having exactly one field. It rests on one ability, so it is flagged as an open
// question rather than written into the spec as fact.
func evalImpactSetTarget(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	target, err := evaluator.resolveFinder(ctx, node, frame)
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandSetTarget,
		EntityID:     frame.Addressee,
		TargetID:     target,
		ExecutionKey: frame.EvaluationID + "|" + node.Key,
	})
}
