// Package session owns connection-level protocol exchanges.
package session

import (
	"context"
	"fmt"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("github.com/SarnautCore/server/internal/session")

// Client performs the gateway side of the initial session handshake.
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

// Server accepts gateway connections and answers their initial hello.
type Server struct {
	ProtocolVersion sarnautv1.ProtocolVersion
	BuildID         string
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
	_, span := tracer.Start(ctx, "session.server.handshake")
	defer span.End()
	span.SetAttributes(attribute.String("network.peer.address", connection.RemoteAddr().String()))

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
