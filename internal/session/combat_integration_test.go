package session_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

// targetMobID is the M2 combat target of mechanics/combat.md section 6.1.
const targetMobID = "mob.paper-harbor.tide-crab"

// TestKillLoopOverQUIC runs the whole M2 combat slice over a real QUIC
// connection: connect, enter, target, cast until the mob dies, and read the
// death event.
//
// The unit tests in `internal/combat` drive the simulation directly and can
// therefore step a tick at a time. This one cannot, and that is the point: it
// is the only test that exercises the reliable-channel round trip that the
// combat verb actually travels on, the mapping in both directions, and the
// per-session event fan-out. It takes five seconds of wall clock because six
// casts a global cooldown apart is five seconds, which is a fact about the
// rules rather than about the test.
func TestKillLoopOverQUIC(t *testing.T) {
	t.Parallel()

	content := loadFixturePack(t)
	anchor, ok := targetAnchor(content)
	if !ok {
		t.Fatalf("the fixture pack has no placement for %q", targetMobID)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "KillLoopZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
		// Six metres from the mob, which is the scenario input of the worked
		// example. The pack's own player spawn is on the other side of the map.
		PlayerSpawn: anchor.Add(world.Vec3{X: 6}),
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})
	if err := combatModule.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "kill-loop-test",
		Zones: map[string]session.ZoneBinding{zone.ID(): {
			World: zone, Combat: combatModule,
			CombatLoadouts: session.CombatLoadouts{"chargen.league.warrior": {
				AbilityIDs: rules.AbilityIDs(), MaxHealth: combat.MaxHealth(1, 1),
			}},
		}},
		// The shard admits nobody without a ticket, so even a combat test has to
		// come through admission (ADR 0030). The character materializes six
		// metres from the mob because that is the worked example's scenario
		// input: with admission in place it is the chargen spawn that decides
		// where a fresh character stands, not the zone's configured one.
		Authority: new(stubAuthority),
		Characters: newStubCharacters(integrationTemplate(charstore.Vec3{
			X: anchor.X + 6,
			Y: anchor.Y,
			Z: anchor.Z,
		})),
		Logger: slog.New(slog.DiscardHandler),
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
		BuildID:         "kill-loop-client",
		Ticket:          stubTicket,
	}
	if _, err := client.Handshake(ctx, connection); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	entered, err := client.EnterZone(connection, zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}

	// The client picks its target the way a real one does: out of a snapshot,
	// by content id. Nothing tells it the entity id up front.
	target := findTargetInSnapshots(t, ctx, client, connection)
	if target.GetMaxHealth() != 120 {
		t.Fatalf("target max_health = %d, want the pack-derived 120", target.GetMaxHealth())
	}

	// The sequence number counts frames sent, not casts landed. A refused cast
	// does not advance the server's accepted sequence, but resending the same
	// number would be a retransmit and would be discarded without a reply.
	var total int32
	var seq, casts, killedAt uint64
	var deaths []*sarnautv1.DeathEvent
	for attempt := 0; attempt < 200 && killedAt == 0; attempt++ {
		seq++
		if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
			ClientSeq: seq,
			Payload: &sarnautv1.ClientMessage_AbilityUse{
				AbilityUse: &sarnautv1.AbilityUse{
					TargetId:  target.GetEntityId(),
					AbilityId: "ability.melee.harbor-cleave",
				},
			},
		}); err != nil {
			t.Fatalf("SendCommand() error = %v", err)
		}

		event, death := awaitCombatEvent(t, client, connection)
		if death != nil {
			deaths = append(deaths, death)
		}
		if event.GetRejection() == sarnautv1.AbilityRejection_ABILITY_REJECTION_ON_COOLDOWN {
			// The client casts as fast as the round trip allows; the server is
			// the one that decides when the cooldown is up.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if event.GetRejection() != sarnautv1.AbilityRejection_ABILITY_REJECTION_NONE {
			t.Fatalf("cast %d refused: %v", casts+1, event.GetRejection())
		}
		casts++
		total += event.GetDamage()
		if event.GetDamage() != 20 {
			t.Errorf("cast %d damage = %d, want 20", casts, event.GetDamage())
		}
		if event.GetKillingBlow() {
			killedAt = casts
		}
	}

	if killedAt != 6 {
		t.Errorf("the mob died on cast %d, want the sixth", killedAt)
	}
	if total != 120 {
		t.Errorf("total damage = %d, want 120", total)
	}

	if len(deaths) == 0 {
		deaths = append(deaths, awaitDeathEvent(t, client, connection))
	}
	if len(deaths) != 1 {
		t.Errorf("%d death events arrived for one kill, want exactly one", len(deaths))
	}
	death := deaths[0]
	if death.GetVictimEntityId() != target.GetEntityId() {
		t.Errorf("death event victim = %d, want %d", death.GetVictimEntityId(), target.GetEntityId())
	}
	if death.GetKillerEntityId() != entered.GetOwnEntityId() {
		t.Errorf("death event killer = %d, want this session's entity %d",
			death.GetKillerEntityId(), entered.GetOwnEntityId())
	}
	if death.GetVictimLevel() != 2 {
		t.Errorf("death event victim level = %d, want 2", death.GetVictimLevel())
	}

	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	cancel()
	select {
	case err := <-serveErrors:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
}

func targetAnchor(content *pack.Pack) (world.Vec3, bool) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID != targetMobID {
			continue
		}
		return world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}, true
	}
	return world.Vec3{}, false
}

func findTargetInSnapshots(
	t *testing.T,
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
) *sarnautv1.EntitySnapshot {
	t.Helper()
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetContentId() == targetMobID {
				return entity
			}
		}
	}
}

// awaitCombatEvent reads the reliable stream until a combat event arrives,
// returning any death event it passed on the way.
//
// Combat is never a datagram, so this is the only channel either of them can
// come on, and they are ordered: the hit that killed the mob is written before
// the death it caused.
func awaitCombatEvent(
	t *testing.T,
	client session.Client,
	connection transport.Connection,
) (*sarnautv1.CombatEvent, *sarnautv1.DeathEvent) {
	t.Helper()
	var death *sarnautv1.DeathEvent
	for {
		message, err := client.ReadReliableMessage(connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if seen := message.GetDeathEvent(); seen != nil {
			death = seen
		}
		if event := message.GetCombatEvent(); event != nil {
			return event, death
		}
	}
}

func awaitDeathEvent(
	t *testing.T,
	client session.Client,
	connection transport.Connection,
) *sarnautv1.DeathEvent {
	t.Helper()
	for {
		message, err := client.ReadReliableMessage(connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if event := message.GetDeathEvent(); event != nil {
			return event
		}
	}
}
