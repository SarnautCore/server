package script

import (
	"context"
	"fmt"
)

// This file is the count-special mechanism. Ten of the twelve extracted tutorial
// objectives are quest-count-special, and the survey traced every one of them to
// one of two shapes. Neither is a quest-module feature; both are interpreter
// work, and the quest module reaches them only as a Host.Apply command.
//
// Shape A, equip: the quest names triggerAgents: TriggerAgentSelf -> DressTrigger.
// The trigger is bound to the player, EquipTrigger gates on slot MAINHAND or
// TWOHANDED, and the Switch inside it runs impactsOn once, which increments the
// counter. Quest_1_20.
//
// Shape B, kill: the quest's startImpacts run ImpactFindSpawnTable over a spawn
// table and ImpactAttachTrigger binds RatKiller to every mob in it. RatKiller's
// HealthTrigger watches health against FullHealthCalcer(multiplier=0), which is
// zero, which is death. Its impactsOn holds a ReturningImpact wrapping
// ImpactIncreaseQuestCount, so the increment lands on the killer rather than on
// the corpse. Quest_1_30, Quest_2_10 twice.
//
// The evaluator holds no registry and no per-entity state. Attachments go out
// through Host.Apply and come back through Fire, exactly the way ImpactsDeferred
// hands work to Host.Enqueue and gets it back later. That is what keeps a
// restart from losing a trigger and keeps this package free of a second store.

// Fire delivers one host event to one attached trigger. It is the trigger
// caller of ADR 0036 — the second of the four — and the host is the only thing
// that decides when an event happened.
func (evaluator *Evaluator) Fire(ctx context.Context, attachment Attachment, event Event) error {
	if !evaluator.options.Enabled {
		return ErrDisabled
	}
	if attachment.Trigger == nil {
		return fmt.Errorf(
			"script: attachment for %s carries no trigger node; the host resolves %s before firing",
			attachment.EntityID, attachment.TriggerRef.ID,
		)
	}
	if event.EntityID != attachment.EntityID {
		return fmt.Errorf(
			"script: event for %s delivered to a trigger attached to %s",
			event.EntityID, attachment.EntityID,
		)
	}

	frame := evaluator.attachedFrame(attachment)
	frame.Event = event.Kind.String()
	// The cause of a health change is the killer. Binding it to the frame's
	// caster is what ReturningImpact reads, and it is the only reason shape B
	// credits a player rather than the dying rat.
	if event.CauseID != "" {
		frame.CasterID = event.CauseID
	}

	run, err := evaluator.admit(attachment.Trigger, frame, "trigger is outside the M3 implemented tier")
	if err != nil || !run {
		return err
	}
	for _, effect := range attachment.Trigger.Nodes("effects") {
		if err := evaluator.deliver(ctx, effect, frame, event); err != nil {
			return err
		}
	}
	return nil
}

// Detach ends an attachment. ADR 0036 says a Switch's impactsOff "runs once when
// it detaches or expires", so the off-branches run here and the detach command
// follows them.
//
// It walks the trigger's own effects and no deeper. Whether a nested gate was
// open — whether the player still had that weapon equipped — is host state, and
// this package deliberately holds none; recursing would run the off-branch of a
// Switch that never ran its on-branch.
func (evaluator *Evaluator) Detach(ctx context.Context, attachment Attachment) error {
	if !evaluator.options.Enabled {
		return ErrDisabled
	}

	frame := evaluator.attachedFrame(attachment)
	frame.Event = "detach"
	if attachment.Trigger != nil {
		run, err := evaluator.admit(attachment.Trigger, frame, "trigger is outside the M3 implemented tier")
		if err != nil {
			return err
		}
		if run {
			for _, effect := range attachment.Trigger.Nodes("effects") {
				if err := evaluator.deactivate(ctx, effect, frame); err != nil {
					return err
				}
			}
		}
	}

	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandDetachTrigger,
		EntityID:     attachment.EntityID,
		Attachment:   &attachment,
		ExecutionKey: frame.EvaluationID + "|detach|" + attachment.TriggerRef.ID,
	})
}

