package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/observability"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func main() {
	dumpSpawns := flag.String(
		"dump-spawns",
		"",
		"resolve the configured content pack, write its NPC spawn set as JSON to this path, and exit",
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *dumpSpawns); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "shard: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dumpSpawnsTo string) error {
	settings, err := config.Load("shard")
	if err != nil {
		return err
	}

	content, err := pack.Load(settings.Content.PackPath, pack.Options{
		AllowExtra: settings.Content.AllowExtra,
	})
	if err != nil {
		return fmt.Errorf("load content pack: %w", err)
	}
	if dumpSpawnsTo != "" {
		payload, err := renderSpawnDump(content)
		if err != nil {
			return fmt.Errorf("render spawn dump: %w", err)
		}
		if err := os.WriteFile(dumpSpawnsTo, payload, 0o644); err != nil {
			return fmt.Errorf("write spawn dump: %w", err)
		}
		return nil
	}

	logger := observability.NewLogger(settings.LogLevel).With("service", settings.ServiceName)
	if content.KeepExtra() {
		logger.Warn(
			"content pack carries the untyped extra passthrough",
			"pack_path", content.Directory(),
			"pack_id", content.ID(),
		)
	}

	shutdownTelemetry, err := observability.Setup(
		ctx,
		settings.OTel,
		settings.ServiceName,
		settings.BuildID,
	)
	if err != nil {
		return err
	}
	defer shutDownTelemetry(logger, shutdownTelemetry)

	clients, err := infra.Open(ctx, settings)
	if err != nil {
		return err
	}
	defer func() {
		if err := clients.Close(); err != nil {
			logger.Error("close infrastructure clients", "error", err)
		}
	}()

	tlsConfig, err := transport.NewDevServerTLSConfig()
	if err != nil {
		return err
	}
	listener, err := transport.ListenQUIC(settings.QUIC.ListenAddress, tlsConfig)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	status := new(health.Status)
	healthErrors := startHealthServer(ctx, settings.HealthAddress, status)

	zoneContent := content.Zone()
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               settings.World.ZoneID,
		TickInterval:     settings.World.TickInterval,
		SnapshotInterval: settings.World.SnapshotInterval,
		MaxMoveSpeed:     settings.World.MaxMoveSpeed,
		PlayerSpawn: world.Vec3{
			X: zoneContent.PlayerSpawn.X,
			Y: zoneContent.PlayerSpawn.Y,
			Z: zoneContent.PlayerSpawn.Z,
		},
	})
	if err != nil {
		return fmt.Errorf("create zone: %w", err)
	}
	// Combat resolves every gameplay rule against the pack, so it is built
	// from the pack before anything is spawned, and it is what spawns: a mob's
	// level is a draw from the zone spawn stream, which combat owns.
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		return fmt.Errorf("read combat rules from content pack: %w", err)
	}
	combatModule := combat.New(logger, zone, rules, combat.Options{Seed: settings.World.SpawnSeed})
	spawns := content.NPCSpawns()
	if err := combatModule.Populate(spawns); err != nil {
		return fmt.Errorf("populate zone: %w", err)
	}
	logger.Info(
		"zone content loaded",
		"zone_id", zone.ID(),
		"pack_id", content.ID(),
		"ruleset", zoneContent.Ruleset,
		"zone_slug", zoneContent.Slug,
		"npc_count", len(spawns),
		"ability_count", len(rules.AbilityIDs()),
		"player_faction", rules.PlayerFaction(),
	)

	worldModule := world.New(logger, zone)
	go worldModule.Run(ctx)
	go combatModule.Run(ctx)

	// Persistence and admission. Both are required: a shard that cannot save
	// loses progress silently, and a shard that cannot redeem a ticket would
	// have to admit anonymous peers, which is the one thing it must never do
	// (ADR 0030, ADR 0031).
	pool, err := clients.RequirePostgres()
	if err != nil {
		return err
	}
	repository, err := store.NewPostgres(pool)
	if err != nil {
		return err
	}
	if clients.NATS == nil {
		return errors.New(
			"no NATS configured: the shard redeems ADR 0030 tickets over NATS request/reply. " +
				"Set SARNAUT_NATS_URL",
		)
	}

	worker := store.NewSaveWorker(
		repository,
		logger,
		settings.Persistence.SaveQueueSize,
		settings.Persistence.SaveTimeout,
	)
	go worker.Run(ctx)

	templates, err := chargenTemplates(content, zoneContent.PlayerSpawn)
	if err != nil {
		return err
	}
	characters := store.NewCharacterService(
		repository,
		templates,
		worker,
		logger,
		settings.Persistence.SaveTimeout,
	)
	instanceID := shardInstanceID(settings.Auth.InstanceID)
	logger.Info("shard admission wired",
		"instance_id", instanceID,
		"chargen_options", sortedOptionIDs(templates),
	)

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         settings.BuildID,
		Zones: map[string]session.ZoneBinding{
			zone.ID(): {World: zone, Combat: combatModule},
		},
		Authority:    session.NewNATSAuthority(clients.NATS, instanceID, settings.Auth.RequestTimeout),
		Characters:   characters,
		SaveInterval: settings.Persistence.SaveInterval,
		Logger:       logger,
	}
	sessionErrors := make(chan error, 1)
	go func() {
		sessionErrors <- server.Serve(ctx, listener)
	}()

	status.SetReady(true)
	logger.Info(
		"shard ready",
		"quic_address", listener.Addr().String(),
		"health_address", settings.HealthAddress,
	)

	select {
	case <-ctx.Done():
		status.SetReady(false)
		return nil
	case err := <-healthErrors:
		return err
	case err := <-sessionErrors:
		return err
	}
}

func startHealthServer(ctx context.Context, address string, status *health.Status) <-chan error {
	errorsChannel := make(chan error, 1)
	go func() {
		errorsChannel <- health.Serve(ctx, address, status)
	}()
	return errorsChannel
}

func shutDownTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shut down telemetry", "error", err)
	}
}
