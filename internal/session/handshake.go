// Package session owns connection-level protocol exchanges.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

var tracer = otel.Tracer("github.com/SarnautCore/server/internal/session")

// Client performs the client side of the shard session protocol.
type Client struct {
	ProtocolVersion sarnautv1.ProtocolVersion
	BuildID         string

	// PackID is the runtime pack digest this peer loaded (ADR 0029). Empty
	// means unverified, which the shard accepts only while it carries no pack
	// of its own.
	PackID string

	// Ticket is the opaque single-use shard ticket presented on
	// EnterZoneRequest (ADR 0030). Empty until the auth service exists.
	Ticket string
}

// Handshake sends a client hello and waits for the shard hello.
func (client Client) Handshake(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.ServerHello, error) {
	_, span := tracer.Start(ctx, "session.client.handshake")
	defer span.End()
	span.SetAttributes(attribute.String("network.peer.address", connection.RemoteAddr().String()))

	hello := &sarnautv1.ClientHello{
		ProtocolVersion: client.ProtocolVersion,
		BuildId:         client.BuildID,
		PackId:          client.PackID,
	}
	if err := transport.WriteMessage(connection, hello); err != nil {
		return nil, fmt.Errorf("write client hello: %w", err)
	}

	response := new(sarnautv1.ServerHello)
	if err := transport.ReadMessage(connection, response); err != nil {
		return nil, fmt.Errorf("read server hello: %w", err)
	}
	if response.GetProtocolVersion() != client.ProtocolVersion {
		return nil, fmt.Errorf(
			"server protocol version %s does not match client version %s",
			response.GetProtocolVersion(),
			client.ProtocolVersion,
		)
	}
	if client.PackID != "" && response.GetPackId() != client.PackID {
		return nil, fmt.Errorf(
			"server content pack %q does not match client pack %q",
			response.GetPackId(),
			client.PackID,
		)
	}

	return response, nil
}

// EnterZone requests admission and returns the authoritative player spawn.
func (client Client) EnterZone(
	connection transport.Connection,
	zoneID string,
) (*sarnautv1.EnterZoneResponse, error) {
	request := &sarnautv1.EnterZoneRequest{ZoneId: zoneID, Ticket: client.Ticket}
	if err := transport.WriteMessage(connection, request); err != nil {
		return nil, fmt.Errorf("write enter zone request: %w", err)
	}
	payload, err := transport.ReadFrame(connection)
	if err != nil {
		return nil, fmt.Errorf("read enter zone response: %w", err)
	}

	response := new(sarnautv1.EnterZoneResponse)
	if err := proto.Unmarshal(payload, response); err == nil && response.GetZoneId() == zoneID {
		return response, nil
	}
	// Two message types are possible at this position: the response, or the
	// refusal the shard writes when admission fails (protocol/session.md rule
	// 5.4.3). Protobuf is not self-describing, so the same bytes are tried
	// against both rather than reported as a zone-id mismatch, which is what a
	// refused client used to see.
	refusal := new(sarnautv1.ServerMessage)
	if err := proto.Unmarshal(payload, refusal); err == nil && refusal.GetError() != nil {
		return nil, &ProtocolViolation{
			Code:   refusal.GetError().GetCode(),
			Detail: refusal.GetError().GetDetail(),
		}
	}
	return nil, fmt.Errorf("server entered zone %q, want %q", response.GetZoneId(), zoneID)
}

// SendMoveIntent sends movement as a QUIC datagram when both peers support it.
// The datagram carries a whole ClientMessage, not a bare intent (ADR 0026).
func (client Client) SendMoveIntent(
	connection transport.Connection,
	intent *sarnautv1.ClientMoveIntent,
) error {
	envelope := &sarnautv1.ClientMessage{
		ClientSeq: intent.GetSeq(),
		Payload:   &sarnautv1.ClientMessage_MoveIntent{MoveIntent: intent},
	}
	if !connection.SupportsUnreliable() {
		return transport.WriteMessage(connection, envelope)
	}
	payload, err := transport.MarshalUnreliable(envelope)
	if err != nil {
		return fmt.Errorf("marshal move intent: %w", err)
	}
	if err := connection.SendUnreliable(payload); err != nil {
		return fmt.Errorf("send move intent: %w", err)
	}
	return nil
}

// SendCommand writes one client verb on the reliable channel. Everything except
// movement travels here, combat included.
func (client Client) SendCommand(
	connection transport.Connection,
	message *sarnautv1.ClientMessage,
) error {
	if err := transport.WriteMessage(connection, message); err != nil {
		return fmt.Errorf("write client command: %w", err)
	}
	return nil
}

// Logout asks for a clean exit so the shard's save checkpoint runs ahead of the
// disconnect rather than racing it.
func (client Client) Logout(connection transport.Connection) error {
	return client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Logout{Logout: new(sarnautv1.Logout)},
	})
}

// ReadServerMessage receives one server envelope from the carrier this
// connection negotiated.
func (client Client) ReadServerMessage(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.ServerMessage, error) {
	message := new(sarnautv1.ServerMessage)
	if !connection.SupportsUnreliable() {
		if err := transport.ReadMessage(connection, message); err != nil {
			return nil, fmt.Errorf("read server message: %w", err)
		}
		return message, nil
	}
	payload, err := connection.ReceiveUnreliable(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive server message: %w", err)
	}
	if err := transport.UnmarshalUnreliable(payload, message); err != nil {
		return nil, fmt.Errorf("decode server message: %w", err)
	}
	return message, nil
}

