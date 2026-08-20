package session

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/world"
	"google.golang.org/protobuf/proto"
)

const (
	aoiReportSchema = "sarnaut.replication-aoi-report.v1"
	aoiReportPath   = "testdata/replication-aoi.json"
)

type aoiReport struct {
	Schema      string       `json:"schema"`
	CapturedAt  string       `json:"captured_at"`
	Host        baselineHost `json:"host"`
	Zone        aoiZone      `json:"zone"`
	Snapshot    aoiSnapshot  `json:"snapshot"`
	Step        baselineStep `json:"step"`
	Reduction   aoiReduction `json:"reduction_vs_pre_change_baseline"`
	Methodology string       `json:"methodology"`
}

type aoiZone struct {
	EntityCount        int     `json:"entity_count"`
	InterestedEntities int     `json:"interested_entities"`
	PlayerCount        int     `json:"player_count"`
	NPCCount           int     `json:"npc_count"`
	SubscriberCount    int     `json:"subscriber_count"`
	InterestRadiusM    float32 `json:"interest_radius_m"`
	TickIntervalMS     float64 `json:"tick_interval_ms"`
	SnapshotIntervalMS float64 `json:"snapshot_interval_ms"`
}

type aoiSnapshot struct {
	EncodedBytes   int     `json:"encoded_bytes_per_publish"`
	BytesPerSecond float64 `json:"bytes_per_second"`
	ChunkCount     int     `json:"chunk_count"`
	LargestChunk   int     `json:"largest_chunk_bytes"`
}

type aoiReduction struct {
	BaselineBytesPerSecond float64 `json:"baseline_bytes_per_second"`
	BytesPerSecond         float64 `json:"bytes_per_second"`
	BytesPerSecondPercent  float64 `json:"bytes_per_second_percent"`
	BaselineStepP99NS      int64   `json:"baseline_step_p99_ns"`
	StepP99NS              int64   `json:"step_p99_ns"`
	StepP99Percent         float64 `json:"step_p99_percent"`
}

func BenchmarkReplicationAOIAt288Entities(b *testing.B) {
	tickIntervalMS, snapshotIntervalMS := 33.333333, 66.666666
	tickInterval := time.Duration(tickIntervalMS * float64(time.Millisecond))
	snapshotInterval := time.Duration(snapshotIntervalMS * float64(time.Millisecond))
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "AOIBenchZone",
		TickInterval:     tickInterval,
		SnapshotInterval: snapshotInterval,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		b.Fatalf("NewZone() error = %v", err)
	}

	playerID, _ := zone.Join()
	for index := 0; index < baselineEntityCount-1; index++ {
		zone.SpawnNPC(world.NPCSpec{
			ContentID: "mob.fixture.baseline",
			NameKey:   "Mob.Fixture.Baseline.Name.txt",
			Faction:   "faction.fixture.hostile",
			Level:     1,
			MaxHealth: 100,
			Position:  world.Vec3{X: float32(index) * 1.5, Y: float32(index) * 0.5},
			Heading:   float32(index) * 0.01,
		})
	}
	sink := new(aoiSizingSink)
	if err := zone.Subscribe(playerID, sink); err != nil {
		b.Fatalf("Subscribe() error = %v", err)
	}
	// The second publish is steady state. Spawn events belong to the reliable
	// transition channel and are deliberately not part of snapshot bytes/s.
	zone.PublishSnapshot()
	zone.PublishSnapshot()
	if sink.batches != 2 || sink.dropped != 0 {
		b.Fatalf("snapshot measurement = %d batches, %d dropped entities", sink.batches, sink.dropped)
	}

	const window = baselineStepWindow
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
	p50 := percentile(samples, 0.50)
	p99 := percentile(samples, 0.99)
	b.ReportMetric(sink.bytesPerSecond(snapshotInterval), "snapshot-bytes/s")
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns/step")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/step")

	if os.Getenv("SARNAUT_WRITE_AOI_REPORT") == "1" && len(samples) >= baselineStepSamples {
		writeAOIReport(b, sink, snapshotInterval, p50, p99, samples[len(samples)-1], len(samples), window)
	}
}

func TestReplicationAOIReportIsCommitted(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(aoiReportPath)
	if err != nil {
		t.Fatalf("read %s: %v", aoiReportPath, err)
	}
	var report aoiReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode %s: %v", aoiReportPath, err)
	}
	if report.Schema != aoiReportSchema {
		t.Errorf("schema = %q, want %q", report.Schema, aoiReportSchema)
	}
	if report.Zone.EntityCount != baselineEntityCount || report.Zone.InterestedEntities <= 0 {
		t.Errorf("zone measurement = %+v", report.Zone)
	}
	if report.Snapshot.BytesPerSecond <= 0 || report.Step.P99Nano <= 0 {
		t.Errorf("performance measurement = snapshot %+v, step %+v", report.Snapshot, report.Step)
	}
}

