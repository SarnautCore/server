package session

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/cohort"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func TestChatRequestCrossesTheSessionWithAuthenticatedSenderAndNoServerEcho(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "ChatZone",
		TickInterval:     time.Second,
		SnapshotInterval: time.Second,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	alice := Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000a081"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000c081"),
		CharacterName:   "Alice",
		ChargenOptionID: "chargen.league.warrior",
	}
	bob := Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000a082"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000c082"),
		CharacterName:   "Bob",
		ChargenOptionID: "chargen.league.warrior",
	}
	authority := newFakeAuthority()
	authority.mint("ticket-alice", alice)
	authority.mint("ticket-bob", bob)
	chatModule := chat.New(chat.Options{Directory: integrationDirectory{
		"alice": {CharacterID: alice.CharacterID, Name: alice.CharacterName},
		"bob":   {CharacterID: bob.CharacterID, Name: bob.CharacterName},
	}})
	parties := party.New()
	cohorts := cohort.NewPresenceRegistry()
	friendRepository := social.NewMemoryFriendRepository(map[uuid.UUID]string{
		alice.CharacterID: alice.CharacterName,
		bob.CharacterID:   bob.CharacterName,
	})
	friends, err := social.NewFriends(friendRepository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := friends.Replace(t.Context(), alice.CharacterID, []uuid.UUID{bob.CharacterID}); err != nil {
		t.Fatal(err)
	}
	server := Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "chat-integration",
		Zones:           map[string]ZoneBinding{zone.ID(): {World: zone}},
		Authority:       authority,
		Characters:      newFakeCharacters(testTemplate(charstore.Vec3{})),
		Chat:            chatModule,
		Party:           parties,
		Cohorts:         cohorts,
		Friends:         friends,
		Logger:          slog.New(slog.DiscardHandler),
		sessions:        newSessionRegistry(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aliceClient := admitChatClient(t, ctx, server, zone.ID(), "ticket-alice")
	defer aliceClient.close()
	bobClient := admitChatClient(t, ctx, server, zone.ID(), "ticket-bob")
	defer bobClient.close()
	aliceFriends := aliceClient.read(t).GetSocialFriendsReplacement()
	if aliceFriends == nil || aliceFriends.GetRevision() != 1 || len(aliceFriends.GetFriends()) != 1 ||
		aliceFriends.GetFriends()[0].GetCharacterId() != bob.CharacterID.String() ||
		aliceFriends.GetFriends()[0].GetDisplayName() != bob.CharacterName {
		t.Fatalf("Alice friend replacement = %+v, want authoritative Bob", aliceFriends)
	}
	bobFriends := bobClient.read(t).GetSocialFriendsReplacement()
	if bobFriends == nil || bobFriends.GetRevision() != 0 || len(bobFriends.GetFriends()) != 0 {
		t.Fatalf("Bob friend replacement = %+v, want authoritative empty set", bobFriends)
	}
	if _, err := friends.Replace(t.Context(), alice.CharacterID, nil); err != nil {
		t.Fatal(err)
	}
	aliceFriends = aliceClient.read(t).GetSocialFriendsReplacement()
	if aliceFriends == nil || aliceFriends.GetRevision() != 2 || len(aliceFriends.GetFriends()) != 0 {
		t.Fatalf("Alice live friend replacement = %+v, want authoritative empty revision 2", aliceFriends)
	}

	aliceClient.write(t, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_ChatSendRequest{ChatSendRequest: &sarnautv1.ChatSendRequest{
			RequestId: 81,
			Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER,
			Text:      "raw  e\u0301  U0001f680",
			Target: &sarnautv1.ChatSendRequest_WhisperCharacterName{
				WhisperCharacterName: "Bob",
			},
		}},
	})
	delivery := bobClient.read(t).GetChatDelivery()
	if delivery == nil {
		t.Fatal("Bob received no ChatDelivery")
	}
	if delivery.GetSenderEntityId() != aliceClient.entityID || delivery.GetSenderName() != "Alice" ||
		delivery.GetBody().GetUserText() != "raw  e\u0301  U0001f680" || delivery.GetWhisperPeerName() != "Alice" ||
		delivery.GetRequestId() != 0 {
		t.Fatalf("remote ChatDelivery = %+v, want authenticated Alice and raw text", delivery)
	}

	// Empty is validated before the accepted-send throttle. The first frame
	// Alice receives must be this refusal; an illicit server echo would be in
	// front of it on the same reliable stream.
	aliceClient.write(t, &sarnautv1.ClientMessage{
		ClientSeq: 2,
		Payload: &sarnautv1.ClientMessage_ChatSendRequest{ChatSendRequest: &sarnautv1.ChatSendRequest{
			RequestId: 82,
			Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_ZONE,
			Text:      "",
		}},
	})
	rejection := aliceClient.read(t).GetChatRejection()
	if rejection == nil || rejection.GetRequestId() != 82 ||
		rejection.GetReason() != sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_EMPTY ||
		rejection.GetDetail().GetProductLocalizationId() != "chat.error.empty" {
		t.Fatalf("Alice first response = %+v, want typed EMPTY rejection and no echo", rejection)
	}
	if _, refusal := parties.Membership(alice.CharacterID); refusal != party.AudienceNoParty {
		t.Fatalf("live authenticated party presence = %v, want connected solo", refusal)
	}
	if !cohorts.Connected(alice.CharacterID) {
		t.Fatal("live authenticated cohort presence is missing")
	}
	aliceClient.close()
	select {
	case <-aliceClient.result:
	case <-ctx.Done():
		t.Fatal("Alice session did not tear down party presence")
	}
	if _, refusal := parties.Membership(alice.CharacterID); refusal != party.AudienceNotMember {
		t.Fatalf("closed party presence = %v, want not member", refusal)
	}
	if cohorts.Connected(alice.CharacterID) {
		t.Fatal("closed authenticated cohort presence is still connected")
	}
}

