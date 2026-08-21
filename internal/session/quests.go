package session

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// This file is the quest half of the translation layer `mapping.go` describes:
// the wire schema on one side, `internal/quests` domain values on the other,
// and nothing else (ADR 0028).

// questGrantTimeout bounds the storage transaction one accept, turn-in or
// abandon may spend.
//
// The reader goroutine is blocked for its duration, which is the deliberate
// choice for the same reason the loot take makes it: rule 5.7 is all or nothing
// and answers with what was committed, so there is nothing useful to do
// concurrently. The bound exists so a wedged database ends the session instead
// of the session ending with it.
const questGrantTimeout = 5 * time.Second

// questQueueDepth bounds one session's quest-update backlog.
//
// Quest updates are not latest-wins the way snapshots are: a counter reaching
// its limit happens once and no later frame carries it again. The queue is deep
// enough to absorb a burst, and an overflow is recorded on the session span
// rather than being silent.
const questQueueDepth = 64

// questSender writes quest updates to one session's reliable stream.
//
// It exists so that the quest module's fan-out goroutine never blocks on a slow
// peer, and it is per-session because a quest log is replicated to its owner and
// to nobody else: there is no radius at which somebody else's journal becomes
// public.
type questSender struct {
	writer *reliableWriter
	span   trace.Span
	queue  chan quests.Update
}

func newQuestSender(writer *reliableWriter, span trace.Span) *questSender {
	return &questSender{
		writer: writer,
		span:   span,
		queue:  make(chan quests.Update, questQueueDepth),
	}
}

// OfferQuestUpdate queues one update. It is called from the quest module's
// fan-out goroutine and must not block, so a full queue drops and says so.
func (sender *questSender) OfferQuestUpdate(update quests.Update) {
	select {
	case sender.queue <- update:
	default:
		sender.span.AddEvent("session.quest_update.dropped", trace.WithAttributes(
			attribute.String("sarnaut.quest.id", update.QuestID),
		))
	}
}

func (sender *questSender) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-sender.queue:
			message, err := questUpdateToProto(update)
			if err != nil {
				// A domain update with no wire projection is a mapping bug, not
				// a peer problem. It is recorded and skipped rather than killing
				// a session that did nothing wrong.
				sender.span.RecordError(err)
				continue
			}
			if err := sender.writer.write(message); err != nil {
				return fmt.Errorf("write quest update: %w", err)
			}
		}
	}
}

// questInteract answers ClientMessage.interact against a quest giver.
//
// It reports whether the target was one. `interact` is the generic "use the
// thing I am looking at" verb and a corpse arrives on it too, so a target that
// gives no quest is not an error and the caller falls through to loot.
func (reader *commandReader) questInteract(targetEntityID uint64) (bool, error) {
	if reader.quests == nil {
		return false, nil
	}
	updates, refusal := reader.quests.Interact(reader.entityID, targetEntityID)
	if refusal == quests.RefusalNotAQuestGiver {
		return false, nil
	}
	if refusal != quests.RefusalNone {
		return true, reader.writeQuestUpdate(quests.Update{Refusal: refusal})
	}
	for _, update := range updates {
		if err := reader.writeQuestUpdate(update); err != nil {
			return true, err
		}
	}
	return true, nil
}

// questAccept, questTurnIn and questAbandon answer the three client verbs.
//
// A refusal is not a protocol violation and does not end the session: a full
// bag, an unfinished prerequisite or a double-click is ordinary play, and the
// QuestStateUpdate carrying the reason is the answer the client gets. Only the
// absence of a quest module is worth an error frame, because then the verb can
// never be served here.
func (reader *commandReader) questAccept(request *sarnautv1.QuestAccept) error {
	if err := reader.runQuestVerb(func(ctx context.Context) (quests.Result, error) {
		return reader.quests.Accept(ctx, reader.entityID, request.GetQuestId(), request.GetStarterEntityId())
	}); err != nil {
		return err
	}
	// A committed accept activates the quest's script surface: startImpacts
	// run and trigger agents bind. It happens after the client has its state
	// update, mirroring the authored order — a quest exists before its
	// impacts run — and it is a no-op on the default composition, where the
	// catalog admits no quest that would need it.
	if reader.lastQuestVerbCommitted {
		reader.scripts.QuestActivated(reader.entityID, request.GetQuestId())
	}
	return nil
}

func (reader *commandReader) questTurnIn(request *sarnautv1.QuestTurnIn) error {
	return reader.runQuestVerb(func(ctx context.Context) (quests.Result, error) {
		return reader.quests.TurnIn(ctx, reader.entityID, request.GetQuestId(), request.GetFinisherEntityId())
	})
}

func (reader *commandReader) questAbandon(request *sarnautv1.QuestAbandon) error {
	return reader.runQuestVerb(func(ctx context.Context) (quests.Result, error) {
		return reader.quests.Abandon(ctx, reader.entityID, request.GetQuestId())
	})
}

