package world_test

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/world"
)

// benchmarkEntityCount is the InstLeague1 figure the replication baseline was
// captured at, kept the same so the two measurements are comparable.
const benchmarkEntityCount = 288

// stepReportPath is where BenchmarkZoneStep writes its percentile report.
const stepReportPath = "testdata/step-p99.txt"

// BenchmarkZoneStep measures one tick of a fully populated zone.
//
// It reports the p99 as a custom metric rather than leaving it to the harness's
// mean, because a tick loop is a deadline system: the mean says the budget is
// comfortable while the tail says a subscriber saw a stutter. Run it with
//
//	go test ./internal/world -run '^$' -bench BenchmarkZoneStep -benchtime 200000x
//
// and set SARNAUT_WRITE_STEP_REPORT=1 to rewrite testdata/step-p99.txt.
func BenchmarkZoneStep(b *testing.B) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "BenchZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		b.Fatalf("NewZone() error = %v", err)
	}
	playerID, _ := zone.Join()
	if err := zone.Subscribe(playerID, discardingSink{}); err != nil {
		b.Fatalf("Subscribe() error = %v", err)
	}
	// A grid of NPCs 4 m apart, which spreads them over enough spatial cells
	// that a neighbour query is doing real work rather than hitting one bucket.
	for index := 0; index < benchmarkEntityCount-1; index++ {
		zone.SpawnNPC(world.NPCSpec{
			ContentID: "mob.fixture.bench",
			Level:     2,
			MaxHealth: 120,
			Position: world.Vec3{
				X: float32(index%24) * 4,
				Y: float32(index/24) * 4,
			},
		})
	}

	// Windowed timing, because the Windows monotonic clock rounds a single
	// step to zero. Each sample is a window of consecutive steps divided by the
	// window size, which is the same method the replication baseline used.
	const window = 500
	samples := make([]time.Duration, 0, b.N/window+1)

	b.ResetTimer()
	for index := 0; index < b.N; index += window {
		size := window
		if remaining := b.N - index; remaining < size {
			size = remaining
		}
		if err := zone.ApplyMoveIntent(playerID, world.MoveIntent{
			Seq:      uint64(index) + 1,
			Input:    world.Vec3{X: 1},
			Duration: 200 * time.Millisecond,
		}); err != nil {
			b.Fatalf("ApplyMoveIntent() error = %v", err)
		}
		started := time.Now()
		for step := 0; step < size; step++ {
			zone.Step()
		}
		samples = append(samples, time.Since(started)/time.Duration(size))
	}
	b.StopTimer()

	sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
	p50 := percentileOf(samples, 0.50)
	p99 := percentileOf(samples, 0.99)
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns/step")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/step")

	if os.Getenv("SARNAUT_WRITE_STEP_REPORT") == "1" {
		writeStepReport(b, len(samples), window, p50, p99, samples[len(samples)-1])
	}
}

// TestStepReportIsCommitted keeps the recorded measurement present and
// readable. It does not re-measure: a number taken on CI hardware is not the
// number the report describes, and a benchmark that failed a threshold on a
// shared runner would be noise rather than signal.
func TestStepReportIsCommitted(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(stepReportPath)
	if err != nil {
		t.Fatalf("read %s: %v", stepReportPath, err)
	}
	report := string(contents)
	for _, wanted := range []string{"entities=288", "p99_ns=", "p50_ns=", "goos=", "go_version="} {
		if !strings.Contains(report, wanted) {
			t.Errorf("%s does not record %q", stepReportPath, wanted)
		}
	}
}

func writeStepReport(b *testing.B, samples, window int, p50, p99, worst time.Duration) {
	b.Helper()
	report := fmt.Sprintf(`# Zone.Step() cost at a full InstLeague1 entity count.
#
# Each sample times a window of consecutive Zone.Step() calls and divides, because
# the Windows monotonic clock rounds one step to zero. Rewrite with:
#   SARNAUT_WRITE_STEP_REPORT=1 go test ./internal/world -run '^$' \
#     -bench BenchmarkZoneStep -benchtime %dx
benchmark=BenchmarkZoneStep
entities=%d
window_steps=%d
samples=%d
p50_ns=%d
p99_ns=%d
max_ns=%d
goos=%s
goarch=%s
go_version=%s
num_cpu=%d
captured_at=%s
`,
		samples*window,
		benchmarkEntityCount,
		window,
		samples,
		p50.Nanoseconds(),
		p99.Nanoseconds(),
		worst.Nanoseconds(),
		runtime.GOOS,
		runtime.GOARCH,
		runtime.Version(),
		runtime.NumCPU(),
		time.Now().UTC().Format("2006-01-02"),
	)
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		b.Fatalf("create testdata: %v", err)
	}
	if err := os.WriteFile(stepReportPath, []byte(report), 0o600); err != nil {
		b.Fatalf("write %s: %v", stepReportPath, err)
	}
	b.Logf("wrote %s: p50 %v, p99 %v", stepReportPath, p50, p99)
}

func percentileOf(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(fraction * float64(len(sorted)-1))
	return sorted[index]
}

type discardingSink struct{}

func (discardingSink) OfferSnapshot(world.Snapshot) {}
