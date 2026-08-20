package session

import (
	"bytes"
	"context"
	"net"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func TestSnapshotSenderFallsBackToReliableStream(t *testing.T) {
	connection := &stubConnection{}
	sender := newSnapshotSender(connection, newReliableWriter(connection))
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
	sender := newSnapshotSender(connection, newReliableWriter(connection))
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

type stubConnection struct {
	bytes.Buffer
	unreliable bool
	datagrams  [][]byte
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
