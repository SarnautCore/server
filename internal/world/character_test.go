package world_test

import (
	"math"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/world"
)

// configuredSpawn is what the zone below is built with, so a test can tell "the
// caller's position" from "the zone's fallback" without reading zone state.
var configuredSpawn = world.Vec3{X: 12, Y: 4.5}

func newCharacterZone(t *testing.T) *world.Zone {
	t.Helper()
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "SnapshotZone",
		TickInterval:     10 * time.Millisecond,
		SnapshotInterval: 20 * time.Millisecond,
		MaxMoveSpeed:     6,
		PlayerSpawn:      configuredSpawn,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	return zone
}

// JoinAt is what puts a returning character back where it logged out. Join
// keeps the configured spawn, which is right for debug tools and for entities
// no character owns.
func TestJoinAtPlacesAPlayerAtALoadedPosition(t *testing.T) {
	t.Parallel()

	zone := newCharacterZone(t)
	loaded := world.Vec3{X: 51.9, Y: 4.5, Z: 1.5}

	entityID, spawn := zone.JoinAt(loaded, 1.25)
	if spawn != loaded {
		t.Errorf("JoinAt() spawn = %+v, want %+v", spawn, loaded)
	}
	view, ok := zone.SnapshotCharacter(entityID)
	if !ok {
		t.Fatal("SnapshotCharacter() reported no entity for a player that just joined")
	}
	if view.Position != loaded || view.Heading != 1.25 {
		t.Errorf("entity = %+v, want the loaded position and heading", view)
	}
	// Level and health are deliberately absent here: they are combat's to
	// assign, and the entity is not replicated until `combat.Module.Admit` has
	// filled them in. What JoinAt itself promises is an identified, live entity
	// at the loaded position.
	if view.EntityID != entityID || !view.Alive {
		t.Errorf("entity = %+v, want a live, identified character", view)
	}

	if _, configured := zone.Join(); configured != configuredSpawn {
		t.Errorf("Join() spawn = %+v, want the configured %+v", configured, configuredSpawn)
	}
}

// A stored position that is not a number would put an entity nowhere the
// simulation can reason about.
func TestJoinAtFallsBackOnANonFinitePosition(t *testing.T) {
	t.Parallel()

	zone := newCharacterZone(t)
	if _, spawn := zone.JoinAt(world.Vec3{X: float32(math.Inf(1))}, 0); spawn != configuredSpawn {
		t.Errorf("JoinAt(infinite) spawn = %+v, want the configured spawn", spawn)
	}
	if _, spawn := zone.JoinAt(world.Vec3{Y: float32(math.NaN())}, 0); spawn != configuredSpawn {
		t.Errorf("JoinAt(NaN) spawn = %+v, want the configured spawn", spawn)
	}
	if _, spawn := zone.JoinAt(world.Vec3{X: 1}, float32(math.NaN())); spawn != configuredSpawn {
		t.Errorf("JoinAt(NaN heading) spawn = %+v, want the configured spawn", spawn)
	}
}

// Leave stays a pure in-memory eviction: no I/O, no error return, no
// persistence hook, ever (ADR 0031 §8). What a caller can observe is that the
// snapshot is gone afterwards — which is exactly why the save has to be taken
// before it runs.
func TestSnapshotCharacterReportsNothingAfterLeave(t *testing.T) {
	t.Parallel()

	zone := newCharacterZone(t)
	entityID, _ := zone.JoinAt(world.Vec3{X: 3}, 0)
	if _, ok := zone.SnapshotCharacter(entityID); !ok {
		t.Fatal("SnapshotCharacter() found nothing before Leave")
	}
	zone.Leave(entityID)
	if _, ok := zone.SnapshotCharacter(entityID); ok {
		t.Error("SnapshotCharacter() still reports an entity after Leave")
	}
	if count := zone.EntityCount(); count != 0 {
		t.Errorf("EntityCount() = %d after Leave, want 0", count)
	}

	// An NPC is not a character, so it is never snapshotted or saved as one.
	npcID := zone.SpawnNPC(world.NPCSpec{Position: world.Vec3{X: 1}})
	if _, ok := zone.SnapshotCharacter(npcID); ok {
		t.Error("SnapshotCharacter() returned an NPC")
	}
	if count := zone.EntityCount(); count != 1 {
		t.Errorf("EntityCount() = %d, want the one NPC", count)
	}
}
