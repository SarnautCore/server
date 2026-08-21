package interest_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/interest"
)

func newManager(t *testing.T) *interest.Manager {
	t.Helper()
	manager, err := interest.New(interest.ReplicationRadiusMetres)
	if err != nil {
		t.Fatalf("interest.New() error = %v", err)
	}
	return manager
}

func TestObserveScopesAndOrdersOneObserversView(t *testing.T) {
	t.Parallel()
	manager := newManager(t)
	manager.Upsert(interest.Entity{ID: 4, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 10}})
	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindPlayer, Position: gametypes.Vec3{X: 48}})
	manager.Upsert(interest.Entity{ID: 3, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 48, Z: 1}})
	manager.Upsert(interest.Entity{ID: 1, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 1_000}})

	got := manager.Observe(99, gametypes.Vec3{}, nil)
	want := []uint64{2, 4}
	if !reflect.DeepEqual(got.Current, want) {
		t.Fatalf("current = %v, want %v", got.Current, want)
	}
	if !reflect.DeepEqual(got.Entered, want) || len(got.Left) != 0 {
		t.Fatalf("first delta = %+v, want both current ids entered", got)
	}
}

func TestObserveReportsCrossingsExactlyOnce(t *testing.T) {
	t.Parallel()
	manager := newManager(t)
	manager.Upsert(interest.Entity{ID: 1, Kind: gametypes.EntityKindPlayer})
	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 1_000}})
	manager.Observe(1, gametypes.Vec3{}, nil)

	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 47}})
	entered := manager.Observe(1, gametypes.Vec3{}, nil)
	steady := manager.Observe(1, gametypes.Vec3{}, nil)
	if !reflect.DeepEqual(entered.Entered, []uint64{2}) || len(steady.Entered) != 0 {
		t.Fatalf("entry deltas = %+v then %+v", entered, steady)
	}

	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindNPC, Position: gametypes.Vec3{X: 49}})
	left := manager.Observe(1, gametypes.Vec3{}, nil)
	steady = manager.Observe(1, gametypes.Vec3{}, nil)
	if !reflect.DeepEqual(left.Left, []uint64{2}) || len(steady.Left) != 0 {
		t.Fatalf("exit deltas = %+v then %+v", left, steady)
	}
}

func TestObserveTracksObserversIndependentlyAndHonoursVisibility(t *testing.T) {
	t.Parallel()
	manager := newManager(t)
	manager.Upsert(interest.Entity{ID: 1, Kind: gametypes.EntityKindPlayer, Position: gametypes.Vec3{X: -40}})
	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindPlayer, Position: gametypes.Vec3{X: 40}})
	manager.Upsert(interest.Entity{ID: 3, Kind: gametypes.EntityKindNPC})

	hidden := map[uint64]bool{3: true}
	visible := func(id uint64) bool { return !hidden[id] }
	left := manager.Observe(1, gametypes.Vec3{X: -40}, visible)
	right := manager.Observe(2, gametypes.Vec3{X: 40}, visible)
	if !reflect.DeepEqual(left.Current, []uint64{1}) || !reflect.DeepEqual(right.Current, []uint64{2}) {
		t.Fatalf("observer views = %v and %v", left.Current, right.Current)
	}

	hidden[3] = false
	if got := manager.Observe(1, gametypes.Vec3{X: -40}, visible).Entered; !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("newly visible entry = %v, want [3]", got)
	}
	if got := manager.Observe(2, gametypes.Vec3{X: 40}, visible).Entered; !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("second observer entry = %v, want [3]", got)
	}
}

func TestWithinFiltersKindAndCanStop(t *testing.T) {
	t.Parallel()
	manager := newManager(t)
	manager.Upsert(interest.Entity{ID: 3, Kind: gametypes.EntityKindNPC})
	manager.Upsert(interest.Entity{ID: 1, Kind: gametypes.EntityKindNPC})
	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindPlayer})

	var got []uint64
	manager.Within(gametypes.Vec3{}, 10, gametypes.EntityKindNPC, func(id uint64) bool {
		got = append(got, id)
		return false
	})
	if !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("visited = %v, want the first NPC only", got)
	}
}

func TestRemoveForgetAndInvalidInputs(t *testing.T) {
	t.Parallel()
	manager := newManager(t)
	manager.Upsert(interest.Entity{ID: 1, Kind: gametypes.EntityKindPlayer})
	manager.Upsert(interest.Entity{ID: 2, Kind: gametypes.EntityKindNPC})
	manager.Observe(1, gametypes.Vec3{}, nil)
	manager.Remove(2)
	if got := manager.Observe(1, gametypes.Vec3{}, nil).Left; !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("left after remove = %v, want [2]", got)
	}

	manager.Forget(1)
	if got := manager.Observe(1, gametypes.Vec3{}, nil).Entered; !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("entered after Forget = %v, want [1]", got)
	}

	manager.Upsert(interest.Entity{ID: 3, Position: gametypes.Vec3{X: float32(math.NaN())}})
	var visited bool
	manager.Within(gametypes.Vec3{}, float32(math.Inf(1)), gametypes.EntityKindUnspecified, func(uint64) bool {
		visited = true
		return true
	})
	if visited {
		t.Fatal("invalid query visited an entity")
	}
	if _, err := interest.New(0); err == nil {
		t.Fatal("interest.New(0) succeeded")
	}
}
