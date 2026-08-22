package combat_test

import (
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

// BenchmarkCombatStep is the AI-inclusive companion to
// `internal/world`.BenchmarkZoneStep: the same entity count, with every NPC a
// content-described mob running the aggro pass of mechanics/combat.md rule 5.7
// on every tick.
//
// It is the measurement that would regress if the aggro scan went back to
// walking the whole registry per mob, because that is the term that is
// quadratic in the entity count.
func BenchmarkCombatStep(b *testing.B) {
	content, err := pack.Load("../../testdata/packs/demo", pack.Options{})
	if err != nil {
		b.Fatalf("pack.Load() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		b.Fatalf("RulesFromPack() error = %v", err)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "BenchZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		b.Fatalf("NewZone() error = %v", err)
	}
	module := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{})

	// One placement per entity, spread over a grid, all of them the same
	// content-described mob.
	template, ok := spawnOf(content, targetMob)
	if !ok {
		b.Fatalf("the pack has no placement for %q", targetMob)
	}
	spawns := make([]pack.NPCSpawn, 0, benchmarkEntities-1)
	for index := 0; index < benchmarkEntities-1; index++ {
		spawn := template
		spawn.PlacementID = "placement.bench." + string(rune('a'+index%26)) + itoa(index)
		spawn.Position = pack.Vec3{X: float32(index%24) * 4, Y: float32(index/24) * 4}
		spawns = append(spawns, spawn)
	}
	if err := module.Populate(spawns); err != nil {
		b.Fatalf("Populate() error = %v", err)
	}
	playerID, _ := zone.Join()
	if err := module.Admit(playerID, combat.PlayerAdmission{
		Level: 1, Health: combat.MaxHealth(1, 1), MaxHealth: combat.MaxHealth(1, 1),
		AbilityIDs: rules.AbilityIDs(),
	}); err != nil {
		b.Fatalf("Admit() error = %v", err)
	}
	if err := zone.Subscribe(playerID, discardSnapshots{}); err != nil {
		b.Fatalf("Subscribe() error = %v", err)
	}

	const window = 500
	samples := make([]time.Duration, 0, b.N/window+1)
	b.ResetTimer()
	for index := 0; index < b.N; index += window {
		size := window
		if remaining := b.N - index; remaining < size {
			size = remaining
		}
		started := time.Now()
		for step := 0; step < size; step++ {
			zone.Step()
		}
		samples = append(samples, time.Since(started)/time.Duration(size))
	}
	b.StopTimer()

	sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
	b.ReportMetric(float64(samples[int(0.50*float64(len(samples)-1))].Nanoseconds()), "p50-ns/step")
	b.ReportMetric(float64(samples[int(0.99*float64(len(samples)-1))].Nanoseconds()), "p99-ns/step")
}

const benchmarkEntities = 288

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
