package script

import (
	"context"
	"fmt"
)

// This file contains the authored quest opcodes that translate directly into
// host work. Control flow, triggers, combat scaling and summon choreography
// remain in their focused files.

func evalGiveItem(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	item, err := requiredRef(node, frame, "item", "item")
	if err != nil {
		return err
	}
	count := int64(1)
	if value, ok := node.Field("count"); ok {
		if value.Kind != ValueInteger || value.Integer <= 0 {
			return nodeRefusal(node, frame, "field \"count\" must be a positive integer")
		}
		count = value.Integer
	}
	isCursed := false
	if value, ok := node.Field("isCursed"); ok {
		if value.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"isCursed\" must be boolean")
		}
		isCursed = value.Bool
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandGiveItem, EntityID: frame.Addressee, Ref: item, Count: count,
		IsCursed:     isCursed,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalClientData(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	data, err := requiredRef(node, frame, "data", "")
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandClientData, EntityID: frame.Addressee, Ref: data,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalClientDataCoords(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	data, err := requiredRef(node, frame, "data", "")
	if err != nil {
		return err
	}
	destinations, err := resolveLocatorList(ctx, evaluator, node, frame, "locators")
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		return nodeRefusal(node, frame, "field \"locators\" must contain at least one map pointer")
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandClientDataCoords, EntityID: frame.Addressee, Ref: data,
		Destinations: destinations, ExecutionKey: executionKey(frame, node),
	})
}

func evalBuffCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if node.Opcode == "BuffAttacher" {
		if err := requireOnlyFields(node, frame,
			"buff", "durationScaler", "durationScalerTarget", "scalerTarget",
		); err != nil {
			return err
		}
		for _, field := range []string{"durationScaler", "durationScalerTarget", "scalerTarget"} {
			if err := admitTrivialScaler(evaluator, node, frame, field); err != nil {
				return err
			}
		}
	} else if err := requireOnlyFields(node, frame, "buff", "checkCaster"); err != nil {
		return err
	}
	buff, err := requiredRef(node, frame, "buff", "")
	if err != nil {
		return err
	}
	kind := CommandAttachBuff
	if node.Opcode == "BuffDetacher" {
		kind = CommandDetachBuff
	}
	checkCaster := true
	if value, ok := node.Field("checkCaster"); ok {
		if value.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"checkCaster\" must be boolean")
		}
		checkCaster = value.Bool
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, Ref: buff, Bool: checkCaster,
		ExecutionKey: executionKey(frame, node),
	})
}

func admitTrivialScaler(evaluator *Evaluator, owner *Node, frame Frame, field string) error {
	children := owner.Nodes(field)
	if len(children) == 0 {
		return nil
	}
	if len(children) != 1 {
		return nodeRefusal(owner, frame, fmt.Sprintf("field %q must contain one scaler", field))
	}
	scaler := children[0]
	run, err := evaluator.admit(scaler, frame, "buff duration scaler is outside the implemented tier")
	if err != nil {
		return err
	}
	if !run || scaler.Opcode != "TrivialScaler" || len(scaler.Fields) != 0 {
		return nodeRefusal(owner, frame, fmt.Sprintf("field %q must be an empty TrivialScaler", field))
	}
	return nil
}

func evalReferenceCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	specs := map[string]struct {
		field string
		kind  CommandKind
	}{
		"AttachAbility":     {"ability", CommandAttachAbility},
		"ImpactMobChat":     {"msg", CommandMobChat},
		"SpawnTableObjects": {"table", CommandSpawnTableObjects},
		"ResetSpawnTable":   {"table", CommandResetSpawnTable},
	}
	spec, ok := specs[node.Opcode]
	if !ok {
		return nodeRefusal(node, frame, "no reference-command translation is registered")
	}
	ref, err := requiredRef(node, frame, spec.field, "")
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: spec.kind, EntityID: frame.Addressee, Ref: ref,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalSpawnSingle(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	field := "mob"
	kind := CommandSpawnSingleMob
	if node.Opcode == "SpawnSingleDevice" {
		field, kind = "device", CommandSpawnSingleDevice
	}
	locator, err := requiredLocator(node, frame, field)
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, Locator: &locator,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalEntityCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	kinds := map[string]CommandKind{
		"ImpactClearTarget":  CommandClearTarget,
		"ImpactStopTalk":     CommandStopTalk,
		"ImpactKill":         CommandKill,
		"ImpactDisintegrate": CommandDisintegrate,
		"DeviceDie":          CommandDeviceDie,
	}
	kind, ok := kinds[node.Opcode]
	if !ok {
		return nodeRefusal(node, frame, "no entity-command translation is registered")
	}
	if len(node.Fields) != 0 {
		return nodeRefusal(node, frame, "this impact accepts no fields")
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, ExecutionKey: executionKey(frame, node),
	})
}

func evalScalarCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	field := "value"
	kind := CommandActivateAggro
	optional := false
	if node.Opcode == "ImpactDeviceDisintergrate" {
		field, kind, optional = "delay", CommandDeviceDisintegrate, true
	}
	count := int64(0)
	if value, ok := node.Field(field); ok {
		var parsed bool
		count, parsed = exactInteger(value)
		if !parsed || count < 0 {
			return nodeRefusal(node, frame, fmt.Sprintf("field %q must be a non-negative exact integer", field))
		}
	} else if !optional {
		return nodeRefusal(node, frame, fmt.Sprintf("field %q is missing", field))
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, Count: count,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalTextCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	field := "switchType"
	kind := CommandDoorSwitch
	if node.Opcode == "ImpactDeviceSetVisualState" {
		field, kind = "visualState", CommandDeviceVisualState
	}
	value, ok := node.Field(field)
	if !ok {
		return nodeRefusal(node, frame, fmt.Sprintf("field %q is missing", field))
	}
	command := Command{Kind: kind, EntityID: frame.Addressee, ExecutionKey: executionKey(frame, node)}
	switch value.Kind {
	case ValueText:
		if value.Text == "" {
			return nodeRefusal(node, frame, fmt.Sprintf("field %q is empty", field))
		}
		command.Text = value.Text
	case ValueInteger:
		command.Count = value.Integer
	default:
		return nodeRefusal(node, frame, fmt.Sprintf("field %q must be text or integer", field))
	}
	return evaluator.host.Apply(ctx, command)
}

func evalAddExperience(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	level, err := requiredInteger(node, frame, "mobLevel", 1)
	if err != nil {
		return err
	}
	count, err := requiredInteger(node, frame, "mobCount", 1)
	if err != nil {
		return err
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandAddExperience, EntityID: frame.Addressee,
		Count: count, OtherCount: level,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalScriptZoneDisabled(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	zone, err := requiredRef(node, frame, "zone", "")
	if err != nil {
		return err
	}
	disabled, ok := node.Field("disable")
	if !ok || disabled.Kind != ValueBool {
		return nodeRefusal(node, frame, "field \"disable\" is missing or is not boolean")
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandSetScriptZoneDisabled, EntityID: frame.Addressee,
		Ref: zone, Bool: disabled.Bool, ExecutionKey: executionKey(frame, node),
	})
}

func evalScriptZoneVariable(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	zone, err := requiredRef(node, frame, "zone", "")
	if err != nil {
		return err
	}
	variable, err := requiredRef(node, frame, "variable", "")
	if err != nil {
		return err
	}
	summand, err := requiredInteger(node, frame, "summand", -1<<62)
	if err != nil {
		return err
	}
	reset := false
	if value, ok := node.Field("reset"); ok {
		if value.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"reset\" must be boolean")
		}
		reset = value.Bool
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandAddScriptZoneVariable, EntityID: frame.Addressee,
		Ref: zone, OtherRef: variable, Count: summand, Bool: reset,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalDestinationCommand(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	locator, err := requiredLocator(node, frame, "locator")
	if err != nil {
		return err
	}
	destination, err := evaluator.host.Locate(ctx, DestinationRequest{
		Map: locator.Map, ScriptID: locator.ScriptID, ZoneID: frame.ZoneID,
	})
	if err != nil {
		return fmt.Errorf("resolve map locator %s/%s: %w", locator.Map.ID, locator.ScriptID, err)
	}
	if yaw, ok := node.Field("yaw"); ok {
		amount, valid := amountFromValue(yaw)
		if !valid {
			return nodeRefusal(node, frame, "field \"yaw\" is not an exact number")
		}
		destination.Yaw = amount.decimal()
	}
	running := false
	if value, ok := node.Field("runningMode"); ok {
		if value.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"runningMode\" must be boolean")
		}
		running = value.Bool
	}
	kind := CommandGoTo
	if node.Opcode == "ImpactTeleportLoc" {
		kind = CommandTeleport
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, Destination: destination, Bool: running,
		ExecutionKey: executionKey(frame, node),
	})
}

