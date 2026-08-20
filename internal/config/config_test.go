package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/config"
)

func TestLoadAppliesYAMLThenEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("build_id: yaml-build\nworld:\n  tick_interval: 75ms\n  snapshot_interval: 100ms\ncontent:\n  pack_path: fixture-pack\n")
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
	if got.Content.PackPath != "fixture-pack" {
		t.Errorf("Content.PackPath = %q, want fixture-pack", got.Content.PackPath)
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

func TestLoadRejectsAShardWithNoContentPack(t *testing.T) {
	t.Setenv("SARNAUT_CONFIG", "")
	t.Setenv("SARNAUT_CONTENT_PACK", "")

	_, err := config.Load("shard")
	if !errors.Is(err, config.ErrNoContentPack) {
		t.Fatalf("Load() error = %v, want ErrNoContentPack", err)
	}
	// The message has to tell an operator what to do: there is no fallback.
	for _, want := range []string{"SARNAUT_CONTENT_PACK", "sarnaut-pack build", "no fallback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error %q does not mention %q", err, want)
		}
	}
}

func TestLoadReadsTheContentPackFromTheEnvironment(t *testing.T) {
	t.Setenv("SARNAUT_CONFIG", "")
	t.Setenv("SARNAUT_CONTENT_PACK", filepath.Join("packs", "classic", "demo"))
	t.Setenv("SARNAUT_CONTENT_ALLOW_EXTRA", "true")

	got, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Content.PackPath != filepath.Join("packs", "classic", "demo") {
		t.Errorf("Content.PackPath = %q", got.Content.PackPath)
	}
	if !got.Content.AllowExtra {
		t.Error("Content.AllowExtra = false, want true")
	}
}

// Only the shard serves a zone, so only the shard needs a pack.
func TestLoadDoesNotRequireAContentPackForOtherServices(t *testing.T) {
	t.Setenv("SARNAUT_CONFIG", "")
	t.Setenv("SARNAUT_CONTENT_PACK", "")

	for _, service := range []string{"gateway", "auth", "probe"} {
		if _, err := config.Load(service); err != nil {
			t.Errorf("Load(%q) error = %v", service, err)
		}
	}
}

func TestDefaultsCarryNoPrivatePath(t *testing.T) {
	t.Setenv("SARNAUT_CONFIG", "")
	t.Setenv("SARNAUT_CONTENT_PACK", "")

	got, err := config.Load("gateway")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Content.PackPath != "" {
		t.Errorf("Content.PackPath = %q, want empty: a public repository ships no private path", got.Content.PackPath)
	}
	if got.Content.AllowExtra {
		t.Error("Content.AllowExtra defaults to true, want false")
	}
}
