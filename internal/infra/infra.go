// Package infra opens optional infrastructure clients used by the service skeletons.
package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SarnautCore/server/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// Clients holds configured PostgreSQL, Valkey, and NATS connections.
type Clients struct {
	Postgres *pgxpool.Pool
	Valkey   *redis.Client
	NATS     *nats.Conn
}

// Open connects only clients whose endpoints are configured.
func Open(ctx context.Context, settings config.Config) (*Clients, error) {
	clients := new(Clients)
	if err := clients.openPostgres(ctx, settings.Postgres); err != nil {
		return nil, err
	}
	if err := clients.openValkey(ctx, settings.Valkey); err != nil {
		return nil, errors.Join(err, clients.Close())
	}
	if err := clients.openNATS(settings.ServiceName, settings.NATS); err != nil {
		return nil, errors.Join(err, clients.Close())
	}
	return clients, nil
}

// Close releases every configured client.
func (clients *Clients) Close() error {
	if clients == nil {
		return nil
	}
	if clients.Postgres != nil {
		clients.Postgres.Close()
	}
	if clients.NATS != nil {
		clients.NATS.Close()
	}
	if clients.Valkey != nil {
		return clients.Valkey.Close()
	}
	return nil
}

func (clients *Clients) openPostgres(ctx context.Context, settings config.PostgresConfig) error {
	if settings.DSN == "" {
		return nil
	}

	pool, err := pgxpool.New(ctx, settings.DSN)
	if err != nil {
		return fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	clients.Postgres = pool

	pingContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingContext); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return nil
}

func (clients *Clients) openValkey(ctx context.Context, settings config.ValkeyConfig) error {
	if settings.Address == "" {
		return nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:     settings.Address,
		Password: settings.Password,
		DB:       settings.DB,
	})
	clients.Valkey = client

	pingContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingContext).Err(); err != nil {
		return fmt.Errorf("ping Valkey: %w", err)
	}
	return nil
}

func (clients *Clients) openNATS(serviceName string, settings config.NATSConfig) error {
	if settings.URL == "" {
		return nil
	}

	client, err := nats.Connect(
		settings.URL,
		nats.Name("sarnaut-"+serviceName),
		nats.Timeout(3*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	clients.NATS = client
	return nil
}
