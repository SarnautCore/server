package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	"github.com/SarnautCore/server/internal/transport"
)

func TestPrivateChallengeResponse(t *testing.T) {
	client, server := newTestPipe()
	defer client.Close()
	defer server.Close()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	clientConfig := TrustConfig{
		InstanceID: "gateway-1", ShardID: "shard-1", KeyID: "m3-a", Secret: secret,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x11}, nonceSize)),
	}
	serverConfig := clientConfig
	serverConfig.Random = bytes.NewReader(bytes.Repeat([]byte{0x22}, nonceSize))
	results := make(chan error, 1)
	go func() {
		err := AuthenticateShard(context.Background(), server, serverConfig)
		_ = server.Close()
		results <- err
	}()
	if err := AuthenticateGateway(context.Background(), client, clientConfig); err != nil {
		t.Fatalf("AuthenticateGateway() error = %v", err)
	}
	if err := <-results; err != nil {
		t.Fatalf("AuthenticateShard() error = %v", err)
	}
}

func TestPrivateChallengeRefusesWrongSecret(t *testing.T) {
	client, server := newTestPipe()
	defer client.Close()
	defer server.Close()
	clientConfig := TrustConfig{
		InstanceID: "gateway-1", ShardID: "shard-1", KeyID: "m3-a",
		Secret: bytes.Repeat([]byte{0x5a}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x11}, nonceSize)),
	}
	serverConfig := clientConfig
	serverConfig.Secret = bytes.Repeat([]byte{0xa5}, 32)
	serverConfig.Random = bytes.NewReader(bytes.Repeat([]byte{0x22}, nonceSize))
	results := make(chan error, 1)
	go func() {
		err := AuthenticateShard(context.Background(), server, serverConfig)
		_ = server.Close()
		results <- err
	}()
	if err := AuthenticateGateway(context.Background(), client, clientConfig); err == nil {
		t.Fatal("AuthenticateGateway() succeeded with the wrong secret")
	}
	_ = client.Close()
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("AuthenticateShard() succeeded with the wrong secret")
		}
	case <-time.After(time.Second):
		t.Fatal("shard authentication did not stop after the refused proof")
	}
}

func TestRoutesFailClosed(t *testing.T) {
	_, err := NewRoutes("pack-a", []Route{{
		ZoneID: "InstLeague1", ShardID: "shard-1", PrivateAddress: "127.0.0.1:4243",
		PackID: "pack-b", Enabled: true,
	}})
	if err == nil {
		t.Fatal("NewRoutes() accepted a route with a different pack")
	}
	routes, err := NewRoutes("pack-a", []Route{{
		ZoneID: "InstLeague1", ShardID: "shard-1", PrivateAddress: "127.0.0.1:4243",
		PackID: "pack-a", Enabled: false,
	}})
	if err != nil {
		t.Fatalf("NewRoutes() error = %v", err)
	}
	if _, err := routes.Resolve("InstLeague1"); err == nil {
		t.Fatal("Resolve() admitted a disabled route")
	}
}

func TestUnavailableTransferIsRefusedBeforeDetach(t *testing.T) {
	gatewaySide, shardSide := newTestPipe()
	publicSide, publicPeer := newTestPipe()
	defer gatewaySide.Close()
	defer shardSide.Close()
	defer publicSide.Close()
	defer publicPeer.Close()
	broker := newAttachmentBroker()
	broker.set(7, gatewaySide)
	lost := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relayEvents(ctx, broker, "session-1", 7, gatewaySide, publicSide, lost)
	if err := transport.WriteMessage(shardSide, &privatev1.Control{Payload: &privatev1.Control_TransferRequested{
		TransferRequested: &privatev1.TransferRequested{
			SessionId: "session-1", CurrentEpoch: 7, DestinationZoneId: "InstFuture",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	response := new(privatev1.Control)
	if err := transport.ReadMessage(shardSide, response); err != nil {
		t.Fatal(err)
	}
	refusal := response.GetTransferRefused()
	if refusal == nil || refusal.GetCode() != privatev1.RefusalCode_REFUSAL_CODE_ZONE_UNAVAILABLE {
		t.Fatalf("transfer response = %v", response)
	}
}

type testPipe struct{ net.Conn }

func newTestPipe() (*testPipe, *testPipe) {
	left, right := net.Pipe()
	return &testPipe{Conn: left}, &testPipe{Conn: right}
}

func (connection *testPipe) CloseWrite() error { return connection.Close() }
func (*testPipe) SupportsUnreliable() bool     { return false }
func (*testPipe) SendUnreliable([]byte) error  { return errors.ErrUnsupported }
func (*testPipe) ReceiveUnreliable(context.Context) ([]byte, error) {
	return nil, errors.ErrUnsupported
}
