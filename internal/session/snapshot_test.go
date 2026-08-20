package session

import (
	"bytes"
	"context"
	"net"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
)

func TestSnapshotSenderFallsBackToReliableStream(t *testing.T) {
	connection := &stubConnection{}
	sender := newSnapshotSender(connection)
	want := &sarnautv1.SnapshotBatch{
		ServerTick: 7,
		Entities: []*sarnautv1.EntitySnapshot{{
			EntityId: 11,
			Kind:     sarnautv1.EntityKind_ENTITY_KIND_NPC,
		}},
	}
	if err := sender.send(want); err != nil {
		t.Fatalf("send() error = %v", err)
	}

	got := new(sarnautv1.SnapshotBatch)
	if err := transport.ReadMessage(&connection.Buffer, got); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if got.GetServerTick() != 7 || len(got.GetEntities()) != 1 || got.GetEntities()[0].GetEntityId() != 11 {
		t.Fatalf("fallback snapshot = %v, want tick 7 and entity 11", got)
	}
}

func TestSnapshotSenderSplitsDatagramsBelowPacketLimit(t *testing.T) {
	connection := &stubConnection{unreliable: true}
	sender := newSnapshotSender(connection)
	snapshot := &sarnautv1.SnapshotBatch{ServerTick: 9}
	for id := uint64(1); id <= 200; id++ {
		snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{
			EntityId: id,
			Kind:     sarnautv1.EntityKind_ENTITY_KIND_NPC,
			Position: &sarnautv1.Vec3{X: float32(id), Y: 2, Z: 3},
			Velocity: &sarnautv1.Vec3{},
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
		batch := new(sarnautv1.SnapshotBatch)
		if err := transport.UnmarshalUnreliable(payload, batch); err != nil {
			t.Fatalf("UnmarshalUnreliable() error = %v", err)
		}
		if batch.GetServerTick() != 9 {
			t.Errorf("server tick = %d, want 9", batch.GetServerTick())
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