// ReadReliableMessage receives one server envelope from the ordered stream,
// whatever the connection negotiated. Errors and events always arrive here.
func (client Client) ReadReliableMessage(
	connection transport.Connection,
) (*sarnautv1.ServerMessage, error) {
	message := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(connection, message); err != nil {
		return nil, fmt.Errorf("read server message: %w", err)
	}
	return message, nil
}

// ReadSnapshot receives server envelopes until a whole tick has arrived. A
// typed refusal is returned as an error; any other case is skipped, because a
// snapshot reader is not the place to handle combat.
//
// A tick the shard had to split across datagrams arrives as chunk_count batches
// sharing one server_tick (protocol/session.md rule 5.5.7). They are merged here
// rather than handed up one at a time: a chunk is a fragment of the world, and a
// caller that treated one as the world would conclude every entity in a sibling
// chunk had vanished. An incomplete tick is abandoned the moment a newer one
// starts, because snapshot delivery is lossy by design and waiting for a lost
// chunk would stall replication behind it.
func (client Client) ReadSnapshot(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.SnapshotBatch, error) {
	assembly := new(snapshotAssembly)
	for {
		message, err := client.ReadServerMessage(ctx, connection)
		if err != nil {
			return nil, err
		}
		switch payload := message.GetPayload().(type) {
		case *sarnautv1.ServerMessage_SnapshotBatch:
			if whole := assembly.add(payload.SnapshotBatch); whole != nil {
				return whole, nil
			}
		case *sarnautv1.ServerMessage_Error:
			return nil, &ProtocolViolation{
				Code:   payload.Error.GetCode(),
				Detail: payload.Error.GetDetail(),
			}
		default:
			continue
		}
	}
}

// snapshotAssembly reassembles one tick's chunks. It holds at most one
// in-progress tick: chunks of an older tick are worthless once a newer one has
// started, so there is nothing to age out and no unbounded buffer for a peer to
// grow by sending chunk_index values it never completes.
type snapshotAssembly struct {
	tick   uint64
	count  uint32
	chunks map[uint32]*sarnautv1.SnapshotBatch
}

// add takes one batch and returns the whole tick once every chunk of it has
// arrived, or nil while it is still incomplete.
func (assembly *snapshotAssembly) add(batch *sarnautv1.SnapshotBatch) *sarnautv1.SnapshotBatch {
	// A count of 0 comes from a batch that was never chunked at all, which is
	// every batch the reliable fallback carries and every snapshot that fits in
	// one datagram. Treating it as a one-chunk tick keeps one code path.
	count := batch.GetChunkCount()
	if count <= 1 {
		// A whole tick. Anything half-assembled is stale the moment one arrives.
		assembly.chunks = nil
		return batch
	}
	if assembly.chunks != nil && batch.GetServerTick() < assembly.tick {
		// A chunk of a tick that has already been overtaken. Datagrams reorder;
		// resurrecting the older tick would publish the world backwards.
		return nil
	}
	if assembly.chunks == nil || batch.GetServerTick() != assembly.tick {
		assembly.tick = batch.GetServerTick()
		assembly.count = count
		assembly.chunks = make(map[uint32]*sarnautv1.SnapshotBatch, count)
	}
	if batch.GetChunkIndex() >= count {
		// A chunk that claims to be past the end of its own tick. Dropping it is
		// the conservative read: the tick simply never completes and the next one
		// replaces it.
		return nil
	}
	assembly.chunks[batch.GetChunkIndex()] = batch
	if uint32(len(assembly.chunks)) != assembly.count {
		return nil
	}

	whole := &sarnautv1.SnapshotBatch{ServerTick: assembly.tick, ChunkCount: 1}
	for index := uint32(0); index < assembly.count; index++ {
		whole.Entities = append(whole.Entities, assembly.chunks[index].GetEntities()...)
	}
	assembly.chunks = nil
	return whole
}

// Server accepts sessions and binds admitted players to configured zones.
type Server struct {
	ProtocolVersion sarnautv1.ProtocolVersion
	BuildID         string

	// PackID is the runtime pack digest this shard loaded (ADR 0029). The shard
	// binary sets it from the pack it actually opened. While it is empty the
	// shard makes no content-identity claim and gates nothing, which is what the
	// transport-level tests rely on; a shard that leaves it empty while serving
	// real content also answers ServerHello with an empty pack_id, and every
	// client that names its own pack then refuses the handshake.
	PackID string

	// AllowUnverifiedPack admits a client that names no pack at all while this
	// shard names one (protocol/session.md rule 5.1.4). Default false: two peers
	// that agree on message shape and disagree on content tables produce
	// plausible-looking wrong gameplay, which costs far more to diagnose than a
	// refused connection. It has no effect on a client that names a *different*
	// pack, which is always refused.
	AllowUnverifiedPack bool

	Zones map[string]ZoneBinding

	// Authority redeems the ticket an EnterZoneRequest carries (ADR 0030,
	// ADR 0033 §3). A Server with no Authority admits nobody: identity is not
	// something a shard is allowed to assume, and failing closed is the only
	// safe default for a security check.
	Authority Authority

	// Characters loads a character at zone entry and takes the checkpoints of
	// protocol/session.md §5.7. Required for the same reason: a session with
	// nowhere to save is a session that loses the player's progress silently.
	Characters CharacterStore

	// Chat is the shard-wide authenticated chat router. Nil keeps chat
	// fail-closed while preserving typed ChatRejection responses.
	Chat *chat.Module

	// Friends publishes complete authoritative friend identity/name snapshots
	// used by recipient-local systems such as AntiSpam. It never supplies a
	// server-side spam score.
	Friends *social.Friends

	// Party observes authenticated session lifecycles. Group chat consumes the
	// same authority's audience snapshots, so a connection cannot claim cohort
	// membership through a client payload or stale teardown.
	Party PartySessions

	// Cohorts publishes authenticated online presence for guild and raid
	// recipient authorities. Durable membership remains outside the session.
	Cohorts CohortSessions

	// LocalChat admits authenticated player entities to the world-owned Say
	// topology. Its authority also serves Chat.Options.SayAudience.
	LocalChat LocalChatSessions

	// SaveInterval is checkpoint S2's cadence. Zero means
	// [DefaultSaveInterval].
	SaveInterval time.Duration

	// AdmissionTimeout bounds how long an unidentified peer may hold a
	// connection: hello through ticket redemption. Zero means
	// [DefaultAdmissionTimeout].
	AdmissionTimeout time.Duration

	Logger *slog.Logger

	// sessions arbitrates two connections for one character. It is created by
	// Serve, so every handler a Serve call spawns shares one registry.
	sessions *sessionRegistry
}

