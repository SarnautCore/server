package world

import (
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
)

func TestCommandRunsPostCommitAfterUnlockAndBeforeReturn(t *testing.T) {
	t.Parallel()
	zone, err := NewZone(ZoneConfig{
		ID: "post-command", TickInterval: time.Second,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	err = zone.GameCommand(func(tick gametypes.Tick) error {
		order = append(order, "locked")
		tick.(gametypes.PostCommitTick).AfterUnlock(func() {
			order = append(order, "post")
			if err := zone.GameCommand(func(gametypes.Tick) error {
				order = append(order, "reentered")
				return nil
			}); err != nil {
				t.Errorf("reentrant GameCommand() error = %v", err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(order); got != 3 || order[0] != "locked" || order[1] != "post" || order[2] != "reentered" {
		t.Fatalf("callback order = %v, want locked, post, reentered", order)
	}
}

type postCommitSystem struct {
	zone *Zone
	done bool
}

func (system *postCommitSystem) Step(tick gametypes.Tick) {
	if system.done {
		return
	}
	tick.(gametypes.PostCommitTick).AfterUnlock(func() {
		if err := system.zone.GameCommand(func(gametypes.Tick) error {
			system.done = true
			return nil
		}); err != nil {
			panic(err)
		}
	})
}

func TestStepRunsPostCommitOutsideSimulationLock(t *testing.T) {
	t.Parallel()
	zone, err := NewZone(ZoneConfig{
		ID: "post-step", TickInterval: time.Second,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	system := &postCommitSystem{zone: zone}
	zone.GameAddSystem(system)
	zone.Step()
	if !system.done {
		t.Fatal("post-commit callback did not finish before Step returned")
	}
}

func TestPanickingCommandStillReleasesZoneLock(t *testing.T) {
	t.Parallel()
	zone, err := NewZone(ZoneConfig{
		ID: "panic-command", TickInterval: time.Second,
		SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = zone.GameCommand(func(gametypes.Tick) error { panic("test panic") })
	}()
	done := make(chan struct{})
	go func() {
		_ = zone.GameCommand(func(gametypes.Tick) error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("zone lock remained held after command panic")
	}
}
