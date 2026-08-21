package script

import (
	"context"
	"fmt"
)

// ResolveDestination validates and resolves a DestinationLocator to its
// absolute map position. It is a value operation: the parent impact decides
// whether to summon, turn, teleport, or do something else with the result.
func (evaluator *Evaluator) ResolveDestination(
	ctx context.Context, node *Node, frame Frame,
) (Destination, error) {
	if !evaluator.options.Enabled {
		return Destination{}, ErrDisabled
	}
	if node == nil {
		return Destination{}, destinationRefusal(nil, frame, "destination is missing")
	}
	run, err := evaluator.admit(node, frame, "destination is outside the M3 implemented tier")
	if err != nil || !run {
		return Destination{}, err
	}
	if node.Opcode != "DestinationLocator" {
		return Destination{}, destinationRefusal(node, frame, "no destination handler registered")
	}

	locators := node.Nodes("locator")
	if len(locators) != 1 || locators[0].Family != FamilyBasic || locators[0].Opcode != "Struct" {
		return Destination{}, destinationRefusal(
			node, frame, fmt.Sprintf("field \"locator\" must contain exactly one Struct, found %d", len(locators)),
		)
	}
	locator := locators[0]
	mapValue, ok := locator.Field("map")
	if !ok || mapValue.Kind != ValueRef || mapValue.Ref.ID == "" || mapValue.Ref.RowType != "map" {
		return Destination{}, destinationRefusal(
			node, frame, "locator.map is missing or is not a product map reference",
		)
	}
	scriptID, ok := locator.Field("scriptID")
	if !ok || scriptID.Kind != ValueText || scriptID.Text == "" {
		return Destination{}, destinationRefusal(
			node, frame, "locator.scriptID is missing or empty",
		)
	}

	yaw := integerAmount(0)
	if value, ok := node.Field("yaw"); ok {
		parsed, parsedOK := amountFromValue(value)
		if !parsedOK {
			return Destination{}, destinationRefusal(node, frame, "field \"yaw\" is not an exact number")
		}
		yaw = parsed
	}

	resolved, err := evaluator.host.Locate(ctx, DestinationRequest{
		Map: mapValue.Ref, ScriptID: scriptID.Text, ZoneID: frame.ZoneID,
	})
	if err != nil {
		return Destination{}, fmt.Errorf(
			"resolve map locator %s/%s: %w", mapValue.Ref.ID, scriptID.Text, err,
		)
	}
	if resolved.Map.ID == "" {
		resolved.Map = mapValue.Ref
	}
	if resolved.Map.ID != mapValue.Ref.ID {
		return Destination{}, destinationRefusal(
			node, frame, fmt.Sprintf(
				"locator resolved on map %q, want %q", resolved.Map.ID, mapValue.Ref.ID,
			),
		)
	}
	resolved.Yaw = yaw.decimal()
	return resolved, nil
}

func destinationRefusal(node *Node, frame Frame, reason string) error {
	key := frame.SourceID
	family := FamilyImpact
	opcode := "DestinationLocator"
	if node != nil {
		key, family, opcode = node.Key, node.Family, node.Opcode
	}
	return &RefusedError{
		SourceID: frame.SourceID, NodeKey: key, Family: family, Opcode: opcode, Reason: reason,
	}
}
