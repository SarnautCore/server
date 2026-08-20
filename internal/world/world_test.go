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
