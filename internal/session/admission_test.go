package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

// admissionFixture is one shard with one zone, one authority and one character
// store, driven over an in-memory connection.
type admissionFixture struct {
	ctx        context.Context
	zone       *world.Zone
	authority  *fakeAuthority
	characters *fakeCharacters
	server     Server
	logs       *safeBuffer
}

func newAdmissionFixture(t *testing.T, spawn charstore.Vec3) *admissionFixture {
	t.Helper()

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "AdmissionZone",
		TickInterval:     2 * time.Millisecond,
		SnapshotInterval: 4 * time.Millisecond,
		MaxMoveSpeed:     6,
		PlayerSpawn:      world.Vec3{X: -100, Y: -100},
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go zone.Run(ctx)

	logs := new(safeBuffer)
	fixture := &admissionFixture{
		ctx:        ctx,
		zone:       zone,
		authority:  newFakeAuthority(),
		characters: newFakeCharacters(testTemplate(spawn)),
		logs:       logs,
	}
	fixture.server = Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "admission-test",
		// No combat module: these tests are about admission, and a session in a
		// zone without one still joins, saves and leaves.
		Zones:        map[string]ZoneBinding{zone.ID(): {World: zone}},
		Authority:    fixture.authority,
		Characters:   fixture.characters,
		SaveInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		sessions:     newSessionRegistry(),
	}
	return fixture
}

// connect runs one session handler against a client connection and returns the
// client plus the channel the handler's result arrives on.
func (fixture *admissionFixture) connect(t *testing.T, ticket string) (Client, transport.Connection, chan error) {
	t.Helper()
	serverSide, clientSide := newPipeConnections(true)
	results := make(chan error, 1)
	go func() { results <- fixture.server.handle(fixture.ctx, serverSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})

	client := Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "admission-client",
		Ticket:          ticket,
	}
	if _, err := client.Handshake(fixture.ctx, clientSide); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	return client, clientSide, results
}

// Each refusal is a distinct logged reason, one opaque answer on the wire, and
// no entity. The distinctness is the point: an operator reading the log can
// tell an expired ticket from somebody else's character, and a peer cannot.
func TestEveryAdmissionRefusalIsDistinctlyLoggedAndCreatesNoEntity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		ticket     string
		setup      func(*admissionFixture) string
		wantReason string
	}{
		{
			name:       "no ticket at all",
			setup:      func(*admissionFixture) string { return "" },
			wantReason: ReasonNoTicket,
		},
		{
			name: "a forged ticket",
			setup: func(fixture *admissionFixture) string {
				return fixture.authority.refuse("sarnaut_tk_forged", ReasonMalformed)
			},
			wantReason: ReasonMalformed,
		},
		{
			name: "an expired ticket",
			setup: func(fixture *admissionFixture) string {
				return fixture.authority.refuse("sarnaut_tk_expired", ReasonUnknownTicket)
			},
			wantReason: ReasonUnknownTicket,
		},
		{
			name: "a ticket for another account's character",
			setup: func(fixture *admissionFixture) string {
				return fixture.authority.refuse("sarnaut_tk_someone-elses", ReasonNotOwned)
			},
			wantReason: ReasonNotOwned,
		},
		{
			name: "a character already played on another shard",
			setup: func(fixture *admissionFixture) string {
				return fixture.authority.refuse("sarnaut_tk_locked", ReasonPlayLockHeld)
			},
			wantReason: ReasonPlayLockHeld,
		},
		{
			name: "auth unreachable",
			setup: func(fixture *admissionFixture) string {
				fixture.authority.unavailable = true
				return "sarnaut_tk_anything"
			},
			wantReason: ReasonAuthUnavailable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture := newAdmissionFixture(t, charstore.Vec3{X: 5})
			ticket := testCase.setup(fixture)
			client, connection, results := fixture.connect(t, ticket)

			if _, err := client.EnterZone(connection, fixture.zone.ID()); err == nil {
				t.Fatal("EnterZone() succeeded; the shard admitted an unauthenticated peer")
			}

			select {
			case err := <-results:
				if err == nil {
					t.Fatal("handle() returned nil for a refused admission")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handle() did not return after refusing admission")
			}

			if count := fixture.zone.EntityCount(); count != 0 {
				t.Errorf("zone holds %d entities after a refusal, want 0", count)
			}
			if fixture.characters.loads != 0 {
				t.Errorf("a refused session loaded a character %d times", fixture.characters.loads)
			}
			logs := fixture.logs.String()
			if !strings.Contains(logs, `"reason":"`+testCase.wantReason+`"`) {
				t.Errorf("logs do not carry reason %q:\n%s", testCase.wantReason, logs)
			}
			if strings.Contains(logs, ticket) && ticket != "" {
				t.Errorf("the refusal log carries the ticket itself:\n%s", logs)
			}
		})
	}
}