// PartySessions is the narrow authenticated lifecycle seam used by a shard
// session. The party module supplies the corresponding audience reader to chat.
type PartySessions interface {
	Connect(party.Actor) party.Refusal
	Disconnect(party.Actor) party.Refusal
}

type CohortSessions interface {
	Connect(uuid.UUID) (func(), error)
}

// LocalChatSessions binds authenticated world entities to the Say audience.
type LocalChatSessions interface {
	Admit(uuid.UUID, uint64, gametypes.Zone) (LocalChatSession, error)
}

type LocalChatSession interface {
	Close()
}

// ZoneBinding is the set of modules that serve one hosted zone.
//
// They are bound together rather than held in parallel maps because a session
// needs both: the zone admits it, and combat gives it a level, a faction and
// somewhere to send an ability use. Combat may be nil, which is what the
// transport-level tests use; a session in a zone with no combat module simply
// refuses ability use.
type ZoneBinding struct {
	World  *world.Zone
	Combat *combat.Module
	// Loot may be nil, in which case a session in this zone refuses loot_take
	// and ignores interact. A zone with combat but no loot is a legal, if
	// unrewarding, composition; a zone with loot but no combat has nothing to
	// create a corpse.
	Loot *loot.Module
	// Quests may be nil, in which case a session in this zone refuses the three
	// quest verbs and interact falls through to the loot handler.
	Quests *quests.Module
	// Scripts is the impact interpreter's adapter, and nil is the default
	// composition: no script evaluates, no zone hook fires, and the quest
	// catalog keeps skipping count-special definitions. Wiring it is the
	// flag-on path that lets those quests progress.
	Scripts *ScriptDriver
}

// DefaultSaveInterval is protocol/session.md's PERIODIC_SAVE_INTERVAL_S: the
// ceiling on progress an unclean shard exit can destroy.
const DefaultSaveInterval = 60 * time.Second

// DefaultAdmissionTimeout is how long an unidentified peer may take to get from
// an accepted connection to a redeemed ticket. It is generous by the standards
// of the exchange — two writes and a NATS round trip — and finite by the
// standards of an attacker, which is the whole point.
const DefaultAdmissionTimeout = 10 * time.Second

// refusalDrainGrace is how long a refused connection is left half-closed so the
// peer can read the refusal frame. It is a full second because it has to cover a
// loaded client on a real network, not a local read that was already scheduled;
// it is only a second because the alternative to closing at all is letting a
// rejected peer hold a connection until the idle timeout.
const refusalDrainGrace = time.Second

// closeAfterRefusal ends a connection, giving a typed refusal a window to reach
// the peer first.
//
// A QUIC CONNECTION_CLOSE does not wait for queued stream data to be read, so
// closing outright immediately after writing a ServerMessage.error discards the
// reason and leaves the peer with an opaque connection abort. Half-closing
// flushes the FIN behind the frame; the grace period is what gives the peer time
// to read it. Every other cause closes at once — a healthy session has nothing
// left to say, and a peer that is already gone must not hold a goroutine.
func closeAfterRefusal(ctx context.Context, connection transport.Connection, cause error) {
	var violation *ProtocolViolation
	if errors.As(cause, &violation) {
		_ = connection.CloseWrite()
		select {
		case <-time.After(refusalDrainGrace):
		case <-ctx.Done():
		}
	}
	_ = connection.Close()
}

func (server Server) logger() *slog.Logger {
	if server.Logger != nil {
		return server.Logger
	}
	return slog.Default()
}

// Serve handles connections until the context ends or the listener fails.
func (server Server) Serve(ctx context.Context, listener transport.Listener) error {
	if server.sessions == nil {
		server.sessions = newSessionRegistry()
	}
	for {
		connection, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept session: %w", err)
		}

		go func() {
			err := server.handle(ctx, connection)
			if err == nil {
				_ = connection.CloseWrite()
				return
			}
			closeAfterRefusal(ctx, connection, err)
		}()
	}
}

