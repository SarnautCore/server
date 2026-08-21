package script

import "github.com/SarnautCore/server/internal/pack"

// FromPackNode copies one validated compiled node into the evaluator's domain
// type. Pack owns decoding and structural checks; script owns execution.
func FromPackNode(source pack.ScriptNode) *Node {
	node := &Node{
		Key: source.Key, Family: Family(source.Family), Opcode: source.Opcode, Tier: tierFromPack(source.Tier),
		Fields: make([]Field, 0, len(source.Fields)),
	}
	for _, field := range source.Fields {
		node.Fields = append(node.Fields, Field{Name: field.Name, Value: valueFromPack(field.Value)})
	}
	return node
}

func valueFromPack(source pack.ScriptValue) Value {
	result := Value{
		Integer: source.Integer, Mantissa: source.Mantissa, Scale: source.Scale,
		Bool: source.Bool, Text: source.Text, DurationMS: source.DurationMS,
		Ref: Ref{ID: source.Ref.ID, RowType: source.Ref.RowType},
	}
	switch source.Kind {
	case pack.ScriptValueInteger:
		result.Kind = ValueInteger
	case pack.ScriptValueDecimal:
		result.Kind = ValueDecimal
	case pack.ScriptValueBool:
		result.Kind = ValueBool
	case pack.ScriptValueText:
		result.Kind = ValueText
	case pack.ScriptValueRef:
		result.Kind = ValueRef
	case pack.ScriptValueDurationMS:
		result.Kind = ValueDurationMS
	case pack.ScriptValueNode:
		result.Kind = ValueNode
		if source.Node != nil {
			result.Node = FromPackNode(*source.Node)
		}
	case pack.ScriptValueList:
		result.Kind = ValueList
		result.List = make([]Value, 0, len(source.List))
		for _, entry := range source.List {
			result.List = append(result.List, valueFromPack(entry))
		}
	default:
		result.Kind = ValueUnspecified
	}
	return result
}

func tierFromPack(tier pack.ScriptTier) Tier {
	switch tier {
	case pack.ScriptTierInert:
		return TierInert
	case pack.ScriptTierImplemented:
		return TierImplemented
	default:
		return TierRefused
	}
}
