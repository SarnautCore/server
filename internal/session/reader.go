package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

// ErrClientLogout reports the clean exit a client asks for with
// ClientMessage.logout. It is not a failure: the handler unwinds through the
// same teardown as any other return, with the save checkpoint ahead of it.
var ErrClientLogout = errors.New("client requested logout")

// ProtocolViolation is a refusal the shard has already reported to the peer as
// a ServerMessage.error before returning. The connection is closed afterwards:
// ProtocolVersion is compared for exact equality at both ends, so a frame the
// dispatch table does not recognise is a protocol violation and not a
// forward-compatibility event (ADR 0026).
type ProtocolViolation struct {
	Code   sarnautv1.ErrorCode
	Detail string
}

func (violation *ProtocolViolation) Error() string {
	return fmt.Sprintf("protocol violation %s: %s", violation.Code, violation.Detail)
}

// carrier names the channel one envelope arrived on. Both carriers move whole
// envelopes and share one dispatch table; only the eligibility rules differ.
type carrier int

const (
	carrierReliable carrier = iota
	carrierUnreliable
)

func (via carrier) String() string {
	if via == carrierUnreliable {
		return "unreliable"
	}
	return "reliable"
}

// reliableWriter serialises the ordered stream. Two goroutines write to it: the
// snapshot sender on its datagram fallback, and the command readers reporting a
// protocol violation.
type reliableWriter struct {
	mu         sync.Mutex
	connection transport.Connection
}

func newReliableWriter(connection transport.Connection) *reliableWriter {
	return &reliableWriter{connection: connection}
}

func (writer *reliableWriter) write(message proto.Message) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return transport.WriteMessage(writer.connection, message)
}

// commandReader turns client envelopes into zone commands. One instance serves
// both reader goroutines, so the eligibility rules are stated once.
type commandReader struct {
	connection transport.Connection
	writer     *reliableWriter
	zone       *world.Zone
	combat     *combat.Module
	loot       *loot.Module
	quests     *quests.Module
	// character is this session's view of its own persisted state. The loot
	// take refreshes it, because the award commits the bag and the session's
	// next checkpoint must not write the pre-loot one back over it.
	character *characterSession
	entityID  uint64
	// characterID is the identity the quest log is keyed on. Unlike entityID it
	// survives a reconnect, which is why the quest module is addressed by it.
	characterID uuid.UUID
	datagrams   bool
	span        trace.Span
}

// readReliable drains the ordered stream. It is always started, whether or not
// datagrams were negotiated: without it a reliable client frame would sit in
// the QUIC receive buffer until flow control stalled the connection
// (ADR 0026).
//
// The context is accepted for symmetry with readUnreliable and deliberately
// ignored: stream.Read does not observe cancellation, so the supervisor closes
// the connection to unblock this loop.
func (reader *commandReader) readReliable(_ context.Context) error {
	for {
		message := new(sarnautv1.ClientMessage)
		if err := transport.ReadMessage(reader.connection, message); err != nil {
			return fmt.Errorf("read client message: %w", err)
		}
		if err := reader.dispatch(message, carrierReliable); err != nil {
			return err
		}
	}
}

// readUnreliable drains datagrams. It runs only when the transport negotiated
// them, and it is the only carrier a move intent may use while it runs.
func (reader *commandReader) readUnreliable(ctx context.Context) error {
	for {
		payload, err := reader.connection.ReceiveUnreliable(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive client datagram: %w", err)
		}
		message := new(sarnautv1.ClientMessage)
		if err := transport.UnmarshalUnreliable(payload, message); err != nil {
			return reader.refuse(
				sarnautv1.ErrorCode_ERROR_CODE_MALFORMED_PAYLOAD,
				"client datagram is not a ClientMessage",
			)
		}
		if err := reader.dispatch(message, carrierUnreliable); err != nil {
			return err
		}
	}
}

