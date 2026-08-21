package script_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/script"
)

// Content ids from the real tutorial documents, so a failure names something a
// reader can go and look at. Quest_1_20 is the DressTrigger quest and the first
// quest-count-special objective in the chain; Quest_1_30 is the RatKiller
// kill-counting shape.
const (
	dressCountID = "questcount.inst-league1.quest-1-20.count-id-1"
	ratCountID   = "questcount.inst-league1.quest-1-30.count-id-1"
	warriorClass = "class.warrior"
	druidClass   = "class.druid"

	playerID = "character.tester"
	ratID    = "mob.inst-league1.rat"
)

// fakeHost records an ordered call trace instead of touching the world. ADR 0036
// makes golden call traces over the discovered trigger corpus a binding check
// rather than an optional example, so the trace format is the assertion surface:
// one line per host call, in the order the evaluator made them.
type fakeHost struct {
	nowMS int64
	// class answers QueryCharacterClass per entity.
	class  map[string]string
	avatar map[string]bool
	// maxHealth answers QueryMaxHealth, which is FullHealthCalcer's only input.
	maxHealth map[string]int64
	// resolved answers Resolve per finder opcode: a spawn table resolves to its
	// live mobs, in the bytewise id order ADR 0036 requires.
	resolved map[string][]string
	located  map[string]script.Position
	// scale answers the three per-opcode scale queries with an exact decimal.
	scale map[script.QueryKind]script.Decimal
	// applyErr, when set, makes the next Apply fail.
	applyErr error
	// applyErrors injects failures at exact Apply calls. It lets lifecycle tests
	// fail a later effect and, independently, its compensation.
	applyErrors map[int]error
	applyCalls  int
	// attached records what CommandAttachTrigger asked for, so a test can fire
	// an event at exactly the attachment the evaluator created rather than at
	// one the test invented.
	attached []script.Attachment
	commands []script.Command

	trace []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		// A fixed clock: deferred due times are asserted exactly, and a wall
		// clock would make that flaky for no benefit.
		nowMS:     1_000_000,
		class:     map[string]string{playerID: warriorClass},
		avatar:    map[string]bool{playerID: true},
		maxHealth: map[string]int64{ratID: 40, playerID: 300},
		resolved:  map[string][]string{},
		located:   map[string]script.Position{},
		// A scale of 2 rather than 1, so that a handler which forgot to
		// multiply would still be caught.
		scale: map[script.QueryKind]script.Decimal{
			script.QueryPhysicalScale:       {Mantissa: 2},
			script.QueryPhysicalRangedScale: {Mantissa: 15, Scale: 1},
			script.QueryWeaponSpeedScale:    {Mantissa: 13, Scale: 1},
		},
	}
}

// decimalText renders an exact decimal for a trace line without going through a
// float on the way, which would defeat the point of carrying one.
func decimalText(value script.Decimal) string {
	if value.Scale == 0 {
		return fmt.Sprintf("%d", value.Mantissa)
	}
	divisor := int64(1)
	for index := int32(0); index < value.Scale; index++ {
		divisor *= 10
	}
	whole, fraction := value.Mantissa/divisor, value.Mantissa%divisor
	if fraction < 0 {
		fraction = -fraction
	}
	sign := ""
	if value.Mantissa < 0 && whole == 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, value.Scale, fraction)
}

func (host *fakeHost) Now() time.Time { return time.UnixMilli(host.nowMS) }

func (host *fakeHost) Query(_ context.Context, query script.Query) (script.Value, error) {
	switch query.Kind {
	case script.QueryCharacterClass:
		got := host.class[query.EntityID]
		host.trace = append(host.trace, fmt.Sprintf("query class %s -> %s", query.EntityID, got))
		return script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: got}}, nil
	case script.QueryMaxHealth:
		got := host.maxHealth[query.EntityID]
		host.trace = append(host.trace, fmt.Sprintf("query max-health %s -> %d", query.EntityID, got))
		return script.Value{Kind: script.ValueInteger, Integer: got}, nil
	case script.QueryIsAvatar:
		got := host.avatar[query.EntityID]
		host.trace = append(host.trace, fmt.Sprintf("query is-avatar %s -> %t", query.EntityID, got))
		return script.Value{Kind: script.ValueBool, Bool: got}, nil
	case script.QueryPhysicalScale, script.QueryPhysicalRangedScale, script.QueryWeaponSpeedScale:
		got := host.scale[query.Kind]
		host.trace = append(host.trace, fmt.Sprintf(
			"query scale/%d %s slot=%s -> %s",
			query.Kind, query.EntityID, query.Slot, decimalText(got),
		))
		return script.Value{Kind: script.ValueDecimal, Mantissa: got.Mantissa, Scale: got.Scale}, nil
	default:
		return script.Value{}, fmt.Errorf("fake host has no answer for query kind %d", query.Kind)
	}
}

func (host *fakeHost) Locate(_ context.Context, request script.DestinationRequest) (script.Destination, error) {
	position, ok := host.located[request.Map.ID+"|"+request.ScriptID]
	if !ok {
		return script.Destination{}, fmt.Errorf("fake host has no locator %s/%s", request.Map.ID, request.ScriptID)
	}
	host.trace = append(host.trace, fmt.Sprintf(
		"locate %s/%s -> %.3f,%.3f,%.3f",
		request.Map.ID, request.ScriptID, position.X, position.Y, position.Z,
	))
	return script.Destination{Map: request.Map, Position: position}, nil
}

func (host *fakeHost) Resolve(_ context.Context, request script.ResolveRequest) ([]string, error) {
	entities := host.resolved[request.Finder]
	host.trace = append(host.trace, fmt.Sprintf(
		"resolve %s %s -> %s", request.Finder, request.Ref.ID, strings.Join(entities, ","),
	))
	return entities, nil
}

