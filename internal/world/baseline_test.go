package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"google.golang.org/protobuf/proto"
)

// The replication baseline is the frozen "before" measurement that the M2
// replication-hardening work compares against. It is captured on the tree that
// still has the positionally typed wire format and the pre-ADR-0026
// EntitySnapshot, so the cost of the wire envelope and of the content and
// combat fields is measurable rather than asserted.
//
// Re-capture with:
//
//	SARNAUT_CAPTURE_BASELINE=1 go test ./internal/world -run TestCaptureReplicationBaseline
//
// Do not re-capture to make a later comparison pass. A new number belongs in a
// new file with its own provenance.
const (
	baselineSchema      = "sarnaut.replication-baseline.v1"
	baselineEntityCount = 288
	baselinePath        = "testdata/replication-baseline.json"

	// Windows resolves the Go monotonic clock at roughly half a millisecond,
	// which rounds a single Zone.step() to zero. Each sample therefore times a
	// window of consecutive steps and divides, so the recorded percentiles are
	// per-step nanoseconds derived from windows that are hundreds of clock
	// ticks long.
	baselineStepWindow  = 2000
	baselineStepSamples = 300
)

type replicationBaseline struct {
	Schema      string           `json:"schema"`
	CapturedAt  string           `json:"captured_at"`
	CapturedOn  string           `json:"captured_on_commit"`
	Describes   string           `json:"describes"`
	Host        baselineHost     `json:"host"`
	Zone        baselineZone     `json:"zone"`
	Snapshot    baselineSnapshot `json:"snapshot"`
	Step        baselineStep     `json:"step"`
	Methodology string           `json:"methodology"`
}

type baselineHost struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	GoVersion string `json:"go_version"`
	NumCPU    int    `json:"num_cpu"`
}

type baselineZone struct {
	EntityCount        int     `json:"entity_count"`
	PlayerCount        int     `json:"player_count"`
	NPCCount           int     `json:"npc_count"`
	SubscriberCount    int     `json:"subscriber_count"`
	TickIntervalMS     float64 `json:"tick_interval_ms"`
	SnapshotIntervalMS float64 `json:"snapshot_interval_ms"`
}

type baselineSnapshot struct {
	BatchBytes     int     `json:"batch_bytes"`
	BytesPerEntity float64 `json:"bytes_per_entity"`
	BytesPerSecond float64 `json:"bytes_per_second"`
}

type baselineStep struct {
	Samples    int   `json:"samples"`
	WindowSize int   `json:"window_steps"`
	P50Nano    int64 `json:"p50_ns"`
	P99Nano    int64 `json:"p99_ns"`
	MaxNano    int64 `json:"max_ns"`
}

// TestReplicationBaselineIsCommitted keeps the frozen measurement readable and
// well formed. It deliberately does not re-measure: the file records the tree
// as it was before ADR 0026 landed, so a fresh measurement would not match.
func TestReplicationBaselineIsCommitted(t *testing.T) {
	t.Parallel()

	baseline, err := loadReplicationBaseline()
	if err != nil {
		t.Fatalf("load replication baseline: %v", err)
	}
	if baseline.Schema != baselineSchema {
		t.Errorf("schema = %q, want %q", baseline.Schema, baselineSchema)
	}
	if baseline.Zone.EntityCount != baselineEntityCount {
		t.Errorf("entity_count = %d, want %d", baseline.Zone.EntityCount, baselineEntityCount)
	}
	if baseline.Snapshot.BatchBytes <= 0 || baseline.Snapshot.BytesPerSecond <= 0 {
		t.Errorf("snapshot measurement = %+v, want positive values", baseline.Snapshot)
	}
	if baseline.Step.P99Nano <= 0 || baseline.Step.P99Nano < baseline.Step.P50Nano {
		t.Errorf("step measurement = %+v, want a positive p99 at or above p50", baseline.Step)
	}
}

