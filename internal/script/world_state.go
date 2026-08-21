package script

import (
	"context"
	"fmt"
)

// evalImpactTurnMob faces the current addressee toward an authored absolute
// destination. Retail limits this impact to a stationary, non-combat mob; the
// session host owns that runtime state and refuses any other entity.
func evalImpactTurnMob(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if err := requireOnlyFields(node, frame, "destination"); err != nil {
		return err
	}
	if frame.Addressee == "" {
		return worldStateRefusal(node, frame, "the invocation has no addressee to turn")
	}
	destinations := node.Nodes("destination")
	if len(destinations) != 1 {
		return worldStateRefusal(node, frame, fmt.Sprintf(
			"field \"destination\" must contain exactly one destination, found %d", len(destinations),
		))
	}
	destination, err := evaluator.ResolveDestination(ctx, destinations[0], frame)
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandTurnMob,
		EntityID:     frame.Addressee,
		Destination:  destination,
		ExecutionKey: frame.EvaluationID + "|" + node.Key,
	})
}

// evalImpactSummon validates the mob row and destination before it asks the
// host to mutate the world. Child impacts ride on the command because only the
// host learns the spawned entity id. The host re-enters this evaluator with
// that id as the addressee and rolls the spawn back if a child fails.
func evalImpactSummon(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if err := requireOnlyFields(node, frame, "destination", "impacts", "object"); err != nil {
		return err
	}
	object, ok := node.Field("object")
	if !ok || object.Kind != ValueRef || object.Ref.ID == "" || object.Ref.RowType != "mob" {
		return worldStateRefusal(node, frame, "field \"object\" is missing or is not a mob reference")
	}
	destinations := node.Nodes("destination")
	if len(destinations) != 1 {
		return worldStateRefusal(node, frame, fmt.Sprintf(
			"field \"destination\" must contain exactly one destination, found %d", len(destinations),
		))
	}
	impacts, err := strictNodeList(node, frame, "impacts")
	if err != nil {
		return err
	}
	destination, err := evaluator.ResolveDestination(ctx, destinations[0], frame)
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandSummon,
		Summon: &SummonCommand{
			Object: object.Ref, Destination: destination, Impacts: impacts, Frame: frame,
		},
		ExecutionKey: frame.EvaluationID + "|" + node.Key,
	})
}

func strictNodeList(node *Node, frame Frame, name string) ([]*Node, error) {
	value, ok := node.Field(name)
	if !ok {
		return nil, nil
	}
	if value.Kind != ValueList {
		return nil, worldStateRefusal(node, frame, fmt.Sprintf("field %q is not an impact list", name))
	}
	children := make([]*Node, 0, len(value.List))
	for index, entry := range value.List {
		if entry.Kind != ValueNode || entry.Node == nil {
			return nil, worldStateRefusal(node, frame, fmt.Sprintf(
				"field %q entry %d is not an impact node", name, index,
			))
		}
		children = append(children, entry.Node)
	}
	return children, nil
}

func requireOnlyFields(node *Node, frame Frame, names ...string) error {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	for _, field := range node.Fields {
		if _, ok := allowed[field.Name]; !ok {
			return worldStateRefusal(node, frame, fmt.Sprintf("field %q is unsupported", field.Name))
		}
	}
	return nil
}

func worldStateRefusal(node *Node, frame Frame, reason string) error {
	return &RefusedError{
		SourceID: frame.SourceID,
		NodeKey:  node.Key,
		Family:   node.Family,
		Opcode:   node.Opcode,
		Reason:   reason,
	}
}