// attachedFrame restores the attaching invocation and points the addressee at
// the bearer: the player for shape A, the dying rat for shape B.
func (evaluator *Evaluator) attachedFrame(attachment Attachment) Frame {
	frame := attachment.Frame
	frame.Addressee = attachment.EntityID
	return frame
}

func (kind EventKind) String() string {
	switch kind {
	case EventHealthChanged:
		return "health-changed"
	case EventEquipChanged:
		return "equip-changed"
	default:
		return fmt.Sprintf("event(%d)", uint8(kind))
	}
}

// deliver offers an event to one effect. An effect that does not read this event
// kind does nothing and says so quietly: a trigger document holds several
// effects and only one of them is usually about the event that arrived.
func (evaluator *Evaluator) deliver(ctx context.Context, node *Node, frame Frame, event Event) error {
	run, err := evaluator.admit(node, frame, "effect is outside the M3 implemented tier")
	if err != nil || !run {
		return err
	}

	switch node.Opcode {
	case "EquipTrigger":
		return evaluator.deliverEquip(ctx, node, frame, event)
	case "HealthTrigger":
		return evaluator.deliverHealth(ctx, node, frame, event)
	case "Switch", "EffectTrigger":
		// A lifecycle effect has no opinion about events; it runs on attach and
		// detach. Reaching one here means it sat directly under the trigger
		// rather than under a gate, which is legal and simply not this event.
		return nil
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no trigger effect handler registered",
		}
	}
}

// deliverEquip is shape A's gate. DressTrigger holds two EquipTrigger effects,
// one for MAINHAND and one for TWOHANDED, because a two-handed weapon does not
// occupy the main hand and the objective is "arm yourself" either way.
//
// A gate does not run impacts. It activates the effects inside it, and in
// DressTrigger that is a Switch whose impactsOn holds the increment.
func (evaluator *Evaluator) deliverEquip(ctx context.Context, node *Node, frame Frame, event Event) error {
	if event.Kind != EventEquipChanged || !event.Equipped {
		return nil
	}
	slot, ok := node.Field("slot")
	if !ok || slot.Kind != ValueText {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"slot\" is missing or is not text",
		}
	}
	if slot.Text != event.Slot {
		return nil
	}
	for _, inner := range node.Nodes("effects") {
		if err := evaluator.activate(ctx, inner, frame); err != nil {
			return err
		}
	}
	return nil
}

// deliverHealth is shape B's gate, and the crossing test is the whole of it.
//
// The threshold comes from the healthOn calcer, evaluated against the bearer, so
// FullHealthCalcer(multiplier=0) is zero and the trigger is a death watch. The
// test is a crossing and not a level because a corpse stays at zero: a level
// test would re-fire the increment on every later health event the host
// delivered for that entity, and quest 1-30 would complete on one rat.
//
// impactsOff is deliberately not fired from here. The reflection schema in
// Types/types.xml describes HealthTrigger as firing impacts when health drops
// below a level, with "the second set called on detach, if the first was called
// first — their meaning is cleanup". So impactsOff is detach-time cleanup, not
// an upward crossing, and it lives in deactivate.
//
// healthOff is therefore read as the re-arm threshold rather than as a second
// firing edge. A pure crossing model re-arms on its own — health has to climb
// back above healthOn before it can cross down again — so nothing in the
// tutorial depends on the difference. RatKiller's healthOff is FloatZero, which
// never re-arms anything. Stateful hysteresis, and the "only if the first
// fired" gate that goes with it, is host state and waits for the session
// adapter that owns the registry.
func (evaluator *Evaluator) deliverHealth(ctx context.Context, node *Node, frame Frame, event Event) error {
	if event.Kind != EventHealthChanged {
		return nil
	}

	onThreshold, err := evaluator.calc(ctx, first(node.Nodes("healthOn")), frame)
	if err != nil {
		return err
	}
	crossedDown, err := crossedDownward(
		integerAmount(event.PreviousHealth), integerAmount(event.Health), onThreshold, node, frame,
	)
	if err != nil {
		return err
	}
	if !crossedDown {
		return nil
	}

	if err := evaluator.evalAll(ctx, node, "impactsOn", frame); err != nil {
		return err
	}
	// HealthTrigger's fifth field. The schema gives it healthOn, healthOff,
	// impactsOn, impactsOff and effects — a nested effect list that attaches
	// while the trigger is on. RatKiller does not use it, but
	// IL_QuestSpells/SummonZombie.(AbilityResource).xdb does, and it is inside
	// the tutorial's reach.
	for _, inner := range node.Nodes("effects") {
		if err := evaluator.activate(ctx, inner, frame); err != nil {
			return err
		}
	}
	return nil
}

