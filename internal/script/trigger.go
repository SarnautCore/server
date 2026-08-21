package script

import (
	"context"
	"errors"
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
	if attachment.EntityID == "" {
		return fmt.Errorf(
			"script: attachment of %s is spawn-scoped to mob world %s; the host materializes a per-entity view before firing",
			attachment.TriggerRef.ID, attachment.MobWorld.ID,
		)
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

// ActivateAttachment attaches the persistent effects in one resolved trigger
// in authored order. The host calls it once before publishing the attachment
// as live. A replay is safe because persistent commands are idempotent by
// EffectID.
func (evaluator *Evaluator) ActivateAttachment(ctx context.Context, attachment Attachment) error {
	if !evaluator.options.Enabled {
		return ErrDisabled
	}
	if attachment.EntityID == "" || attachment.ID == "" {
		return fmt.Errorf("script: attachment activation requires an entity and attachment id")
	}
	if attachment.Trigger == nil {
		return fmt.Errorf("script: attachment %s carries no resolved trigger", attachment.ID)
	}
	frame := evaluator.attachedFrame(attachment)
	evaluator.lifecycleOrdinal++
	frame.LifecycleAttempt = evaluator.lifecycleOrdinal
	frame.Event = "attach"
	run, err := evaluator.admit(attachment.Trigger, frame, "trigger is outside the M3 implemented tier")
	if err != nil || !run {
		return err
	}
	effects := attachment.Trigger.Nodes("effects")
	for index, effect := range effects {
		if err := evaluator.activate(ctx, effect, frame); err != nil {
			cleanup := newAttachmentCleanup(attachment, frame, effects[:index], true, false)
			failure := &attachmentActivationError{causes: []error{err}, cleanup: &cleanup}
			if rollbackErr := evaluator.ContinueAttachmentCleanup(ctx, failure.cleanup); rollbackErr != nil {
				failure.causes = append(failure.causes, rollbackErr)
			} else {
				failure.cleanup = nil
			}
			return failure
		}
	}
	return nil
}

type attachmentActivationError struct {
	causes  []error
	cleanup *AttachmentCleanup
}

func (failure *attachmentActivationError) Error() string {
	return errors.Join(failure.causes...).Error()
}
func (failure *attachmentActivationError) Unwrap() []error { return failure.causes }

// AttachmentRollbackCleanup returns the exact unfinished compensation from a
// failed activation. It retains rollback intent, the activation attempt, and
// only the effects that have not yet cleaned up.
func AttachmentRollbackCleanup(err error) (AttachmentCleanup, bool) {
	var failure *attachmentActivationError
	if !errors.As(err, &failure) || failure.cleanup == nil {
		return AttachmentCleanup{}, false
	}
	cleanup := *failure.cleanup
	cleanup.remaining = append([]attachmentCleanupStep(nil), failure.cleanup.remaining...)
	return cleanup, true
}

// AttachmentCleanup is resumable attachment cleanup owned by the host. It
// records progress so a retry never repeats an impactsOff branch or changes a
// replayed effect from an earlier activation attempt.
type AttachmentCleanup struct {
	attachment    Attachment
	frame         Frame
	remaining     []attachmentCleanupStep
	rollback      bool
	detachTrigger bool
}

type attachmentCleanupStepKind uint8

const (
	cleanupAdmitEffect attachmentCleanupStepKind = iota
	cleanupImpact
	cleanupPersistentEffect
	cleanupUnsupportedEffect
)

type attachmentCleanupStep struct {
	kind attachmentCleanupStepKind
	node *Node
	// skipAfterAdmit is the number of this effect's action steps. An inert
	// effect skips them without losing the plan's position.
	skipAfterAdmit int
}

// AttachmentID identifies the cleanup without exposing its mutable progress.
func (cleanup AttachmentCleanup) AttachmentID() string { return cleanup.attachment.ID }

func newAttachmentCleanup(
	attachment Attachment, frame Frame, effects []*Node, rollback, detachTrigger bool,
) AttachmentCleanup {
	remaining := make([]attachmentCleanupStep, 0, len(effects)*2)
	for index := len(effects) - 1; index >= 0; index-- {
		effect := effects[index]
		var actions []attachmentCleanupStep
		switch effect.Opcode {
		case "Switch", "EffectTrigger":
			for _, impact := range effect.Nodes("impactsOff") {
				actions = append(actions, attachmentCleanupStep{kind: cleanupImpact, node: impact})
			}
		case "HealthTrigger":
			if !rollback {
				for _, impact := range effect.Nodes("impactsOff") {
					actions = append(actions, attachmentCleanupStep{kind: cleanupImpact, node: impact})
				}
			}
		case "EquipTrigger":
		case "CombatStateTrigger":
		case "Guard", "ScalerAllInputDamage", "ScalerAllOutputDamage":
			actions = append(actions, attachmentCleanupStep{kind: cleanupPersistentEffect, node: effect})
		default:
			actions = append(actions, attachmentCleanupStep{kind: cleanupUnsupportedEffect, node: effect})
		}
		remaining = append(remaining, attachmentCleanupStep{
			kind: cleanupAdmitEffect, node: effect, skipAfterAdmit: len(actions),
		})
		remaining = append(remaining, actions...)
	}
	return AttachmentCleanup{
		attachment: attachment, frame: frame, remaining: remaining,
		rollback: rollback, detachTrigger: detachTrigger,
	}
}

// BeginAttachmentDetach validates a normal lifecycle detach and returns its
// resumable cleanup. The host retains the value until Continue succeeds.
func (evaluator *Evaluator) BeginAttachmentDetach(attachment Attachment) (AttachmentCleanup, error) {
	if !evaluator.options.Enabled {
		return AttachmentCleanup{}, ErrDisabled
	}
	frame := evaluator.attachedFrame(attachment)
	frame.Event = "detach"
	var effects []*Node
	if attachment.Trigger != nil {
		run, err := evaluator.admit(attachment.Trigger, frame, "trigger is outside the M3 implemented tier")
		if err != nil {
			return AttachmentCleanup{}, err
		}
		if run {
			effects = attachment.Trigger.Nodes("effects")
		}
	}
	return newAttachmentCleanup(attachment, frame, effects, false, true), nil
}

// ContinueAttachmentCleanup resumes at the first unfinished effect. Successful
// effects leave the plan immediately, so later retries cannot run them twice.
func (evaluator *Evaluator) ContinueAttachmentCleanup(
	ctx context.Context, cleanup *AttachmentCleanup,
) error {
	if cleanup == nil {
		return fmt.Errorf("script: nil attachment cleanup")
	}
	for len(cleanup.remaining) > 0 {
		step := cleanup.remaining[0]
		var err error
		switch step.kind {
		case cleanupAdmitEffect:
			var run bool
			run, err = evaluator.admit(step.node, cleanup.frame, "effect is outside the M3 implemented tier")
			if err == nil && !run {
				cleanup.remaining = cleanup.remaining[1+step.skipAfterAdmit:]
				continue
			}
		case cleanupImpact:
			err = evaluator.eval(ctx, step.node, cleanup.frame)
		case cleanupPersistentEffect:
			err = evaluator.deactivatePersistentEffect(
				ctx, step.node, cleanup.frame, cleanup.rollback,
			)
		case cleanupUnsupportedEffect:
			err = &RefusedError{
				SourceID: cleanup.frame.SourceID, NodeKey: step.node.Key,
				Family: step.node.Family, Opcode: step.node.Opcode,
				Reason: "no trigger effect handler registered",
			}
		default:
			err = fmt.Errorf("script: attachment cleanup has unknown step %d", step.kind)
		}
		if err != nil {
			return fmt.Errorf("cleanup node %s: %w", step.node.Key, err)
		}
		cleanup.remaining = cleanup.remaining[1:]
	}
	if !cleanup.detachTrigger {
		return nil
	}
	if err := evaluator.host.Apply(ctx, Command{
		Kind:         CommandDetachTrigger,
		EntityID:     cleanup.attachment.EntityID,
		Attachment:   &cleanup.attachment,
		ExecutionKey: cleanup.frame.EvaluationID + "|detach|" + cleanup.attachment.TriggerRef.ID,
	}); err != nil {
		return err
	}
	cleanup.detachTrigger = false
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
	cleanup, err := evaluator.BeginAttachmentDetach(attachment)
	if err != nil {
		return err
	}
	return evaluator.ContinueAttachmentCleanup(ctx, &cleanup)
}

// attachedFrame restores the attaching invocation and points the addressee at
// the bearer: the player for shape A, the dying rat for shape B.
func (evaluator *Evaluator) attachedFrame(attachment Attachment) Frame {
	frame := attachment.Frame
	frame.Addressee = attachment.EntityID
	frame.AttachmentID = attachment.ID
	return frame
}

func (kind EventKind) String() string {
	switch kind {
	case EventHealthChanged:
		return "health-changed"
	case EventEquipChanged:
		return "equip-changed"
	case EventCombatStateChanged:
		return "combat-state-changed"
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
	case "CombatStateTrigger":
		return evaluator.deliverCombatState(ctx, node, frame, event)
	case "EffectTrigger":
		return evaluator.deliverEffectEvent(ctx, node, frame, event)
	case "Switch", "Guard", "ScalerAllInputDamage", "ScalerAllOutputDamage":
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

func (evaluator *Evaluator) deliverEffectEvent(
	ctx context.Context, node *Node, frame Frame, event Event,
) error {
	if event.EventClass == "" {
		return nil
	}
	matched := false
	value, ok := node.Field("eventClasses")
	if !ok || value.Kind != ValueList {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"eventClasses\" is missing or is not a list",
		}
	}
	for _, entry := range value.List {
		if entry.Kind != ValueText || entry.Text == "" {
			return &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "field \"eventClasses\" contains a non-text or empty entry",
			}
		}
		matched = matched || entry.Text == event.EventClass
	}
	if !matched {
		return nil
	}
	sources := node.Nodes("eventsSource")
	if len(sources) != 1 {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"eventsSource\" must contain exactly one finder",
		}
	}
	source, err := evaluator.resolveFinderNode(ctx, sources[0], frame)
	if err != nil {
		return err
	}
	actualSource := event.SourceID
	if actualSource == "" {
		actualSource = event.EntityID
	}
	if source != actualSource {
		return nil
	}
	return evaluator.evalAll(ctx, node, "impacts", frame)
}

func (evaluator *Evaluator) deliverCombatState(
	ctx context.Context, node *Node, frame Frame, event Event,
) error {
	if event.Kind != EventCombatStateChanged {
		return nil
	}
	if event.InCombat {
		return evaluator.evalAll(ctx, node, "onEnter", frame)
	}
	return evaluator.evalAll(ctx, node, "onLeave", frame)
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
	case "Switch":
		return evaluator.evalAll(ctx, node, "impactsOn", frame)
	case "Guard", "ScalerAllInputDamage", "ScalerAllOutputDamage":
		return evaluator.activatePersistentEffect(ctx, node, frame)
	case "EquipTrigger", "HealthTrigger", "CombatStateTrigger", "EffectTrigger":
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

// --- binding --------------------------------------------------------------

// evalAttachTrigger is shape B's binder, reached from inside ImpactFindSpawnTable
// so that it runs once per mob of the table with that mob as the addressee.
func evalAttachTrigger(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	return evaluator.attach(ctx, node, frame, frame.Addressee, Ref{}, false)
}

// evalTriggerAgent covers the four TriggerAgent binders the tutorial reaches.
//
// TriggerAgentSelf and TriggerAgentInterlocutor bind to an entity the
// invocation already names. TriggerAgentSimple and TriggerAgentOnTagged bind
// across a MobWorld — the reflection schema gives both exactly one field over
// the TriggerAgentResource base, `mobWorld`, plus OnTagged's `onSelf` — so they
// emit a spawn-scoped attachment and the host registry owns which live mobs it
// lands on. Quest_4_10 is the Simple use (QuestCompl on LI_Necromancer) and
// Quest_2_10 the OnTagged one (GibberSummon on RuffianMageMiniboss2_2).
func evalTriggerAgent(ctx context.Context, evaluator *Evaluator, node *Node, frame Frame) error {
	var bearer string
	switch node.Opcode {
	case "TriggerAgentSelf":
		// Self is the quest's own character. Quest_1_20 binds DressTrigger this
		// way, and the objective is about what that player equips.
		bearer = frame.Addressee
	case "TriggerAgentInterlocutor":
		bearer = frame.InterlocutorID
	case "TriggerAgentSimple", "TriggerAgentOnTagged":
		world, ok := node.Field("mobWorld")
		if !ok || world.Kind != ValueRef {
			return &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "field \"mobWorld\" is missing or is not a MobWorld reference",
			}
		}
		// onSelf is OnTagged's other schema field. Nothing in the tutorial
		// spells it, so its semantics are unverified against data; false is the
		// schema default and is accepted, true is refused rather than guessed.
		if onSelf, ok := node.Field("onSelf"); ok && (onSelf.Kind != ValueBool || onSelf.Bool) {
			return &RefusedError{
				SourceID: frame.SourceID, NodeKey: node.Key,
				Family: node.Family, Opcode: node.Opcode,
				Reason: "field \"onSelf\" is outside the M3 implemented shape; no tutorial document sets it",
			}
		}
		return evaluator.attach(ctx, node, frame, "", world.Ref, node.Opcode == "TriggerAgentOnTagged")
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
	return evaluator.attach(ctx, node, frame, bearer, Ref{}, false)
}

// attach turns a trigger reference into a host command. The trigger is named by
// reference rather than inlined, because a TriggerResource is its own content
// row and ADR 0036 resolves hrefs to a canonical content id at extraction. The
// host loads the row; an inline node is accepted too, so a fixture and a pack
// take the same path.
//
// An empty bearer with a non-empty mobWorld is a spawn scope: the command names
// no entity, and the host registry decides which live and future mobs of that
// world the trigger lands on.
func (evaluator *Evaluator) attach(
	ctx context.Context, node *Node, frame Frame, bearer string, mobWorld Ref, onlyTagged bool,
) error {
	value, ok := node.Field("trigger")
	if !ok {
		return &RefusedError{
			SourceID: frame.SourceID, NodeKey: node.Key,
			Family: node.Family, Opcode: node.Opcode,
			Reason: "field \"trigger\" is missing",
		}
	}

	attachment := Attachment{EntityID: bearer, MobWorld: mobWorld, OnlyTagged: onlyTagged, Frame: frame}
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

	// The key's scope leg is whatever names the attachment's reach: the bearer
	// for an entity scope, the mob world for a spawn scope. Both are stable
	// across a replay, which is what an execution key is for.
	scope := bearer
	if scope == "" {
		scope = mobWorld.ID
	}
	attachment.ID = frame.EvaluationID + "|" + node.Key + "|" + scope
	return evaluator.host.Apply(ctx, Command{
		Kind:         CommandAttachTrigger,
		EntityID:     bearer,
		Ref:          attachment.TriggerRef,
		Attachment:   &attachment,
		ExecutionKey: attachment.ID,
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
