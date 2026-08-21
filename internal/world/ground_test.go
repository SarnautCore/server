package world_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/world"
)

func slopedGround(t testing.TB) *world.Heightfield {
	t.Helper()
	// z = x + 2y over a 6 by 6 grid.
	heights := make([]float32, 36)
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			heights[y*6+x] = float32(x + 2*y)
		}
	}
	ground, err := world.NewHeightfield(world.HeightfieldSpec{
		CellSize: 1, Width: 6, Height: 6, Heights: heights, MaxGrade: 3,
	})
	if err != nil {
		t.Fatalf("NewHeightfield() error = %v", err)
	}
	return ground
}

func newGroundedZone(t testing.TB, ground world.Ground) *world.Zone {
	t.Helper()
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "ground-fixture", TickInterval: 100 * time.Millisecond,
		SnapshotInterval: time.Second, MaxMoveSpeed: 2,
		PlayerSpawn: world.Vec3{X: 0.5, Y: 0.5, Z: -100}, Ground: ground,
		GroundSampleStep: 0.25,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	return zone
}

func TestHeightfieldInterpolatesAuthoredGround(t *testing.T) {
	t.Parallel()
	ground := slopedGround(t)
	sample, ok := ground.Sample(1.25, 2.5)
	if !ok {
		t.Fatal("Sample() rejected an interior point")
	}
	if diff := math.Abs(float64(sample.Height - 6.25)); diff > 0.0001 {
		t.Fatalf("height = %v, want 6.25", sample.Height)
	}
	if !sample.Walkable {
		t.Fatal("authored walkable slope was refused")
	}
	if _, ok := ground.Sample(-0.01, 1); ok {
		t.Fatal("Sample() accepted a point outside the grid")
	}
}

func TestPlayerMovementTracksSlopeAndIgnoresClientZ(t *testing.T) {
	t.Parallel()
	zone := newGroundedZone(t, slopedGround(t))
	entityID, placed := zone.JoinAt(world.Vec3{X: 0.5, Y: 0.5, Z: 900}, 0)
	if placed.Z != 1.5 {
		t.Fatalf("join z = %v, want sampled 1.5", placed.Z)
	}
	if err := zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq: 1, Input: world.Vec3{X: 1, Z: -900}, Duration: 250 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ApplyMoveIntent() error = %v", err)
	}
	for index := 0; index < 3; index++ {
		zone.Step()
	}
	state, ok := zone.SnapshotCharacter(entityID)
	if !ok {
		t.Fatal("SnapshotCharacter() did not find player")
	}
	if diff := math.Abs(float64(state.Position.Z - (state.Position.X + 2*state.Position.Y))); diff > 0.0001 {
		t.Fatalf("position = %#v, want z=x+2y", state.Position)
	}
	if state.Position.Z == placed.Z {
		t.Fatalf("z stayed at fixed spawn value %v", placed.Z)
	}
}

