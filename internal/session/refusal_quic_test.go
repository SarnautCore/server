package session_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

// Every refusal test in this package runs over real QUIC on purpose.
//
// The net.Pipe harness the unit tests use is unbuffered: a write completes only
// once the reader has consumed it, so a refusal is always delivered there no
// matter what the server does with the connection afterwards. Over QUIC a
// CONNECTION_CLOSE does not wait for queued stream data, so a refusal written
// immediately before a close can be discarded and the peer sees an opaque
// connection abort with no diagnosis. Only this harness can tell the difference.

const testPackID = "1f4a0c2e5b8d7a9c0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d"

// TestShardRefusesAMismatchedPackAndTheReasonSurvivesTheClose covers two
// findings at once: a shard that states its pack id can refuse a client with a
// different one, and the refusal actually reaches that client.
func TestShardRefusesAMismatchedPackAndTheReasonSurvivesTheClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := serveShard(t, ctx, session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
		PackID:          testPackID,
		Logger:          slog.New(slog.DiscardHandler),
	})

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	defer func() { _ = connection.Close() }()

	// The exchange is written out by hand rather than run through
	// session.Client, because the client refuses locally on the ServerHello and
	// would never read the frame this test is about.
	if err := transport.WriteMessage(connection, &sarnautv1.ClientHello{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildId:         "client-test",
		PackId:          "0000000000000000000000000000000000000000000000000000000000000000",
	}); err != nil {
		t.Fatalf("write client hello: %v", err)
	}
	hello := new(sarnautv1.ServerHello)
	if err := transport.ReadMessage(connection, hello); err != nil {
		t.Fatalf("read server hello: %v", err)
	}
	// The shard answers with its own hello first, so the client can display both
	// digests instead of guessing (ADR 0027).
	if hello.GetPackId() != testPackID {
		t.Fatalf("ServerHello.pack_id = %q, want the shard's own pack id", hello.GetPackId())
	}

	refusal := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(connection, refusal); err != nil {
		t.Fatalf("read refusal: %v; the typed error did not survive the close", err)
	}
	if got := refusal.GetError().GetCode(); got != sarnautv1.ErrorCode_ERROR_CODE_PACK_MISMATCH {
		t.Errorf("error code = %v, want ERROR_CODE_PACK_MISMATCH", got)
	}
}

// A shard that names a pack must still admit the client that names the same one,
// or the gate is just an outage.
func TestShardAdmitsAClientCarryingTheSamePackID(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := serveShard(t, ctx, session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
		PackID:          testPackID,
		Logger:          slog.New(slog.DiscardHandler),
	})

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	defer func() { _ = connection.Close() }()

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "client-test",
		PackID:          testPackID,
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	if hello.GetPackId() != testPackID {
		t.Errorf("ServerHello.pack_id = %q, want %q", hello.GetPackId(), testPackID)
	}
}

// A client that makes no content claim is refused unless the shard was
// configured to take it (protocol/session.md rule 5.1.4).
func TestShardGatesAClientThatNamesNoPackOnAllowUnverifiedPack(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		allow   bool
		wantErr bool
	}{
		{name: "refused by default", allow: false, wantErr: true},
		{name: "admitted when allowed", allow: true, wantErr: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			address := serveShard(t, ctx, session.Server{
				ProtocolVersion:     sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
				BuildID:             "shard-test",
				PackID:              testPackID,
				AllowUnverifiedPack: testCase.allow,
				Logger:              slog.New(slog.DiscardHandler),
			})

			connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
			if err != nil {
				t.Fatalf("DialQUIC() error = %v", err)
			}
			defer func() { _ = connection.Close() }()

			// PackID empty: the client states nothing about its content.
			client := session.Client{
				ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
				BuildID:         "client-test",
			}
			if _, err := client.Handshake(ctx, connection); err != nil {
				t.Fatalf("Handshake() error = %v", err)
			}
			// A packless client's own Handshake cannot detect the refusal, so the
			// next reliable frame is what says whether it was admitted.
			refusal, err := readRefusal(t, ctx, connection)
			switch {
			case testCase.wantErr && err != nil:
				t.Fatalf("read refusal: %v; the typed error did not survive the close", err)
			case testCase.wantErr:
				if got := refusal.GetError().GetCode(); got != sarnautv1.ErrorCode_ERROR_CODE_PACK_MISMATCH {
					t.Errorf("error code = %v, want ERROR_CODE_PACK_MISMATCH", got)
				}
			case err == nil:
				t.Fatalf("the shard refused a packless client with allow_unverified_pack set: %v", refusal)
			}
		})
	}
}