func (host *fakeHost) Apply(_ context.Context, command script.Command) error {
	host.applyCalls++
	if err := host.applyErrors[host.applyCalls]; err != nil {
		return err
	}
	if host.applyErr != nil {
		return host.applyErr
	}
	host.commands = append(host.commands, command)
	switch command.Kind {
	case script.CommandAttachTrigger:
		host.attached = append(host.attached, *command.Attachment)
		scope := command.EntityID
		if command.Attachment.MobWorld.ID != "" {
			scope = "mobworld:" + command.Attachment.MobWorld.ID
			if command.Attachment.OnlyTagged {
				scope += " tagged-only"
			}
		}
		host.trace = append(host.trace, fmt.Sprintf(
			"apply attach-trigger %s to %s key=%s",
			command.Ref.ID, scope, command.ExecutionKey,
		))
	case script.CommandDetachTrigger:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply detach-trigger %s from %s key=%s",
			command.Attachment.TriggerRef.ID, command.EntityID, command.ExecutionKey,
		))
	case script.CommandTagMobForKill:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply tag-mob-for-kill %s key=%s", command.EntityID, command.ExecutionKey,
		))
	case script.CommandDamage:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply damage %s on %s avoidable=%t threat=%s key=%s",
			decimalText(command.Magnitude), command.EntityID,
			command.CanBeAvoided, decimalText(command.ThreatMultiplier), command.ExecutionKey,
		))
	case script.CommandSetTarget:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply set-target %s -> %s key=%s",
			command.EntityID, command.TargetID, command.ExecutionKey,
		))
	case script.CommandAttachGuard:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply attach-guard %s radius=%s notice=%t key=%s",
			command.EntityID, decimalText(command.Guard.Radius), command.Guard.NoticeTarget, command.ExecutionKey,
		))
	case script.CommandDetachGuard:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply detach-guard %s effect=%s key=%s",
			command.EntityID, command.EffectID, command.ExecutionKey,
		))
	case script.CommandAttachDamageModifier:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply attach-damage-modifier %s direction=%d coeff=%s stacks=%d key=%s",
			command.EntityID, command.DamageModifier.Direction,
			decimalText(command.DamageModifier.Scaler.Coefficient),
			command.DamageModifier.StackCount, command.ExecutionKey,
		))
	case script.CommandDetachDamageModifier:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply detach-damage-modifier %s effect=%s key=%s",
			command.EntityID, command.EffectID, command.ExecutionKey,
		))
	default:
		host.trace = append(host.trace, fmt.Sprintf(
			"apply increase-quest-count %s +%d on %s key=%s",
			command.Ref.ID, command.Count, command.EntityID, command.ExecutionKey,
		))
	}
	return nil
}

func (host *fakeHost) Enqueue(_ context.Context, deferred script.Deferred) error {
	host.trace = append(host.trace, fmt.Sprintf(
		"enqueue %s due=%d", deferred.Node.Opcode, deferred.DueAtMS,
	))
	return nil
}

// --- node builders -------------------------------------------------------
//
// These keep the tests readable without introducing a second representation:
// every builder returns a plain *script.Node.

func impact(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyImpact, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func predicate(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyPredicate, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func effect(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyEffect, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func triggerNode(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyTrigger, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func calcer(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyCalcer, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func basic(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyBasic, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func scaler(key, opcode string, fields ...script.Field) *script.Node {
	return &script.Node{
		Key: key, Family: script.FamilyScaler, Opcode: opcode,
		Tier: script.TierImplemented, Fields: fields,
	}
}

func field(name string, value script.Value) script.Field {
	return script.Field{Name: name, Value: value}
}

func nodeList(nodes ...*script.Node) script.Value {
	values := make([]script.Value, 0, len(nodes))
	for _, node := range nodes {
		values = append(values, script.Value{Kind: script.ValueNode, Node: node})
	}
	return script.Value{Kind: script.ValueList, List: values}
}

func ref(id string) script.Value {
	return script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: id}}
}

func duration(ms uint64) script.Value {
	return script.Value{Kind: script.ValueDurationMS, DurationMS: ms}
}

func text(value string) script.Value {
	return script.Value{Kind: script.ValueText, Text: value}
}

func integer(value int64) script.Value {
	return script.Value{Kind: script.ValueInteger, Integer: value}
}

// decimal spells an exact authored number: mantissa * 10^-scale. The content's
// multipliers are decimals and the interpreter never turns one into a float, so
// the fixtures do not either.
func decimal(mantissa int64, scale int32) script.Value {
	return script.Value{Kind: script.ValueDecimal, Mantissa: mantissa, Scale: scale}
}

func node(child *script.Node) script.Value {
	return script.Value{Kind: script.ValueNode, Node: child}
}

// increaseQuestCount is the leaf every quest-count-special objective ends at.
func increaseQuestCount(key, countID string) *script.Node {
	return impact(key, "ImpactIncreaseQuestCount", field("id", ref(countID)))
}

func newFrame() script.Frame {
	return script.Frame{
		EvaluationID: "eval-1",
		PackID:       "pack-1",
		SourceID:     "quest.inst-league1.quest-1-20",
		ZoneID:       "zone.inst-league1",
		CasterID:     playerID,
		TargetID:     playerID,
		Addressee:    playerID,
	}
}

func enabled() script.Options { return script.Options{Enabled: true} }

func run(host *fakeHost, node *script.Node, frame script.Frame) (*script.Evaluator, error) {
	evaluator := script.New(host, enabled())
	return evaluator, evaluator.Evaluate(context.Background(), node, frame)
}
