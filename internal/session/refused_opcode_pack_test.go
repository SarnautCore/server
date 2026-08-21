package session_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
)

const (
	refusedOpcodeQuestID = "quest.inst-league1.quest-4-30"
	refusedOpcodeMapID   = "ext.maps.inst-league-start.map-resource"
	triggerTargetID      = "trigger.inst-league1.quest-3-20.trigger-target"
	gibberDeathID        = "trigger.inst-league1.quest-4-30.gibber-death"
	finalQuestID         = "trigger.inst-league1.quest-4-30.final-quest"
)

type packTraceHost struct {
	commands []script.Command
	trace    []string
	effects  *script.EffectRegistry
}

func newPackTraceHost() *packTraceHost {
	return &packTraceHost{effects: script.NewEffectRegistry()}
}

func (*packTraceHost) Now() time.Time { return time.Unix(0, 0) }

func (host *packTraceHost) Query(_ context.Context, query script.Query) (script.Value, error) {
	if query.Kind != script.QueryIsAvatar {
		return script.Value{}, fmt.Errorf("unexpected query kind %d", query.Kind)
	}
	host.trace = append(host.trace, "avatar:"+query.EntityID)
	return script.Value{Kind: script.ValueBool, Bool: query.EntityID == "character.player"}, nil
}

func (*packTraceHost) Resolve(context.Context, script.ResolveRequest) ([]string, error) {
	return nil, nil
}

func (host *packTraceHost) Locate(_ context.Context, request script.DestinationRequest) (script.Destination, error) {
	position := script.Position{X: 10, Y: 20, Z: 3}
	if request.ScriptID == "Firewall" {
		position = script.Position{X: -4, Y: 8, Z: 1}
	}
	host.trace = append(host.trace, "locate:"+request.ScriptID)
	return script.Destination{Map: request.Map, Position: position}, nil
}

func (host *packTraceHost) Apply(_ context.Context, command script.Command) error {
	host.commands = append(host.commands, command)
	switch command.Kind {
	case script.CommandAttachGuard, script.CommandDetachGuard,
		script.CommandAttachDamageModifier, script.CommandDetachDamageModifier:
		_, err := host.effects.Apply(command, script.EffectOwner{Mob: true, CellPlaced: true})
		return err
	default:
		return nil
	}
}

func (*packTraceHost) Enqueue(context.Context, script.Deferred) error { return nil }

func TestCompiledPackDrivesAllSevenReachableFormerRefusals(t *testing.T) {
	t.Parallel()

	content := loadCompiledScriptPack(t)
	directory := content.Directory()
	questNodes, triggerRows := refusedOpcodeRows()
	questRow := &contentv1.QuestScript{
		Id: "script." + refusedOpcodeQuestID, QuestId: refusedOpcodeQuestID,
	}
	for _, node := range questNodes {
		questRow.StartImpacts = append(questRow.StartImpacts, scriptNodeToProto(node))
	}
	replaceSessionPackTable(t, directory, "quest-scripts", contentv1.RowType_ROW_TYPE_QUEST_SCRIPT,
		[]compiledPackRow{{id: questRow.GetId(), message: questRow}})
	replaceSessionPackTable(t, directory, "script-triggers", contentv1.RowType_ROW_TYPE_SCRIPT_TRIGGER,
		triggerRows)
	resealSessionPack(t, directory)

	loaded, err := pack.Load(directory, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load(refused-opcode fixture) error = %v", err)
	}
	source := session.NewPackQuestScriptSource(loaded)
	activation, ok := source.QuestActivation(refusedOpcodeQuestID)
	if !ok || len(activation.StartImpacts) != 2 {
		t.Fatalf("compiled destination activation = %d nodes, %t", len(activation.StartImpacts), ok)
	}

	host := newPackTraceHost()
	evaluator := script.New(host, script.Options{Enabled: true})
	frame := script.Frame{
		EvaluationID: "pack-eval", PackID: loaded.ID(), ZoneID: "zone.inst-league1",
		SourceID: refusedOpcodeQuestID, CasterID: "character.player",
		TargetID: "mob.target", Addressee: "mob.target",
	}
	first, err := evaluator.ResolveDestination(t.Context(), activation.StartImpacts[0], frame)
	if err != nil || first.Position != (script.Position{X: 10, Y: 20, Z: 3}) || first.Yaw != (script.Decimal{}) {
		t.Fatalf("DemonSpawn4 destination = %#v, %v", first, err)
	}
	second, err := evaluator.ResolveDestination(t.Context(), activation.StartImpacts[1], frame)
	if err != nil || second.Position != (script.Position{X: -4, Y: 8, Z: 1}) || second.Yaw != (script.Decimal{}) {
		t.Fatalf("Firewall destination = %#v, %v", second, err)
	}

	activatePackTrigger(t, evaluator, source, triggerTargetID, "target-attachment", frame)
	activatePackTrigger(t, evaluator, source, gibberDeathID, "gibber-attachment", frame)

	final, ok := source.Trigger(script.Ref{ID: finalQuestID})
	if !ok {
		t.Fatalf("compiled pack has no %s", finalQuestID)
	}
	predicates := final.Nodes("predicates")
	if len(predicates) != 1 {
		t.Fatalf("final trigger predicates = %d, want 1", len(predicates))
	}
	avatarFrame := frame
	avatarFrame.Addressee = "character.player"
	isAvatar, err := evaluator.Predicate(t.Context(), predicates[0], avatarFrame)
	if err != nil || !isAvatar {
		t.Fatalf("PredicateIsAvatar() = %t, %v", isAvatar, err)
	}

	assertPackCommandSequence(t, host.commands)
	if got, want := host.trace, []string{"locate:DemonSpawn4", "locate:Firewall", "avatar:character.player"}; !equalStrings(got, want) {
		t.Fatalf("pack trace = %v, want %v", got, want)
	}
	if census := evaluator.Census(); census.Implemented["DestinationLocator"] != 2 ||
		census.Implemented["Guard"] != 2 ||
		census.Implemented["ScalerAllInputDamage"] != 4 ||
		census.Implemented["ScalerAllOutputDamage"] != 2 ||
		census.Implemented["PredicateIsAvatar"] != 1 {
		t.Fatalf("compiled-pack census = %#v", census.Implemented)
	}
}

