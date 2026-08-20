package session

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func TestSnapshotSenderFallsBackToReliableStream(t *testing.T) {
	connection := &stubConnection{}
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	want := world.Snapshot{
		ServerTick: 7,
		Entities: []world.EntitySnapshot{{
			EntityID: 11,
			Kind:     world.EntityKindNPC,
			Alive:    true,
		}},
	}
	if err := sender.send(want); err != nil {
		t.Fatalf("send() error = %v", err)
	}

	got := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(&connection.Buffer, got); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if got.GetServerTick() != 7 {
		t.Errorf("envelope server_tick = %d, want 7", got.GetServerTick())
	}
	batch := got.GetSnapshotBatch()
	if batch == nil {
		t.Fatalf("server message = %v, want a snapshot_batch case", got)
	}
	if batch.GetServerTick() != 7 || len(batch.GetEntities()) != 1 || batch.GetEntities()[0].GetEntityId() != 11 {
		t.Fatalf("fallback snapshot = %v, want tick 7 and entity 11", batch)
	}
}

func TestSnapshotSenderSplitsDatagramsBelowPacketLimit(t *testing.T) {
	connection := &stubConnection{unreliable: true}
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	snapshot := world.Snapshot{ServerTick: 9}
	for id := uint64(1); id <= 200; id++ {
		snapshot.Entities = append(snapshot.Entities, world.EntitySnapshot{
			EntityID:  id,
			Kind:      world.EntityKindNPC,
			Position:  world.Vec3{X: float32(id), Y: 2, Z: 3},
			ContentID: "mob.fixture.critter",
			Level:     2,
			Health:    100,
			MaxHealth: 100,
			Alive:     true,
		})
	}
	if err := sender.send(snapshot); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	if len(connection.datagrams) < 2 {
		t.Fatalf("datagram count = %d, want a split snapshot", len(connection.datagrams))
	}
	entityCount := 0
	for _, payload := range connection.datagrams {
		if len(payload) > transport.MaxUnreliableMessageSize {
			t.Errorf("datagram size = %d, want <= %d", len(payload), transport.MaxUnreliableMessageSize)
		}
		message := new(sarnautv1.ServerMessage)
		if err := transport.UnmarshalUnreliable(payload, message); err != nil {
			t.Fatalf("UnmarshalUnreliable() error = %v", err)
		}
		batch := message.GetSnapshotBatch()
		if batch == nil {
			t.Fatalf("datagram = %v, want a snapshot_batch case", message)
		}
		if batch.GetServerTick() != 9 || message.GetServerTick() != 9 {
			t.Errorf("server tick = %d/%d, want 9", message.GetServerTick(), batch.GetServerTick())
		}
		entityCount += len(batch.GetEntities())
	}
	if entityCount != 200 {
		t.Errorf("replicated entities = %d, want 200", entityCount)
	}
}

func TestSnapshotSenderWritesInterestTransitionsReliably(t *testing.T) {
	connection := &stubConnection{unreliable: true}
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	spawn := world.EntitySnapshot{
		EntityID:  41,
		Kind:      world.EntityKindNPC,
		ContentID: "mob.fixture.entering",
		Alive:     true,
	}
	if err := sender.send(world.Snapshot{
		ServerTick: 12,
		Entities:   []world.EntitySnapshot{spawn},
		Spawns:     []world.EntitySnapshot{spawn},
		Despawns:   []uint64{39},
	}); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	if err := sender.send(world.Snapshot{ServerTick: 13, Entities: []world.EntitySnapshot{spawn}}); err != nil {
		t.Fatalf("send(next unchanged snapshot) error = %v", err)
	}

	spawnMessage := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(&connection.Buffer, spawnMessage); err != nil {
		t.Fatalf("read spawn event: %v", err)
	}
	if got := spawnMessage.GetSpawnEvent().GetEntity().GetEntityId(); got != 41 {
		t.Fatalf("spawn entity id = %d, want 41", got)
	}
	if spawnMessage.GetServerTick() != 12 {
		t.Errorf("spawn server tick = %d, want 12", spawnMessage.GetServerTick())
	}

	despawnMessage := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(&connection.Buffer, despawnMessage); err != nil {
		t.Fatalf("read despawn event: %v", err)
	}
	if got := despawnMessage.GetDespawnEvent().GetEntityId(); got != 39 {
		t.Fatalf("despawn entity id = %d, want 39", got)
	}
	if despawnMessage.GetServerTick() != 12 {
		t.Errorf("despawn server tick = %d, want 12", despawnMessage.GetServerTick())
	}

	unexpected := new(sarnautv1.ServerMessage)
	if err := transport.ReadMessage(&connection.Buffer, unexpected); err == nil {
		t.Fatalf("unexpected extra reliable interest event: %v", unexpected)
	}
	if len(connection.datagrams) != 2 {
		t.Fatalf("snapshot datagrams = %d, want one for each tick", len(connection.datagrams))
	}
}

