package session_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func TestShardReplicatesFixtureNPCAndAuthoritativeMovementOverQUIC(t *testing.T) {
	t.Parallel()

	content := loadFixturePack(t)
	spawns := content.NPCSpawns()
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "FixtureZone",
		TickInterval:     5 * time.Millisecond,
		SnapshotInterval: 10 * time.Millisecond,
		MaxMoveSpeed:     6,
		PlayerSpawn: world.Vec3{
			X: content.Zone().PlayerSpawn.X,
			Y: content.Zone().PlayerSpawn.Y,
			Z: content.Zone().PlayerSpawn.Z,
		},
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})
	if err := combatModule.Populate(spawns); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	// Generous, because this test now runs a whole session — join, replicate,
	// move, log out — over real QUIC under -race on shared CI hardware.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go zone.Run(ctx)
	go combatModule.Run(ctx)

	serverTLS, err := transport.NewDevServerTLSConfig()
	if err != nil {
		t.Fatalf("NewDevServerTLSConfig() error = %v", err)
	}
	listener, err := transport.ListenQUIC("127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("ListenQUIC() error = %v", err)
	}
	defer func() { _ = listener.Close() }()

	// A fresh character materializes at the chargen spawn, which in the fixture
	// is not the zone's configured PlayerSpawn: this test pins the pack's zone
	// spawn, so the template says so explicitly.
	spawn := store.Vec3{
		X: content.Zone().PlayerSpawn.X,
		Y: content.Zone().PlayerSpawn.Y,
		Z: content.Zone().PlayerSpawn.Z,
	}
	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
		Zones:           map[string]session.ZoneBinding{zone.ID(): {World: zone, Combat: combatModule}},
		Authority:       new(stubAuthority),
		Characters:      newStubCharacters(integrationTemplate(spawn)),
		Logger:          slog.New(slog.DiscardHandler),
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()

	connection, err := transport.DialQUIC(ctx, listener.Addr().String(), transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	defer func() { _ = connection.Close() }()
	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "client-test",
		Ticket:          stubTicket,
	}
	if _, err := client.Handshake(ctx, connection); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	entered, err := client.EnterZone(connection, zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	// The pack, not the composition root, decides where a player starts.
	if entered.GetSpawnPosition().GetX() != content.Zone().PlayerSpawn.X {
		t.Errorf(
			"EnterZone() spawn x = %v, want the pack's player spawn %v",
			entered.GetSpawnPosition().GetX(), content.Zone().PlayerSpawn.X,
		)
	}
	if !connection.SupportsUnreliable() {
		t.Fatal("QUIC connection did not negotiate datagrams")
	}

	waitForNPC(t, ctx, client, connection)

	// A reliable verb the shard understands but does not handle yet must not
	// disturb the datagram move path, and it must not go unread: the reliable
	// reader runs even though datagrams were negotiated.
	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_AbilityUse{
			AbilityUse: &sarnautv1.AbilityUse{TargetId: 1},
		},
	}); err != nil {
		t.Fatalf("SendCommand() error = %v", err)
	}
	if err := client.SendMoveIntent(connection, &sarnautv1.ClientMoveIntent{
		Seq:       1,
		Input:     &sarnautv1.Vec3{X: 1},
		Heading:   0.5,
		DtSeconds: 0.2,
	}); err != nil {
		t.Fatalf("SendMoveIntent() error = %v", err)
	}
	waitForAdvance(t, ctx, client, connection, entered.GetOwnEntityId(), entered.GetSpawnPosition().GetX())

	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	waitForSessionEnd(t, ctx, client, connection)

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

// loadFixturePack reads the golden pack vendored at `testdata/packs/demo`. It
// is compiled from the hand-authored demo dataset, so this test needs neither
// the private data repository nor a YAML parser.
func loadFixturePack(t *testing.T) *pack.Pack {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	if len(content.NPCSpawns()) == 0 {
		t.Fatal("fixture pack resolved no NPCs")
	}
	return content
}

func waitForNPC(
	t *testing.T,
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
) {
	t.Helper()
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetKind() != sarnautv1.EntityKind_ENTITY_KIND_NPC {
				continue
			}
			if !entity.GetAlive() {
				t.Error("alive = false, want true for a fixture NPC")
			}
			if entity.GetLevel() == 0 || entity.GetMaxHealth() == 0 {
				t.Errorf("level = %d, max_health = %d, want both set",
					entity.GetLevel(), entity.GetMaxHealth())
			}
			return
		}
	}
}

// waitForSessionEnd asserts that a clean logout ends the session rather than
// leaving the connection open until the client gives up.
func waitForSessionEnd(
	t *testing.T,
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := client.ReadSnapshot(ctx, connection); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session stayed open after logout")
		}
	}
}

func waitForAdvance(
	t *testing.T,
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
	entityID uint64,
	startX float32,
) {
	t.Helper()
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetEntityId() == entityID && entity.GetPosition().GetX() > startX {
				return
			}
		}
	}
}
