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
	contents := []byte("build_id: yaml-build\nworld:\n  tick_interval: 75ms\n  snapshot_interval: 100ms\ncontent:\n  pack_path: fixture-pack\n  skip_unsupported_quests: true\n")
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
	if !got.Content.SkipUnsupportedQuests {
		t.Error("Content.SkipUnsupportedQuests = false, want the YAML value true")
	}
	if got.ServiceName != "shard" {
		t.Errorf("ServiceName = %q, want %q", got.ServiceName, "shard")
	}
}

func TestLoadDefaultsPersistenceToTheADRCadence(t *testing.T) {
	// A shard has no default content pack (ADR 0029), so every successful
	// shard Load has to name one before the persistence defaults are reached.
	t.Setenv("SARNAUT_CONTENT_PACK", "fixture-pack")

	got, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Persistence.SaveInterval != time.Minute {
		t.Errorf("Persistence.SaveInterval = %s, want 1m", got.Persistence.SaveInterval)
	}
	if got.Persistence.SaveTimeout != 5*time.Second {
		t.Errorf("Persistence.SaveTimeout = %s, want 5s", got.Persistence.SaveTimeout)
	}
	if got.Persistence.SaveQueueSize != 256 {
		t.Errorf("Persistence.SaveQueueSize = %d, want 256", got.Persistence.SaveQueueSize)
	}
}

func TestLoadReadsPersistenceFromYAMLAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("persistence:\n  save_interval: 30s\n  save_timeout: 2s\n  save_queue_size: 32\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("SARNAUT_CONFIG", path)
	t.Setenv("SARNAUT_CONTENT_PACK", "fixture-pack")
	t.Setenv("SARNAUT_PERSISTENCE_SAVE_QUEUE_SIZE", "64")

	got, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Persistence.SaveInterval != 30*time.Second {
		t.Errorf("Persistence.SaveInterval = %s, want 30s", got.Persistence.SaveInterval)
	}
	if got.Persistence.SaveTimeout != 2*time.Second {
		t.Errorf("Persistence.SaveTimeout = %s, want 2s", got.Persistence.SaveTimeout)
	}
	if got.Persistence.SaveQueueSize != 64 {
		t.Errorf("Persistence.SaveQueueSize = %d, want the environment override 64", got.Persistence.SaveQueueSize)
	}
}