func activatePackTrigger(
	t *testing.T,
	evaluator *script.Evaluator,
	source *session.PackQuestScriptSource,
	triggerID, attachmentID string,
	frame script.Frame,
) {
	t.Helper()
	document, ok := source.Trigger(script.Ref{ID: triggerID})
	if !ok {
		t.Fatalf("compiled pack has no %s", triggerID)
	}
	attachment := script.Attachment{
		ID: attachmentID, EntityID: "mob.target",
		TriggerRef: script.Ref{ID: triggerID}, Trigger: document, Frame: frame,
	}
	if err := evaluator.ActivateAttachment(t.Context(), attachment); err != nil {
		t.Fatalf("activate %s: %v", triggerID, err)
	}
	if err := evaluator.Detach(t.Context(), attachment); err != nil {
		t.Fatalf("detach %s: %v", triggerID, err)
	}
}

func assertPackCommandSequence(t *testing.T, commands []script.Command) {
	t.Helper()
	if len(commands) != 10 {
		t.Fatalf("persistent command count = %d, want 10", len(commands))
	}
	wantKinds := []script.CommandKind{
		script.CommandAttachDamageModifier, script.CommandAttachDamageModifier,
		script.CommandDetachDamageModifier, script.CommandDetachDamageModifier, script.CommandDetachTrigger,
		script.CommandAttachGuard, script.CommandAttachDamageModifier,
		script.CommandDetachDamageModifier, script.CommandDetachGuard, script.CommandDetachTrigger,
	}
	for index, want := range wantKinds {
		if commands[index].Kind != want {
			t.Fatalf("command[%d] kind = %d, want %d", index, commands[index].Kind, want)
		}
	}
	if got := commands[0].DamageModifier; got.Direction != script.DamageOutgoing ||
		got.Scaler.Coefficient != (script.Decimal{Mantissa: -9, Scale: 1}) {
		t.Fatalf("compiled output modifier = %#v", got)
	}
	if got := commands[1].DamageModifier; got.Direction != script.DamageIncoming ||
		got.Scaler.Coefficient != (script.Decimal{Mantissa: -9, Scale: 1}) {
		t.Fatalf("compiled input modifier = %#v", got)
	}
	if commands[2].EffectID != commands[1].EffectID || commands[3].EffectID != commands[0].EffectID {
		t.Fatalf("TriggerTarget detach order is not reverse attach order")
	}
	if got := commands[5].Guard; got.Radius != (script.Decimal{Mantissa: 15}) || got.NoticeTarget {
		t.Fatalf("compiled Guard = %#v", got)
	}
	if got := commands[6].DamageModifier; got.Direction != script.DamageIncoming ||
		got.Scaler.Coefficient != (script.Decimal{Mantissa: 100}) || got.StackCount != 1 {
		t.Fatalf("compiled GibberDeath modifier = %#v", got)
	}
}