// handle admits one session and then supervises it. It owns no I/O loop of its
// own: the reliable reader, the optional datagram reader and the snapshot
// sender each run as a goroutine, and handle returns only once every one of
// them has stopped, so the deferred Zone.Leave cannot run while a sink is still
// in use (ADR 0026).
func (server Server) handle(ctx context.Context, connection transport.Connection) error {
	ctx, span := tracer.Start(ctx, "session.server")
	defer span.End()
	span.SetAttributes(attribute.String("network.peer.address", connection.RemoteAddr().String()))

	// Nothing below observes a context on its own: a transport read blocks on the
	// QUIC stream and cancellation does not touch it, so closing the connection
	// is the only way to unblock one. Both guards are armed here, before the
	// first read, and not after admission as they used to be — the pre-admission
	// reads are exactly the ones an unauthenticated peer controls.
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
			// handle returned under its own power; it owns the close, including
			// the drain window a typed refusal needs.
			return
		case <-sessionCtx.Done():
		}
		// `finished` is closed before the deferred cancel runs, so a cancel that
		// came from handle's own return is always visible here by now. The second
		// check is not redundant: both channels are ready on that path, and a
		// plain two-case select would pick between them at random and close the
		// connection out from under a refusal half the time.
		select {
		case <-finished:
		default:
			_ = connection.Close()
		}
	}()
	// A peer that completes the QUIC handshake and then never writes a
	// ClientHello keeps its connection alive with PING frames — quic-go's idle
	// timeout is reset by any packet — and would otherwise pin this goroutine for
	// the life of the process, unbounded and unauthenticated. Admission gets a
	// deadline of its own. A live session does not need one: by then the peer is
	// identified and the zone's own rules apply.
	admissionTimer := time.AfterFunc(server.admissionTimeout(), func() {
		_ = connection.Close()
	})
	defer admissionTimer.Stop()

	if err := server.exchangeHello(connection); err != nil {
		return err
	}
	request := new(sarnautv1.EnterZoneRequest)
	if err := transport.ReadMessage(connection, request); err != nil {
		return fmt.Errorf("read enter zone request: %w", err)
	}
	binding, ok := server.Zones[request.GetZoneId()]
	if !ok {
		return fmt.Errorf("zone %q is not hosted by this shard", request.GetZoneId())
	}
	zone := binding.World

	// Admission runs before anything else in EnterZone. A session that fails
	// redemption never reaches CharacterBound and never touches the zone, so no
	// entity exists to clean up on this path (protocol/session.md rule 5.4.3).
	admission, err := server.admit(ctx, connection, request.GetTicket())
	if err != nil {
		return err
	}
	// The peer is identified. Everything past this point is either shard work or
	// a session the zone supervises, so the admission deadline is spent.
	admissionTimer.Stop()

	// L1: the character's saved snapshot, or a fresh one materialized from the
	// chargen table. It runs before the entity is created because the entity
	// needs the loaded position, and before the response is written so that a
	// load failure is still reportable while the session is healthy.
	loaded, err := server.Characters.Load(ctx, admission.CharacterID, admission.ChargenOptionID, zone.ID())
	if err != nil {
		server.logger().ErrorContext(ctx, "character load failed",
			"character_id", admission.CharacterID.String(),
			"zone_id", zone.ID(),
			"error", err,
		)
		return server.refuseEnterZone(connection, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL,
			"the character could not be loaded")
	}
	character := newCharacterSession(admission, zone.ID(), loaded)

	// One live session per character. The newer connection wins, and the older
	// one is fully torn down before this one creates its entity, so the zone
	// never holds two entities for one character.
	release := server.sessions.claim(ctx, admission.CharacterID, cancel)
	defer release()
	if sessionCtx.Err() != nil {
		// Evicted while waiting for our own predecessor: a third connection
		// arrived. It wins; this one stops before touching the zone.
		return sessionCtx.Err()
	}
	if server.Party != nil {
		actor := party.Actor{CharacterID: admission.CharacterID, SessionID: uuid.New()}
		if refusal := server.Party.Connect(actor); refusal != party.RefusalNone {
			return fmt.Errorf("connect authenticated party presence: %s", refusal)
		}
		defer server.Party.Disconnect(actor)
	}
	if server.Cohorts != nil {
		releaseCohorts, err := server.Cohorts.Connect(admission.CharacterID)
		if err != nil {
			return fmt.Errorf("connect authenticated cohort presence: %w", err)
		}
		defer releaseCohorts()
	}

	position, heading := character.spawn()
	entityID, spawn := zone.JoinAt(position, heading)
	if binding.Quests != nil {
		// Armed before the teardown defer below, which means it runs *after*
		// it. Deferred calls unwind last-in-first-out, and teardown takes
		// checkpoint S1 by reading this module's quest log: released first, the
		// session would persist an empty quest set over a real one.
		defer binding.Quests.Release(entityID)
	}
	// Teardown is armed the moment the entity exists, in the same statement
	// sequence — not after the response is written and not after the
	// subscription succeeds (protocol/session.md rule 5.4.5). It runs S1 from a
	// snapshot taken while the entity still exists and only then evicts, which
	// is why it is one deferred function rather than two: `Zone.Leave` stays a
	// pure in-memory eviction and the ordering is stated here instead of being
	// inferred from the order two defers were armed (ADR 0031 §8).
	defer server.teardown(zone, entityID, character, admission)
	// The entity is not replicated until Subscribe, so its combat identity is
	// in place before any peer sees it — and before S0, which persists the
	// level and health the combat module just gave it.
	if binding.Combat != nil {
		if err := binding.Combat.Admit(entityID); err != nil {
			return err
		}
		defer binding.Combat.Release(entityID)
	}
	if server.LocalChat != nil {
		localChat, err := server.LocalChat.Admit(admission.CharacterID, entityID, zone)
		if err != nil {
			return server.refuseEnterZone(connection, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL,
				"the character could not enter local chat")
		}
		defer localChat.Close()
	}
	// Loot ownership is keyed on the character, not on this entity or this
	// session, because mechanics/loot.md rule 5.8.4 keeps a corpse yours across
	// a reconnect and both of the others are destroyed by one. What the module
	// needs from here is only the mapping between them.
	if binding.Loot != nil {
		binding.Loot.Admit(entityID, admission.CharacterID)
		defer binding.Loot.Release(entityID)
	}
	// The quest log is loaded from the rows L1 just read and reconciled against
	// the bag it read with them (mechanics/quests.md rule 5.5.3). It happens
	// before S0 so that the first checkpoint of the session writes the log this
	// module now owns rather than the one the session is about to stop reading.
	if binding.Quests != nil {
		if err := binding.Quests.Admit(entityID, admission.CharacterID, quests.Character{
			Level:     characterLevel(loaded.State.Level),
			Quests:    loaded.Quests,
			Inventory: loaded.Inventory,
		}); err != nil {
			server.logger().ErrorContext(ctx, "quest log load failed",
				"character_id", admission.CharacterID.String(),
				"zone_id", zone.ID(),
				"error", err,
			)
			return server.refuseEnterZone(connection, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL,
				"the character's quest log could not be loaded")
		}
		character.bindQuests(binding.Quests)
	}

	// S0 stamps the zone this character is now in. It is not a redundant
	// write-back of what L1 just read: a later load and any operator
	// inspection depend on it.
	character.checkpoint(zone, entityID, server.Characters, server.logger(), "S0")

	server.logger().InfoContext(ctx, "character entered zone",
		"account_id", admission.AccountID.String(),
		"character_id", admission.CharacterID.String(),
		"zone_id", zone.ID(),
		"entity_id", entityID,
	)

	response := &sarnautv1.EnterZoneResponse{
		ZoneId:      zone.ID(),
		OwnEntityId: entityID,
		SpawnPosition: &sarnautv1.Vec3{
			X: spawn.X,
			Y: spawn.Y,
			Z: spawn.Z,
		},
	}
	if err := transport.WriteMessage(connection, response); err != nil {
		return fmt.Errorf("write enter zone response: %w", err)
	}

	writer := newReliableWriter(connection)
	sender := newSnapshotSender(connection, writer, span)
	events := newEventSender(writer, span)
	if binding.Combat != nil {
		binding.Combat.Subscribe(entityID, events)
	}
	questUpdates := newQuestSender(writer, span)
	if binding.Quests != nil {
		binding.Quests.Subscribe(admission.CharacterID, questUpdates)
		defer binding.Quests.Unsubscribe(admission.CharacterID)
	}
	if err := zone.Subscribe(entityID, sender); err != nil {
		return err
	}
	var chatEvents *chatSender
	if server.Chat != nil || server.Friends != nil {
		chatEvents = newChatSender(connection, writer, span)
	}
	var chatSession *chat.Session
	if server.Chat != nil {
		chatSession, err = server.Chat.Join(chat.Presence{
			CharacterID: admission.CharacterID,
			EntityID:    entityID,
			Name:        admission.CharacterName,
			ZoneID:      zone.ID(),
			Observe: func() (chat.Observation, bool) {
				view, exists := zone.SnapshotCharacter(entityID)
				return chat.Observation{
					Position: chat.Position{X: view.Position.X, Y: view.Position.Y, Z: view.Position.Z},
					Alive:    view.Alive,
				}, exists
			},
		}, chatEvents)
		if err != nil {
			return server.refuseEnterZone(connection, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL,
				"the character could not enter chat")
		}
		defer chatSession.Close()
	}
	if server.Friends != nil {
		friendSession, err := server.Friends.Join(sessionCtx, admission.CharacterID, socialFriendsSink{events: chatEvents})
		if err != nil {
			return server.refuseEnterZone(connection, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL,
				"the character's friends could not be projected")
		}
		defer friendSession.Close()
	}

	reader := &commandReader{
		ctx:         sessionCtx,
		connection:  connection,
		writer:      writer,
		zone:        zone,
		combat:      binding.Combat,
		loot:        binding.Loot,
		quests:      binding.Quests,
		scripts:     binding.Scripts,
		character:   character,
		entityID:    entityID,
		characterID: admission.CharacterID,
		chat:        chatSession,
		chatEvents:  chatEvents,
		datagrams:   connection.SupportsUnreliable(),
		span:        span,
	}
	// Every goroutine below reports exactly once into results, and handle does
	// not return until it has read all of them: the deferred teardown must not
	// run while a sender still holds the sink (ADR 0026). The buffer is sized to
	// the maximum so none of them blocks on a send after the first error.
	results := make(chan error, 8)
	running := 5
	go func() { results <- sender.run(sessionCtx) }()
	go func() { results <- events.run(sessionCtx) }()
	go func() { results <- questUpdates.run(sessionCtx) }()
	go func() { results <- reader.readReliable(sessionCtx) }()
	go func() {
		results <- character.runPeriodicSaves(
			sessionCtx,
			zone,
			entityID,
			server.Characters,
			server.Authority,
			server.saveInterval(),
			server.logger(),
		)
	}()
	if chatEvents != nil {
		running++
		go func() { results <- chatEvents.run(sessionCtx) }()
	}
	if reader.datagrams {
		running += 2
		go func() { results <- sender.runTransitions(sessionCtx) }()
		go func() { results <- reader.readUnreliable(sessionCtx) }()
	}
	// The whole quest log goes out once the readers are running, so a client
	// draws its journal from the same source every later update comes from
	// rather than from a snapshot that does not carry quests.
	if binding.Quests != nil {
		for _, update := range binding.Quests.Log(admission.CharacterID) {
			questUpdates.OfferQuestUpdate(update)
		}
	}

	first := <-results
	// The close comes before the cancel, not after it: cancelling first would
	// wake the watchdog, and its unconditional Close would discard a refusal
	// this session has only just written.
	closeAfterRefusal(ctx, connection, first)
	cancel()
	for pending := 1; pending < running; pending++ {
		<-results
	}

	if errors.Is(first, ErrClientLogout) {
		span.AddEvent("session.logout")
		return nil
	}
	return first
}