// TestCaptureReplicationBaseline rewrites the committed measurement. It is
// gated because a measurement taken on a different tree is not a baseline.
func TestCaptureReplicationBaseline(t *testing.T) {
	if os.Getenv("SARNAUT_CAPTURE_BASELINE") != "1" {
		t.Skip("set SARNAUT_CAPTURE_BASELINE=1 to rewrite " + baselinePath)
	}

	tickIntervalMS, snapshotIntervalMS := 33.333333, 66.666666
	tickInterval := time.Duration(tickIntervalMS * float64(time.Millisecond))
	snapshotInterval := time.Duration(snapshotIntervalMS * float64(time.Millisecond))
	zone, err := NewZone(ZoneConfig{
		ID:               "BaselineZone",
		TickInterval:     tickInterval,
		SnapshotInterval: snapshotInterval,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}

	playerID, _ := zone.Join()
	for index := 0; index < baselineEntityCount-1; index++ {
		zone.SpawnNPC(Vec3{X: float32(index) * 1.5, Y: float32(index) * 0.5, Z: 0}, float32(index)*0.01)
	}

	sink := new(sizingSink)
	if err := zone.Subscribe(playerID, sink); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	zone.publishSnapshot()
	if sink.batches != 1 {
		t.Fatalf("captured %d batches, want 1", sink.batches)
	}

	durations := make([]time.Duration, 0, baselineStepSamples)
	for sample := 0; sample < baselineStepSamples; sample++ {
		if err := zone.ApplyMoveIntent(playerID, &sarnautv1.ClientMoveIntent{
			Seq:       uint64(sample) + 1,
			Input:     &sarnautv1.Vec3{X: 1},
			DtSeconds: 0.2,
		}); err != nil {
			t.Fatalf("ApplyMoveIntent() error = %v", err)
		}
		started := time.Now()
		for step := 0; step < baselineStepWindow; step++ {
			zone.step()
		}
		durations = append(durations, time.Since(started)/baselineStepWindow)
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })

	batchesPerSecond := float64(time.Second) / float64(snapshotInterval)
	baseline := replicationBaseline{
		Schema:     baselineSchema,
		CapturedAt: time.Now().UTC().Format("2006-01-02"),
		CapturedOn: os.Getenv("SARNAUT_CAPTURE_BASELINE_COMMIT"),
		Describes: "Pre-ADR-0026 replication cost: bare SnapshotBatch frames and an " +
			"EntitySnapshot without content_id, name_key, level, faction, health, max_health or alive.",
		Host: baselineHost{
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
			GoVersion: runtime.Version(),
			NumCPU:    runtime.NumCPU(),
		},
		Zone: baselineZone{
			EntityCount:        baselineEntityCount,
			PlayerCount:        1,
			NPCCount:           baselineEntityCount - 1,
			SubscriberCount:    1,
			TickIntervalMS:     33.333333,
			SnapshotIntervalMS: 66.666666,
		},
		Snapshot: baselineSnapshot{
			BatchBytes:     sink.lastSize,
			BytesPerEntity: float64(sink.lastSize) / float64(baselineEntityCount),
			BytesPerSecond: float64(sink.lastSize) * batchesPerSecond,
		},
		Step: baselineStep{
			Samples:    len(durations),
			WindowSize: baselineStepWindow,
			P50Nano:    percentile(durations, 0.50).Nanoseconds(),
			P99Nano:    percentile(durations, 0.99).Nanoseconds(),
			MaxNano:    durations[len(durations)-1].Nanoseconds(),
		},
		Methodology: fmt.Sprintf(
			"One subscriber, all-entities interest. snapshot.batch_bytes is proto.Size of the published "+
				"SnapshotBatch; bytes_per_second scales it by 1000/snapshot_interval_ms. step percentiles "+
				"are per-step nanoseconds over %d samples, each timing a window of %d consecutive "+
				"Zone.step() calls, because the Windows monotonic clock rounds one step to zero. A move "+
				"intent is applied before each window, so roughly the first seven steps of a window "+
				"integrate the player and the rest walk the registry only.",
			baselineStepSamples,
			baselineStepWindow,
		),
	}

	encoded, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		t.Fatalf("create testdata directory: %v", err)
	}
	if err := os.WriteFile(baselinePath, encoded, 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf(
		"captured %s: %d batch bytes, %.0f bytes/s, step p99 %dns",
		baselinePath,
		baseline.Snapshot.BatchBytes,
		baseline.Snapshot.BytesPerSecond,
		baseline.Step.P99Nano,
	)
}

func loadReplicationBaseline() (replicationBaseline, error) {
	var baseline replicationBaseline
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		return baseline, err
	}
	if err := json.Unmarshal(raw, &baseline); err != nil {
		return baseline, fmt.Errorf("decode %s: %w", baselinePath, err)
	}
	return baseline, nil
}

func percentile(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(fraction * float64(len(sorted)-1))
	return sorted[index]
}

type sizingSink struct {
	batches  int
	lastSize int
}

func (sink *sizingSink) OfferSnapshot(batch *sarnautv1.SnapshotBatch) {
	sink.batches++
	sink.lastSize = proto.Size(batch)
}
