// Package session owns connection-level protocol exchanges.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

// ReadSnapshot receives server envelopes until one carries a snapshot batch. A
// typed refusal is returned as an error; any other case is skipped, because a
// snapshot reader is not the place to handle combat.
func (client Client) ReadSnapshot(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.SnapshotBatch, error) {
	for {
		message, err := client.ReadServerMessage(ctx, connection)
		if err != nil {
			return nil, err
		}
		switch payload := message.GetPayload().(type) {
		case *sarnautv1.ServerMessage_SnapshotBatch:
			return payload.SnapshotBatch, nil
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

// Server accepts sessions and binds admitted players to configured zones.
type Server struct {
	ProtocolVersion sarnautv1.ProtocolVersion
	BuildID         string

	// PackID is the runtime pack digest this shard loaded (ADR 0029). While it
	// is empty the shard makes no content-identity claim and gates nothing.
	// TODO(m2-pack-v0): populate from config and honour
	// content.allow_unverified_pack.
	PackID string

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

	// SaveInterval is checkpoint S2's cadence. Zero means
	// [DefaultSaveInterval].
	SaveInterval time.Duration

	Logger *slog.Logger

	// sessions arbitrates two connections for one character. It is created by
	// Serve, so every handler a Serve call spawns shares one registry.
	sessions *sessionRegistry
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
}

// DefaultSaveInterval is protocol/session.md's PERIODIC_SAVE_INTERVAL_S: the
// ceiling on progress an unclean shard exit can destroy.
const DefaultSaveInterval = 60 * time.Second

// refusalDrainGrace is how long a refused connection is left half-closed so the
// peer can read the refusal frame. It is short: the alternative to closing at
// all is letting a rejected peer hold a connection until the idle timeout.
const refusalDrainGrace = 250 * time.Millisecond

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
			var violation *ProtocolViolation
			if errors.As(err, &violation) {
				// The refusal is already on the wire. Closing the connection
				// outright here would discard it — a QUIC CONNECTION_CLOSE does
				// not wait for stream data to be read — and the peer would see a
				// dropped connection instead of the reason. Half-close, let it
				// drain, then close.
				_ = connection.CloseWrite()
				select {
				case <-time.After(refusalDrainGrace):
				case <-ctx.Done():
				}
			}
			_ = connection.Close()
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

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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

	position, heading := character.spawn()
	entityID, spawn := zone.JoinAt(position, heading)
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
	sender := newSnapshotSender(connection, writer)
	events := newEventSender(writer, span)
	if binding.Combat != nil {
		binding.Combat.Subscribe(entityID, events)
	}
	if err := zone.Subscribe(entityID, sender); err != nil {
		return err
	}

	// stream.Read does not observe a context, so cancellation alone cannot
	// unblock the reliable reader. Closing the connection can.
	go func() {
		<-sessionCtx.Done()
		_ = connection.Close()
	}()

	reader := &commandReader{
		connection: connection,
		writer:     writer,
		zone:       zone,
		combat:     binding.Combat,
		entityID:   entityID,
		datagrams:  connection.SupportsUnreliable(),
		span:       span,
	}
	// Every goroutine below reports exactly once into results, and handle does
	// not return until it has read all of them: the deferred teardown must not
	// run while a sender still holds the sink (ADR 0026). The buffer is sized to
	// the maximum so none of them blocks on a send after the first error.
	results := make(chan error, 5)
	running := 4
	go func() { results <- sender.run(sessionCtx) }()
	go func() { results <- events.run(sessionCtx) }()
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
	if reader.datagrams {
		running = 5
		go func() { results <- reader.readUnreliable(sessionCtx) }()
	}

	first := <-results
	cancel()
	_ = connection.Close()
	for pending := 1; pending < running; pending++ {
		<-results
	}

	if errors.Is(first, ErrClientLogout) {
		span.AddEvent("session.logout")
		return nil
	}
	return first
}

func (server Server) saveInterval() time.Duration {
	if server.SaveInterval > 0 {
		return server.SaveInterval
	}
	return DefaultSaveInterval
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
	connection transport.Connection
	writer     *reliableWriter
	queue      chan world.Snapshot
}

func newSnapshotSender(connection transport.Connection, writer *reliableWriter) *snapshotSender {
	return &snapshotSender{
		connection: connection,
		writer:     writer,
		queue:      make(chan world.Snapshot, 1),
	}
}

// OfferSnapshot takes the newest view. The queue is one deep and
// latest-wins: a session that cannot keep up wants the current world, not a
// backlog of stale ones.
func (sender *snapshotSender) OfferSnapshot(snapshot world.Snapshot) {
	select {
	case sender.queue <- snapshot:
		return
	default:
	}
	select {
	case <-sender.queue:
	default:
	}
	select {
	case sender.queue <- snapshot:
	default:
	}
}

func (sender *snapshotSender) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case snapshot := <-sender.queue:
			if err := sender.send(snapshot); err != nil {
				return err
			}
		}
	}
}

// send maps one snapshot onto the wire and puts it out.
//
// The protobuf tree is built here, per session, immediately before it is
// written. That is what retires ADR 0026's shared-batch hazard by
// construction: no two sessions can be handed the same mutable message,
// because the message does not exist until one of them is being served.
func (sender *snapshotSender) send(snapshot world.Snapshot) error {
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
	for _, chunk := range splitSnapshot(batch) {
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

// splitSnapshot chunks a batch so that each datagram stays under the packet
// limit. The measurement is of the encoded ServerMessage, not of the bare
// batch: a datagram carries an envelope, so measuring the payload alone
// produces datagrams over the cap by exactly the envelope overhead
// (protocol/session.md rule 5.5.7).
func splitSnapshot(snapshot *sarnautv1.SnapshotBatch) []*sarnautv1.ServerMessage {
	tick := snapshot.GetServerTick()
	current := &sarnautv1.SnapshotBatch{ServerTick: tick}
	envelope := snapshotMessage(current)
	result := make([]*sarnautv1.ServerMessage, 0, 1)
	for _, entity := range snapshot.GetEntities() {
		current.Entities = append(current.Entities, entity)
		if proto.Size(envelope) <= transport.MaxUnreliableMessageSize {
			continue
		}
		current.Entities = current.Entities[:len(current.Entities)-1]
		if len(current.Entities) > 0 {
			result = append(result, envelope)
		}
		current = &sarnautv1.SnapshotBatch{
			ServerTick: tick,
			Entities:   []*sarnautv1.EntitySnapshot{entity},
		}
		envelope = snapshotMessage(current)
	}
	if len(current.Entities) > 0 || len(result) == 0 {
		result = append(result, envelope)
	}
	return result
}
