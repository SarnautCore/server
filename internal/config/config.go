// Package config loads service settings from YAML and environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config contains the shared skeleton configuration for all three processes.
type Config struct {
	ServiceName   string
	BuildID       string
	LogLevel      string
	HealthAddress string
	QUIC          QUICConfig
	World         WorldConfig
	Content       ContentConfig
	NATS          NATSConfig
	Postgres      PostgresConfig
	Persistence   PersistenceConfig
	Valkey        ValkeyConfig
	Auth          AuthConfig
	OTel          OTelConfig
}

// AuthConfig configures the auth service and the shard's side of it (ADR 0030).
type AuthConfig struct {
	// ListenAddress is the auth service's HTTP API. It is separate from the
	// health listener: probes and the account API have different exposure.
	ListenAddress string

	// NameBlocklist is matched against normalized character names.
	//
	// ADR 0032 §3 puts the blocklist in a `chargen.name_blocklist` pack
	// document. That document type does not exist yet, so for M2 it lives here:
	// still data rather than Go source, and changing it is a restart rather
	// than a rebuild. It moves into the pack with the document type.
	NameBlocklist []string

	// InstanceID identifies this shard in the Valkey play lock. Empty means the
	// process derives one from its host name and pid, so two shards on one host
	// do not claim to be each other.
	InstanceID string

	// RequestTimeout bounds one shard-to-auth NATS request. ADR 0030 fixes two
	// seconds, with no retry.
	RequestTimeout time.Duration
}

type QUICConfig struct {
	ListenAddress string
	ShardAddress  string
}

type WorldConfig struct {
	ZoneID           string
	TickInterval     time.Duration
	SnapshotInterval time.Duration
	MaxMoveSpeed     float32
	// SpawnSeed pins the zone spawn stream that draws mob levels and respawn
	// delays (mechanics/combat.md rules 5.1.5 and 5.9.6). It has a fixed
	// default rather than a clock-derived one so that two runs of the same
	// build over the same pack produce the same zone, which is what makes the
	// slice driver and the acceptance tests reproducible.
	SpawnSeed uint64
}

// ContentConfig points the shard at one compiled runtime pack. There is no
// default and no search path: a misconfigured shard fails loudly instead of
// quietly loading something else (ADR 0029).
type ContentConfig struct {
	// PackPath is the directory holding `manifest.json` and `tables/`.
	PackPath string
	// AllowExtra permits a pack built with `--keep-extra`, whose rows carry
	// verbatim MY.GAMES attribute names (ADR 0011). Default false.
	AllowExtra bool
}

type NATSConfig struct {
	URL string
}

type PostgresConfig struct {
	DSN string
}

// PersistenceConfig tunes the checkpoint save path of ADR 0031 §5. The defaults
// are the ADR's values; they are configurable so a load test can shorten the
// cadence without a rebuild, not because operators are expected to change them.
type PersistenceConfig struct {
	// SaveInterval is the per-character checkpoint cadence.
	SaveInterval time.Duration
	// SaveQueueSize bounds the save backlog. A full queue drops a checkpoint
	// rather than blocking the tick.
	SaveQueueSize int
	// SaveTimeout bounds one save transaction.
	SaveTimeout time.Duration
}

type ValkeyConfig struct {
	Address  string
	Password string
	DB       int
}

type OTelConfig struct {
	Endpoint string
	Insecure bool
}

type fileConfig struct {
	BuildID       *string `yaml:"build_id"`
	LogLevel      *string `yaml:"log_level"`
	HealthAddress *string `yaml:"health_address"`
	QUIC          struct {
		ListenAddress *string `yaml:"listen_address"`
		ShardAddress  *string `yaml:"shard_address"`
	} `yaml:"quic"`
	World struct {
		ZoneID           *string  `yaml:"zone_id"`
		TickInterval     *string  `yaml:"tick_interval"`
		SnapshotInterval *string  `yaml:"snapshot_interval"`
		MaxMoveSpeed     *float32 `yaml:"max_move_speed"`
		SpawnSeed        *uint64  `yaml:"spawn_seed"`
	} `yaml:"world"`
	Content struct {
		PackPath   *string `yaml:"pack_path"`
		AllowExtra *bool   `yaml:"allow_extra"`
	} `yaml:"content"`
	NATS struct {
		URL *string `yaml:"url"`
	} `yaml:"nats"`
	Postgres struct {
		DSN *string `yaml:"dsn"`
	} `yaml:"postgres"`
	Persistence struct {
		SaveInterval  *string `yaml:"save_interval"`
		SaveQueueSize *int    `yaml:"save_queue_size"`
		SaveTimeout   *string `yaml:"save_timeout"`
	} `yaml:"persistence"`
	Valkey struct {
		Address  *string `yaml:"address"`
		Password *string `yaml:"password"`
		DB       *int    `yaml:"db"`
	} `yaml:"valkey"`
	Auth struct {
		ListenAddress  *string   `yaml:"listen_address"`
		NameBlocklist  *[]string `yaml:"name_blocklist"`
		InstanceID     *string   `yaml:"instance_id"`
		RequestTimeout *string   `yaml:"request_timeout"`
	} `yaml:"auth"`
	OTel struct {
		Endpoint *string `yaml:"endpoint"`
		Insecure *bool   `yaml:"insecure"`
	} `yaml:"otel"`
}