// characterLevel converts a stored level into the unsigned one the gameplay
// modules use. The schema's own CHECK keeps it at or above one; the floor here
// is for the fixture stores that have no schema, where a zero would otherwise
// underflow into a level no quest could ever gate against.
func characterLevel(level int32) uint32 {
	if level < 1 {
		return 1
	}
	return uint32(level)
}

func (server Server) saveInterval() time.Duration {
	if server.SaveInterval > 0 {
		return server.SaveInterval
	}
	return DefaultSaveInterval
}

func (server Server) admissionTimeout() time.Duration {
	if server.AdmissionTimeout > 0 {
		return server.AdmissionTimeout
	}
	return DefaultAdmissionTimeout
}

// admit redeems the ticket an EnterZoneRequest carried.
//
// Every refusal is logged with its own reason and answered with the same
// opaque UNAUTHENTICATED: the log is where an operator can tell an expired
// ticket from somebody else's character, and the wire is not. No entity exists
// on any path out of here.
func (server Server) admit(
	ctx context.Context,
	connection transport.Connection,
	ticket string,
) (Admission, error) {
	if server.Authority == nil || server.Characters == nil {
		// A misconfigured shard refuses rather than admitting anonymously.
		server.logger().ErrorContext(ctx, "session refused",
			"reason", "shard_has_no_auth_wiring",
		)
		return Admission{}, server.refuseEnterZone(connection,
			sarnautv1.ErrorCode_ERROR_CODE_INTERNAL, "this shard cannot admit sessions")
	}
	if ticket == "" {
		server.logger().InfoContext(ctx, "session refused",
			"reason", ReasonNoTicket,
			"peer", connection.RemoteAddr().String(),
		)
		return Admission{}, server.refuseEnterZone(connection,
			sarnautv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED, "a shard ticket is required")
	}

	admission, err := server.Authority.RedeemTicket(ctx, ticket)
	if err != nil {
		reason := refusalReason(err)
		if reason == "" {
			reason = ReasonAuthUnavailable
		}
		server.logger().InfoContext(ctx, "session refused",
			"reason", reason,
			"peer", connection.RemoteAddr().String(),
		)
		return Admission{}, server.refuseEnterZone(connection,
			sarnautv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED, "the shard ticket was refused")
	}
	return admission, nil
}

