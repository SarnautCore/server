package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/config"
)

func TestLoadAppliesYAMLThenEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("build_id: yaml-build\nworld:\n  tick_interval: 75ms\n  snapshot_interval: 100ms\ncontent:\n  root_path: fixture-data\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("SARNAUT_CONFIG", path)
	t.Setenv("SARNAUT_BUILD_ID", "env-build")

	got, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.BuildID != "env-build" {
		t.Errorf("BuildID = %q, want %q", got.BuildID, "env-build")
	}
	if got.World.TickInterval != 75*time.Millisecond {
		t.Errorf("World.TickInterval = %s, want 75ms", got.World.TickInterval)
	}
	if got.World.SnapshotInterval != 100*time.Millisecond {
		t.Errorf("World.SnapshotInterval = %s, want 100ms", got.World.SnapshotInterval)
	}
	if got.Content.RootPath != "fixture-data" {
		t.Errorf("Content.RootPath = %q, want fixture-data", got.Content.RootPath)
	}
	if got.ServiceName != "shard" {
		t.Errorf("ServiceName = %q, want %q", got.ServiceName, "shard")
	}
}

func TestLoadRejectsInvalidYAMLDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("world:\n  tick_interval: soon\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("SARNAUT_CONFIG", path)

	if _, err := config.Load("shard"); err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
}
