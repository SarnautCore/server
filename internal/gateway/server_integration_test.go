package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/gateway"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestPublicSessionRedeemsAtGatewayAndAssertsIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publicGateway, publicClient := pipeConnections()
	privateGateway, privateShard := pipeConnections()
	listener := &singleListener{connection: publicGateway, ready: make(chan struct{})}
	authority := &fakeAuthority{admission: gateway.Admission{
		AccountID:     uuid.MustParse("019200f0-0000-7000-8000-00000000a001"),
		CharacterID:   uuid.MustParse("019200f0-0000-7000-8000-00000000c001"),
		CharacterName: "Anne", ChargenOptionID: "chargen.league.warrior",
	}}
	secret := bytes.Repeat([]byte{0x5a}, 32)
	trust := gateway.TrustConfig{
		InstanceID: "gateway-test", ShardID: "shard-test", KeyID: "m3-a", Secret: secret,
	}
	routes, err := gateway.NewRoutes("pack-test", []gateway.Route{{
		ZoneID: "InstLeague1", ShardID: "shard-test", PrivateAddress: "pipe",
		PackID: "pack-test", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var statesMu sync.Mutex
	var states []gateway.SessionState
	server := &gateway.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "test", PackID: "pack-test", Authority: authority, Routes: routes,
		DialShard: func(context.Context, gateway.Route) (transport.Connection, error) { return privateGateway, nil },
		Trust:     trust, RenewInterval: 5 * time.Millisecond,
		StateChanged: func(state gateway.SessionState) { statesMu.Lock(); states = append(states, state); statesMu.Unlock() },
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()

	assertions := make(chan *privatev1.AttachRequest, 1)
	go serveFakeShard(t, ctx, privateShard, trust, assertions, false)
	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "test", PackID: "pack-test", Ticket: "sarnaut_tk_test",
	}
	if _, err := client.Handshake(ctx, publicClient); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	entered, err := client.EnterZone(publicClient, "InstLeague1")
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	if entered.GetZoneId() != "InstLeague1" {
		t.Fatalf("zone = %q", entered.GetZoneId())
	}
	assertion := <-assertions
	if assertion.GetIdentity().GetCharacterId() != authority.admission.CharacterID.String() {
		t.Fatalf("asserted character = %q", assertion.GetIdentity().GetCharacterId())
	}
	if assertion.GetSessionId() == "" || assertion.GetAttachmentEpoch() != 1 {
		t.Fatalf("attach key = (%q, %d)", assertion.GetSessionId(), assertion.GetAttachmentEpoch())
	}
	waitFor(t, time.Second, func() bool { _, renewed, _ := authority.counts(); return renewed > 0 })
	if err := client.Logout(publicClient); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	_ = publicClient.Close()
	waitFor(t, time.Second, func() bool { _, _, released := authority.counts(); return released == 1 })
	redeemed, renewed, released := authority.counts()
	if redeemed != 1 || renewed == 0 || released != 1 {
		t.Fatalf("authority calls = redeem %d renew %d release %d", redeemed, renewed, released)
	}
	cancel()
	if err := <-serveErrors; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	statesMu.Lock()
	defer statesMu.Unlock()
	wantPrefix := []gateway.SessionState{
		gateway.StateConnected, gateway.StateVersioned, gateway.StateAuthenticated,
		gateway.StateAttaching, gateway.StateInZone,
	}
	if len(states) < len(wantPrefix) {
		t.Fatalf("states = %v", states)
	}
	for index, want := range wantPrefix {
		if states[index] != want {
			t.Fatalf("states[%d] = %v, want %v; all = %v", index, states[index], want, states)
		}
	}
}

func TestPrivateFailureReattachesWithNewEpoch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publicGateway, publicClient := pipeConnections()
	firstGateway, firstShard := pipeConnections()
	secondGateway, secondShard := pipeConnections()
	authority := &fakeAuthority{admission: gateway.Admission{
		AccountID: uuid.New(), CharacterID: uuid.New(), CharacterName: "Anne", ChargenOptionID: "option",
	}}
	secret := bytes.Repeat([]byte{0x4c}, 32)
	trust := gateway.TrustConfig{InstanceID: "gateway-test", ShardID: "shard-test", KeyID: "m3-a", Secret: secret}
	routes, _ := gateway.NewRoutes("pack-test", []gateway.Route{{
		ZoneID: "InstLeague1", ShardID: "shard-test", PrivateAddress: "pipe", PackID: "pack-test", Enabled: true,
	}})
	connections := make(chan transport.Connection, 2)
	connections <- firstGateway
	connections <- secondGateway
	server := &gateway.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "test", PackID: "pack-test", Authority: authority, Routes: routes, Trust: trust,
		DialShard: func(context.Context, gateway.Route) (transport.Connection, error) { return <-connections, nil },
	}
	listener := &singleListener{connection: publicGateway, ready: make(chan struct{})}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()
	firstAssertions := make(chan *privatev1.AttachRequest, 1)
	secondAssertions := make(chan *privatev1.AttachRequest, 1)
	go serveFakeShard(t, ctx, firstShard, trust, firstAssertions, true)
	go serveFakeShard(t, ctx, secondShard, trust, secondAssertions, false)
	client := session.Client{ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID: "test", PackID: "pack-test", Ticket: "ticket"}
	if _, err := client.Handshake(ctx, publicClient); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EnterZone(publicClient, "InstLeague1"); err != nil {
		t.Fatal(err)
	}
	first := <-firstAssertions
	second := <-secondAssertions
	if second.GetSessionId() != first.GetSessionId() || second.GetAttachmentEpoch() != first.GetAttachmentEpoch()+1 {
		t.Fatalf("reattach keys first=(%s,%d) second=(%s,%d)",
			first.GetSessionId(), first.GetAttachmentEpoch(), second.GetSessionId(), second.GetAttachmentEpoch())
	}
	_ = publicClient.Close()
	cancel()
	if err := <-serveErrors; err != nil {
		t.Fatal(err)
	}
}

