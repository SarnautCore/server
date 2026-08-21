package session

import (
	"context"
	"strings"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/transport"
)

// enter runs one full admission and returns the client, its connection and the
// handler's result channel.
func (fixture *admissionFixture) enter(t *testing.T, ticket string) (Client, transport.Connection, chan error, *sarnautv1.EnterZoneResponse) {
	t.Helper()
	client, connection, results := fixture.connect(t, ticket)
	entered, err := client.EnterZone(connection, fixture.zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	return client, connection, results, entered
}

// moveUntilAdvanced pushes move intents until the zone reports the player past
// its spawn, so the test asserts on a position the simulation actually produced
// rather than on one it wrote itself.
func moveUntilAdvanced(
	t *testing.T,
	ctx context.Context,
	client Client,
	connection transport.Connection,
	entityID uint64,
	startX float32,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sequence := uint64(1); time.Now().Before(deadline); sequence++ {
		if err := client.SendMoveIntent(connection, &sarnautv1.ClientMoveIntent{
			Seq:       sequence,
			Input:     &sarnautv1.Vec3{X: 1},
			DtSeconds: 0.2,
		}); err != nil {
			t.Fatalf("SendMoveIntent() error = %v", err)
		}
		batch, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range batch.GetEntities() {
			if entity.GetEntityId() == entityID && entity.GetPosition().GetX() > startX+0.5 {
				return
			}
		}
	}
	t.Fatal("the player never advanced past its spawn")
}

// Reconnecting puts the character back where it logged out, not at the origin
// and not at the chargen spawn. This is the property the whole checkpoint
// arrangement exists to produce.
func TestReconnectRestoresTheSavedPositionRatherThanTheOrigin(t *testing.T) {
	t.Parallel()

	spawn := charstore.Vec3{X: 12, Y: 4.5}
	fixture := newAdmissionFixture(t, spawn)
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_first", admission)

	client, connection, results, entered := fixture.enter(t, "sarnaut_tk_first")
	if entered.GetSpawnPosition().GetX() != spawn.X {
		t.Fatalf("first spawn = %v, want the chargen spawn %+v", entered.GetSpawnPosition(), spawn)
	}
	moveUntilAdvanced(t, fixture.ctx, client, connection, entered.GetOwnEntityId(), spawn.X)

	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("handle() did not return after logout")
	}

	saved, ok := fixture.characters.saved(admission.CharacterID)
	if !ok {
		t.Fatal("nothing was saved for the character")
	}
	if saved.State.Position.X <= spawn.X {
		t.Fatalf("saved x = %v, want the simulated position past the spawn %v", saved.State.Position.X, spawn.X)
	}

	// A fresh session for the same character: a new connection, a new ticket,
	// a new entity id, and the same position.
	fixture.authority.mint("sarnaut_tk_second", admission)
	_, _, _, rejoined := fixture.enter(t, "sarnaut_tk_second")
	if got := rejoined.GetSpawnPosition().GetX(); got != saved.State.Position.X {
		t.Errorf("reconnect spawned at x = %v, want the saved %v", got, saved.State.Position.X)
	}
	if rejoined.GetSpawnPosition().GetX() == 0 {
		t.Error("reconnect spawned at the origin")
	}
	if rejoined.GetOwnEntityId() == entered.GetOwnEntityId() {
		t.Error("the entity id was reused across visits; it is per-zone-visit by contract")
	}
}

// One live session per character. The play lock stops a second shard; this is
// the case it cannot arbitrate — two connections on the same shard, which is
// what a reconnect after a half-open connection looks like.
func TestASecondSessionEvictsTheFirstAndTheZoneHoldsOneEntity(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionFixture(t, charstore.Vec3{X: 12})
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_one", admission)
	fixture.authority.mint("sarnaut_tk_two", admission)

	_, _, firstResults, first := fixture.enter(t, "sarnaut_tk_one")
	if count := fixture.zone.EntityCount(); count != 1 {
		t.Fatalf("zone holds %d entities after one session, want 1", count)
	}

	_, _, _, second := fixture.enter(t, "sarnaut_tk_two")

	// The first handler returns on its own, without the test closing anything.
	select {
	case <-firstResults:
	case <-time.After(5 * time.Second):
		t.Fatal("the first session was not evicted by the second")
	}
	if first.GetOwnEntityId() == second.GetOwnEntityId() {
		t.Error("the second session reused the first entity id")
	}
	// The count is the assertion that matters: an eviction that left the old
	// entity behind would leave a ghost standing in the zone forever.
	deadline := time.Now().Add(3 * time.Second)
	for fixture.zone.EntityCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count := fixture.zone.EntityCount(); count != 1 {
		t.Errorf("zone holds %d entities after the eviction, want 1", count)
	}
}

