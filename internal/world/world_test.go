package world_test

import (
	"context"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/world"
)

func TestZoneClampsMovementAndKeepsFlatGround(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "fixture",
		TickInterval:     5 * time.Millisecond,
		SnapshotInterval: 10 * time.Millisecond,
		MaxMoveSpeed:     4,
		PlayerSpawn:      world.Vec3{Z: 9},
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	entityID, _ := zone.Join()
	sink := &captureSink{snapshots: make(chan *sarnautv1.SnapshotBatch, 4)}
	if err := zone.Subscribe(entityID, sink); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := zone.ApplyMoveIntent(entityID, &sarnautv1.ClientMoveIntent{
		Seq:       1,
		Input:     &sarnautv1.Vec3{X: 3, Y: 4, Z: 100},
		DtSeconds: 1,
	}); err != nil {
		t.Fatalf("ApplyMoveIntent() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go zone.Run(ctx)

	deadline := time.After(time.Second)
	for {
		select {
		case snapshot := <-sink.snapshots:
			for _, entity := range snapshot.GetEntities() {
				if entity.GetEntityId() != entityID || entity.GetPosition().GetX() == 0 {
					continue
				}
				if entity.GetPosition().GetZ() != 9 {
					t.Errorf("position.z = %v, want 9", entity.GetPosition().GetZ())
				}
				if speed := entity.GetVelocity().GetX()*entity.GetVelocity().GetX() + entity.GetVelocity().GetY()*entity.GetVelocity().GetY(); speed > 16.001 {
					t.Errorf("velocity squared = %v, want <= 16", speed)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for movement snapshot")
		}
	}
}

type captureSink struct {
	snapshots chan *sarnautv1.SnapshotBatch
}

func (sink *captureSink) OfferSnapshot(snapshot *sarnautv1.SnapshotBatch) {
	select {
	case sink.snapshots <- snapshot:
	default:
	}
}

func TestPublishSnapshotGivesEverySinkItsOwnBatch(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "fixture",
		TickInterval:     2 * time.Millisecond,
		SnapshotInterval: 4 * time.Millisecond,
		MaxMoveSpeed:     4,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	zone.SpawnNPC(world.Vec3{X: 1, Y: 2}, 0.5)
	zone.SpawnNPC(world.Vec3{X: 3, Y: 4}, 1.5)

	// A sink that rewrites what it was handed. Sharing one batch across sinks
	// is race-free only while nobody does this, which is a property of today's
	// callers rather than of the interface (ADR 0026).
	rewriting := &mutatingSink{sentinel: 1234, received: make(chan *sarnautv1.SnapshotBatch, 8)}
	observing := &recordingSink{received: make(chan *sarnautv1.SnapshotBatch, 8)}
	for _, sink := range []world.SnapshotSink{rewriting, observing} {
		entityID, _ := zone.Join()
		if err := zone.Subscribe(entityID, sink); err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go zone.Run(ctx)

	deadline := time.After(3 * time.Second)
	byTick := make(map[uint64]*sarnautv1.SnapshotBatch)
	for {
		var mine, theirs *sarnautv1.SnapshotBatch
		select {
		case batch := <-rewriting.received:
			byTick[batch.GetServerTick()] = batch
			continue
		case batch := <-observing.received:
			mine = byTick[batch.GetServerTick()]
			theirs = batch
			if mine == nil {
				continue
			}
		case <-deadline:
			t.Fatal("timed out waiting for both sinks to observe the same tick")
		}

		if mine == theirs {
			t.Fatal("both sinks received the same *SnapshotBatch pointer")
		}
		// The rewriting sink appended one entity to its own batch. The other
		// sink's batch is unchanged, which is the whole point.
		if len(mine.GetEntities()) != len(theirs.GetEntities())+1 {
			t.Fatalf("entity counts = %d and %d, want the rewritten batch to hold exactly one more",
				len(mine.GetEntities()), len(theirs.GetEntities()))
		}
		for index := range theirs.GetEntities() {
			if mine.GetEntities()[index] == theirs.GetEntities()[index] {
				t.Fatalf("entity %d is the same *EntitySnapshot pointer in both batches", index)
			}
			if got := theirs.GetEntities()[index].GetHealth(); got == rewriting.sentinel {
				t.Fatalf("entity %d observed the other sink's rewrite", index)
			}
		}
		return
	}
}

type recordingSink struct {
	received chan *sarnautv1.SnapshotBatch
}

func (sink *recordingSink) OfferSnapshot(snapshot *sarnautv1.SnapshotBatch) {
	select {
	case sink.received <- snapshot:
	default:
	}
}

type mutatingSink struct {
	sentinel int32
	received chan *sarnautv1.SnapshotBatch
}

func (sink *mutatingSink) OfferSnapshot(snapshot *sarnautv1.SnapshotBatch) {
	for _, entity := range snapshot.GetEntities() {
		entity.Health = sink.sentinel
	}
	snapshot.Entities = append(snapshot.Entities, &sarnautv1.EntitySnapshot{EntityId: 999})
	select {
	case sink.received <- snapshot:
	default:
	}
}
