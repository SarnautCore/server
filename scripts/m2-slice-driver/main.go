// Command m2-slice-driver plays the M2 vertical slice headlessly and reports
// whether each step of it works.
//
// It is the slice's smoke test, and it is meant to grow: every later M2 server
// task adds a step to the sequence below rather than writing a driver of its
// own. After the combat task the sequence is connect, enter, target, cast and
// kill; loot, quests and persistence each append to it.
//
// It is built on `cmd/probe`, which is the same session client, driven to a
// script instead of to a duration. By default it stands the shard up in
// process from a content pack, so it needs nothing running and no
// configuration; `-address` points it at a shard that is already up.
//
// Output is one line per step, `PASS <step>` or `FAIL <step>`, and a non-zero
// exit if any step failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"github.com/google/uuid"
)

// defaultPack is the vendored fixture, relative to the repository root. The
// driver deliberately runs on synthetic content: a slip in the content lane
// must not be able to stall the first demoable kill.
const defaultPack = "testdata/packs/demo"

// defaultTarget is the M2 combat target of mechanics/combat.md section 6.1.
const defaultTarget = "mob.paper-harbor.tide-crab"

// castDistance is where the driver stands to cast, in metres. It is the
// scenario input of the worked example.
const castDistance = 6

func main() {
	address := flag.String("address", "", "shard QUIC address; empty starts one in process")
	packPath := flag.String("pack", defaultPack, "content pack directory")
	zoneID := flag.String("zone", "M2Slice", "zone to enter when the shard is started in process")
	targetMob := flag.String("target", defaultTarget, "canonical id of the mob to kill")
	abilityID := flag.String("ability", "", "canonical id of the ability to cast; empty uses the caster's first")
	ticket := flag.String("ticket", "", "shard ticket to present to an already-running shard; the in-process shard mints its own")
	timeout := flag.Duration("timeout", 90*time.Second, "give up after this long")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	driver := &driver{out: os.Stdout}
	err := driver.run(ctx, *address, *packPath, *zoneID, *targetMob, *abilityID, *ticket)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "m2-slice-driver: %v\n", err)
	}
	if driver.failed || err != nil {
		os.Exit(1)
	}
}

type driver struct {
	out    io.Writer
	failed bool
}

func (driver *driver) pass(step string, format string, arguments ...any) {
	_, _ = fmt.Fprintf(driver.out, "PASS %-8s %s\n", step, fmt.Sprintf(format, arguments...))
}

func (driver *driver) fail(step string, format string, arguments ...any) {
	driver.failed = true
	_, _ = fmt.Fprintf(driver.out, "FAIL %-8s %s\n", step, fmt.Sprintf(format, arguments...))
}

func (driver *driver) run(ctx context.Context, address, packPath, zoneID, targetMob, abilityID, ticket string) error {
	if address == "" {
		hosted, err := startInProcessShard(ctx, packPath, zoneID, targetMob)
		if err != nil {
			return err
		}
		address, zoneID, ticket = hosted.address, hosted.zoneID, hosted.ticket
		driver.pass("host", "in-process shard on %s, zone %s, pack %s", address, zoneID, hosted.packID)
	}

	connection, err := transport.DialQUIC(ctx, address, transport.NewDevClientTLSConfig())
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	defer func() { _ = connection.Close() }()

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "m2-slice-driver",
		Ticket:          ticket,
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		driver.fail("connect", "handshake: %v", err)
		return nil
	}
	entered, err := client.EnterZone(connection, zoneID)
	if err != nil {
		driver.fail("connect", "enter zone %s: %v", zoneID, err)
		return nil
	}
	driver.pass("connect", "zone=%s entity=%d datagrams=%t server_pack=%q",
		entered.GetZoneId(), entered.GetOwnEntityId(), connection.SupportsUnreliable(), hello.GetPackId())

	target, err := findTarget(ctx, client, connection, targetMob)
	if err != nil {
		driver.fail("target", "%v", err)
		return nil
	}
	driver.pass("target", "%s entity=%d level=%d health=%d/%d",
		target.GetContentId(), target.GetEntityId(), target.GetLevel(),
		target.GetHealth(), target.GetMaxHealth())

	report, err := driver.castUntilDead(client, connection, target, abilityID)
	if err != nil {
		driver.fail("cast", "%v", err)
		return nil
	}
	driver.pass("cast", "%d casts of %s for %d damage each, %d refused for cooldown",
		report.casts, report.abilityID, report.damagePerCast, report.refusals)

	switch {
	case report.death == nil:
		driver.fail("kill", "%s survived %d casts", target.GetContentId(), report.casts)
	case report.death.GetKillerEntityId() != entered.GetOwnEntityId():
		driver.fail("kill", "kill credit went to entity %d, not to this session's %d",
			report.death.GetKillerEntityId(), entered.GetOwnEntityId())
	case report.totalDamage != target.GetMaxHealth():
		driver.fail("kill", "%d total damage against a %d health pool",
			report.totalDamage, target.GetMaxHealth())
	default:
		driver.pass("kill", "%s died on cast %d to %d total damage; corpse despawns at tick %d",
			target.GetContentId(), report.casts, report.totalDamage,
			report.death.GetCorpseDespawnTick())
	}

	if err := client.Logout(connection); err != nil {
		driver.fail("logout", "%v", err)
		return nil
	}
	driver.pass("logout", "clean exit requested")
	return nil
}