func TestSnapshotSenderPreservesTransitionsWhenNewestSnapshotWins(t *testing.T) {
	connection := newBlockingSnapshotConnection()
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)

	sender.OfferSnapshot(world.Snapshot{ServerTick: 1})
	go func() { done <- sender.run(ctx) }()
	<-connection.firstSendStarted

	entity := world.EntitySnapshot{EntityID: 41, Kind: world.EntityKindNPC, Alive: true}
	sender.OfferSnapshot(world.Snapshot{
		ServerTick: 2,
		Entities:   []world.EntitySnapshot{entity},
		Spawns:     []world.EntitySnapshot{entity},
	})
	sender.OfferSnapshot(world.Snapshot{
		ServerTick: 3,
		Despawns:   []uint64{41},
	})
	go func() { done <- sender.runTransitions(ctx) }()
	close(connection.releaseFirstSend)
	<-connection.sent
	<-connection.sent
	for write := 0; write < 4; write++ {
		select {
		case <-connection.reliableWrites:
		case <-time.After(2 * time.Second):
			t.Fatal("interest sender did not finish two reliable event frames")
		}
	}
	cancel()
	for index := 0; index < 2; index++ {
		if err := <-done; err != nil {
			t.Fatalf("sender loop error = %v", err)
		}
	}

	var spawns, despawns int
	for connection.Len() > 0 {
		message := new(sarnautv1.ServerMessage)
		if err := transport.ReadMessage(&connection.Buffer, message); err != nil {
			t.Fatalf("read reliable interest event: %v", err)
		}
		if message.GetSpawnEvent() != nil {
			spawns++
		}
		if message.GetDespawnEvent() != nil {
			despawns++
		}
	}
	if spawns != 1 || despawns != 1 {
		t.Fatalf("interest events after snapshot coalescing = %d spawn, %d despawn; want 1 each", spawns, despawns)
	}
}

// A split tick is only useful if the receiver can put it back together. Before
// chunk_index/chunk_count existed there was no field to do it on: every chunk
// carried the same server_tick and nothing else, so a receiver had no way to
// tell a partial view from a complete one, and both receivers took the first or
// last chunk as the whole world.
func TestReadSnapshotReassemblesEveryChunkOfASplitTick(t *testing.T) {
	connection := &stubConnection{unreliable: true}
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	snapshot := world.Snapshot{ServerTick: 9}
	for id := uint64(1); id <= 200; id++ {
		snapshot.Entities = append(snapshot.Entities, world.EntitySnapshot{
			EntityID:  id,
			Kind:      world.EntityKindNPC,
			Position:  world.Vec3{X: float32(id), Y: 2, Z: 3},
			ContentID: "mob.fixture.critter",
			NameKey:   "Mob.Fixture.Critter.Name.txt",
			Level:     2,
			Health:    100,
			MaxHealth: 100,
			Alive:     true,
		})
	}
	if err := sender.send(snapshot); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	if len(connection.datagrams) < 2 {
		t.Fatalf("datagram count = %d, want a split snapshot", len(connection.datagrams))
	}

	client := Client{}
	replay := &replayConnection{datagrams: connection.datagrams}
	whole, err := client.ReadSnapshot(t.Context(), replay)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}
	if whole.GetServerTick() != 9 {
		t.Errorf("server tick = %d, want 9", whole.GetServerTick())
	}
	if len(whole.GetEntities()) != 200 {
		t.Fatalf("reassembled entities = %d, want all 200", len(whole.GetEntities()))
	}
	for index, entity := range whole.GetEntities() {
		if entity.GetEntityId() != uint64(index+1) {
			t.Fatalf("entity %d = %d, want the chunks merged in order", index, entity.GetEntityId())
		}
	}
	if replay.read != len(connection.datagrams) {
		t.Errorf("datagrams consumed = %d, want all %d", replay.read, len(connection.datagrams))
	}
}