// crossedDownward reports whether health moved from above a threshold to at or
// below it during this event.
func crossedDownward(previous, current, threshold amount, node *Node, frame Frame) (bool, error) {
	was, wasOK := previous.compare(threshold)
	is, isOK := current.compare(threshold)
	if !wasOK || !isOK {
		return false, thresholdRefusal(node, frame, threshold)
	}
	return was > 0 && is <= 0, nil
}

func thresholdRefusal(node *Node, frame Frame, threshold amount) error {
	return &RefusedError{
		SourceID: frame.SourceID, NodeKey: node.Key,
		Family: node.Family, Opcode: node.Opcode,
		Reason: fmt.Sprintf("health threshold %s is not comparable with the event's health", threshold),
	}
}

func first(nodes []*Node) *Node {
	if len(nodes) == 0 {
		return nil
	}
	return nodes[0]
}

// activate runs an effect's on-branch. It is reached when a gate opens and when
// a lifecycle effect attaches.
func (evaluator *Evaluator) activate(ctx context.Context, node *Node, frame Frame) error {
	run, err := evaluator.admit(node, frame, "effect is outside the M3 implemented tier")
	if err != nil || !run {
		return err
	}
	switch node.Opcode {
	case "Switch", "EffectTrigger":
		return evaluator.evalAll(ctx, node, "impactsOn", frame)
	case "EquipTrigger", "HealthTrigger":
		// Arming a gate runs nothing. Its impacts wait for an event.
		return nil
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no trigger effect handler registered",
		}
	}
}

// deactivate runs an effect's off-branch.
func (evaluator *Evaluator) deactivate(ctx context.Context, node *Node, frame Frame) error {
	run, err := evaluator.admit(node, frame, "effect is outside the M3 implemented tier")
	if err != nil || !run {
		return err
	}
	switch node.Opcode {
	case "Switch", "EffectTrigger", "HealthTrigger":
		// HealthTrigger belongs here rather than in deliver: its impactsOff is
		// detach-time cleanup, as the schema describes it, not a second firing
		// edge.
		return evaluator.evalAll(ctx, node, "impactsOff", frame)
	case "EquipTrigger":
		return nil
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no trigger effect handler registered",
		}
	}
}

// --- binding --------------------------------------------------------------

// evalAttachTrigger is shape B's binder, reached from inside ImpactFindSpawnTable
// so that it runs once per mob of the table with that mob as the addressee.
func evalAttachTrigger(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	return evaluator.attach(ctx, node, frame, frame.Addressee)
}

// evalTriggerAgent is shape A's binder. The agent opcode names whom to bind to,
// which is the only difference between the four TriggerAgent types.
func evalTriggerAgent(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	var bearer string
	switch node.Opcode {
	case "TriggerAgentSelf":
		// Self is the quest's own character. Quest_1_20 binds DressTrigger this
		// way, and the objective is about what that player equips.
		bearer = frame.Addressee
	case "TriggerAgentInterlocutor":
		bearer = frame.InterlocutorID
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "no trigger agent handler registered",
		}
	}
	if bearer == "" {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the invocation names no entity to bind the trigger to",
		}
	}
	return evaluator.attach(ctx, node, frame, bearer)
}