type killReport struct {
	abilityID     string
	casts         int
	refusals      int
	damagePerCast int32
	totalDamage   int32
	death         *sarnautv1.DeathEvent
}

// castUntilDead casts as fast as the server will let it, which is how a client
// behaves: the cooldown is the server's to enforce, and a refusal costs
// nothing (mechanics/combat.md section 6.2).
func (driver *driver) castUntilDead(
	client session.Client,
	connection transport.Connection,
	target *sarnautv1.EntitySnapshot,
	abilityID string,
) (killReport, error) {
	report := killReport{abilityID: abilityID}
	var seq uint64
	for attempt := 0; attempt < 500; attempt++ {
		seq++
		if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
			ClientSeq: seq,
			Payload: &sarnautv1.ClientMessage_AbilityUse{
				AbilityUse: &sarnautv1.AbilityUse{
					TargetId:  target.GetEntityId(),
					AbilityId: abilityID,
				},
			},
		}); err != nil {
			return report, fmt.Errorf("send ability use: %w", err)
		}

		for report.death == nil {
			message, err := client.ReadReliableMessage(connection)
			if err != nil {
				return report, fmt.Errorf("read server message: %w", err)
			}
			if death := message.GetDeathEvent(); death != nil {
				report.death = death
				break
			}
			event := message.GetCombatEvent()
			if event == nil {
				continue
			}
			switch event.GetRejection() {
			case sarnautv1.AbilityRejection_ABILITY_REJECTION_NONE:
				report.casts++
				report.abilityID = event.GetAbilityId()
				report.damagePerCast = event.GetDamage()
				report.totalDamage += event.GetDamage()
			case sarnautv1.AbilityRejection_ABILITY_REJECTION_ON_COOLDOWN:
				report.refusals++
				time.Sleep(100 * time.Millisecond)
			default:
				return report, fmt.Errorf("cast refused: %s", event.GetRejection())
			}
			break
		}
		if report.death != nil {
			return report, nil
		}
	}
	return report, nil
}

func findTarget(
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
	contentID string,
) (*sarnautv1.EntitySnapshot, error) {
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			return nil, fmt.Errorf("read snapshot: %w", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetContentId() == contentID && entity.GetAlive() {
				return entity, nil
			}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no snapshot carried a live %s", contentID)
		}
	}
}

type hostedShard struct {
	address string
	zoneID  string
	packID  string
	// ticket is the single-use shard ticket the in-process authority minted for
	// this run. The shard admits nobody without one (ADR 0030).
	ticket string
}