// Every chunk must still fit a datagram *after* the chunk fields are stamped on
// it, which is what the reserved header space is for.
func TestSplitSnapshotStampsChunkFieldsWithoutBustingTheCap(t *testing.T) {
	snapshot := &sarnautv1.SnapshotBatch{ServerTick: 4, ChunkCount: 1}
	for id := uint64(1); id <= 200; id++ {
		snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{
			EntityId:  id,
			ContentId: "mob.fixture.critter",
			NameKey:   "Mob.Fixture.Critter.Name.txt",
			Faction:   "faction.fixture.hostile",
		})
	}
	chunks, dropped := splitSnapshot(snapshot)
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want none", dropped)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want a split", len(chunks))
	}
	for index, chunk := range chunks {
		payload, err := transport.MarshalUnreliable(chunk)
		if err != nil {
			t.Fatalf("MarshalUnreliable(chunk %d) error = %v", index, err)
		}
		if len(payload) > transport.MaxUnreliableMessageSize {
			t.Errorf("chunk %d is %d bytes, want <= %d",
				index, len(payload), transport.MaxUnreliableMessageSize)
		}
		batch := chunk.GetSnapshotBatch()
		if batch.GetChunkIndex() != uint32(index) {
			t.Errorf("chunk %d carries chunk_index %d", index, batch.GetChunkIndex())
		}
		if batch.GetChunkCount() != uint32(len(chunks)) {
			t.Errorf("chunk %d carries chunk_count %d, want %d",
				index, batch.GetChunkCount(), len(chunks))
		}
	}
}

// content_id, name_key and faction are unbounded pack strings, so one authored
// row can produce an entity no datagram can carry. Emitting it anyway fails
// MarshalUnreliable, which ends the session — for every player in the zone, over
// one entity none of them could have seen.
func TestSplitSnapshotDropsAnEntityNoDatagramCanCarry(t *testing.T) {
	snapshot := &sarnautv1.SnapshotBatch{ServerTick: 3, ChunkCount: 1}
	snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{EntityId: 1})
	snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{
		EntityId: 2,
		NameKey:  strings.Repeat("N", 2*transport.MaxUnreliableMessageSize),
	})
	snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{EntityId: 3})

	chunks, dropped := splitSnapshot(snapshot)
	if len(dropped) != 1 || dropped[0] != 2 {
		t.Fatalf("dropped = %v, want exactly entity 2", dropped)
	}
	var carried []uint64
	for index, chunk := range chunks {
		payload, err := transport.MarshalUnreliable(chunk)
		if err != nil {
			t.Fatalf("MarshalUnreliable(chunk %d) error = %v", index, err)
		}
		if len(payload) > transport.MaxUnreliableMessageSize {
			t.Errorf("chunk %d is %d bytes, want <= %d",
				index, len(payload), transport.MaxUnreliableMessageSize)
		}
		for _, entity := range chunk.GetSnapshotBatch().GetEntities() {
			carried = append(carried, entity.GetEntityId())
		}
	}
	if len(carried) != 2 || carried[0] != 1 || carried[1] != 3 {
		t.Errorf("carried entities = %v, want the two that fit", carried)
	}
}

func TestSnapshotSenderCountsOversizedEntitiesInsteadOfFailing(t *testing.T) {
	connection := &stubConnection{unreliable: true}
	sender := newSnapshotSender(connection, newReliableWriter(connection), nil)
	if err := sender.send(world.Snapshot{
		ServerTick: 1,
		Entities: []world.EntitySnapshot{
			{EntityID: 1, Kind: world.EntityKindNPC, Alive: true},
			{
				EntityID: 2,
				Kind:     world.EntityKindNPC,
				NameKey:  strings.Repeat("N", 2*transport.MaxUnreliableMessageSize),
				Alive:    true,
			},
		},
	}); err != nil {
		t.Fatalf("send() error = %v, want the oversized entity dropped rather than fatal", err)
	}
	if got := sender.oversized.Load(); got != 1 {
		t.Errorf("oversized counter = %d, want 1", got)
	}
	if len(connection.datagrams) != 1 {
		t.Fatalf("datagram count = %d, want one", len(connection.datagrams))
	}
}

