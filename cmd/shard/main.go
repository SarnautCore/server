package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/gateway"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/observability"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
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

	var tlsConfig *tls.Config
	listenAddress := settings.QUIC.PrivateListenAddress
	if settings.Private.AllowDirectShard {
		tlsConfig, err = transport.NewDevServerTLSConfig()
		listenAddress = settings.QUIC.ListenAddress
	} else {
		if len(settings.Private.Secret) != 32 {
			return errors.New("shard requires SARNAUT_PRIVATE_SHARED_SECRET_HEX")
		}
		tlsConfig, err = transport.LoadPrivateServerTLSConfig(
			settings.Private.CertificatePath, settings.Private.PrivateKeyPath,
		)
	}
	if err != nil {
		return err
	}
	listener, err := transport.ListenQUIC(listenAddress, tlsConfig)
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

	// Loot rolls against the same pack combat does, and its rules are read
	// before anything else so that a tree naming an item the pack does not
	// carry stops the boot rather than a player's kill.
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		return fmt.Errorf("read loot rules from content pack: %w", err)
	}

	worldModule := world.New(logger, zone)
	go worldModule.Run(ctx)
	go combatModule.Run(ctx)

	// Persistence remains shard-owned. Public admission and its NATS authority
	// live in the gateway; this process receives only an asserted identity.
	pool, err := clients.RequirePostgres()
	if err != nil {
		return err
	}
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		return err
	}

	worker := charstore.NewSaveWorker(
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
	characters := charstore.NewCharacterService(
		repository,
		templates,
		worker,
		logger,
		settings.Persistence.SaveTimeout,
	)
	// The bag is behind the repository, and the loot module is behind the bag:
	// nothing in `internal/loot` can reach a database, and nothing in
	// `internal/inventory` can reach the zone.
	bags, err := charstore.NewInventoryService(repository, inventory.LimitsFromPack(content), 0)
	if err != nil {
		return err
	}
	lootModule := loot.New(logger, zone, lootRules, bags, loot.Options{
		WorldSeed: settings.World.WorldSeed,
	})
	logger.Info("zone loot wired",
		"loot_tables", lootRules.TableCount(),
		"items", content.ItemCount(),
		"bag_slots", bags.Slots(),
	)

	// Quests read the same pack and grant through the same bag. The catalog is
	// built before anything is served. By default, a definition this build
	// cannot play stops the boot and names the quest. The content opt-in skips
	// only unsupported objective kinds; invalid prerequisites and rewards still
	// stop startup.
	if settings.Content.EnableImpactInterpreter {
		if err := content.ValidateQuestScriptCoverage(); err != nil {
			return fmt.Errorf("validate quest script coverage: %w", err)
		}
	}
	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{
		SkipUnsupportedQuests: settings.Content.SkipUnsupportedQuests,
		AllowCountSpecial:     settings.Content.EnableImpactInterpreter,
		Logger:                logger,
	})
	if err != nil {
		return fmt.Errorf("read quests from content pack: %w", err)
	}
	questModule := quests.New(logger, zone, catalog, bags)
	go questModule.Run(ctx)
	logger.Info("zone quests wired", "quests", catalog.Count())

	binding := session.ZoneBinding{
		World:  zone,
		Combat: combatModule,
		Loot:   lootModule,
		Quests: questModule,
	}
	if settings.Content.EnableImpactInterpreter {
		binding.Scripts = session.NewScriptDriver(
			logger,
			zone,
			questModule,
			session.NewPackQuestScriptSource(content),
			script.Options{Enabled: true},
		)
		binding.Scripts.BindCombat(combatModule)
		logger.Info("zone quest scripts wired", "quest_scripts", len(content.QuestScriptIDs()))
	}
	// One death, two consumers, and neither of them knows the other exists
	// (mechanics/combat.md rules 5.9.3 and 5.9.4).
	combatModule.SetKillSink(binding.KillSink())

	logger.Info("shard attachment wired",
		"shard_id", settings.Private.ShardID,
		"chargen_options", sortedOptionIDs(templates),
		"direct_compatibility", settings.Private.AllowDirectShard,
	)

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         settings.BuildID,
		// The shard states the identity of the pack it actually opened, so the
		// ADR 0027 content check has something to compare against. Left empty it
		// is worse than unwired: ServerHello would carry no pack id, and every
		// client that names its own would refuse the handshake.
		PackID:              content.ID(),
		AllowUnverifiedPack: settings.Content.AllowUnverifiedPack,
		Zones:               map[string]session.ZoneBinding{zone.ID(): binding},
		Characters:          characters,
		SaveInterval:        settings.Persistence.SaveInterval,
		Logger:              logger,
	}
	sessionErrors := make(chan error, 1)
	go func() {
		if settings.Private.AllowDirectShard {
			if clients.NATS == nil {
				sessionErrors <- errors.New("direct shard compatibility requires NATS")
				return
			}
			server.Authority = gateway.NewNATSAuthority(
				clients.NATS, shardInstanceID(settings.Auth.InstanceID), settings.Auth.RequestTimeout,
			)
			sessionErrors <- server.Serve(ctx, listener)
			return
		}
		sessionErrors <- server.ServePrivate(ctx, listener, gateway.TrustConfig{
			InstanceID: settings.Private.GatewayInstanceID,
			ShardID:    settings.Private.ShardID,
			KeyID:      settings.Private.KeyID,
			Secret:     settings.Private.Secret,
		})
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