func TestFriendsProjectWithoutAChatModule(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "SocialZone",
		TickInterval:     time.Second,
		SnapshotInterval: time.Second,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	owner := Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000a083"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000c083"),
		CharacterName:   "Carol",
		ChargenOptionID: "chargen.league.warrior",
	}
	friendID := uuid.MustParse("019200f0-0000-7000-8000-00000000c084")
	authority := newFakeAuthority()
	authority.mint("ticket-carol", owner)
	repository := social.NewMemoryFriendRepository(map[uuid.UUID]string{friendID: "Dmitri"})
	friends, err := social.NewFriends(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := friends.Replace(t.Context(), owner.CharacterID, []uuid.UUID{friendID}); err != nil {
		t.Fatal(err)
	}
	server := Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "social-integration",
		Zones:           map[string]ZoneBinding{zone.ID(): {World: zone}},
		Authority:       authority,
		Characters:      newFakeCharacters(testTemplate(charstore.Vec3{})),
		Friends:         friends,
		Logger:          slog.New(slog.DiscardHandler),
		sessions:        newSessionRegistry(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := admitChatClient(t, ctx, server, zone.ID(), "ticket-carol")
	defer client.close()
	replacement := client.read(t).GetSocialFriendsReplacement()
	if replacement == nil || replacement.GetRevision() != 1 || len(replacement.GetFriends()) != 1 ||
		replacement.GetFriends()[0].GetCharacterId() != friendID.String() ||
		replacement.GetFriends()[0].GetDisplayName() != "Dmitri" {
		t.Fatalf("friend replacement = %+v, want authoritative Dmitri", replacement)
	}
}

type integrationDirectory map[string]chat.Character

func (directory integrationDirectory) ResolveCharacterName(_ context.Context, name string) (chat.Character, bool, error) {
	character, ok := directory[strings.ToLower(name)]
	return character, ok, nil
}

type chatIntegrationClient struct {
	ctx        context.Context
	client     Client
	connection transport.Connection
	serverSide transport.Connection
	entityID   uint64
	result     chan error
}

func admitChatClient(t *testing.T, ctx context.Context, server Server, zoneID, ticket string) *chatIntegrationClient {
	t.Helper()
	serverSide, clientSide := newPipeConnections(false)
	results := make(chan error, 1)
	go func() { results <- server.handle(ctx, serverSide) }()
	client := Client{ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1, BuildID: "chat-client", Ticket: ticket}
	if _, err := client.Handshake(ctx, clientSide); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	writes := serverSide.writeStarted
	waitForPipeFrameWrite(t, writes, "server hello")
	entered, err := client.EnterZone(clientSide, zoneID)
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	waitForPipeFrameWrite(t, writes, "enter-zone response")
	return &chatIntegrationClient{
		ctx: ctx, client: client, connection: clientSide, serverSide: serverSide,
		entityID: entered.GetOwnEntityId(), result: results,
	}
}

func (client *chatIntegrationClient) write(t *testing.T, message *sarnautv1.ClientMessage) {
	t.Helper()
	if err := transport.WriteMessage(client.connection, message); err != nil {
		t.Fatalf("write client chat: %v", err)
	}
}

func (client *chatIntegrationClient) read(t *testing.T) *sarnautv1.ServerMessage {
	t.Helper()
	message, err := client.client.ReadReliableMessage(client.connection)
	if err != nil {
		t.Fatalf("read server chat: %v", err)
	}
	return message
}

func (client *chatIntegrationClient) close() {
	_ = client.connection.Close()
	_ = client.serverSide.Close()
}