// refuseEnterZone writes a typed error and returns it, so the caller's `return`
// both closes the connection and reports why.
func (server Server) refuseEnterZone(
	connection transport.Connection,
	code sarnautv1.ErrorCode,
	detail string,
) error {
	violation := &ProtocolViolation{Code: code, Detail: detail}
	message := &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_Error{
			Error: &sarnautv1.Error{Code: code, Detail: detail},
		},
	}
	if err := transport.WriteMessage(connection, message); err != nil {
		return errors.Join(violation, fmt.Errorf("write admission refusal: %w", err))
	}
	return violation
}

// teardown is checkpoint S1 followed by eviction, in that order and in one
// deferred call (protocol/session.md rule 5.7.5).
//
// The snapshot is read first, because Zone.Leave deletes the entity and a read
// afterwards would find nothing and save a stale position. The save itself goes
// through the bounded worker, whose context is the shard's lifetime rather than
// this connection's: the disconnect path is frequently reached *because* the
// connection context was cancelled, and a save issued on that context would
// fail every time, on exactly the shutdown that most needs it to succeed.
func (server Server) teardown(
	zone *world.Zone,
	entityID uint64,
	character *characterSession,
	admission Admission,
) {
	saved := character.checkpoint(zone, entityID, server.Characters, server.logger(), "S1")
	zone.Leave(entityID)

	// The play lock is released on a context of its own for the same reason the
	// save is: the connection's context is already cancelled here. A release
	// that fails is not fatal — the lock's TTL frees the character within a
	// minute either way.
	releaseCtx, cancel := context.WithTimeout(context.Background(), playLockReleaseTimeout)
	defer cancel()
	if err := server.Authority.ReleasePlayLock(releaseCtx, admission.CharacterID); err != nil {
		server.logger().Warn("play lock release failed",
			"character_id", admission.CharacterID.String(),
			"error", err,
		)
	}
	server.logger().Info("character left zone",
		"character_id", admission.CharacterID.String(),
		"zone_id", zone.ID(),
		"entity_id", entityID,
		"final_save_enqueued", saved,
	)
}

// playLockReleaseTimeout bounds the disconnect-path release. It is short: the
// TTL is the real guarantee, and a slow release must not hold a goroutine open
// through shutdown.
const playLockReleaseTimeout = 2 * time.Second