// Load applies built-in defaults, an optional YAML file, then SARNAUT_* variables.
func Load(serviceName string) (Config, error) {
	configuration := defaults(serviceName)

	if path := os.Getenv("SARNAUT_CONFIG"); path != "" {
		if err := applyYAML(path, &configuration); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvironment(&configuration); err != nil {
		return Config{}, err
	}
	if configuration.World.TickInterval <= 0 {
		return Config{}, fmt.Errorf("world tick interval must be positive")
	}
	if configuration.World.SnapshotInterval <= 0 {
		return Config{}, fmt.Errorf("world snapshot interval must be positive")
	}
	if configuration.World.MaxMoveSpeed <= 0 {
		return Config{}, fmt.Errorf("world maximum move speed must be positive")
	}
	if configuration.World.ZoneID == "" {
		return Config{}, fmt.Errorf("world zone id must not be empty")
	}
	if serviceName == shardServiceName && configuration.Content.PackPath == "" {
		return Config{}, ErrNoContentPack
	}
	if configuration.Persistence.SaveInterval <= 0 || configuration.Persistence.SaveTimeout <= 0 {
		return Config{}, fmt.Errorf("persistence save interval and timeout must be positive")
	}
	if configuration.Persistence.SaveQueueSize <= 0 {
		return Config{}, fmt.Errorf("persistence save queue size must be positive")
	}
	if configuration.Auth.RequestTimeout <= 0 {
		return Config{}, fmt.Errorf("auth request timeout must be positive")
	}
	if serviceName == authServiceName && configuration.Auth.ListenAddress == "" {
		return Config{}, fmt.Errorf("auth listen address must not be empty")
	}

	return configuration, nil
}

// shardServiceName is the only service that loads content.
const shardServiceName = "shard"

// authServiceName is the account service.
const authServiceName = "auth"

// ErrNoContentPack reports a shard started without a content pack. It spells
// out the fix because there is deliberately no fallback: a shard with no pack
// cannot serve a zone (ADR 0029).
var ErrNoContentPack = errors.New(
	"no content pack configured: set SARNAUT_CONTENT_PACK to a pack directory " +
		"(one holding manifest.json and tables/), or set content.pack_path in the " +
		"config file. Build one with `sarnaut-pack build`; there is no default and " +
		"no fallback path",
)

func defaults(serviceName string) Config {
	healthAddress := "127.0.0.1:8080"
	switch serviceName {
	case "shard":
		healthAddress = "127.0.0.1:8081"
	case "auth":
		healthAddress = "127.0.0.1:8082"
	}

	return Config{
		ServiceName:   serviceName,
		BuildID:       "dev",
		LogLevel:      "info",
		HealthAddress: healthAddress,
		QUIC: QUICConfig{
			ListenAddress: "127.0.0.1:4242",
			ShardAddress:  "127.0.0.1:4242",
		},
		World: WorldConfig{
			ZoneID:           "InstLeague1",
			TickInterval:     time.Second / 30,
			SnapshotInterval: time.Second / 15,
			MaxMoveSpeed:     7,
		},
		// Content deliberately has no default: a public repository must not
		// ship a private path, and a shard must not guess where its content is.
		Persistence: PersistenceConfig{
			SaveInterval:  60 * time.Second,
			SaveQueueSize: 256,
			SaveTimeout:   5 * time.Second,
		},
		Auth: AuthConfig{
			ListenAddress:  "127.0.0.1:8083",
			NameBlocklist:  []string{"gm", "admin", "sarnaut"},
			RequestTimeout: 2 * time.Second,
		},
	}
}

func applyYAML(path string, configuration *Config) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}

	var values fileConfig
	if err := yaml.Unmarshal(contents, &values); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}

	if err := applyFileValues(configuration, values); err != nil {
		return fmt.Errorf("apply config file %q: %w", path, err)
	}
	return nil
}