func (reader *commandReader) dispatch(message *sarnautv1.ClientMessage, via carrier) error {
	switch payload := message.GetPayload().(type) {
	case *sarnautv1.ClientMessage_MoveIntent:
		return reader.applyMoveIntent(payload.MoveIntent, via)
	case *sarnautv1.ClientMessage_Logout:
		if via != carrierReliable {
			return reader.refuseCarrier("logout", via)
		}
		return ErrClientLogout
	case *sarnautv1.ClientMessage_AbilityUse:
		if via != carrierReliable {
			// Combat is never a datagram (ADR 0026, combat.md rule 5.2.1).
			return reader.refuseCarrier("ability_use", via)
		}
		return reader.useAbility(payload.AbilityUse, message.GetClientSeq())
	case *sarnautv1.ClientMessage_Interact:
		if via != carrierReliable {
			return reader.refuseCarrier("interact", via)
		}
		return reader.interact(payload.Interact)
	case *sarnautv1.ClientMessage_LootTake:
		if via != carrierReliable {
			// Looting is never a datagram: rule 5.6 answers with what was
			// committed, and an answer that may be dropped is not an answer.
			return reader.refuseCarrier("loot_take", via)
		}
		return reader.lootTake(payload.LootTake)
	case *sarnautv1.ClientMessage_QuestAccept:
		if via != carrierReliable {
			// A quest verb is never a datagram: it answers with what was
			// committed, and an answer that may be dropped is not an answer.
			return reader.refuseCarrier("quest_accept", via)
		}
		return reader.questAccept(payload.QuestAccept)
	case *sarnautv1.ClientMessage_QuestTurnIn:
		if via != carrierReliable {
			return reader.refuseCarrier("quest_turn_in", via)
		}
		return reader.questTurnIn(payload.QuestTurnIn)
	case *sarnautv1.ClientMessage_QuestAbandon:
		if via != carrierReliable {
			return reader.refuseCarrier("quest_abandon", via)
		}
		return reader.questAbandon(payload.QuestAbandon)
	default:
		return reader.refuse(
			sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE,
			fmt.Sprintf("client message on the %s channel carries no supported payload case", via),
		)
	}
}

func (reader *commandReader) applyMoveIntent(intent *sarnautv1.ClientMoveIntent, via carrier) error {
	if reader.datagrams && via == carrierReliable {
		// The client does not get to pick a carrier per frame: latest-value-wins
		// movement on the ordered stream would reintroduce the head-of-line
		// stall datagrams exist to avoid.
		return reader.refuseCarrier("move_intent", via)
	}
	decoded, err := moveIntentFromProto(intent)
	if err != nil {
		reader.span.RecordError(err)
		return nil
	}
	if err := reader.zone.ApplyMoveIntent(reader.entityID, decoded); err != nil {
		// A malformed intent is dropped, not fatal: movement is
		// latest-value-wins and the next sample is 50 ms away.
		reader.span.RecordError(err)
	}
	return nil
}

// useAbility routes one ability use into the combat module.
//
// A refusal is not a protocol violation and does not end the session: the
// module has already published a CombatEvent carrying the reason, which is the
// answer the client gets. Only the absence of a combat module is worth an
// error frame, because then the verb can never be served here.
func (reader *commandReader) useAbility(use *sarnautv1.AbilityUse, clientSeq uint64) error {
	if reader.combat == nil {
		return reader.refuse(
			sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE,
			"this zone hosts no combat module",
		)
	}
	if _, err := reader.combat.UseAbility(reader.entityID, abilityRequestFromProto(use, clientSeq)); err != nil {
		reader.span.RecordError(err)
	}
	return nil
}

func (reader *commandReader) refuseCarrier(name string, via carrier) error {
	return reader.refuse(
		sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE,
		fmt.Sprintf("%s is not eligible for the %s channel", name, via),
	)
}

func (reader *commandReader) refuse(code sarnautv1.ErrorCode, detail string) error {
	violation := &ProtocolViolation{Code: code, Detail: detail}
	reader.span.RecordError(violation)
	message := &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_Error{
			Error: &sarnautv1.Error{Code: code, Detail: detail},
		},
	}
	if err := reader.writer.write(message); err != nil {
		return errors.Join(violation, fmt.Errorf("write protocol error: %w", err))
	}
	return violation
}