func evalGoThroughPath(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	destinations, err := resolveLocatorList(ctx, evaluator, node, frame, "path")
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		return nodeRefusal(node, frame, "field \"path\" must contain at least one map pointer")
	}
	running := false
	if value, ok := node.Field("runningMode"); ok {
		if value.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"runningMode\" must be boolean")
		}
		running = value.Bool
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: CommandGoThroughPath, EntityID: frame.Addressee,
		Destinations: destinations, Bool: running, ExecutionKey: executionKey(frame, node),
	})
}

func evalLocatedEntities(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	field := "device"
	if node.Opcode == "ImpactFindSingleMob" {
		field = "mob"
	}
	locator, err := requiredLocator(node, frame, field)
	if err != nil {
		return err
	}
	entities, err := evaluator.host.Resolve(ctx, ResolveRequest{
		Finder: node.Opcode, Frame: frame, Locator: &locator,
	})
	if err != nil {
		return fmt.Errorf("resolve %s %s/%s: %w", node.Opcode, locator.Map.ID, locator.ScriptID, err)
	}
	for _, entityID := range entities {
		scoped := frame
		scoped.Addressee = entityID
		if err := evaluator.evalAll(ctx, node, "impacts", scoped); err != nil {
			return err
		}
	}
	return nil
}

func evalNearbyEntities(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	radiusValue, ok := node.Field("radius")
	if !ok {
		return nodeRefusal(node, frame, "field \"radius\" is missing")
	}
	radius, valid := amountFromValue(radiusValue)
	if !valid || radius.mantissa < 0 {
		return nodeRefusal(node, frame, "field \"radius\" must be a non-negative exact number")
	}
	request := ResolveRequest{Finder: node.Opcode, Frame: frame, Radius: radius.decimal()}
	if node.Opcode == "ImpactDevicesAround" {
		device, err := requiredRef(node, frame, "device", "")
		if err != nil {
			return err
		}
		request.Ref = device
	} else {
		group, ok := node.Field("affectGroup")
		if !ok || group.Kind != ValueText || group.Text == "" {
			return nodeRefusal(node, frame, "field \"affectGroup\" is missing or is not text")
		}
		holder, ok := node.Field("affectHolder")
		if !ok || holder.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"affectHolder\" is missing or is not boolean")
		}
		onBehalf, ok := node.Field("onBehalfOfHolder")
		if !ok || onBehalf.Kind != ValueBool {
			return nodeRefusal(node, frame, "field \"onBehalfOfHolder\" is missing or is not boolean")
		}
		request.AffectGroup = group.Text
		request.AffectHolder = holder.Bool
		request.OnBehalfOfHolder = onBehalf.Bool
	}
	entities, err := evaluator.host.Resolve(ctx, request)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", node.Opcode, err)
	}
	for _, entityID := range entities {
		scoped := frame
		scoped.Addressee = entityID
		if err := evaluator.evalAll(ctx, node, "impacts", scoped); err != nil {
			return err
		}
	}
	return nil
}

func evalImpactsToInterlocutor(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if frame.InterlocutorID == "" {
		return nodeRefusal(node, frame, "the invocation names no interlocutor")
	}
	scoped := frame
	scoped.Addressee = frame.InterlocutorID
	return evaluator.evalAll(ctx, node, "impacts", scoped)
}