// The disconnect save must survive the context that caused the disconnect. A
// shard shutting down cancels the session context, and an implementation that
// passed it into the save would lose the character's position on every
// shutdown — invisibly, in any test that only exercises clean logout.
func TestAShutdownStillSavesTheSimulatedPosition(t *testing.T) {
	t.Parallel()

	spawn := charstore.Vec3{X: 12, Y: 4.5}
	fixture := newAdmissionFixture(t, spawn)
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_shutdown", admission)

	// A session context the test can cancel, standing in for shard shutdown.
	sessionCtx, shutdown := context.WithCancel(fixture.ctx)
	serverSide, clientSide := newPipeConnections(true)
	results := make(chan error, 1)
	go func() { results <- fixture.server.handle(sessionCtx, serverSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})

	client := Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shutdown-client",
		Ticket:          "sarnaut_tk_shutdown",
	}
	if _, err := client.Handshake(sessionCtx, clientSide); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	entered, err := client.EnterZone(clientSide, fixture.zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	moveUntilAdvanced(t, sessionCtx, client, clientSide, entered.GetOwnEntityId(), spawn.X)

	shutdown()
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("handle() did not return after the context was cancelled")
	}

	saved, ok := fixture.characters.saved(admission.CharacterID)
	if !ok {
		t.Fatal("the shutdown path saved nothing")
	}
	if saved.State.Position.X <= spawn.X {
		t.Errorf("saved x = %v, want the last simulated position past %v", saved.State.Position.X, spawn.X)
	}
	if count := fixture.zone.EntityCount(); count != 0 {
		t.Errorf("zone holds %d entities after teardown, want 0", count)
	}
}

// S1 has to read the entity before it is evicted. Reading afterwards finds
// nothing and writes a stale or zero position, which is the failure ADR 0031 §8
// exists to prevent — and it is invisible unless a test looks at what was
// written rather than at whether a write happened.
func TestTheFinalCheckpointIsTakenBeforeTheEntityIsEvicted(t *testing.T) {
	t.Parallel()

	spawn := charstore.Vec3{X: 12, Y: 4.5}
	fixture := newAdmissionFixture(t, spawn)
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_order", admission)

	client, connection, results, entered := fixture.enter(t, "sarnaut_tk_order")
	moveUntilAdvanced(t, fixture.ctx, client, connection, entered.GetOwnEntityId(), spawn.X)
	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("handle() did not return after logout")
	}

	written := fixture.characters.written()
	if len(written) < 2 {
		t.Fatalf("only %d checkpoints were written; want at least S0 and S1", len(written))
	}
	final := written[len(written)-1]
	if final.State.Position == (charstore.Vec3{}) {
		t.Error("the final checkpoint wrote the origin: it read the entity after eviction")
	}
	if final.State.Position.X <= spawn.X {
		t.Errorf("the final checkpoint wrote x = %v, want the last simulated position", final.State.Position.X)
	}
	// Save sequences advance strictly, which is what makes the stale-write
	// guard able to reject a slow write from a dying session.
	for index := 1; index < len(written); index++ {
		if written[index].State.SaveSeq <= written[index-1].State.SaveSeq {
			t.Fatalf("save_seq did not advance: %d then %d",
				written[index-1].State.SaveSeq, written[index].State.SaveSeq)
		}
	}
	if written[0].State.SaveSeq <= 0 {
		t.Error("the first checkpoint carried no save sequence")
	}
}

// S2 bounds what an unclean exit destroys. Without it the disconnect save is
// the only writer, and a crash loses the entire session.
func TestPeriodicCheckpointsRunWhileTheSessionIsOpen(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionFixture(t, charstore.Vec3{X: 12})
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_periodic", admission)

	_, connection, _, _ := fixture.enter(t, "sarnaut_tk_periodic")
	defer func() { _ = connection.Close() }()

	// The fixture's interval is 50 ms, so three checkpoints is S0 plus two
	// periodic ones rather than a coincidence.
	if !fixture.characters.waitForCheckpoint(3, 5*time.Second) {
		t.Fatalf("only %d checkpoints ran; the periodic saver is not running", len(fixture.characters.written()))
	}
	if _, renewed, _ := fixture.authority.counts(); renewed != 0 {
		// The renewal ticker is 20 s and this test lasts under a second, so a
		// renewal here would mean the interval is wrong.
		t.Errorf("the play lock was renewed %d times in under a second", renewed)
	}
}

// A dropped checkpoint is reported, not swallowed: the bounded queue is allowed
// to drop, and an operator has to be able to see that it did.
func TestADroppedCheckpointIsLogged(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionFixture(t, charstore.Vec3{X: 12})
	fixture.characters.full = true
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_full", admission)

	client, connection, results, _ := fixture.enter(t, "sarnaut_tk_full")
	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("handle() did not return after logout")
	}

	if logs := fixture.logs.String(); !strings.Contains(logs, "character checkpoint dropped") {
		t.Errorf("a dropped checkpoint was not logged:\n%s", logs)
	}
}