func TestMoveIntentRefusesAuthoredNonWalkableSpace(t *testing.T) {
	t.Parallel()
	walkable := make([]bool, 36)
	for index := range walkable {
		walkable[index] = true
	}
	// The cell reached by moving right from (0.1, 0.1) uses vertex (2, 0).
	walkable[2] = false
	ground, err := world.NewHeightfield(world.HeightfieldSpec{
		CellSize: 1, Width: 6, Height: 6, Heights: make([]float32, 36), Walkable: walkable,
	})
	if err != nil {
		t.Fatalf("NewHeightfield() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID: "non-walkable-fixture", TickInterval: 100 * time.Millisecond,
		SnapshotInterval: time.Second, MaxMoveSpeed: 8,
		PlayerSpawn: world.Vec3{X: 0.1, Y: 0.1}, Ground: ground,
		GroundSampleStep: 0.25,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	entityID, _ := zone.JoinAt(world.Vec3{X: 0.1, Y: 0.1}, 0)
	err = zone.ApplyMoveIntent(entityID, world.MoveIntent{
		Seq: 1, Input: world.Vec3{X: 1}, Duration: 250 * time.Millisecond,
	})
	var refusal *world.MoveRefusalError
	if !errors.As(err, &refusal) || refusal.Reason != world.MoveRefusalNonWalkable {
		t.Fatalf("ApplyMoveIntent() error = %#v, want non-walkable refusal", err)
	}
	zone.Step()
	state, _ := zone.SnapshotCharacter(entityID)
	if state.Position != (world.Vec3{X: 0.1, Y: 0.1}) {
		t.Fatalf("refused player moved to %#v", state.Position)
	}
}

func TestWorldMovementGroundsMobSpawnAndAIMove(t *testing.T) {
	t.Parallel()
	zone := newGroundedZone(t, slopedGround(t))
	mobID := zone.SpawnNPC(world.NPCSpec{Position: world.Vec3{X: 1, Y: 1, Z: -7}})
	if err := zone.Command(func(tick *world.Tick) error {
		mob := tick.Entity(mobID)
		if got := mob.Position().Z; got != 3 {
			t.Fatalf("spawn z = %v, want 3", got)
		}
		tick.MoveTo(mob, world.Vec3{X: 2, Y: 2, Z: 999})
		if got := mob.Position().Z; got != 6 {
			t.Fatalf("move z = %v, want 6", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Command() error = %v", err)
	}
}

func TestTutorialPlacementSweepUsesGlobalFrame(t *testing.T) {
	t.Parallel()
	ground := slopedGround(t)
	// This is an authored public test fixture, not extracted game data. It
	// covers every fixture placement and catches swapped or patch-local axes.
	placements := []world.Vec3{
		{X: 0.25, Y: 0.75, Z: -8},
		{X: 1.5, Y: 2.25, Z: 44},
		{X: 3.75, Y: 1.125, Z: 0},
		{X: 4.5, Y: 4.5, Z: 100},
	}
	changed := 0
	for index, placement := range placements {
		sample, ok := ground.Sample(placement.X, placement.Y)
		if !ok {
			t.Fatalf("placement %d is outside terrain", index)
		}
		if math.Abs(float64(sample.Height-(placement.X+2*placement.Y))) > 0.0001 {
			t.Fatalf("placement %d sampled %v", index, sample.Height)
		}
		if sample.Height != placement.Z {
			changed++
		}
	}
	if changed != len(placements) {
		t.Fatalf("ground snap changed %d/%d old fixed-Z values", changed, len(placements))
	}
}

func TestPlacementSweepReportsEveryMismatch(t *testing.T) {
	t.Parallel()
	ground := slopedGround(t)
	report, err := world.SweepGroundPlacements(ground, []world.GroundPlacement{
		{ID: "placement.good", Position: world.Vec3{X: 1, Y: 1, Z: 3}},
		{ID: "placement.bad-height", Position: world.Vec3{X: 2, Y: 2, Z: 99}},
		{ID: "placement.bad-frame", Position: world.Vec3{X: 40, Y: 40, Z: 0}},
	}, 0.01)
	if err != nil {
		t.Fatalf("SweepGroundPlacements() error = %v", err)
	}
	if report.Checked != 3 || len(report.Mismatches) != 2 {
		t.Fatalf("placement sweep = %#v", report)
	}
	if report.Mismatches[0].ID != "placement.bad-height" || report.Mismatches[1].ID != "placement.bad-frame" ||
		!report.Mismatches[1].OutsideTerrain {
		t.Fatalf("placement mismatches = %#v", report.Mismatches)
	}
}

func BenchmarkGroundedMovementM2Population(b *testing.B) {
	zone := newGroundedZone(b, slopedGround(b))
	for index := 0; index < 288; index++ {
		x := 0.25 + float32(index%5)
		y := 0.25 + float32((index/5)%5)
		zone.SpawnNPC(world.NPCSpec{Position: world.Vec3{X: x, Y: y}})
	}
	playerID, _ := zone.JoinAt(world.Vec3{X: 0.25, Y: 0.25}, 0)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		direction := float32(0.01)
		if index%2 != 0 {
			direction = -direction
		}
		if err := zone.ApplyMoveIntent(playerID, world.MoveIntent{
			Seq: uint64(index + 1), Input: world.Vec3{X: direction}, Duration: time.Millisecond,
		}); err != nil {
			b.Fatal(err)
		}
		zone.Step()
	}
}