func writeAOIReport(
	b *testing.B,
	sink *aoiSizingSink,
	snapshotInterval time.Duration,
	p50, p99, worst time.Duration,
	samples, window int,
) {
	b.Helper()
	baseline, err := loadReplicationBaseline()
	if err != nil {
		b.Fatalf("load baseline: %v", err)
	}
	bytesPerSecond := sink.bytesPerSecond(snapshotInterval)
	report := aoiReport{
		Schema:     aoiReportSchema,
		CapturedAt: time.Now().UTC().Format("2006-01-02"),
		Host: baselineHost{
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
			GoVersion: runtime.Version(),
			NumCPU:    runtime.NumCPU(),
		},
		Zone: aoiZone{
			EntityCount:        baselineEntityCount,
			InterestedEntities: sink.entities,
			PlayerCount:        1,
			NPCCount:           baselineEntityCount - 1,
			SubscriberCount:    1,
			InterestRadiusM:    world.ReplicationInterestRadiusMetres,
			TickIntervalMS:     33.333333,
			SnapshotIntervalMS: 66.666666,
		},
		Snapshot: aoiSnapshot{
			EncodedBytes:   sink.bytes,
			BytesPerSecond: bytesPerSecond,
			ChunkCount:     sink.chunks,
			LargestChunk:   sink.largestChunk,
		},
		Step: baselineStep{
			Samples:    samples,
			WindowSize: window,
			P50Nano:    p50.Nanoseconds(),
			P99Nano:    p99.Nanoseconds(),
			MaxNano:    worst.Nanoseconds(),
		},
		Reduction: aoiReduction{
			BaselineBytesPerSecond: baseline.Snapshot.BytesPerSecond,
			BytesPerSecond:         bytesPerSecond,
			BytesPerSecondPercent:  percentOf(bytesPerSecond, baseline.Snapshot.BytesPerSecond),
			BaselineStepP99NS:      baseline.Step.P99Nano,
			StepP99NS:              p99.Nanoseconds(),
			StepP99Percent:         percentOf(float64(p99.Nanoseconds()), float64(baseline.Step.P99Nano)),
		},
		Methodology: fmt.Sprintf(
			"One subscriber at the origin and %d NPCs placed on the pre-change baseline's line. "+
				"encoded_bytes_per_publish is the sum of proto.Size over every chunked ServerMessage "+
				"for one steady-state snapshot; bytes_per_second scales it by 1000/snapshot_interval_ms. "+
				"Reliable spawn/despawn events are excluded. Step percentiles are per-step nanoseconds "+
				"over %d samples of %d consecutive Zone.Step calls.",
			baselineEntityCount-1,
			samples,
			window,
		),
	}

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		b.Fatalf("marshal report: %v", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		b.Fatalf("create testdata: %v", err)
	}
	if err := os.WriteFile(aoiReportPath, encoded, 0o600); err != nil {
		b.Fatalf("write %s: %v", aoiReportPath, err)
	}
	b.Logf("wrote %s: %.0f snapshot bytes/s, step p99 %dns", aoiReportPath, bytesPerSecond, p99.Nanoseconds())
}

func percentOf(value, baseline float64) float64 {
	if baseline == 0 {
		return 0
	}
	return value / baseline * 100
}

type aoiSizingSink struct {
	batches      int
	entities     int
	bytes        int
	chunks       int
	largestChunk int
	dropped      int
}

func (sink *aoiSizingSink) OfferSnapshot(snapshot world.Snapshot) {
	sink.batches++
	batch, err := snapshotToProto(snapshot)
	if err != nil {
		panic(err)
	}
	chunks, dropped := splitSnapshot(batch)
	sink.entities = len(snapshot.Entities)
	sink.bytes = 0
	sink.chunks = len(chunks)
	sink.largestChunk = 0
	for _, chunk := range chunks {
		size := proto.Size(chunk)
		sink.bytes += size
		if size > sink.largestChunk {
			sink.largestChunk = size
		}
	}
	sink.dropped = len(dropped)
}

func (sink *aoiSizingSink) bytesPerSecond(interval time.Duration) float64 {
	return float64(sink.bytes) * float64(time.Second) / float64(interval)
}