func refusedOpcodeRows() ([]*script.Node, []compiledPackRow) {
	mapRef := script.Value{Kind: script.ValueRef, Ref: script.Ref{ID: refusedOpcodeMapID, RowType: "map-resource"}}
	destination := func(key, scriptID string, authoredYaw bool) *script.Node {
		locator := packScriptNode(key+"/locator", script.FamilyBasic, "Struct", script.TierInert,
			packField("map", mapRef), packField("scriptID", script.Value{Kind: script.ValueText, Text: scriptID}))
		fields := []script.Field{packField("locator", script.Value{Kind: script.ValueNode, Node: locator})}
		if authoredYaw {
			fields = append(fields, packField("yaw", script.Value{Kind: script.ValueInteger}))
		}
		return packScriptNode(key, script.FamilyImpact, "DestinationLocator", script.TierImplemented, fields...)
	}
	questNodes := []*script.Node{
		destination("script.inst-league1.quest-4-30/startImpacts[4]/impacts[0]/destination", "DemonSpawn4", true),
		destination("script.inst-league1.quest-4-30/startImpacts[5]/impacts[0]/impacts[4]/destination", "Firewall", false),
	}

	linear := func(key string, coefficient script.Value) *script.Node {
		return packScriptNode(key, script.FamilyScaler, "LinearEffectScaler", script.TierImplemented,
			packField("coeff", coefficient))
	}
	modifier := func(key, opcode string, coefficient script.Value) *script.Node {
		return packScriptNode(key, script.FamilyEffect, opcode, script.TierImplemented,
			packField("scaler", script.Value{Kind: script.ValueNode, Node: linear(key+"/scaler", coefficient)}))
	}
	trigger := func(id string, effects ...*script.Node) compiledPackRow {
		root := packScriptNode(id, script.FamilyTrigger, "TriggerResource", script.TierImplemented,
			packField("effects", packNodeList(effects...)))
		return compiledPackRow{id: id, message: &contentv1.ScriptTrigger{Id: id, Root: scriptNodeToProto(root)}}
	}
	triggerTarget := trigger(triggerTargetID,
		modifier(triggerTargetID+"/effects[1]", "ScalerAllOutputDamage", script.Value{Kind: script.ValueDecimal, Mantissa: -9, Scale: 1}),
		modifier(triggerTargetID+"/effects[2]", "ScalerAllInputDamage", script.Value{Kind: script.ValueDecimal, Mantissa: -9, Scale: 1}),
	)
	gibber := trigger(gibberDeathID,
		packScriptNode(gibberDeathID+"/effects[0]", script.FamilyEffect, "Guard", script.TierImplemented,
			packField("scanRadius", script.Value{Kind: script.ValueInteger, Integer: 15})),
		modifier(gibberDeathID+"/effects[1]", "ScalerAllInputDamage", script.Value{Kind: script.ValueInteger, Integer: 100}),
	)
	predicate := packScriptNode(
		finalQuestID+"/effects[0]/impactsOn[1]/impacts[0]/impacts[0]/predicate",
		script.FamilyPredicate, "PredicateIsAvatar", script.TierImplemented,
		packField("toLog", script.Value{Kind: script.ValueBool}),
	)
	final := compiledPackRow{id: finalQuestID, message: &contentv1.ScriptTrigger{
		Id: finalQuestID,
		Root: scriptNodeToProto(packScriptNode(finalQuestID, script.FamilyTrigger, "TriggerResource", script.TierImplemented,
			packField("predicates", packNodeList(predicate)))),
	}}
	return questNodes, []compiledPackRow{triggerTarget, gibber, final}
}

func packScriptNode(key string, family script.Family, opcode string, tier script.Tier, fields ...script.Field) *script.Node {
	return &script.Node{Key: key, Family: family, Opcode: opcode, Tier: tier, Fields: fields}
}

func packField(name string, value script.Value) script.Field {
	return script.Field{Name: name, Value: value}
}

func packNodeList(nodes ...*script.Node) script.Value {
	values := make([]script.Value, 0, len(nodes))
	for _, node := range nodes {
		values = append(values, script.Value{Kind: script.ValueNode, Node: node})
	}
	return script.Value{Kind: script.ValueList, List: values}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
