package world_test

import (
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/world"
)

func newTestZone(t *testing.T, spawn world.Vec3) *world.Zone {
	t.Helper()
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "fixture",
		TickInterval:     5 * time.Millisecond,
		SnapshotInterval: 10 * time.Millisecond,
		MaxMoveSpeed:     4,
		PlayerSpawn:      spawn,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	return zone
}

func TestZoneClampsMovementAndKeepsFlatGround(t *testing.T) {
	t.Parallel()

	zone := newTestZone(t, world.Vec3{Z: 9})
	entityID, _ := zone.Join()
	sink := new(captureSink)
	if err := zone.Subscribe(entityID, sink); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	// The input is over unit length on the ground plane and has a wild Z. The
	// first is clamped to the configured speed; the second is ignored, because
	// nothing resolves terrain height yet.
	if err := zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq:      1,
		Input:    world.Vec3{X: 3, Y: 4, Z: 100},
		Duration: time.Second,
	}); err != nil {
		t.Fatalf("ApplyMoveIntent() error = %v", err)
	}

	for step := 0; step < 10; step++ {
		zone.Step()
	}
	zone.PublishSnapshot()

	view, ok := sink.find(entityID)
	if !ok {
		t.Fatal("the moving player is not in the snapshot")
	}
	if view.Position.X <= 0 {
		t.Errorf("position.x = %v, want the player to have moved", view.Position.X)
	}
	if view.Position.Z != 9 {
		t.Errorf("position.z = %v, want the spawn height 9", view.Position.Z)
	}
	speed := view.Velocity.X*view.Velocity.X + view.Velocity.Y*view.Velocity.Y
	if speed > 16.001 {
		t.Errorf("velocity squared = %v, want <= 16", speed)
	}
}

func TestZoneRejectsNonFiniteAndStaleIntents(t *testing.T) {
	t.Parallel()

	zone := newTestZone(t, world.Vec3{})
	entityID, _ := zone.Join()

	infinity := float32(1)
	for i := 0; i < 40; i++ {
		infinity *= 1e10
	}
	if err := zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq:   1,
		Input: world.Vec3{X: infinity},
	}); err == nil {
		t.Error("ApplyMoveIntent() accepted a non-finite input")
	}
	if err := zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq:      5,
		Input:    world.Vec3{X: 1},
		Duration: 100 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ApplyMoveIntent() error = %v", err)
	}
	// Older sequence numbers are discarded rather than rewinding the player.
	if err := zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq:      4,
		Input:    world.Vec3{X: -1},
		Duration: 100 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ApplyMoveIntent() error = %v", err)
	}

	zone.Step()
	sink := new(captureSink)
	if err := zone.Subscribe(entityID, sink); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	zone.PublishSnapshot()
	view, ok := sink.find(entityID)
	if !ok {
		t.Fatal("the player is not in the snapshot")
	}
	if view.Position.X <= 0 {
		t.Errorf("position.x = %v, want the newer intent to have won", view.Position.X)
	}

	if err := zone.ApplyMoveIntent(9999, world.MoveIntent{Seq: 1}); err == nil {
		t.Error("ApplyMoveIntent() accepted an unknown entity")
	}
}

func TestJoinedPlayerIsNotReplicatedUntilSubscribe(t *testing.T) {
	t.Parallel()

	zone := newTestZone(t, world.Vec3{})
	watcherID, _ := zone.Join()
	watcher := new(captureSink)
	if err := zone.Subscribe(watcherID, watcher); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// A second player joins but never subscribes. Until it does, it has no
	// combat identity, and publishing it would be a lie the client has to
	// correct one snapshot later.
	pendingID, _ := zone.Join()
	zone.PublishSnapshot()
	if _, ok := watcher.find(pendingID); ok {
		t.Error("an unsubscribed player is being replicated")
	}

	if err := zone.Subscribe(pendingID, new(captureSink)); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	zone.PublishSnapshot()
	if _, ok := watcher.find(pendingID); !ok {
		t.Error("a subscribed player is not being replicated")
	}
}

func TestPublishSnapshotGivesEverySinkTheSameValue(t *testing.T) {
	t.Parallel()

	zone := newTestZone(t, world.Vec3{})
	zone.SpawnNPC(world.NPCSpec{ContentID: "mob.fixture.one", Position: world.Vec3{X: 1, Y: 2}, Heading: 0.5})
	zone.SpawnNPC(world.NPCSpec{ContentID: "mob.fixture.two", Position: world.Vec3{X: 3, Y: 4}, Heading: 1.5})

	// ADR 0026's shared-batch hazard is retired by construction here: the
	// boundary type is an immutable value, so a sink that wanted to rewrite
	// what it was handed has nothing to rewrite. The protobuf tree, which is
	// mutable, is built per session in `internal/session`.
	first, second := new(captureSink), new(captureSink)
	for _, sink := range []world.SnapshotSink{first, second} {
		entityID, _ := zone.Join()
		if err := zone.Subscribe(entityID, sink); err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
	}
	zone.PublishSnapshot()

	if first.snapshot.ServerTick != second.snapshot.ServerTick {
		t.Fatalf("server ticks = %d and %d, want one publish to be one tick",
			first.snapshot.ServerTick, second.snapshot.ServerTick)
	}
	if len(first.snapshot.Entities) != len(second.snapshot.Entities) {
		t.Fatalf("entity counts = %d and %d", len(first.snapshot.Entities), len(second.snapshot.Entities))
	}
	for index := range first.snapshot.Entities {
		if first.snapshot.Entities[index] != second.snapshot.Entities[index] {
			t.Fatalf("entity %d differs between sinks", index)
		}
	}
}

func TestSnapshotEntitiesAreInAscendingIDOrder(t *testing.T) {
	t.Parallel()

	zone := newTestZone(t, world.Vec3{})
	for index := 0; index < 40; index++ {
		zone.SpawnNPC(world.NPCSpec{Position: world.Vec3{X: float32(40 - index)}})
	}
	entityID, _ := zone.Join()
	sink := new(captureSink)
	if err := zone.Subscribe(entityID, sink); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	zone.PublishSnapshot()

	var previous uint64
	for _, view := range sink.snapshot.Entities {
		if view.EntityID <= previous {
			t.Fatalf("entity id %d follows %d; ascending order is what rule 5.7.3 tie-breaks on",
				view.EntityID, previous)
		}
		previous = view.EntityID
	}
}

type captureSink struct {
	snapshot world.Snapshot
}

func (sink *captureSink) OfferSnapshot(snapshot world.Snapshot) { sink.snapshot = snapshot }

func (sink *captureSink) find(entityID uint64) (world.EntitySnapshot, bool) {
	for _, view := range sink.snapshot.Entities {
		if view.EntityID == entityID {
			return view, true
		}
	}
	return world.EntitySnapshot{}, false
}