func applyFileValues(configuration *Config, values fileConfig) error {
	setString(&configuration.BuildID, values.BuildID)
	setString(&configuration.LogLevel, values.LogLevel)
	setString(&configuration.HealthAddress, values.HealthAddress)
	setString(&configuration.QUIC.ListenAddress, values.QUIC.ListenAddress)
	setString(&configuration.QUIC.ShardAddress, values.QUIC.ShardAddress)
	setString(&configuration.World.ZoneID, values.World.ZoneID)
	setString(&configuration.Content.PackPath, values.Content.PackPath)
	setString(&configuration.NATS.URL, values.NATS.URL)
	setString(&configuration.Postgres.DSN, values.Postgres.DSN)
	setString(&configuration.Auth.ListenAddress, values.Auth.ListenAddress)
	setString(&configuration.Auth.InstanceID, values.Auth.InstanceID)
	setString(&configuration.Valkey.Address, values.Valkey.Address)
	setString(&configuration.Valkey.Password, values.Valkey.Password)
	setString(&configuration.OTel.Endpoint, values.OTel.Endpoint)
	if values.Content.AllowExtra != nil {
		configuration.Content.AllowExtra = *values.Content.AllowExtra
	}
	if values.Auth.NameBlocklist != nil {
		configuration.Auth.NameBlocklist = *values.Auth.NameBlocklist
	}
	if values.Auth.RequestTimeout != nil {
		duration, err := time.ParseDuration(*values.Auth.RequestTimeout)
		if err != nil {
			return fmt.Errorf("parse auth.request_timeout: %w", err)
		}
		configuration.Auth.RequestTimeout = duration
	}
	if values.Valkey.DB != nil {
		configuration.Valkey.DB = *values.Valkey.DB
	}
	if values.OTel.Insecure != nil {
		configuration.OTel.Insecure = *values.OTel.Insecure
	}
	if values.World.TickInterval != nil {
		duration, err := time.ParseDuration(*values.World.TickInterval)
		if err != nil {
			return fmt.Errorf("parse world.tick_interval: %w", err)
		}
		configuration.World.TickInterval = duration
	}
	if values.World.SnapshotInterval != nil {
		duration, err := time.ParseDuration(*values.World.SnapshotInterval)
		if err != nil {
			return fmt.Errorf("parse world.snapshot_interval: %w", err)
		}
		configuration.World.SnapshotInterval = duration
	}
	if values.World.MaxMoveSpeed != nil {
		configuration.World.MaxMoveSpeed = *values.World.MaxMoveSpeed
	}
	if values.World.SpawnSeed != nil {
		configuration.World.SpawnSeed = *values.World.SpawnSeed
	}
	if values.Persistence.SaveQueueSize != nil {
		configuration.Persistence.SaveQueueSize = *values.Persistence.SaveQueueSize
	}
	if values.Persistence.SaveInterval != nil {
		duration, err := time.ParseDuration(*values.Persistence.SaveInterval)
		if err != nil {
			return fmt.Errorf("parse persistence.save_interval: %w", err)
		}
		configuration.Persistence.SaveInterval = duration
	}
	if values.Persistence.SaveTimeout != nil {
		duration, err := time.ParseDuration(*values.Persistence.SaveTimeout)
		if err != nil {
			return fmt.Errorf("parse persistence.save_timeout: %w", err)
		}
		configuration.Persistence.SaveTimeout = duration
	}
	return nil
}

