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
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/chataudience"
	"github.com/SarnautCore/server/internal/cohort"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/currency"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/observability"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/visibility"
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

	// Persistence and admission. Both are required: a shard that cannot save
	// loses progress silently, and a shard that cannot redeem a ticket would
	// have to admit anonymous peers, which is the one thing it must never do
	// (ADR 0030, ADR 0031).
	pool, err := clients.RequirePostgres()
	if err != nil {
		return err
	}
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		return err
	}
	if clients.NATS == nil {
		return errors.New(
			"no NATS configured: the shard redeems ADR 0030 tickets over NATS request/reply. " +
				"Set SARNAUT_NATS_URL",
		)
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
	partyAuthority := party.New()
	chatInfluence := visibility.NewInfluence()
	chatIgnoreLists := social.NewIgnoreLists()
	sayAudience, err := chataudience.New(content, chatInfluence, chatIgnoreLists)
	if err != nil {
		return fmt.Errorf("construct Say audience: %w", err)
	}
	localChatSessions := newWorldChatSessions(sayAudience, chatInfluence)
	cohortRepository, err := cohort.NewPostgresRepository(pool)
	if err != nil {
		return fmt.Errorf("construct social cohort repository: %w", err)
	}
	cohortPresence := cohort.NewPresenceRegistry()
	cohortAuthority, err := cohort.NewAuthority(cohortRepository, cohortPresence)
	if err != nil {
		return fmt.Errorf("construct social cohort authority: %w", err)
	}
	guildAudience, err := cohort.NewGuildChatAudience(cohortAuthority)
	if err != nil {
		return fmt.Errorf("construct guild chat audience: %w", err)
	}
	raidAudience, err := cohort.NewRaidChatAudience(cohortAuthority)
	if err != nil {
		return fmt.Errorf("construct raid chat audience: %w", err)
	}
	currencyLedger, err := currency.NewPostgres(pool)
	if err != nil {
		return fmt.Errorf("construct alternative currency ledger: %w", err)
	}
	chatModule := chat.New(chat.Options{
		Directory:       characterDirectory{characters: repository},
		CurrencySpender: paidChatCurrencies{ledger: currencyLedger},
		GroupAudience:   partyChatAudience{parties: partyAuthority},
		RaidAudience:    raidAudience,
		GuildAudience:   guildAudience,
		SayAudience:     sayAudience,
	})
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

	instanceID := shardInstanceID(settings.Auth.InstanceID)
	logger.Info("shard admission wired",
		"instance_id", instanceID,
		"chargen_options", sortedOptionIDs(templates),
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
		Authority:           session.NewNATSAuthority(clients.NATS, instanceID, settings.Auth.RequestTimeout),
		Characters:          characters,
		Chat:                chatModule,
		Party:               partyAuthority,
		Cohorts:             cohortPresence,
		LocalChat:           localChatSessions,
		SaveInterval:        settings.Persistence.SaveInterval,
		Logger:              logger,
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