func (reader *commandReader) runQuestVerb(verb func(context.Context) (quests.Result, error)) error {
	reader.lastQuestVerbCommitted = false
	if reader.quests == nil {
		return reader.refuse(
			sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE,
			"this zone hosts no quest module",
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), questGrantTimeout)
	defer cancel()

	result, err := verb(ctx)
	if err != nil {
		// Every refusal already carries its reason in the update, so the error
		// is recorded for an operator and not turned into a frame of its own.
		reader.span.RecordError(err)
	}
	if err := reader.writeQuestUpdate(result.Update); err != nil {
		return err
	}
	if !result.Committed {
		return nil
	}
	reader.lastQuestVerbCommitted = true

	// The session's own view of the bag is refreshed before the client is told
	// anything else, so the next periodic checkpoint saves what the grant
	// committed rather than what zone entry loaded.
	if reader.character != nil {
		reader.character.adopt(result.Inventory, result.Currency, result.SaveSeq)
	}
	if reader.quests != nil && reader.characterID != uuid.Nil {
		reader.quests.InventoryChanged(reader.characterID, result.Inventory)
	}
	return reader.writer.write(&sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_InventoryUpdate{
			InventoryUpdate: &sarnautv1.InventoryUpdate{
				Slots:    inventorySlotsToProto(inventory.FromStore(result.Inventory)),
				Currency: result.Currency,
			},
		},
	})
}

func (reader *commandReader) writeQuestUpdate(update quests.Update) error {
	message, err := questUpdateToProto(update)
	if err != nil {
		reader.span.RecordError(err)
		return nil
	}
	return reader.writer.write(message)
}

func questUpdateToProto(update quests.Update) (*sarnautv1.ServerMessage, error) {
	state, err := questStateToProto(update.State)
	if err != nil {
		return nil, err
	}
	refusal, err := questRefusalToProto(update.Refusal)
	if err != nil {
		return nil, err
	}
	message := &sarnautv1.QuestStateUpdate{
		QuestId:    update.QuestID,
		State:      state,
		Refusal:    refusal,
		Experience: update.Experience,
		Money:      update.Money,
		Honor:      update.Honor,
	}
	for _, objective := range update.Objectives {
		message.Objectives = append(message.Objectives, &sarnautv1.QuestObjectiveProgress{
			Index:      objective.Index,
			Counter:    objective.Counter,
			Limit:      objective.Limit,
			ShowCount:  objective.ShowCount,
			CounterKey: objective.CounterKey,
		})
	}
	for _, item := range update.Items {
		message.Items = append(message.Items, &sarnautv1.LootItem{
			ItemId: item.ItemID,
			Count:  item.Count,
		})
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_QuestStateUpdate{QuestStateUpdate: message},
	}, nil
}

func questStateToProto(state quests.State) (sarnautv1.QuestState, error) {
	switch state {
	case quests.StateUnspecified:
		return sarnautv1.QuestState_QUEST_STATE_UNSPECIFIED, nil
	case quests.StateUnavailable:
		return sarnautv1.QuestState_QUEST_STATE_UNAVAILABLE, nil
	case quests.StateOffered:
		return sarnautv1.QuestState_QUEST_STATE_OFFERED, nil
	case quests.StateAccepted:
		return sarnautv1.QuestState_QUEST_STATE_ACCEPTED, nil
	case quests.StateInProgress:
		return sarnautv1.QuestState_QUEST_STATE_IN_PROGRESS, nil
	case quests.StateCompletable:
		return sarnautv1.QuestState_QUEST_STATE_COMPLETABLE, nil
	case quests.StateTurnedIn:
		return sarnautv1.QuestState_QUEST_STATE_TURNED_IN, nil
	case quests.StateAbandoned:
		return sarnautv1.QuestState_QUEST_STATE_ABANDONED, nil
	default:
		return 0, fmt.Errorf("quest state %d has no wire value", uint8(state))
	}
}

func questRefusalToProto(refusal quests.Refusal) (sarnautv1.QuestRefusal, error) {
	switch refusal {
	case quests.RefusalNone:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_NONE, nil
	case quests.RefusalUnknownQuest:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_UNKNOWN_QUEST, nil
	case quests.RefusalUnavailable:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_UNAVAILABLE, nil
	case quests.RefusalLogFull:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_LOG_FULL, nil
	case quests.RefusalOutOfRange:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_OUT_OF_RANGE, nil
	case quests.RefusalWrongNPC:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_WRONG_NPC, nil
	case quests.RefusalNotComplete:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_NOT_COMPLETE, nil
	case quests.RefusalAlreadyComplete:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_ALREADY_COMPLETE, nil
	case quests.RefusalBagFull:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_BAG_FULL, nil
	case quests.RefusalCannotCancel:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_CANNOT_CANCEL, nil
	case quests.RefusalInternal, quests.RefusalNotAQuestGiver:
		return sarnautv1.QuestRefusal_QUEST_REFUSAL_INTERNAL, nil
	default:
		return 0, fmt.Errorf("quest refusal %d has no wire value", uint8(refusal))
	}
}

// questRows is the seam a checkpoint reads a character's quest log across.
//
// It is an interface so that `characterSession` stays testable with no quest
// module, and so that the checkpoint cannot reach past the log into the
// module's other state. `*quests.Module` is the only implementation.
type questRows interface {
	Rows(characterID uuid.UUID) []charstore.QuestState
}
