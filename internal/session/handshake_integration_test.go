package session_test

import (
	"context"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
)

func TestGatewayAndShardExchangeHelloOverQUIC(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverTLS, err := transport.NewDevServerTLSConfig()
	if err != nil {
		t.Fatalf("NewDevServerTLSConfig() error = %v", err)
	}
	listener, err := transport.ListenQUIC("127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("ListenQUIC() error = %v", err)
	}
	defer func() { _ = listener.Close() }()

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(ctx, listener)
	}()

	connection, err := transport.DialQUIC(
		ctx,
		listener.Addr().String(),
		transport.NewDevClientTLSConfig(),
	)
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	defer func() { _ = connection.Close() }()

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "gateway-test",
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}

	if hello.GetProtocolVersion() != sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1 {
		t.Errorf("protocol_version = %v, want PROTOCOL_VERSION_1", hello.GetProtocolVersion())
	}
	if hello.GetBuildId() != "shard-test" {
		t.Errorf("build_id = %q, want %q", hello.GetBuildId(), "shard-test")
	}

	cancel()
	select {
	case err := <-serveErrors:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
}
