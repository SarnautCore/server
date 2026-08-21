package script

import (
	"context"
	"fmt"
	"sort"
)

const defaultGuardRadiusMantissa int64 = 425

// activatePersistentEffect validates one persistent effect and asks the host
// to register it. The host owns lifetime and persistence; no opcode crosses
// that seam.
func (evaluator *Evaluator) activatePersistentEffect(
	ctx context.Context, node *Node, frame Frame,
) error {
	effectID, err := persistentEffectID(node, frame)
	if err != nil {
		return err
	}
	switch node.Opcode {
	case "Guard":
		guard, err := parseGuard(node, frame)
		if err != nil {
			return err
		}
		return evaluator.host.Apply(ctx, Command{
			Kind: CommandAttachGuard, EntityID: frame.Addressee, EffectID: effectID,
			Guard: &guard, LifecycleAttempt: frame.LifecycleAttempt,
			ExecutionKey: effectID + "|attach",
		})

	case "ScalerAllInputDamage", "ScalerAllOutputDamage":
		modifier, err := evaluator.parseDamageModifier(node, frame, effectID)
		if err != nil {
			return err
		}
		return evaluator.host.Apply(ctx, Command{
			Kind: CommandAttachDamageModifier, EntityID: frame.Addressee, EffectID: effectID,
			DamageModifier: &modifier, LifecycleAttempt: frame.LifecycleAttempt,
			ExecutionKey: effectID + "|attach",
		})
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key, Family: node.Family, Opcode: node.Opcode,
			Reason: "no persistent effect handler registered",
		}
	}
}

func (evaluator *Evaluator) deactivatePersistentEffect(
	ctx context.Context, node *Node, frame Frame, rollback bool,
) error {
	effectID, err := persistentEffectID(node, frame)
	if err != nil {
		return err
	}
	kind := CommandDetachDamageModifier
	if node.Opcode == "Guard" {
		kind = CommandDetachGuard
	}
	return evaluator.host.Apply(ctx, Command{
		Kind: kind, EntityID: frame.Addressee, EffectID: effectID,
		Rollback: rollback, LifecycleAttempt: frame.LifecycleAttempt,
		ExecutionKey: effectID + "|detach",
	})
}

func persistentEffectID(node *Node, frame Frame) (string, error) {
	if frame.AttachmentID == "" {
		return "", &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key, Family: node.Family, Opcode: node.Opcode,
			Reason: "persistent effect ran outside an attachment lifecycle",
		}
	}
	return frame.AttachmentID + "|" + node.Key, nil
}

func parseGuard(node *Node, frame Frame) (Guard, error) {
	radius := amount{mantissa: defaultGuardRadiusMantissa, scale: 1}
	if value, ok := node.Field("scanRadius"); ok {
		parsed, parsedOK := amountFromValue(value)
		if !parsedOK {
			return Guard{}, effectRefusal(node, frame, "field \"scanRadius\" is not an exact number")
		}
		radius = parsed
	}
	if less, ok := radius.compare(integerAmount(0)); !ok || less < 0 {
		return Guard{}, effectRefusal(node, frame, "field \"scanRadius\" is negative or unrepresentable")
	}

	guard := Guard{Radius: radius.decimal()}
	if value, ok := node.Field("noticeTarget"); ok {
		if value.Kind != ValueBool {
			return Guard{}, effectRefusal(node, frame, "field \"noticeTarget\" is not boolean")
		}
		guard.NoticeTarget = value.Bool
	}
	return guard, nil
}

