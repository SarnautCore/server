// Package config loads service settings from YAML and environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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
	NATS          NATSConfig
	Postgres      PostgresConfig
	Valkey        ValkeyConfig
	OTel          OTelConfig
}

type QUICConfig struct {
	ListenAddress string
	ShardAddress  string
}

type WorldConfig struct {
	TickInterval time.Duration
}

type NATSConfig struct {
	URL string
}

type PostgresConfig struct {
	DSN string
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
		TickInterval *string `yaml:"tick_interval"`
	} `yaml:"world"`
	NATS struct {
		URL *string `yaml:"url"`
	} `yaml:"nats"`
	Postgres struct {
		DSN *string `yaml:"dsn"`
	} `yaml:"postgres"`
	Valkey struct {
		Address  *string `yaml:"address"`
		Password *string `yaml:"password"`
		DB       *int    `yaml:"db"`
	} `yaml:"valkey"`
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

	return configuration, nil
}

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
		World: WorldConfig{TickInterval: 50 * time.Millisecond},
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
	setString(&configuration.NATS.URL, values.NATS.URL)
	setString(&configuration.Postgres.DSN, values.Postgres.DSN)
	setString(&configuration.Valkey.Address, values.Valkey.Address)
	setString(&configuration.Valkey.Password, values.Valkey.Password)
	setString(&configuration.OTel.Endpoint, values.OTel.Endpoint)
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
	return nil
}

func applyEnvironment(configuration *Config) error {
	setFromEnvironment(&configuration.BuildID, "SARNAUT_BUILD_ID")
	setFromEnvironment(&configuration.LogLevel, "SARNAUT_LOG_LEVEL")
	setFromEnvironment(&configuration.HealthAddress, "SARNAUT_HEALTH_ADDRESS")
	setFromEnvironment(&configuration.QUIC.ListenAddress, "SARNAUT_QUIC_LISTEN_ADDRESS")
	setFromEnvironment(&configuration.QUIC.ShardAddress, "SARNAUT_SHARD_ADDRESS")
	setFromEnvironment(&configuration.NATS.URL, "SARNAUT_NATS_URL")
	setFromEnvironment(&configuration.Postgres.DSN, "SARNAUT_POSTGRES_DSN")
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