func evalInstantiating(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if value, ok := node.Field("consumeAddress"); ok && value.Kind != ValueBool {
		return nodeRefusal(node, frame, "field \"consumeAddress\" must be boolean")
	}
	return evaluator.evalAll(ctx, node, "impact", frame)
}

func requiredRef(node *Node, frame Frame, name, rowType string) (Ref, error) {
	value, ok := node.Field(name)
	if !ok || value.Kind != ValueRef || value.Ref.ID == "" {
		return Ref{}, nodeRefusal(node, frame, fmt.Sprintf("field %q is missing or is not a content reference", name))
	}
	if rowType != "" && value.Ref.RowType != rowType {
		return Ref{}, nodeRefusal(node, frame, fmt.Sprintf("field %q references %q, want row type %q", name, value.Ref.RowType, rowType))
	}
	return value.Ref, nil
}

func requiredInteger(node *Node, frame Frame, name string, minimum int64) (int64, error) {
	value, ok := node.Field(name)
	if !ok {
		return 0, nodeRefusal(node, frame, fmt.Sprintf("field %q is missing", name))
	}
	integer, valid := exactInteger(value)
	if !valid || integer < minimum {
		return 0, nodeRefusal(node, frame, fmt.Sprintf("field %q must be an exact integer >= %d", name, minimum))
	}
	return integer, nil
}

func exactInteger(value Value) (int64, bool) {
	switch value.Kind {
	case ValueInteger:
		return value.Integer, true
	case ValueDurationMS:
		if value.DurationMS > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value.DurationMS), true
	default:
		return 0, false
	}
}

func requiredLocator(node *Node, frame Frame, field string) (MapLocator, error) {
	children := node.Nodes(field)
	if len(children) != 1 {
		return MapLocator{}, nodeRefusal(node, frame, fmt.Sprintf("field %q must contain exactly one map pointer", field))
	}
	return parseLocator(children[0], node, frame)
}

func parseLocator(locator, owner *Node, frame Frame) (MapLocator, error) {
	if locator.Family != FamilyBasic || locator.Opcode != "Struct" {
		return MapLocator{}, nodeRefusal(owner, frame, "map pointer must be a basic Struct")
	}
	mapValue, ok := locator.Field("map")
	if !ok || mapValue.Kind != ValueRef || mapValue.Ref.ID == "" {
		return MapLocator{}, nodeRefusal(owner, frame, "map pointer has no product map reference")
	}
	scriptID, ok := locator.Field("scriptID")
	if !ok || scriptID.Kind != ValueText || scriptID.Text == "" {
		return MapLocator{}, nodeRefusal(owner, frame, "map pointer has no script id")
	}
	return MapLocator{Map: mapValue.Ref, ScriptID: scriptID.Text}, nil
}

func resolveLocatorList(
	ctx context.Context, evaluator *Evaluator, node *Node, frame Frame, field string,
) ([]Destination, error) {
	locators := node.Nodes(field)
	result := make([]Destination, 0, len(locators))
	for _, child := range locators {
		locator, err := parseLocator(child, node, frame)
		if err != nil {
			return nil, err
		}
		destination, err := evaluator.host.Locate(ctx, DestinationRequest{
			Map: locator.Map, ScriptID: locator.ScriptID, ZoneID: frame.ZoneID,
		})
		if err != nil {
			return nil, fmt.Errorf("resolve map locator %s/%s: %w", locator.Map.ID, locator.ScriptID, err)
		}
		result = append(result, destination)
	}
	return result, nil
}

func executionKey(frame Frame, node *Node) string {
	return frame.EvaluationID + "|" + node.Key + "|" + frame.Addressee
}

func nodeRefusal(node *Node, frame Frame, reason string) error {
	return &RefusedError{
		SourceID: frame.SourceID, NodeKey: node.Key,
		Family: node.Family, Opcode: node.Opcode, Reason: reason,
	}
}
