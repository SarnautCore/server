package session

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func TestReliableReaderStaysAliveWhileDatagramsAreNegotiated(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	// net.Pipe is unbuffered, so a write only completes once the peer reads it.
	// Before ADR 0026 the handler stopped reading the stream the moment
	// datagrams were negotiated, and every one of these writes would block
	// until the test deadline.
	for sequence := uint64(1); sequence <= 4; sequence++ {
		harness.writeReliable(t, &sarnautv1.ClientMessage{
			ClientSeq: sequence,
			Payload: &sarnautv1.ClientMessage_AbilityUse{
				AbilityUse: &sarnautv1.AbilityUse{TargetId: 99},
			},
		})
		harness.writeReliable(t, &sarnautv1.ClientMessage{
			ClientSeq: sequence,
			Payload: &sarnautv1.ClientMessage_QuestAccept{
				QuestAccept: &sarnautv1.QuestAccept{QuestId: "quest.smoke"},
			},
		})
		// A quest verb answers on the reader's own goroutine, and net.Pipe is
		// unbuffered, so the answer has to be taken before the next write. A
		// quest id the pack does not carry is refused and the session lives on,
		// which is the point: a refusal is not a protocol violation.
		if refusal := harness.readQuestUpdate(t).GetRefusal(); refusal !=
			sarnautv1.QuestRefusal_QUEST_REFUSAL_UNKNOWN_QUEST {
			t.Fatalf("quest refusal = %v, want UNKNOWN_QUEST", refusal)
		}
	}

	// The move path is untouched by the verbs that just went past it.
	harness.sendMoveIntent(t, 1)
	harness.waitForAdvance(t)

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}

func TestOrderedStreamFallbackCarriesBothDirections(t *testing.T) {
	t.Parallel()
	harness := startSession(t, false)

	// With no datagrams the identical envelopes travel on the stream. The
	// carrier changes; the bytes and the dispatch do not.
	harness.sendMoveIntent(t, 1)
	harness.waitForAdvance(t)

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}

func TestClientMessageWithoutAPayloadCaseIsRefused(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	harness.writeReliable(t, &sarnautv1.ClientMessage{ClientSeq: 7})

	failure := harness.readError(t)
	if failure.GetCode() != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Errorf("error code = %v, want UNSUPPORTED_MESSAGE", failure.GetCode())
	}

	err := harness.wait(t)
	violation := new(ProtocolViolation)
	if !errors.As(err, &violation) {
		t.Fatalf("handle() error = %v, want a *ProtocolViolation", err)
	}
	if violation.Code != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Errorf("violation code = %v, want UNSUPPORTED_MESSAGE", violation.Code)
	}
}

// A reliable interest transition may already be in flight when a reader
// refuses the next client command. The refusal remains ordered behind that
// transition; clients and tests must not treat the first unrelated frame as a
// missing refusal.
func TestProtocolRefusalFollowsAnAlreadyQueuedReliableEvent(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	// The datagram composition puts snapshots on the unreliable channel, so
	// the first post-entry reliable write is the player's spawn transition.
	harness.waitForServerWrite(t)
	harness.writeReliable(t, &sarnautv1.ClientMessage{ClientSeq: 7})

	message := harness.readReliable(t)
	if message.GetSpawnEvent() == nil {
		t.Fatalf("first server message = %v, want the queued spawn event", message)
	}
	failure := harness.readError(t)
	if failure.GetCode() != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Errorf("error code = %v, want UNSUPPORTED_MESSAGE", failure.GetCode())
	}

	err := harness.wait(t)
	violation := new(ProtocolViolation)
	if !errors.As(err, &violation) {
		t.Fatalf("handle() error = %v, want a *ProtocolViolation", err)
	}
}

func TestMoveIntentOnTheStreamIsRefusedWhileDatagramsAreNegotiated(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	harness.writeReliable(t, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_MoveIntent{
			MoveIntent: &sarnautv1.ClientMoveIntent{Seq: 1, Input: &sarnautv1.Vec3{X: 1}},
		},
	})

	failure := harness.readError(t)
	if failure.GetCode() != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatalf("server error = %v, want UNSUPPORTED_MESSAGE", failure)
	}
	if err := harness.wait(t); err == nil {
		t.Fatal("handle() error = nil, want a protocol violation")
	}
}