func TestLoadRejectsANonPositiveSaveQueueSize(t *testing.T) {
	// Name a pack, or Load fails on the missing content pack first and this
	// test passes without ever reaching the queue size it is about.
	t.Setenv("SARNAUT_CONTENT_PACK", "fixture-pack")
	t.Setenv("SARNAUT_PERSISTENCE_SAVE_QUEUE_SIZE", "0")

	_, err := config.Load("shard")
	if err == nil {
		t.Fatal("Load() error = nil, want a rejected save queue size")
	}
	if !strings.Contains(err.Error(), "save queue size") {
		t.Fatalf("Load() error = %v, want it to name the save queue size", err)
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
	t.Setenv("SARNAUT_CONTENT_SKIP_UNSUPPORTED_QUESTS", "true")
	t.Setenv("SARNAUT_CONTENT_ENABLE_IMPACT_INTERPRETER", "true")

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
	if !got.Content.SkipUnsupportedQuests {
		t.Error("Content.SkipUnsupportedQuests = false, want true")
	}
	if !got.Content.EnableImpactInterpreter {
		t.Error("Content.EnableImpactInterpreter = false, want true")
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
	if got.Content.SkipUnsupportedQuests {
		t.Error("Content.SkipUnsupportedQuests defaults to true, want false")
	}
	if got.Content.EnableImpactInterpreter {
		t.Error("Content.EnableImpactInterpreter defaults to true, want false")
	}
}

func TestLoadDefaultsAuthToTheADRValues(t *testing.T) {
	t.Setenv("SARNAUT_CONFIG", "")

	got, err := config.Load("auth")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Auth.ListenAddress == "" || got.Auth.ListenAddress == got.HealthAddress {
		t.Errorf("Auth.ListenAddress = %q; the account API must not share the health listener", got.Auth.ListenAddress)
	}
	if got.Auth.RequestTimeout != 2*time.Second {
		t.Errorf("Auth.RequestTimeout = %s, want the ADR 0030 two seconds", got.Auth.RequestTimeout)
	}
	// ADR 0032 §3's M2 blocklist: impersonation prefixes and nothing else.
	if strings.Join(got.Auth.NameBlocklist, ",") != "gm,admin,sarnaut" {
		t.Errorf("Auth.NameBlocklist = %v, want the ADR 0032 list", got.Auth.NameBlocklist)
	}
	if got.Auth.InstanceID != "" {
		t.Errorf("Auth.InstanceID = %q, want empty so the process derives one", got.Auth.InstanceID)
	}
}

func TestLoadReadsAuthFromYAMLAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(
		"content:\n  pack_path: fixture-pack\n" +
			"auth:\n  listen_address: 127.0.0.1:9999\n  name_blocklist: [gm, mod]\n" +
			"  instance_id: shard-a\n  request_timeout: 750ms\n",
	)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("SARNAUT_CONFIG", path)

	got, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Auth.ListenAddress != "127.0.0.1:9999" || got.Auth.InstanceID != "shard-a" {
		t.Errorf("Auth = %+v, want the YAML values", got.Auth)
	}
	if got.Auth.RequestTimeout != 750*time.Millisecond {
		t.Errorf("Auth.RequestTimeout = %s, want 750ms", got.Auth.RequestTimeout)
	}
	if strings.Join(got.Auth.NameBlocklist, ",") != "gm,mod" {
		t.Errorf("Auth.NameBlocklist = %v, want the YAML list", got.Auth.NameBlocklist)
	}

	// The environment wins, and a trailing comma is not a blocklist entry that
	// matches every name.
	t.Setenv("SARNAUT_AUTH_NAME_BLOCKLIST", "gm, admin,")
	t.Setenv("SARNAUT_SHARD_INSTANCE_ID", "shard-b")
	got, err = config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strings.Join(got.Auth.NameBlocklist, ",") != "gm,admin" {
		t.Errorf("Auth.NameBlocklist = %v, want the environment list", got.Auth.NameBlocklist)
	}
	if got.Auth.InstanceID != "shard-b" {
		t.Errorf("Auth.InstanceID = %q, want shard-b", got.Auth.InstanceID)
	}

	// An explicit "-" is how a test says "no blocklist at all".
	t.Setenv("SARNAUT_AUTH_NAME_BLOCKLIST", "-")
	got, err = config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got.Auth.NameBlocklist) != 0 {
		t.Errorf("Auth.NameBlocklist = %v, want empty", got.Auth.NameBlocklist)
	}
}

// TestWorldSeedIsConfigurationRatherThanAProcessValue is mechanics/loot.md rule
// 5.2.4. A shard restart must not change the drop a given corpse would produce,
// which is only true if the seed comes from a config file or an environment
// variable and never from the clock or the pid.
func TestWorldSeedIsConfigurationRatherThanAProcessValue(t *testing.T) {
	defaults, err := config.Load("gateway")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if defaults.World.WorldSeed == "" {
		t.Error("World.WorldSeed defaults to empty; every shard would share one loot stream")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("world:\n  world_seed: from-yaml\ncontent:\n  pack_path: fixture-pack\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("SARNAUT_CONFIG", path)

	fromFile, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if fromFile.World.WorldSeed != "from-yaml" {
		t.Errorf("World.WorldSeed = %q, want the file's value", fromFile.World.WorldSeed)
	}

	t.Setenv("SARNAUT_WORLD_SEED", "from-environment")
	fromEnvironment, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if fromEnvironment.World.WorldSeed != "from-environment" {
		t.Errorf("World.WorldSeed = %q, want the environment to win", fromEnvironment.World.WorldSeed)
	}

	// Two loads of one configuration produce one seed. It is the whole point of
	// the rule, and it is what a clock-derived default would break.
	repeat, err := config.Load("shard")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if repeat.World.WorldSeed != fromEnvironment.World.WorldSeed {
		t.Errorf("two loads gave %q then %q", fromEnvironment.World.WorldSeed, repeat.World.WorldSeed)
	}
}