func (server Server) exchangeHello(connection transport.Connection) error {
	hello := new(sarnautv1.ClientHello)
	if err := transport.ReadMessage(connection, hello); err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if hello.GetProtocolVersion() != server.ProtocolVersion {
		return fmt.Errorf(
			"client protocol version %s does not match server version %s",
			hello.GetProtocolVersion(),
			server.ProtocolVersion,
		)
	}
	response := &sarnautv1.ServerHello{
		ProtocolVersion: server.ProtocolVersion,
		BuildId:         server.BuildID,
		PackId:          server.PackID,
	}
	if err := transport.WriteMessage(connection, response); err != nil {
		return fmt.Errorf("write server hello: %w", err)
	}
	// The pack check runs after the version check and after the shard has
	// written its own hello, so the client can display both digests rather than
	// guessing why the connection went away (ADR 0027).
	if server.PackID != "" && hello.GetPackId() != server.PackID {
		if hello.GetPackId() == "" && server.AllowUnverifiedPack {
			// The peer makes no content claim and this shard was configured to
			// take it anyway (protocol/session.md rule 5.1.4). It is logged every
			// time: a shard admitting clients whose content it cannot vouch for is
			// a fact an operator should be able to find in the log rather than in
			// a bug report about impossible gameplay.
			server.logger().Warn("admitted a client that claims no content pack",
				"shard_pack_id", server.PackID,
				"build_id", hello.GetBuildId(),
			)
			return nil
		}
		detail := fmt.Sprintf(
			"client content pack %q does not match shard pack %q",
			hello.GetPackId(),
			server.PackID,
		)
		refusal := &sarnautv1.ServerMessage{
			Payload: &sarnautv1.ServerMessage_Error{
				Error: &sarnautv1.Error{
					Code:   sarnautv1.ErrorCode_ERROR_CODE_PACK_MISMATCH,
					Detail: detail,
				},
			},
		}
		if err := transport.WriteMessage(connection, refusal); err != nil {
			return fmt.Errorf("write pack mismatch: %w", err)
		}
		return &ProtocolViolation{
			Code:   sarnautv1.ErrorCode_ERROR_CODE_PACK_MISMATCH,
			Detail: detail,
		}
	}
	return nil
}

type snapshotSender struct {
	connection     transport.Connection
	writer         *reliableWriter
	snapshotWake   chan struct{}
	transitionWake chan struct{}
	span           trace.Span

	pendingMu          sync.Mutex
	pendingSnapshot    world.Snapshot
	hasPendingSnapshot bool
	pendingTransitions []world.Snapshot

	// oversized counts entities dropped because one entity's own envelope did
	// not fit a datagram. It is a counter rather than an error because the
	// alternative — emitting the datagram anyway — ends the session for every
	// player in the zone over one long content string.
	oversized atomic.Uint64
}

func newSnapshotSender(
	connection transport.Connection,
	writer *reliableWriter,
	span trace.Span,
) *snapshotSender {
	return &snapshotSender{
		connection:     connection,
		writer:         writer,
		snapshotWake:   make(chan struct{}, 1),
		transitionWake: make(chan struct{}, 1),
		span:           span,
	}
}

// OfferSnapshot takes the newest view. Snapshot state is latest-wins: a
// session that cannot keep up wants the current world, not a backlog of stale
// positions. Interest transitions are different. They travel reliably and
// remain queued in publish order even when the snapshots around them coalesce.
func (sender *snapshotSender) OfferSnapshot(snapshot world.Snapshot) {
	hasTransitions := len(snapshot.Spawns) > 0 || len(snapshot.Despawns) > 0
	sender.pendingMu.Lock()
	if hasTransitions {
		sender.pendingTransitions = append(sender.pendingTransitions, world.Snapshot{
			ServerTick: snapshot.ServerTick,
			Spawns:     snapshot.Spawns,
			Despawns:   snapshot.Despawns,
		})
	}
	snapshot.Spawns = nil
	snapshot.Despawns = nil
	sender.pendingSnapshot = snapshot
	sender.hasPendingSnapshot = true
	sender.pendingMu.Unlock()

	select {
	case sender.snapshotWake <- struct{}{}:
	default:
	}
	if hasTransitions {
		select {
		case sender.transitionWake <- struct{}{}:
		default:
		}
	}
}

func (sender *snapshotSender) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sender.snapshotWake:
			snapshot, ok := sender.takePendingSnapshot()
			if !ok {
				continue
			}
			// On the reliable fallback both payload families share one
			// carrier, so write interest changes before the snapshot that
			// reflects them. Datagram sessions use runTransitions instead;
			// keeping it separate prevents a stalled stream from stopping
			// the lossy snapshot channel.
			if !sender.connection.SupportsUnreliable() {
				for _, update := range sender.takePendingTransitions() {
					if err := sender.sendTransitions(update); err != nil {
						return err
					}
				}
			}
			if err := sender.sendSnapshot(snapshot); err != nil {
				return err
			}
		}
	}
}

func (sender *snapshotSender) runTransitions(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sender.transitionWake:
			for _, update := range sender.takePendingTransitions() {
				if err := sender.sendTransitions(update); err != nil {
					return err
				}
			}
		}
	}
}

func (sender *snapshotSender) takePendingSnapshot() (world.Snapshot, bool) {
	sender.pendingMu.Lock()
	defer sender.pendingMu.Unlock()
	if !sender.hasPendingSnapshot {
		return world.Snapshot{}, false
	}
	snapshot := sender.pendingSnapshot
	sender.pendingSnapshot = world.Snapshot{}
	sender.hasPendingSnapshot = false
	return snapshot, true
}

func (sender *snapshotSender) takePendingTransitions() []world.Snapshot {
	sender.pendingMu.Lock()
	defer sender.pendingMu.Unlock()
	transitions := sender.pendingTransitions
	sender.pendingTransitions = nil
	return transitions
}

// send maps one snapshot onto the wire and puts it out.
//
// The protobuf tree is built here, per session, immediately before it is
// written. That is what retires ADR 0026's shared-batch hazard by
// construction: no two sessions can be handed the same mutable message,
// because the message does not exist until one of them is being served.
func (sender *snapshotSender) send(snapshot world.Snapshot) error {
	if err := sender.sendTransitions(snapshot); err != nil {
		return err
	}
	return sender.sendSnapshot(snapshot)
}