func TestSnapshotsCarryContentAndCombatFields(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no snapshot carried the entity")
		default:
		}
		batch, err := harness.client.ReadSnapshot(harness.ctx, harness.connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range batch.GetEntities() {
			if entity.GetEntityId() != harness.entityID {
				continue
			}
			if !entity.GetAlive() {
				t.Error("alive = false, want true for a freshly joined player")
			}
			if entity.GetLevel() == 0 {
				t.Error("level = 0, want a placed level")
			}
			if entity.GetHealth() <= 0 || entity.GetHealth() != entity.GetMaxHealth() {
				t.Errorf("health = %d, max_health = %d, want equal and positive",
					entity.GetHealth(), entity.GetMaxHealth())
			}
			return
		}
	}
}

type sessionHarness struct {
	ctx          context.Context
	client       Client
	connection   transport.Connection
	serverWrites <-chan struct{}
	entityID     uint64
	spawnX       float32
	results      chan error
}

func startSession(t *testing.T, unreliable bool) *sessionHarness {
	t.Helper()

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "HarnessZone",
		TickInterval:     2 * time.Millisecond,
		SnapshotInterval: 4 * time.Millisecond,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	// The harness carries a real combat module over the vendored fixture pack,
	// because a session without one has no level, no faction and nowhere to
	// send an ability use, and half of what these tests assert about a session
	// would be vacuous.
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})
	// A real quest module over the same pack, for the same reason: without one
	// the three quest verbs would be refused as unsupported and the dispatch
	// they are supposed to exercise would never run. Its grants go to a store
	// nothing else writes, which is enough for the verbs these tests send —
	// every one of them is refused before a transaction starts.
	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{})
	if err != nil {
		t.Fatalf("CatalogFromPack() error = %v", err)
	}
	bags, err := charstore.NewInventoryService(charstore.NewMemory(), inventory.LimitsFromPack(content), 0)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	questModule := quests.New(slog.New(slog.DiscardHandler), zone, catalog, bags)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go zone.Run(ctx)
	go combatModule.Run(ctx)
	go questModule.Run(ctx)

	serverSide, clientSide := newPipeConnections(unreliable)
	admission := testAdmission()
	authority := newFakeAuthority()
	authority.mint("sarnaut_tk_harness", admission)
	server := Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "harness",
		Zones: map[string]ZoneBinding{
			zone.ID(): {World: zone, Combat: combatModule, Quests: questModule},
		},
		Authority:  authority,
		Characters: newFakeCharacters(testTemplate(charstore.Vec3{})),
		Logger:     slog.New(slog.DiscardHandler),
		sessions:   newSessionRegistry(),
	}
	results := make(chan error, 1)
	go func() { results <- server.handle(ctx, serverSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})

	client := Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "harness-client",
		Ticket:          "sarnaut_tk_harness",
	}
	if _, err := client.Handshake(ctx, clientSide); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	waitForPipeFrameWrite(t, serverSide.writeStarted, "server hello")
	entered, err := client.EnterZone(clientSide, zone.ID())
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	waitForPipeFrameWrite(t, serverSide.writeStarted, "enter-zone response")

	return &sessionHarness{
		ctx:          ctx,
		client:       client,
		connection:   clientSide,
		serverWrites: serverSide.writeStarted,
		entityID:     entered.GetOwnEntityId(),
		spawnX:       entered.GetSpawnPosition().GetX(),
		results:      results,
	}
}

func waitForPipeWrite(t *testing.T, writes <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-writes:
	case <-time.After(2 * time.Second):
		t.Fatalf("no %s write started", name)
	}
}

func waitForPipeFrameWrite(t *testing.T, writes <-chan struct{}, name string) {
	t.Helper()
	// WriteMessage writes the four-byte header and protobuf payload separately.
	waitForPipeWrite(t, writes, name+" header")
	waitForPipeWrite(t, writes, name+" payload")
}

func (harness *sessionHarness) writeReliable(t *testing.T, message *sarnautv1.ClientMessage) {
	t.Helper()
	written := make(chan error, 1)
	go func() { written <- transport.WriteMessage(harness.connection, message) }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write client message: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write client message blocked: nobody is reading the reliable stream")
	}
}