func (evaluator *Evaluator) parseDamageModifier(
	node *Node, frame Frame, effectID string,
) (DamageModifier, error) {
	stackCount := int64(1)
	if value, ok := node.Field("stackCount"); ok {
		if value.Kind != ValueInteger || value.Integer <= 0 {
			return DamageModifier{}, effectRefusal(node, frame, "field \"stackCount\" must be a positive integer")
		}
		stackCount = value.Integer
	}

	scalers := node.Nodes("scaler")
	if len(scalers) != 1 || scalers[0].Family != FamilyScaler || scalers[0].Opcode != "LinearEffectScaler" {
		return DamageModifier{}, effectRefusal(
			node, frame, fmt.Sprintf("field \"scaler\" must contain one LinearEffectScaler, found %d", len(scalers)),
		)
	}
	scalerNode := scalers[0]
	run, err := evaluator.admit(scalerNode, frame, "damage scaler is outside the M3 implemented tier")
	if err != nil || !run {
		return DamageModifier{}, err
	}
	coefficientValue, ok := scalerNode.Field("coeff")
	if !ok {
		return DamageModifier{}, effectRefusal(node, frame, "LinearEffectScaler field \"coeff\" is required")
	}
	coefficient, ok := amountFromValue(coefficientValue)
	if !ok {
		return DamageModifier{}, effectRefusal(node, frame, "LinearEffectScaler field \"coeff\" is not an exact number")
	}

	modifier := DamageModifier{
		EffectID: effectID, EntityID: frame.Addressee,
		Priority: DamagePriorityScaleAll, Scaler: LinearScaler{Coefficient: coefficient.decimal()},
		StackCount: stackCount,
	}
	if node.Opcode == "ScalerAllInputDamage" {
		modifier.Direction = DamageIncoming
		if only, ok := node.Field("onlyFromCaster"); ok {
			if only.Kind != ValueBool {
				return DamageModifier{}, effectRefusal(node, frame, "field \"onlyFromCaster\" is not boolean")
			}
			if only.Bool {
				if frame.CasterID == "" {
					return DamageModifier{}, effectRefusal(node, frame, "onlyFromCaster has no attaching caster")
				}
				modifier.CapturedOffenderID = frame.CasterID
			}
		}
		if conditions, present := node.Field("attackerConditions"); present {
			if conditions.Kind != ValueList {
				return DamageModifier{}, effectRefusal(node, frame, "field \"attackerConditions\" is not a list")
			}
			for _, entry := range conditions.List {
				if entry.Kind != ValueNode || entry.Node == nil {
					return DamageModifier{}, effectRefusal(node, frame, "attackerConditions contains a non-node entry")
				}
				modifier.AttackerPredicates = append(modifier.AttackerPredicates, entry.Node)
			}
		}
		for _, predicate := range modifier.AttackerPredicates {
			if predicate.Family != FamilyPredicate {
				return DamageModifier{}, effectRefusal(node, frame, "attackerConditions contains a non-predicate node")
			}
		}
	} else {
		modifier.Direction = DamageOutgoing
		if group, ok := node.Field("group"); ok {
			if group.Kind != ValueRef || group.Ref.ID == "" {
				return DamageModifier{}, effectRefusal(node, frame, "field \"group\" is not a content reference")
			}
			copy := group.Ref
			modifier.ActionGroup = &copy
		}
	}
	return modifier, nil
}

func effectRefusal(node *Node, frame Frame, reason string) error {
	return &RefusedError{
		SourceID: frame.SourceID, NodeKey: node.Key, Family: node.Family, Opcode: node.Opcode, Reason: reason,
	}
}

// ScaleDamage folds persistent modifiers in priority order. Equal-priority
// entries retain their attachment order because SliceStable preserves it.
func (evaluator *Evaluator) ScaleDamage(
	ctx context.Context, event DamageEvent, modifiers []DamageModifier,
) (Decimal, error) {
	if !evaluator.options.Enabled {
		return event.Magnitude, ErrDisabled
	}
	if event.OwnerID == "" {
		return event.Magnitude, nil
	}
	ordered := append([]DamageModifier(nil), modifiers...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].Priority < ordered[right].Priority
	})
	value, ok := decimalAmount(event.Magnitude)
	if !ok {
		return Decimal{}, fmt.Errorf("script: damage magnitude is not representable")
	}
	for _, modifier := range ordered {
		if modifier.EntityID != event.OwnerID || modifier.StackCount <= 0 {
			continue
		}
		apply, err := evaluator.damageModifierApplies(ctx, event, modifier)
		if err != nil {
			return Decimal{}, err
		}
		if !apply {
			continue
		}
		coefficient, ok := decimalAmount(modifier.Scaler.Coefficient)
		if !ok {
			return Decimal{}, fmt.Errorf("script: modifier %s coefficient is not representable", modifier.EffectID)
		}
		stacks, ok := coefficient.mul(integerAmount(modifier.StackCount))
		if !ok {
			return Decimal{}, fmt.Errorf("script: modifier %s coefficient times stack count overflows", modifier.EffectID)
		}
		factor, ok := integerAmount(1).add(stacks)
		if !ok {
			return Decimal{}, fmt.Errorf("script: modifier %s factor overflows", modifier.EffectID)
		}
		value, ok = value.mul(factor)
		if !ok {
			return Decimal{}, fmt.Errorf("script: modifier %s damage product overflows", modifier.EffectID)
		}
	}
	return value.decimal(), nil
}

func (evaluator *Evaluator) damageModifierApplies(
	ctx context.Context, event DamageEvent, modifier DamageModifier,
) (bool, error) {
	switch modifier.Direction {
	case DamageIncoming:
		// Retail still scales when the event has no offender, or when the
		// offender address exists but its runtime replica cannot be resolved.
		if event.OffenderID == "" {
			return true, nil
		}
		if modifier.CapturedOffenderID != "" && modifier.CapturedOffenderID != event.OffenderID {
			return false, nil
		}
		if !event.OffenderResolved {
			return true, nil
		}
		frame := Frame{Addressee: event.OffenderID, CasterID: event.OffenderID, TargetID: event.OwnerID}
		for _, predicate := range modifier.AttackerPredicates {
			ok, err := evaluator.Predicate(ctx, predicate, frame)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil

	case DamageOutgoing:
		if modifier.ActionGroup == nil {
			return true, nil
		}
		return event.HasActiveAction && event.ActionGroup.ID == modifier.ActionGroup.ID, nil

	default:
		return false, fmt.Errorf("script: modifier %s has no damage direction", modifier.EffectID)
	}
}

func decimalAmount(value Decimal) (amount, bool) {
	return amountFromValue(Value{Kind: ValueDecimal, Mantissa: value.Mantissa, Scale: value.Scale})
}
