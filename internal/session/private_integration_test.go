package session

import (
	"bytes"
	"context"
	"testing"
	"time"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/gateway"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"google.golang.org/protobuf/proto"
)

func TestPrivateAttachCreatesEntityOnlyAfterAuthentication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "InstLeague1", TickInterval: time.Millisecond, SnapshotInterval: 5 * time.Millisecond,
		MaxMoveSpeed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	go zone.Run(ctx)
	characters := newFakeCharacters(testTemplate(charstore.Vec3{}))
	server := Server{
		Zones: map[string]ZoneBinding{zone.ID(): {World: zone}}, Characters: characters,
		sessions: newSessionRegistry(), privateAttachments: newPrivateAttachmentRegistry(),
		SaveInterval: time.Hour,
	}
	shardSide, gatewaySide := newPipeConnections(false)
	secret := bytes.Repeat([]byte{0x39}, 32)
	trust := gateway.TrustConfig{
		InstanceID: "gateway-test", ShardID: "shard-test", KeyID: "m3-a", Secret: secret,
	}
	results := make(chan error, 1)
	go func() { results <- server.handlePrivate(ctx, shardSide, trust) }()
	if err := gateway.AuthenticateGateway(ctx, gatewaySide, trust); err != nil {
		t.Fatalf("AuthenticateGateway() error = %v", err)
	}
	identity := testAdmission()
	request := &privatev1.AttachRequest{
		SessionId: "019200f0-0000-7000-8000-00000000e001", AttachmentEpoch: 1, ZoneId: zone.ID(),
		Identity: &privatev1.AssertedIdentity{
			AccountId: identity.AccountID.String(), CharacterId: identity.CharacterID.String(),
			CharacterName: identity.CharacterName, ChargenOptionId: identity.ChargenOptionID,
		},
	}
	if err := transport.WriteMessage(gatewaySide, &privatev1.Control{Payload: &privatev1.Control_AttachRequest{
		AttachRequest: request,
	}}); err != nil {
		t.Fatal(err)
	}
	ack := new(privatev1.Control)
	if err := transport.ReadMessage(gatewaySide, ack); err != nil {
		t.Fatal(err)
	}
	if ack.GetAttached() == nil || ack.GetAttached().GetAttachmentEpoch() != 1 {
		t.Fatalf("attach acknowledgement = %v", ack)
	}
	entered := readRelayedEnterZoneResponse(t, gatewaySide)
	if entered.GetOwnEntityId() == 0 || zone.EntityCount() != 1 {
		t.Fatalf("entity id = %d, zone count = %d", entered.GetOwnEntityId(), zone.EntityCount())
	}
	logout, err := proto.Marshal(&sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Logout{Logout: new(sarnautv1.Logout)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteMessage(gatewaySide, &privatev1.Control{Payload: &privatev1.Control_ClientReliable{
		ClientReliable: &privatev1.RelayFrame{Payload: logout},
	}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("handlePrivate() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("private attachment did not stop after logout")
	}
	if zone.EntityCount() != 0 {
		t.Fatalf("zone still holds %d entities", zone.EntityCount())
	}
}

func TestPrivateAttachWrongSecretCreatesNoEntity(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "InstLeague1", TickInterval: time.Millisecond, SnapshotInterval: time.Millisecond, MaxMoveSpeed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Zones:      map[string]ZoneBinding{zone.ID(): {World: zone}},
		Characters: newFakeCharacters(testTemplate(charstore.Vec3{})),
		sessions:   newSessionRegistry(), privateAttachments: newPrivateAttachmentRegistry(),
	}
	shardSide, gatewaySide := newPipeConnections(false)
	serverTrust := gateway.TrustConfig{
		InstanceID: "gateway-test", ShardID: "shard-test", KeyID: "m3-a",
		Secret: bytes.Repeat([]byte{0x39}, 32),
	}
	clientTrust := serverTrust
	clientTrust.Secret = bytes.Repeat([]byte{0x93}, 32)
	results := make(chan error, 1)
	go func() {
		err := server.handlePrivate(context.Background(), shardSide, serverTrust)
		_ = shardSide.Close()
		results <- err
	}()
	if err := gateway.AuthenticateGateway(context.Background(), gatewaySide, clientTrust); err == nil {
		t.Fatal("gateway authentication succeeded with the wrong secret")
	}
	_ = gatewaySide.Close()
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("shard accepted the wrong secret")
		}
	case <-time.After(time.Second):
		t.Fatal("shard did not stop after the refused proof")
	}
	if zone.EntityCount() != 0 {
		t.Fatalf("unauthenticated private peer created %d entities", zone.EntityCount())
	}
}

func readRelayedEnterZoneResponse(t *testing.T, connection transport.Connection) *sarnautv1.EnterZoneResponse {
	t.Helper()
	control := new(privatev1.Control)
	if err := transport.ReadMessage(connection, control); err != nil {
		t.Fatal(err)
	}
	if control.GetServerReliable() == nil {
		t.Fatalf("private control = %v", control)
	}
	response := new(sarnautv1.EnterZoneResponse)
	if err := proto.Unmarshal(control.GetServerReliable().GetPayload(), response); err != nil {
		t.Fatal(err)
	}
	return response
}