func serveFakeShard(
	t *testing.T,
	ctx context.Context,
	connection transport.Connection,
	trust gateway.TrustConfig,
	assertions chan<- *privatev1.AttachRequest,
	failAfterAttach bool,
) {
	t.Helper()
	if err := gateway.AuthenticateShard(ctx, connection, trust); err != nil {
		t.Errorf("shard auth: %v", err)
		return
	}
	control := new(privatev1.Control)
	if err := transport.ReadMessage(connection, control); err != nil {
		t.Errorf("read attach: %v", err)
		return
	}
	request := control.GetAttachRequest()
	assertions <- request
	if err := transport.WriteMessage(connection, &privatev1.Control{Payload: &privatev1.Control_Attached{
		Attached: &privatev1.Attached{SessionId: request.GetSessionId(), AttachmentEpoch: request.GetAttachmentEpoch(), ZoneId: request.GetZoneId()},
	}}); err != nil {
		t.Errorf("write attached: %v", err)
		return
	}
	if failAfterAttach {
		_ = connection.Close()
		return
	}
	payload, err := proto.Marshal(&sarnautv1.EnterZoneResponse{
		ZoneId: request.GetZoneId(), OwnEntityId: 42, SpawnPosition: &sarnautv1.Vec3{},
	})
	if err != nil {
		t.Errorf("marshal enter response: %v", err)
		return
	}
	if err := transport.WriteMessage(connection, &privatev1.Control{Payload: &privatev1.Control_ServerReliable{
		ServerReliable: &privatev1.RelayFrame{Payload: payload},
	}}); err != nil {
		t.Errorf("write enter response: %v", err)
		return
	}
	message := new(privatev1.Control)
	_ = transport.ReadMessage(connection, message)
	_ = connection.Close()
}

type fakeAuthority struct {
	mu                          sync.Mutex
	admission                   gateway.Admission
	redeemed, renewed, released int
}

func (authority *fakeAuthority) RedeemTicket(context.Context, string) (gateway.Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.redeemed++
	return authority.admission, nil
}
func (authority *fakeAuthority) RenewPlayLock(context.Context, uuid.UUID) (bool, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.renewed++
	return true, nil
}
func (authority *fakeAuthority) ReleasePlayLock(context.Context, uuid.UUID) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.released++
	return nil
}
func (authority *fakeAuthority) counts() (int, int, int) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.redeemed, authority.renewed, authority.released
}

type singleListener struct {
	connection transport.Connection
	ready      chan struct{}
	once       sync.Once
}

func (listener *singleListener) Accept(ctx context.Context) (transport.Connection, error) {
	var connection transport.Connection
	listener.once.Do(func() { connection = listener.connection; close(listener.ready) })
	if connection != nil {
		return connection, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*singleListener) Addr() net.Addr { return testAddr("listener") }
func (*singleListener) Close() error   { return nil }

type pipeConnection struct{ net.Conn }

func pipeConnections() (*pipeConnection, *pipeConnection) {
	left, right := net.Pipe()
	return &pipeConnection{left}, &pipeConnection{right}
}
func (connection *pipeConnection) CloseWrite() error { return connection.Close() }
func (*pipeConnection) SupportsUnreliable() bool     { return false }
func (*pipeConnection) SendUnreliable([]byte) error  { return errors.ErrUnsupported }
func (*pipeConnection) ReceiveUnreliable(context.Context) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

type testAddr string

func (address testAddr) Network() string { return "pipe" }
func (address testAddr) String() string  { return string(address) }

func waitFor(t *testing.T, within time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
