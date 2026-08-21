package combat

import (
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/world"
)

func TestGuardObserverHasOneFirstAttachLastDetachLifetime(t *testing.T) {
	t.Parallel()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "GuardLifecycle", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	mobID := zone.SpawnNPC(world.NPCSpec{ContentID: "mob.guard", MaxHealth: 10})
	module := &Module{zone: zone, mobs: map[uint64]*mobState{mobID: {}}}

	apply := func(update GuardUpdate) error {
		return zone.GameCommand(func(tick gametypes.Tick) error {
			return module.ApplyGuardUpdate(tick, mobID, update)
		})
	}
	if err := apply(GuardUpdate{
		Active: true, ObserverRadius: 15, RecheckEvery: 2 * time.Second, AggroMarkDelta: 1,
	}); err != nil {
		t.Fatalf("first Guard attach: %v", err)
	}
	state := module.mobs[mobID]
	if state.guardAggroMarks != 1 || state.guard == nil || state.guard.radius != 15 ||
		state.guard.recheckTicks != 60 {
		t.Fatalf("first Guard runtime state = %#v, observer = %#v", state, state.guard)
	}

	if err := apply(GuardUpdate{
		Active: true, ObserverRadius: 10, NoticeTarget: true,
		RecheckEvery: 2 * time.Second,
	}); err != nil {
		t.Fatalf("aggregate Guard update: %v", err)
	}
	if state.guardAggroMarks != 1 || state.guard == nil || state.guard.radius != 10 ||
		!state.guard.noticeTarget {
		t.Fatalf("aggregate Guard runtime state = %#v, observer = %#v", state, state.guard)
	}

	remove := GuardUpdate{AggroMarkDelta: -1, RemoveAggroState: true}
	if err := apply(remove); err != nil {
		t.Fatalf("last Guard detach: %v", err)
	}
	if state.guardAggroMarks != 0 || state.guard != nil {
		t.Fatalf("last detach retained Guard state = %#v", state)
	}
	if err := apply(GuardUpdate{RemoveAggroState: true}); err != nil {
		t.Fatalf("replayed last detach: %v", err)
	}
}

func TestGuardObserverRejectsAnUnbalancedAggroMark(t *testing.T) {
	t.Parallel()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "GuardMark", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	mobID := zone.SpawnNPC(world.NPCSpec{ContentID: "mob.guard", MaxHealth: 10})
	state := new(mobState)
	module := &Module{zone: zone, mobs: map[uint64]*mobState{mobID: state}}
	err = zone.GameCommand(func(tick gametypes.Tick) error {
		return module.ApplyGuardUpdate(tick, mobID, GuardUpdate{
			Active: true, ObserverRadius: 10, RecheckEvery: time.Second, AggroMarkDelta: -1,
		})
	})
	if err == nil {
		t.Fatal("unbalanced Guard removal succeeded")
	}
	if state.guardAggroMarks != 0 || state.guard != nil {
		t.Fatalf("rejected update mutated Guard state = %#v", state)
	}
}

func TestRespawnLeavesGuardLifetimeToItsAttachmentOwner(t *testing.T) {
	t.Parallel()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "GuardRespawn", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	mobID := zone.SpawnNPC(world.NPCSpec{ContentID: "mob.guard", MaxHealth: 10})
	observer := &guardObserver{radius: 15, recheckTicks: 60}
	state := &mobState{
		phase: phaseDespawned, guardAggroMarks: 1, guard: observer,
		levelMin: 1, levelMax: 1, hpMod: 1,
	}
	module := &Module{
		zone: zone, mobs: map[uint64]*mobState{mobID: state}, stream: newSpawnStream(1),
	}
	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		module.respawn(tick, mobID)
		return nil
	}); err != nil {
		t.Fatalf("respawn command: %v", err)
	}
	if state.guardAggroMarks != 1 || state.guard != observer {
		t.Fatalf("respawn stole Guard lifecycle: state = %#v", state)
	}
}
