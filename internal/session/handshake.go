// Package session owns connection-level protocol exchanges.
package session

import (
	"context"
	"fmt"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
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

	return response, nil
}

// EnterZone requests admission and returns the authoritative player spawn.
func (client Client) EnterZone(
	connection transport.Connection,
	zoneID string,
) (*sarnautv1.EnterZoneResponse, error) {
	if err := transport.WriteMessage(connection, &sarnautv1.EnterZoneRequest{ZoneId: zoneID}); err != nil {
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
func (client Client) SendMoveIntent(
	connection transport.Connection,
	intent *sarnautv1.ClientMoveIntent,
) error {
	if !connection.SupportsUnreliable() {
		return transport.WriteMessage(connection, intent)
	}
	payload, err := transport.MarshalUnreliable(intent)
	if err != nil {
		return fmt.Errorf("marshal move intent: %w", err)
	}
	if err := connection.SendUnreliable(payload); err != nil {
		return fmt.Errorf("send move intent: %w", err)
	}
	return nil
}

// ReadSnapshot receives one snapshot batch from a datagram or stream fallback.
func (client Client) ReadSnapshot(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.SnapshotBatch, error) {
	result := new(sarnautv1.SnapshotBatch)
	if !connection.SupportsUnreliable() {
		if err := transport.ReadMessage(connection, result); err != nil {
			return nil, fmt.Errorf("read snapshot: %w", err)
		}
		return result, nil
	}
	payload, err := connection.ReceiveUnreliable(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive snapshot: %w", err)
	}
	if err := transport.UnmarshalUnreliable(payload, result); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return result, nil
}

// Server accepts sessions and binds admitted players to configured zones.
type Server struct {
	ProtocolVersion sarnautv1.ProtocolVersion
	BuildID         string
	Zones           map[string]*world.Zone
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

func (server Server) handle(ctx context.Context, connection transport.Connection) error {
	_, span := tracer.Start(ctx, "session.server")
	defer span.End()
	span.SetAttributes(attribute.String("network.peer.address", connection.RemoteAddr().String()))

	if err := server.exchangeHello(connection); err != nil {
		return err
	}
	request := new(sarnautv1.EnterZoneRequest)
	if err := transport.ReadMessage(connection, request); err != nil {
		return fmt.Errorf("read enter zone request: %w", err)
	}
	zone, ok := server.Zones[request.GetZoneId()]
	if !ok {
		return fmt.Errorf("zone %q is not hosted by this shard", request.GetZoneId())
	}

	entityID, spawn := zone.Join()
	defer zone.Leave(entityID)
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

	sender := newSnapshotSender(connection)
	if err := zone.Subscribe(entityID, sender); err != nil {
		return err
	}
	sendErrors := make(chan error, 1)
	go func() { sendErrors <- sender.run(ctx) }()

	for {
		intent, err := receiveMoveIntent(ctx, connection)
		if err != nil {
			return err
		}
		if err := zone.ApplyMoveIntent(entityID, intent); err != nil {
			span.RecordError(err)
		}
		select {
		case err := <-sendErrors:
			return err
		default:
		}
	}
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
	}
	if err := transport.WriteMessage(connection, response); err != nil {
		return fmt.Errorf("write server hello: %w", err)
	}
	return nil
}

func receiveMoveIntent(
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.ClientMoveIntent, error) {
	result := new(sarnautv1.ClientMoveIntent)
	if !connection.SupportsUnreliable() {
		if err := transport.ReadMessage(connection, result); err != nil {
			return nil, fmt.Errorf("read move intent: %w", err)
		}
		return result, nil
	}
	payload, err := connection.ReceiveUnreliable(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive move intent: %w", err)
	}
	if err := transport.UnmarshalUnreliable(payload, result); err != nil {
		return nil, fmt.Errorf("decode move intent: %w", err)
	}
	return result, nil
}

type snapshotSender struct {
	connection transport.Connection
	queue      chan *sarnautv1.SnapshotBatch
}

func newSnapshotSender(connection transport.Connection) *snapshotSender {
	return &snapshotSender{
		connection: connection,
		queue:      make(chan *sarnautv1.SnapshotBatch, 1),
	}
}

func (sender *snapshotSender) OfferSnapshot(snapshot *sarnautv1.SnapshotBatch) {
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

func (sender *snapshotSender) send(snapshot *sarnautv1.SnapshotBatch) error {
	if !sender.connection.SupportsUnreliable() {
		if err := transport.WriteMessage(sender.connection, snapshot); err != nil {
			return fmt.Errorf("write snapshot fallback: %w", err)
		}
		return nil
	}
	for _, chunk := range splitSnapshot(snapshot) {
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

func splitSnapshot(snapshot *sarnautv1.SnapshotBatch) []*sarnautv1.SnapshotBatch {
	current := &sarnautv1.SnapshotBatch{ServerTick: snapshot.GetServerTick()}
	result := make([]*sarnautv1.SnapshotBatch, 0, 1)
	for _, entity := range snapshot.GetEntities() {
		current.Entities = append(current.Entities, entity)
		if proto.Size(current) <= transport.MaxUnreliableMessageSize {
			continue
		}
		current.Entities = current.Entities[:len(current.Entities)-1]
		if len(current.Entities) > 0 {
			result = append(result, current)
		}
		current = &sarnautv1.SnapshotBatch{
			ServerTick: snapshot.GetServerTick(),
			Entities:   []*sarnautv1.EntitySnapshot{entity},
		}
	}
	if len(current.Entities) > 0 || len(result) == 0 {
		result = append(result, current)
	}
	return result
}