// attach turns a trigger reference into a host command. The trigger is named by
// reference rather than inlined, because a TriggerResource is its own content
// row and ADR 0036 resolves hrefs to a canonical content id at extraction. The
// host loads the row; an inline node is accepted too, so a fixture and a pack
// take the same path.
func (evaluator *Evaluator) attach(ctx context.Context, node *Node, frame Frame, bearer string) error {
	value, ok := node.Field("trigger")
	if !ok {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"trigger\" is missing",
		}
	}

	attachment := Attachment{EntityID: bearer, Frame: frame}
	// detachesOnDeath is the other field on the TriggerAgentResource base, and
	// it is lifetime rather than behaviour, so it rides on the attachment for
	// the host registry to honour. Quest_1_20 omits it.
	if lifetime, ok := node.Field("detachesOnDeath"); ok && lifetime.Kind == ValueBool {
		attachment.DetachesOnDeath = lifetime.Bool
	}
	switch value.Kind {
	case ValueRef:
		attachment.TriggerRef = value.Ref
	case ValueNode:
		attachment.Trigger = value.Node
		if value.Node != nil {
			attachment.TriggerRef = Ref{ID: value.Node.Key, RowType: "trigger"}
		}
	default:
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"trigger\" is neither a content reference nor an inline trigger",
		}
	}

	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandAttachTrigger,
		EntityID:     bearer,
		Ref:          attachment.TriggerRef,
		Attachment:   &attachment,
		ExecutionKey: frame.EvaluationID + "|" + node.Key + "|" + bearer,
	})
}

// evalFindSpawnTable resolves a spawn table to its live mobs and runs its
// impacts once per mob, with that mob as the addressee. Resolve returns ids in
// bytewise order, so attaching to twelve rats is the same twelve commands in the
// same order on every run.
func evalFindSpawnTable(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	table, ok := node.Field("spawnResource")
	if !ok || table.Kind != ValueRef {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"spawnResource\" is missing or is not a spawn table reference",
		}
	}

	entities, err := evaluator.host.Resolve(ctx, ResolveRequest{
		Finder: node.Opcode, Frame: frame, Ref: table.Ref,
	})
	if err != nil {
		return fmt.Errorf("resolve spawn table %s: %w", table.Ref.ID, err)
	}

	for _, entity := range entities {
		scoped := frame
		scoped.Addressee = entity
		if err := evaluator.evalAll(ctx, node, "impacts", scoped); err != nil {
			return err
		}
	}
	return nil
}

// evalReturningImpact re-dispatches its wrapped impact against the invocation's
// caster instead of the current addressee.
//
// This is the hinge of shape B: the addressee is the dying rat and the caster is
// whoever killed it, so without the redirect the quest counter would be
// incremented on a corpse. The reading is the survey's open question 1, inferred
// from RatKiller and confirmed against the second, independent use in
// Mechanics/Spells/Warrior.
func evalReturningImpact(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	if frame.CasterID == "" {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "the invocation names no caster to return to",
		}
	}
	returned := frame
	returned.Addressee = frame.CasterID
	return evaluator.evalAll(ctx, node, "impact", returned)
}

// evalTagMobForKill marks the addressee as quest-relevant, so that credit and
// loot follow the tag rather than the aggro table. It sits beside
// ImpactAttachTrigger inside every shape B spawn-table block.
func evalTagMobForKill(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandTagMobForKill,
		EntityID:     frame.Addressee,
		ExecutionKey: frame.EvaluationID + "|" + node.Key + "|" + frame.Addressee,
	})
}