// The in-session half of the same guarantee. A refusal raised by the command
// reader is written and then the whole session unwinds — the reliable reader,
// the snapshot sender and the save loop all stop — so the frame has to outlive
// a teardown that closes the connection to unblock its own goroutines.
func TestAnInSessionRefusalReachesThePeerBeforeTheSessionTearsDown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "RefusalZone",
		TickInterval:     5 * time.Millisecond,
		SnapshotInterval: 10 * time.Millisecond,
		MaxMoveSpeed:     6,
		PlayerSpawn:      world.Vec3{X: 1, Y: 1},
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	go zone.Run(ctx)

	address := serveShard(t, ctx, session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
		Zones:           map[string]session.ZoneBinding{zone.ID(): {World: zone}},
		Authority:       new(stubAuthority),
		Characters:      newStubCharacters(integrationTemplate(charstore.Vec3{X: 1, Y: 1})),
		Logger:          slog.New(slog.DiscardHandler),
	})

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
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
	if _, err := client.EnterZone(connection, zone.ID()); err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	if !connection.SupportsUnreliable() {
		t.Fatal("QUIC connection did not negotiate datagrams")
	}

	// Movement on the ordered stream while datagrams are negotiated: understood,
	// ineligible, and refused (protocol/session.md rule 5.5.4).
	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_MoveIntent{
			MoveIntent: &sarnautv1.ClientMoveIntent{
				Seq:       1,
				Input:     &sarnautv1.Vec3{X: 1},
				DtSeconds: 0.1,
			},
		},
	}); err != nil {
		t.Fatalf("SendCommand() error = %v", err)
	}

	refusal, err := readRefusal(t, ctx, connection)
	if err != nil {
		t.Fatalf("read refusal: %v; the typed error did not survive the teardown", err)
	}
	if got := refusal.GetError().GetCode(); got != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Errorf("error code = %v, want ERROR_CODE_UNSUPPORTED_MESSAGE", got)
	}
}

// A peer that completes the QUIC handshake, opens the stream and then says
// nothing must not hold a shard goroutine open indefinitely. quic-go's idle
// timeout is reset by any packet, so an attacker keeping the connection alive
// with PINGs costs nothing and the shard has to impose its own deadline.
func TestShardClosesAConnectionThatNeverSendsAHello(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := serveShard(t, ctx, session.Server{
		ProtocolVersion:  sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:          "shard-test",
		AdmissionTimeout: 250 * time.Millisecond,
		Logger:           slog.New(slog.DiscardHandler),
	})

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	defer func() { _ = connection.Close() }()

	// Four bytes promising a frame that never arrives. The prefix is what opens
	// the QUIC stream, so the shard is genuinely inside exchangeHello and blocked
	// on a read the peer controls — which is the state the deadline is for.
	if _, err := connection.Write([]byte{0, 0, 0x10, 0}); err != nil {
		t.Fatalf("write partial frame: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- transport.ReadMessage(connection, new(sarnautv1.ServerHello))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the shard answered a hello that was never sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shard held an unauthenticated connection past its admission deadline")
	}
}

// A peer that opens a QUIC connection and never opens a session stream on it
// must not stall admission for anyone else. A QUIC stream is invisible to the
// peer until its opener writes, so accepting the stream on the accept loop's own
// goroutine turns one silent client into a denial of service for the shard.
func TestASilentPeerDoesNotBlockTheAcceptLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	address := serveShard(t, ctx, session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "shard-test",
		Logger:          slog.New(slog.DiscardHandler),
	})

	silent, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC(silent) error = %v", err)
	}
	defer func() { _ = silent.Close() }()

	honest, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC(honest) error = %v", err)
	}
	defer func() { _ = honest.Close() }()

	handshake, cancelHandshake := context.WithTimeout(ctx, 5*time.Second)
	defer cancelHandshake()
	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "client-test",
	}
	if _, err := client.Handshake(handshake, honest); err != nil {
		t.Fatalf("Handshake() error = %v; a silent peer is holding the accept loop", err)
	}
}

// serveShard starts one shard on an ephemeral port and returns its address. The
// listener and the Serve goroutine are cleaned up with the test.
func serveShard(t *testing.T, ctx context.Context, server session.Server) string {
	t.Helper()

	serverTLS, err := transport.NewDevServerTLSConfig()
	if err != nil {
		t.Fatalf("NewDevServerTLSConfig() error = %v", err)
	}
	listener, err := transport.ListenQUIC("127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("ListenQUIC() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(ctx, listener) }()
	return listener.Addr().String()
}

// readRefusal reads the next reliable frame and requires it to be a typed error.
func readRefusal(
	t *testing.T,
	ctx context.Context,
	connection transport.Connection,
) (*sarnautv1.ServerMessage, error) {
	t.Helper()

	type result struct {
		message *sarnautv1.ServerMessage
		err     error
	}
	results := make(chan result, 1)
	go func() {
		for {
			message := new(sarnautv1.ServerMessage)
			if err := transport.ReadMessage(connection, message); err != nil {
				results <- result{err: err}
				return
			}
			// Snapshots take the reliable channel too when datagrams are off, and
			// a session that is being torn down may have queued one first.
			if message.GetError() == nil {
				continue
			}
			results <- result{message: message}
			return
		}
	}()
	select {
	case got := <-results:
		return got.message, got.err
	case <-ctx.Done():
		return nil, errors.New("no frame arrived before the test deadline")
	}
}