// startInProcessShard is the same composition as `cmd/shard`, minus the
// telemetry, the health endpoint and the infrastructure clients, and with the
// player spawned next to the target so the driver has something to cast at
// without a pathfinder.
func startInProcessShard(ctx context.Context, packPath, zoneID, targetMob string) (hostedShard, error) {
	content, err := pack.Load(packPath, pack.Options{})
	if err != nil {
		return hostedShard{}, fmt.Errorf("load content pack %q: %w", packPath, err)
	}
	anchor, err := anchorOf(content, targetMob)
	if err != nil {
		return hostedShard{}, err
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		return hostedShard{}, fmt.Errorf("read combat rules: %w", err)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               zoneID,
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     7,
		PlayerSpawn:      anchor.Add(world.Vec3{X: castDistance}),
	})
	if err != nil {
		return hostedShard{}, fmt.Errorf("create zone: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	combatModule := combat.New(logger, zone, rules, combat.Options{})
	if err := combatModule.Populate(content.NPCSpawns()); err != nil {
		return hostedShard{}, fmt.Errorf("populate zone: %w", err)
	}

	tlsConfig, err := transport.NewDevServerTLSConfig()
	if err != nil {
		return hostedShard{}, err
	}
	listener, err := transport.ListenQUIC("127.0.0.1:0", tlsConfig)
	if err != nil {
		return hostedShard{}, err
	}
	// Admission, composed the way `cmd/shard` composes it but over the
	// in-memory repository: the shard refuses a peer with no ticket, so the
	// driver has to mint one rather than skip the check it is meant to cover.
	worker := store.NewSaveWorker(store.NewMemory(), logger, 0, 0)
	characters := store.NewCharacterService(
		store.NewMemory(),
		sliceTemplates{spawn: store.Vec3{
			X: anchor.X + castDistance,
			Y: anchor.Y,
			Z: anchor.Z,
		}},
		worker,
		logger,
		0,
	)
	authority := newSliceAuthority()

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "m2-slice-driver",
		Zones:           map[string]session.ZoneBinding{zone.ID(): {World: zone, Combat: combatModule}},
		Authority:       authority,
		Characters:      characters,
		Logger:          logger,
	}

	go zone.Run(ctx)
	go combatModule.Run(ctx)
	go worker.Run(ctx)
	go func() {
		if err := server.Serve(ctx, listener); err != nil {
			logger.Error("slice shard stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	return hostedShard{
		address: listener.Addr().String(),
		zoneID:  zone.ID(),
		packID:  content.ID(),
		ticket:  authority.ticket,
	}, nil
}

// sliceTemplates materializes the driver's character next to the target.
//
// `cmd/shard` reads the spawn out of the pack's chargen row; the driver
// overrides it for the same reason it overrides the zone's player spawn, which
// is that the worked example starts six metres from the mob and the driver has
// no pathfinder to walk there.
type sliceTemplates struct {
	spawn store.Vec3
}

func (templates sliceTemplates) Template(string) (store.Snapshot, bool) {
	return store.Snapshot{
		State: store.CharacterState{Position: templates.spawn, Level: 1, Health: 100},
	}, true
}

// sliceAuthority is the auth service for one run: one ticket, redeemable once,
// and a play lock nobody contends. The real service is a separate process with
// a database; standing one up would make the driver need infrastructure, which
// is the one thing it promises not to need.
type sliceAuthority struct {
	ticket string

	mu       sync.Mutex
	redeemed bool
}

func newSliceAuthority() *sliceAuthority {
	return &sliceAuthority{ticket: "sarnaut_tk_m2-slice-driver"}
}

func (authority *sliceAuthority) RedeemTicket(_ context.Context, ticket string) (session.Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if ticket != authority.ticket || authority.redeemed {
		return session.Admission{}, &session.Refusal{Reason: session.ReasonUnknownTicket}
	}
	authority.redeemed = true
	return session.Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000e001"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000f001"),
		CharacterName:   "Slice",
		ChargenOptionID: "chargen.league.warrior",
	}, nil
}

func (authority *sliceAuthority) RenewPlayLock(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func (authority *sliceAuthority) ReleasePlayLock(context.Context, uuid.UUID) error { return nil }

func anchorOf(content *pack.Pack, mobID string) (world.Vec3, error) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID != mobID {
			continue
		}
		return world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}, nil
	}
	return world.Vec3{}, fmt.Errorf(
		"content pack %s has no live placement spawning %q; pass -target",
		content.ID(), mobID,
	)
}
