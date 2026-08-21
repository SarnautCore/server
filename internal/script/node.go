// Package script evaluates the recursive impact, predicate, effect, addressee
// finder, scaler and calcer trees that ADR 0036 carries from authored YAML
// through sarnaut.content.v1 pack rows and into the shard.
//
// It is the single evaluator for all four callers named by ADR 0036: quest
// start and reward impacts, trigger activation, script-zone entry/leave and
// variable paths, and spell caster and target impacts. There is deliberately no
// second evaluator and no per-opcode Go struct: an opcode this build has never
// heard of must still round-trip through a pack, which is why Family and Opcode
// are strings and coverage is carried as a Tier rather than as a build-time
// allow-list.
//
// Containment (ADR 0033 §2, ADR 0036): this package imports only the standard
// library, and — once M3-09 lands script rows — internal/pack and
// internal/gametypes. It must never import internal/world, internal/combat,
// internal/quests or internal/session. boundary_test.go asserts that for a
// checkout with no linter installed.
package script

import "fmt"

// Family is the namespace a node's opcode came from. ADR 0036 named five;
// .cache/m3/impact-interpreter-survey.md §4 shows the tutorial reaches seven,
// because HealthTrigger reads its thresholds from a calcer and its off-value
// from basicElements.
type Family string

const (
	FamilyImpact    Family = "impact"
	FamilyPredicate Family = "predicate"
	FamilyEffect    Family = "effect"
	FamilyFinder    Family = "addresseeFinder"
	FamilyScaler    Family = "scaler"
	FamilyCalcer    Family = "calcer"
	FamilyBasic     Family = "basic"
	FamilyTrigger   Family = "trigger"
)

// Tier is ADR 0036's coverage policy. Every node carries exactly one.
type Tier uint8

const (
	// TierRefused evaluation fails loudly, naming the row, node key and opcode,
	// before any child runs. Unknown opcodes and unsupported nodes that can
	// affect authoritative state default here.
	TierRefused Tier = iota
	// TierInert parses, stays in the pack, records a census hit, does nothing.
	// Admissible only where omission cannot alter authoritative state or bypass
	// a safety check.
	TierInert
	// TierImplemented validates the node's fields and executes it.
	TierImplemented
)

func (tier Tier) String() string {
	switch tier {
	case TierRefused:
		return "refused"
	case TierInert:
		return "inert-and-counted"
	case TierImplemented:
		return "implemented"
	default:
		return fmt.Sprintf("tier(%d)", uint8(tier))
	}
}

// Node is the evaluator-owned form of a validated pack.ScriptNode. The pack
// reader owns protobuf and table validation; FromPackNode copies the recursive
// values across this package boundary.
type Node struct {
	// Key is the owning content-row id followed by the field names and list
	// ordinals on the path to this node. Stable across builds of the same
	// content, which is what makes it usable as a probability input and as half
	// of a deferred command's execution key.
	Key    string
	Family Family
	Opcode string
	Tier   Tier
	// Fields are sorted bytewise by name and unique within a node.
	Fields []Field
}

// Field is one named value on a node.
type Field struct {
	Name  string
	Value Value
}

// ValueKind discriminates Value. It is the Go spelling of ScriptValue's oneof.
type ValueKind uint8

const (
	ValueUnspecified ValueKind = iota
	ValueInteger
	ValueDecimal
	ValueBool
	ValueText
	ValueRef
	ValueDurationMS
	ValueNode
	ValueList
)

// Value is one entry of ScriptValue's oneof. Exactly one member is meaningful,
// selected by Kind.
//
// Decimal is an exact signed mantissa and a base-10 scale, never a float:
// probability thresholds and damage scalers are compared with integer
// arithmetic so that a restart makes the same choice (ADR 0036, "Deterministic
// probability and ordering").
type Value struct {
	Kind       ValueKind
	Integer    int64
	Mantissa   int64
	Scale      int32
	Bool       bool
	Text       string
	Ref        Ref
	DurationMS uint64
	Node       *Node
	List       []Value
}

// Ref is a resolved content reference: a canonical id and row type, never a
// source href. The extractor resolves hrefs; nothing downstream sees an xpointer.
type Ref struct {
	ID      string
	RowType string
}

// Decimal is ADR 0036's exact signed mantissa plus base-10 scale, in the shape a
// host command carries it: Mantissa * 10^-Scale. It exists so that a number can
// cross the host boundary without becoming a float on the way — an auto-attack's
// authored 8.75 average damage reaches the combat hook as 875 at scale 2.
type Decimal struct {
	Mantissa int64
	Scale    int32
}

// Field returns the named field's value. Nodes carry few fields and the slice is
// sorted, but linear scan over a handful of entries beats a map allocation per
// node, and the evaluator visits every node of a tree.
func (node *Node) Field(name string) (Value, bool) {
	for index := range node.Fields {
		if node.Fields[index].Name == name {
			return node.Fields[index].Value, true
		}
	}
	return Value{}, false
}

// Nodes returns the child nodes of a list-valued field in stored order, which is
// the order ImpactsDeferred schedules them in and the order the quest-2-20
// reward cascade is visited in. A node-valued field yields one child, so callers
// that accept "one or many" (the XML shape does) need no special case.
func (node *Node) Nodes(name string) []*Node {
	value, ok := node.Field(name)
	if !ok {
		return nil
	}
	switch value.Kind {
	case ValueNode:
		if value.Node == nil {
			return nil
		}
		return []*Node{value.Node}
	case ValueList:
		children := make([]*Node, 0, len(value.List))
		for _, entry := range value.List {
			if entry.Kind == ValueNode && entry.Node != nil {
				children = append(children, entry.Node)
			}
		}
		return children
	default:
		return nil
	}
}