func applyEnvironment(configuration *Config) error {
	setFromEnvironment(&configuration.BuildID, "SARNAUT_BUILD_ID")
	setFromEnvironment(&configuration.LogLevel, "SARNAUT_LOG_LEVEL")
	setFromEnvironment(&configuration.HealthAddress, "SARNAUT_HEALTH_ADDRESS")
	setFromEnvironment(&configuration.QUIC.ListenAddress, "SARNAUT_QUIC_LISTEN_ADDRESS")
	setFromEnvironment(&configuration.QUIC.ShardAddress, "SARNAUT_SHARD_ADDRESS")
	setFromEnvironment(&configuration.World.ZoneID, "SARNAUT_WORLD_ZONE_ID")
	setFromEnvironment(&configuration.Content.PackPath, "SARNAUT_CONTENT_PACK")
	setFromEnvironment(&configuration.NATS.URL, "SARNAUT_NATS_URL")
	setFromEnvironment(&configuration.Postgres.DSN, "SARNAUT_POSTGRES_DSN")
	setFromEnvironment(&configuration.Auth.ListenAddress, "SARNAUT_AUTH_LISTEN_ADDRESS")
	setFromEnvironment(&configuration.Auth.InstanceID, "SARNAUT_SHARD_INSTANCE_ID")
	setFromEnvironment(&configuration.Valkey.Address, "SARNAUT_VALKEY_ADDRESS")
	setFromEnvironment(&configuration.Valkey.Password, "SARNAUT_VALKEY_PASSWORD")
	setFromEnvironment(&configuration.OTel.Endpoint, "SARNAUT_OTEL_ENDPOINT")

	if value := os.Getenv("SARNAUT_WORLD_TICK_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_WORLD_TICK_INTERVAL: %w", err)
		}
		configuration.World.TickInterval = duration
	}
	if value := os.Getenv("SARNAUT_WORLD_SNAPSHOT_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_WORLD_SNAPSHOT_INTERVAL: %w", err)
		}
		configuration.World.SnapshotInterval = duration
	}
	if value := os.Getenv("SARNAUT_WORLD_SPAWN_SEED"); value != "" {
		seed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_WORLD_SPAWN_SEED: %w", err)
		}
		configuration.World.SpawnSeed = seed
	}
	if value := os.Getenv("SARNAUT_WORLD_MAX_MOVE_SPEED"); value != "" {
		speed, err := strconv.ParseFloat(value, 32)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_WORLD_MAX_MOVE_SPEED: %w", err)
		}
		configuration.World.MaxMoveSpeed = float32(speed)
	}
	if value := os.Getenv("SARNAUT_CONTENT_ALLOW_EXTRA"); value != "" {
		allowExtra, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_CONTENT_ALLOW_EXTRA: %w", err)
		}
		configuration.Content.AllowExtra = allowExtra
	}
	if value := os.Getenv("SARNAUT_PERSISTENCE_SAVE_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_PERSISTENCE_SAVE_INTERVAL: %w", err)
		}
		configuration.Persistence.SaveInterval = duration
	}
	if value := os.Getenv("SARNAUT_PERSISTENCE_SAVE_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_PERSISTENCE_SAVE_TIMEOUT: %w", err)
		}
		configuration.Persistence.SaveTimeout = duration
	}
	if value := os.Getenv("SARNAUT_PERSISTENCE_SAVE_QUEUE_SIZE"); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_PERSISTENCE_SAVE_QUEUE_SIZE: %w", err)
		}
		configuration.Persistence.SaveQueueSize = size
	}
	if value := os.Getenv("SARNAUT_AUTH_NAME_BLOCKLIST"); value != "" {
		// Comma separated, so a deployment can widen the blocklist without a
		// config file. An explicit "-" means no blocklist at all, which a test
		// needs and a deployment should not use.
		if value == "-" {
			configuration.Auth.NameBlocklist = []string{}
		} else {
			configuration.Auth.NameBlocklist = splitList(value)
		}
	}
	if value := os.Getenv("SARNAUT_AUTH_REQUEST_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_AUTH_REQUEST_TIMEOUT: %w", err)
		}
		configuration.Auth.RequestTimeout = duration
	}
	if value := os.Getenv("SARNAUT_VALKEY_DB"); value != "" {
		database, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_VALKEY_DB: %w", err)
		}
		configuration.Valkey.DB = database
	}
	if value := os.Getenv("SARNAUT_OTEL_INSECURE"); value != "" {
		insecure, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse SARNAUT_OTEL_INSECURE: %w", err)
		}
		configuration.OTel.Insecure = insecure
	}

	return nil
}

// splitList reads a comma-separated environment value, dropping empty entries
// so a trailing comma is not a blocklist entry that matches every name.
func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func setString(target *string, value *string) {
	if value != nil {
		*target = *value
	}
}

func setFromEnvironment(target *string, name string) {
	if value := os.Getenv(name); value != "" {
		*target = value
	}
}