// The client is told one thing regardless of which refusal it was.
func TestARefusedAdmissionAnswersWithOpaqueUnauthenticated(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionFixture(t, charstore.Vec3{X: 5})
	// Two refusals that a peer must not be able to tell apart.
	fixture.authority.refuse("sarnaut_tk_expired", ReasonUnknownTicket)
	fixture.authority.refuse("sarnaut_tk_someone-elses", ReasonNotOwned)

	details := make(map[string]struct{})
	for _, ticket := range []string{"", "sarnaut_tk_expired", "sarnaut_tk_someone-elses"} {
		client, connection, _ := fixture.connect(t, ticket)
		_, err := client.EnterZone(connection, fixture.zone.ID())
		if err == nil {
			t.Fatalf("EnterZone(%q) succeeded", ticket)
		}
		violation := new(ProtocolViolation)
		if !errors.As(err, &violation) {
			t.Fatalf("EnterZone(%q) error = %v, want a typed refusal the client can render", ticket, err)
		}
		if violation.Code != sarnautv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED {
			t.Errorf("error code = %v, want UNAUTHENTICATED", violation.Code)
		}
		details[violation.Detail] = struct{}{}
	}

	// One answer for a ticket that expired and one for somebody else's
	// character would be two answers; the absent-ticket case is allowed its own,
	// because the peer already knows it sent nothing.
	if len(details) > 2 {
		t.Errorf("the shard gave %d distinguishable refusal details: %v", len(details), details)
	}
	for detail := range details {
		if strings.Contains(detail, "expired") || strings.Contains(detail, "owned") || strings.Contains(detail, "lock") {
			t.Errorf("the refusal detail %q discloses which check failed", detail)
		}
	}
}

// A valid ticket admits exactly one entity, at the position the character store
// loaded — which for a first login is the chargen option's spawn, not the
// zone's configured PlayerSpawn.
func TestAValidTicketSpawnsTheCharacterAtItsChargenSpawn(t *testing.T) {
	t.Parallel()

	spawn := charstore.Vec3{X: 12, Y: 4.5}
	fixture := newAdmissionFixture(t, spawn)
	admission := testAdmission()
	fixture.authority.mint("sarnaut_tk_good", admission)

	client, connection, results := fixture.connect(t, "sarnaut_tk_good")
	entered, err := client.EnterZone(connection, fixture.zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	if entered.GetSpawnPosition().GetX() != spawn.X || entered.GetSpawnPosition().GetY() != spawn.Y {
		t.Errorf("spawn = %v, want the chargen spawn %+v", entered.GetSpawnPosition(), spawn)
	}
	if count := fixture.zone.EntityCount(); count != 1 {
		t.Errorf("zone holds %d entities, want 1", count)
	}

	// S0 stamped the zone before the response was written.
	if !fixture.characters.waitForCheckpoint(1, time.Second) {
		t.Fatal("no checkpoint was taken at zone entry")
	}
	first := fixture.characters.written()[0]
	if first.State.ZoneID != fixture.zone.ID() {
		t.Errorf("S0 wrote zone_id %q, want %q", first.State.ZoneID, fixture.zone.ID())
	}
	if first.State.CharacterID != admission.CharacterID {
		t.Errorf("S0 wrote character %s, want %s", first.State.CharacterID, admission.CharacterID)
	}

	if err := client.Logout(connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	select {
	case <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("handle() did not return after logout")
	}
	if count := fixture.zone.EntityCount(); count != 0 {
		t.Errorf("zone holds %d entities after logout, want 0", count)
	}
	if _, _, released := fixture.authority.counts(); released != 1 {
		t.Errorf("the play lock was released %d times, want 1", released)
	}
}

// safeBuffer is a bytes.Buffer a logger and a test may share.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *safeBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(payload)
}

func (buffer *safeBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