// A tick that never completes must not stall the next one, and it must not grow
// a buffer either: snapshot delivery is lossy by design.
func TestSnapshotAssemblyAbandonsAnIncompleteTick(t *testing.T) {
	assembly := new(snapshotAssembly)
	if got := assembly.add(&sarnautv1.SnapshotBatch{
		ServerTick: 7, ChunkIndex: 0, ChunkCount: 3,
		Entities: []*sarnautv1.EntitySnapshot{{EntityId: 1}},
	}); got != nil {
		t.Fatalf("add(chunk 0 of 3) = %v, want nil while incomplete", got)
	}
	whole := assembly.add(&sarnautv1.SnapshotBatch{
		ServerTick: 8, ChunkCount: 1,
		Entities: []*sarnautv1.EntitySnapshot{{EntityId: 2}},
	})
	if whole == nil || whole.GetServerTick() != 8 || len(whole.GetEntities()) != 1 {
		t.Fatalf("add(whole tick 8) = %v, want tick 8 published immediately", whole)
	}
	if len(assembly.chunks) != 0 {
		t.Errorf("held chunks = %d, want the abandoned tick released", len(assembly.chunks))
	}
}

// replayConnection hands back a recorded datagram stream, so a receiver test
// sees exactly the bytes a sender produced.
type replayConnection struct {
	stubConnection
	datagrams [][]byte
	read      int
}

func (connection *replayConnection) SupportsUnreliable() bool { return true }

func (connection *replayConnection) ReceiveUnreliable(ctx context.Context) ([]byte, error) {
	if connection.read >= len(connection.datagrams) {
		return nil, errors.New("replay exhausted")
	}
	payload := connection.datagrams[connection.read]
	connection.read++
	return payload, nil
}

type stubConnection struct {
	bytes.Buffer
	unreliable bool
	datagrams  [][]byte
}

type blockingSnapshotConnection struct {
	stubConnection
	mu               sync.Mutex
	firstSendStarted chan struct{}
	releaseFirstSend chan struct{}
	sent             chan struct{}
	reliableWrites   chan struct{}
	sendCount        int
}

func newBlockingSnapshotConnection() *blockingSnapshotConnection {
	connection := &blockingSnapshotConnection{
		firstSendStarted: make(chan struct{}),
		releaseFirstSend: make(chan struct{}),
		sent:             make(chan struct{}, 4),
		reliableWrites:   make(chan struct{}, 4),
	}
	connection.unreliable = true
	return connection
}

func (connection *blockingSnapshotConnection) Write(payload []byte) (int, error) {
	connection.mu.Lock()
	written, err := connection.Buffer.Write(payload)
	connection.mu.Unlock()
	connection.reliableWrites <- struct{}{}
	return written, err
}

func (connection *blockingSnapshotConnection) SendUnreliable(payload []byte) error {
	connection.mu.Lock()
	connection.sendCount++
	count := connection.sendCount
	connection.mu.Unlock()
	if count == 1 {
		close(connection.firstSendStarted)
		<-connection.releaseFirstSend
	}
	connection.mu.Lock()
	connection.datagrams = append(connection.datagrams, bytes.Clone(payload))
	connection.mu.Unlock()
	connection.sent <- struct{}{}
	return nil
}

func (connection *stubConnection) Close() error      { return nil }
func (connection *stubConnection) CloseWrite() error { return nil }
func (connection *stubConnection) SupportsUnreliable() bool {
	return connection.unreliable
}
func (connection *stubConnection) SendUnreliable(payload []byte) error {
	connection.datagrams = append(connection.datagrams, bytes.Clone(payload))
	return nil
}
func (connection *stubConnection) ReceiveUnreliable(context.Context) ([]byte, error) {
	panic("ReceiveUnreliable called unexpectedly")
}
func (connection *stubConnection) LocalAddr() net.Addr  { return stubAddress("local") }
func (connection *stubConnection) RemoteAddr() net.Addr { return stubAddress("remote") }

type stubAddress string

func (address stubAddress) Network() string { return "stub" }
func (address stubAddress) String() string  { return string(address) }
