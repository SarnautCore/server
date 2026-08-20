// Package session owns connection-level protocol exchanges.
package session

import (
	"context"
	"errors"
	"fmt"

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
	response := new(sarnautv1.EnterZoneResponse)
	if err := transport.ReadMessage(connection, response); err != nil {
		return nil, fmt.Errorf("read enter zone response: %w", err)
	}
	if response.GetZoneId() != zoneID {
		return nil, fmt.Errorf("server entered zone %q, want %q", response.GetZoneId(), zoneID)
	}
	return response, nil
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

// Serve handles connections until the context ends or the listener fails.
func (server Server) Serve(ctx context.Context, listener transport.Listener) error {
	for {
		connection, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept session: %w", err)
		}

		go func() {
			if err := server.handle(ctx, connection); err != nil {
				_ = connection.Close()
				return
			}
			_ = connection.CloseWrite()
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
	// TODO(m2-auth): redeem request.GetTicket() over NATS before the entity is
	// created, and derive account_id and character_id from the reply
	// (ADR 0030, protocol/session.md rule 5.2).

	entityID, spawn := zone.Join()
	defer zone.Leave(entityID)
	// The entity is not replicated until Subscribe, so its combat identity is
	// in place before any peer sees it.
	if binding.Combat != nil {
		if err := binding.Combat.Admit(entityID); err != nil {
			return err
		}
		defer binding.Combat.Release(entityID)
	}
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

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	results := make(chan error, 4)
	running := 3
	go func() { results <- sender.run(sessionCtx) }()
	go func() { results <- events.run(sessionCtx) }()
	go func() { results <- reader.readReliable(sessionCtx) }()
	if reader.datagrams {
		running = 4
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