// readQuestUpdate drains the reliable stream until a quest update arrives.
// Combat events share the channel and are written by their own goroutine, so
// what comes first is a scheduling detail and not something to assert on.
func (harness *sessionHarness) readQuestUpdate(t *testing.T) *sarnautv1.QuestStateUpdate {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		if update := harness.readReliable(t).GetQuestStateUpdate(); update != nil {
			return update
		}
	}
	t.Fatal("no quest update arrived on the reliable stream")
	return nil
}

func (harness *sessionHarness) readReliable(t *testing.T) *sarnautv1.ServerMessage {
	t.Helper()
	type result struct {
		message *sarnautv1.ServerMessage
		err     error
	}
	results := make(chan result, 1)
	go func() {
		message, err := harness.client.ReadReliableMessage(harness.connection)
		results <- result{message: message, err: err}
	}()
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("read server message: %v", got.err)
		}
		return got.message
	case <-time.After(2 * time.Second):
		t.Fatal("no server message arrived on the reliable stream")
		return nil
	}
}

func (harness *sessionHarness) readError(t *testing.T) *sarnautv1.Error {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		if failure := harness.readReliable(t).GetError(); failure != nil {
			return failure
		}
	}
	t.Fatal("no protocol error arrived on the reliable stream")
	return nil
}

func (harness *sessionHarness) waitForServerWrite(t *testing.T) {
	t.Helper()
	waitForPipeWrite(t, harness.serverWrites, "post-entry reliable event")
}

func (harness *sessionHarness) sendMoveIntent(t *testing.T, sequence uint64) {
	t.Helper()
	if err := harness.client.SendMoveIntent(harness.connection, &sarnautv1.ClientMoveIntent{
		Seq:       sequence,
		Input:     &sarnautv1.Vec3{X: 1},
		DtSeconds: 0.2,
	}); err != nil {
		t.Fatalf("SendMoveIntent() error = %v", err)
	}
}

func (harness *sessionHarness) waitForAdvance(t *testing.T) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the player never advanced")
		default:
		}
		batch, err := harness.client.ReadSnapshot(harness.ctx, harness.connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range batch.GetEntities() {
			if entity.GetEntityId() == harness.entityID &&
				entity.GetPosition().GetX() > harness.spawnX {
				return
			}
		}
	}
}

func (harness *sessionHarness) logout(t *testing.T) {
	t.Helper()
	if err := harness.client.Logout(harness.connection); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
}

func (harness *sessionHarness) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-harness.results:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("handle() did not return")
		return nil
	}
}

// pipeConnection is an in-memory transport.Connection. Its stream half is an
// unbuffered net.Pipe, which makes "nobody is reading" observable as a blocked
// write, and its datagram half is a lossy buffered channel.
type pipeConnection struct {
	net.Conn
	unreliable   bool
	incoming     chan []byte
	outgoing     chan []byte
	writeStarted chan struct{}
}

func newPipeConnections(unreliable bool) (serverSide, clientSide *pipeConnection) {
	serverStream, clientStream := net.Pipe()
	toServer := make(chan []byte, 64)
	toClient := make(chan []byte, 64)
	serverSide = &pipeConnection{
		Conn:         serverStream,
		unreliable:   unreliable,
		incoming:     toServer,
		outgoing:     toClient,
		writeStarted: make(chan struct{}, 16),
	}
	clientSide = &pipeConnection{
		Conn:       clientStream,
		unreliable: unreliable,
		incoming:   toClient,
		outgoing:   toServer,
	}
	return serverSide, clientSide
}

func (connection *pipeConnection) Write(payload []byte) (int, error) {
	if connection.writeStarted != nil {
		select {
		case connection.writeStarted <- struct{}{}:
		default:
		}
	}
	return connection.Conn.Write(payload)
}

func (connection *pipeConnection) CloseWrite() error { return nil }

func (connection *pipeConnection) SupportsUnreliable() bool { return connection.unreliable }

func (connection *pipeConnection) SendUnreliable(payload []byte) error {
	copied := make([]byte, len(payload))
	copy(copied, payload)
	select {
	case connection.outgoing <- copied:
	default:
		// A full queue drops the datagram, exactly as a congested path would.
	}
	return nil
}

func (connection *pipeConnection) ReceiveUnreliable(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case payload := <-connection.incoming:
		return payload, nil
	}
}