func (sender *snapshotSender) sendTransitions(snapshot world.Snapshot) error {
	for _, spawn := range snapshot.Spawns {
		entity, err := entitySnapshotToProto(spawn)
		if err != nil {
			return fmt.Errorf("map spawn: %w", err)
		}
		if err := sender.writer.write(&sarnautv1.ServerMessage{
			ServerTick: snapshot.ServerTick,
			Payload: &sarnautv1.ServerMessage_SpawnEvent{
				SpawnEvent: &sarnautv1.SpawnEvent{Entity: entity},
			},
		}); err != nil {
			return fmt.Errorf("write spawn event: %w", err)
		}
	}
	for _, entityID := range snapshot.Despawns {
		if err := sender.writer.write(&sarnautv1.ServerMessage{
			ServerTick: snapshot.ServerTick,
			Payload: &sarnautv1.ServerMessage_DespawnEvent{
				DespawnEvent: &sarnautv1.DespawnEvent{EntityId: entityID},
			},
		}); err != nil {
			return fmt.Errorf("write despawn event: %w", err)
		}
	}
	return nil
}

func (sender *snapshotSender) sendSnapshot(snapshot world.Snapshot) error {
	batch, err := snapshotToProto(snapshot)
	if err != nil {
		return fmt.Errorf("map snapshot: %w", err)
	}
	if !sender.connection.SupportsUnreliable() {
		if err := sender.writer.write(snapshotMessage(batch)); err != nil {
			return fmt.Errorf("write snapshot fallback: %w", err)
		}
		return nil
	}
	chunks, dropped := splitSnapshot(batch)
	if len(dropped) > 0 {
		sender.oversized.Add(uint64(len(dropped)))
		if sender.span != nil {
			sender.span.AddEvent("session.snapshot.entity_oversized", trace.WithAttributes(
				attribute.Int("sarnaut.entities.dropped", len(dropped)),
				attribute.Int64("sarnaut.entity_id", int64(dropped[0])),
			))
		}
	}
	for _, chunk := range chunks {
		payload, err := transport.MarshalUnreliable(chunk)
		if err != nil {
			return fmt.Errorf("marshal snapshot: %w", err)
		}
		if err := sender.connection.SendUnreliable(payload); err != nil {
			return fmt.Errorf("send snapshot: %w", err)
		}
	}
	return nil
}

func snapshotMessage(batch *sarnautv1.SnapshotBatch) *sarnautv1.ServerMessage {
	return &sarnautv1.ServerMessage{
		ServerTick: batch.GetServerTick(),
		Payload:    &sarnautv1.ServerMessage_SnapshotBatch{SnapshotBatch: batch},
	}
}

// chunkHeaderReserve is the room kept free in every measured chunk for the
// chunk_index/chunk_count pair. Both are uint32 varints behind a one-byte tag,
// so twelve bytes is the worst case, and reserving it unconditionally lets the
// split run before the number of chunks — which is what those fields carry — is
// known. Twelve bytes of a 1100-byte datagram is the price of not having to
// re-run the split after stamping.
const chunkHeaderReserve = 12

// splitSnapshot chunks a batch so that each datagram stays under the packet
// limit, and stamps every chunk with its index and the chunk count so the
// receiver can put the tick back together (protocol/session.md rule 5.5.7).
//
// The measurement is of the encoded ServerMessage, not of the bare batch: a
// datagram carries an envelope, so measuring the payload alone produces
// datagrams over the cap by exactly the envelope overhead.
//
// An entity whose own single-entity envelope will not fit is returned in
// `dropped` instead of being emitted. content_id, name_key and faction are
// unbounded pack strings, so this is reachable from content alone; emitting the
// datagram anyway would fail MarshalUnreliable and take the whole session down
// over one entity nobody could have seen anyway.
func splitSnapshot(snapshot *sarnautv1.SnapshotBatch) (chunks []*sarnautv1.ServerMessage, dropped []uint64) {
	const limit = transport.MaxUnreliableMessageSize - chunkHeaderReserve

	tick := snapshot.GetServerTick()
	current := &sarnautv1.SnapshotBatch{ServerTick: tick}
	envelope := snapshotMessage(current)
	chunks = make([]*sarnautv1.ServerMessage, 0, 1)
	for _, entity := range snapshot.GetEntities() {
		current.Entities = append(current.Entities, entity)
		if proto.Size(envelope) <= limit {
			continue
		}
		// The entity does not fit in this chunk. Close the chunk if it holds
		// anything and retry the entity in a fresh one; an entity that does not
		// fit even alone is one no datagram can carry.
		current.Entities = current.Entities[:len(current.Entities)-1]
		if len(current.Entities) > 0 {
			chunks = append(chunks, envelope)
			current = &sarnautv1.SnapshotBatch{ServerTick: tick}
			envelope = snapshotMessage(current)
			current.Entities = append(current.Entities, entity)
			if proto.Size(envelope) <= limit {
				continue
			}
			current.Entities = current.Entities[:0]
		}
		dropped = append(dropped, entity.GetEntityId())
	}
	if len(current.Entities) > 0 || len(chunks) == 0 {
		chunks = append(chunks, envelope)
	}
	for index, chunk := range chunks {
		batch := chunk.GetSnapshotBatch()
		batch.ChunkIndex = uint32(index)
		batch.ChunkCount = uint32(len(chunks))
	}
	return chunks, dropped
}
