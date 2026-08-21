// Command m2-slice-driver plays the M2 vertical slice headlessly and reports
// whether each step of it works.
//
// It is the slice's smoke test, and it is meant to grow: every later M2 server
// task adds a step to the sequence below rather than writing a driver of its
// own. After the quest task the sequence is connect, enter, accept, target,
// cast, kill, loot and turn in; persistence appends to it next.
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
	"github.com/google/uuid"
)

// defaultPack is the vendored fixture, relative to the repository root. The
// driver deliberately runs on synthetic content: a slip in the content lane
// must not be able to stall the first demoable kill.
const defaultPack = "testdata/packs/demo"

// skipUnsupportedQuestsFor keeps the synthetic fixture on the fail-fast
// default. Supplying any other pack path is the driver's real-content mode,
// whose pack deliberately includes objectives queued for M3.
func skipUnsupportedQuestsFor(packPath string) bool {
	packAbsolute, packErr := filepath.Abs(packPath)
	defaultAbsolute, defaultErr := filepath.Abs(defaultPack)
	if packErr == nil && defaultErr == nil {
		return !strings.EqualFold(filepath.Clean(packAbsolute), filepath.Clean(defaultAbsolute))
	}
	return !strings.EqualFold(filepath.Clean(packPath), filepath.Clean(defaultPack))
}

// defaultQuest is the quest the slice plays end to end, and defaultGiver is the
// NPC that both offers and finishes it. Both are ids in the fixture pack: a
// second quest is played by passing -quest and -giver, and needs no change
// here.
const (
	defaultQuest = "quest.paper-harbor.tide-tally"
	defaultGiver = "mob.paper-harbor.harbor-quartermaster"
)

// defaultTarget is the M2 combat target of mechanics/combat.md section 6.1.
//
// It is also the loot target. Its table is the curated depth-3 tree of
// mechanics/loot.md section 6.1, so the driver's loot step exercises the
// recursion rather than a flat table, and what it drops depends on the roll
// seed — which is why the step asserts that the bag matches the result rather
// than asserting a particular item.
const defaultTarget = "mob.paper-harbor.tide-crab"

// castDistance is where the driver stands to cast, in metres. It is the
// scenario input of the worked example.
const castDistance = 6

// corpseSearchWindow bounds the wait for the corpse container to reach a
// snapshot. Snapshots go out every 66 ms, so this is generous by two orders of
// magnitude and exists only so a broken loot module fails a step rather than
// hanging the driver.
const corpseSearchWindow = 10 * time.Second

func main() {
	address := flag.String("address", "", "shard QUIC address; empty starts one in process")
	packPath := flag.String("pack", defaultPack, "content pack directory")
	zoneID := flag.String("zone", "M2Slice", "zone to enter when the shard is started in process")
	targetMob := flag.String("target", defaultTarget, "canonical id of the mob to kill")
	questID := flag.String("quest", defaultQuest, "canonical id of the quest to accept and turn in")
	giverMob := flag.String("giver", defaultGiver, "canonical id of the NPC that offers and finishes the quest")
	abilityID := flag.String("ability", "", "canonical id of the ability to cast; empty uses the caster's first")
	ticket := flag.String("ticket", "", "shard ticket to present to an already-running shard; the in-process shard mints its own")
	timeout := flag.Duration("timeout", 90*time.Second, "give up after this long")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	driver := &driver{out: os.Stdout}
	err := driver.run(ctx, runOptions{
		address:   *address,
		packPath:  *packPath,
		zoneID:    *zoneID,
		targetMob: *targetMob,
		abilityID: *abilityID,
		ticket:    *ticket,
		questID:   *questID,
		giverMob:  *giverMob,
		// The default synthetic fixture stays fail-fast. An explicit pack is a
		// real-pack run and may deliberately carry objectives queued for M3.
		skipUnsupportedQuests: skipUnsupportedQuestsFor(*packPath),
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "m2-slice-driver: %v\n", err)
	}
	if driver.failed || err != nil {
		os.Exit(1)
	}
}

type driver struct {
	out        io.Writer
	failed     bool
	abilitySeq uint64
}

func (driver *driver) pass(step string, format string, arguments ...any) {
	_, _ = fmt.Fprintf(driver.out, "PASS %-8s %s\n", step, fmt.Sprintf(format, arguments...))
}

func (driver *driver) fail(step string, format string, arguments ...any) {
	driver.failed = true
	_, _ = fmt.Fprintf(driver.out, "FAIL %-8s %s\n", step, fmt.Sprintf(format, arguments...))
}

// runOptions is one run's whole configuration. It is a struct rather than eight
// positional strings because the eighth one was where the mistake was going to
// be.
type runOptions struct {
	address               string
	packPath              string
	zoneID                string
	targetMob             string
	abilityID             string
	ticket                string
	questID               string
	giverMob              string
	skipUnsupportedQuests bool
}

func (driver *driver) run(ctx context.Context, options runOptions) error {
	address, packPath, zoneID := options.address, options.packPath, options.zoneID
	targetMob, abilityID, ticket := options.targetMob, options.abilityID, options.ticket
	// packID is what this run claims in its ClientHello. The in-process shard
	// states the digest of the pack it loaded and refuses a client that names a
	// different one, so the slice exercises the ADR 0027 gate rather than
	// stepping around it. Against an external shard it stays empty, which that
	// shard accepts only under content.allow_unverified_pack.
	var packID string
	killLimit := 1
	var itemObjectiveIndexes []uint32
	if address == "" {
		hosted, err := startInProcessShard(
			ctx, packPath, zoneID, targetMob, options.giverMob, options.questID,
			options.skipUnsupportedQuests,
		)
		if err != nil {
			return err
		}
		address, zoneID, ticket, packID = hosted.address, hosted.zoneID, hosted.ticket, hosted.packID
		killLimit = hosted.killLimit
		itemObjectiveIndexes = hosted.itemObjectiveIndexes
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
		PackID:          packID,
		Ticket:          ticket,
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		driver.fail("connect", "handshake: %v", err)
		return nil
	}
	driver.pass("connect", "handshake ok, datagrams=%t server_pack=%q",
		connection.SupportsUnreliable(), hello.GetPackId())

	entered, err := client.EnterZone(connection, zoneID)
	if err != nil {
		driver.fail("enter", "enter zone %s: %v", zoneID, err)
		return nil
	}
	driver.pass("enter", "zone=%s entity=%d", entered.GetZoneId(), entered.GetOwnEntityId())

	// The quest is accepted before anything dies, because kill credit only
	// reaches an instance that is already accepted (mechanics/quests.md rule
	// 5.4.3).
	giver, ok := driver.acceptQuest(ctx, client, connection, options, itemObjectiveIndexes)
	if !ok {
		return nil
	}

	var totalCasts, totalRefusals, totalDamage int
	var usedAbility string
	var firstVictimID uint64
	killedEntityIDs := make(map[uint64]struct{}, killLimit)
	for killNumber := 1; killNumber <= killLimit; killNumber++ {
		target, err := findTarget(ctx, client, connection, targetMob, killedEntityIDs)
		if err != nil {
			driver.fail("target", "%v", err)
			return nil
		}
		if killNumber == 1 {
			driver.pass("target", "%s entity=%d level=%d health=%d/%d",
				target.GetContentId(), target.GetEntityId(), target.GetLevel(),
				target.GetHealth(), target.GetMaxHealth())
		}

		report, err := driver.castUntilDead(client, connection, target, abilityID)
		if err != nil {
			driver.fail("cast", "%v", err)
			return nil
		}
		totalCasts += report.casts
		totalRefusals += report.refusals
		totalDamage += int(report.totalDamage)
		usedAbility = report.abilityID
		switch {
		case report.death == nil:
			driver.fail("kill", "%s survived %d casts", target.GetContentId(), report.casts)
			return nil
		case report.death.GetKillerEntityId() != entered.GetOwnEntityId():
			driver.fail("kill", "kill credit went to entity %d, not to this session's %d",
				report.death.GetKillerEntityId(), entered.GetOwnEntityId())
			return nil
		case report.totalDamage < target.GetMaxHealth() ||
			report.totalDamage-report.damagePerCast >= target.GetMaxHealth():
			driver.fail("kill", "%d damage in %d-damage casts against a %d health pool",
				report.totalDamage, report.damagePerCast, target.GetMaxHealth())
			return nil
		}
		driver.pass("kill", "%d/%d %s entity=%d died to %d reported damage",
			killNumber, killLimit, targetMob, report.death.GetVictimEntityId(), report.totalDamage)
		if killNumber == 1 {
			firstVictimID = report.death.GetVictimEntityId()
		}
		killedEntityIDs[report.death.GetVictimEntityId()] = struct{}{}
	}
	driver.pass("cast", "%d casts of %s across %d target(s), %d refused for cooldown",
		totalCasts, usedAbility, killLimit, totalRefusals)
	driver.pass("damage", "%d %s target(s) took %d reported damage",
		killLimit, targetMob, totalDamage)
	driver.lootTheCorpse(ctx, client, connection, firstVictimID)
	driver.turnInQuest(client, connection, options.questID, giver)

	if err := client.Logout(connection); err != nil {
		driver.fail("logout", "%v", err)
		return nil
	}
	driver.pass("logout", "clean exit requested")
	return nil
}

// acceptQuest plays the accept half of mechanics/quests.md: walk up to the
// giver, interact to see what it offers, and take the one named on the command
// line.
//
// It asserts the answer's shape rather than a particular quest. Which quests an
// NPC offers is content, and a driver that insisted on one would fail the day a
// row was added — which is the opposite of what this step is for.
func (driver *driver) acceptQuest(
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
	options runOptions,
	itemObjectiveIndexes []uint32,
) (uint64, bool) {
	giver, err := findTarget(ctx, client, connection, options.giverMob, nil)
	if err != nil {
		driver.fail("accept", "no quest giver %s in a snapshot: %v", options.giverMob, err)
		return 0, false
	}
	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{
			Interact: &sarnautv1.Interact{TargetEntityId: giver.GetEntityId()},
		},
	}); err != nil {
		driver.fail("accept", "send interact: %v", err)
		return 0, false
	}
	offer, err := awaitQuestUpdate(client, connection, options.questID)
	if err != nil {
		driver.fail("accept", "%v", err)
		return 0, false
	}
	if offer.GetState() != sarnautv1.QuestState_QUEST_STATE_OFFERED {
		driver.fail("accept", "%s is %s at the giver, want offered", options.questID, offer.GetState())
		return 0, false
	}

	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestAccept{
			QuestAccept: &sarnautv1.QuestAccept{
				QuestId:         options.questID,
				StarterEntityId: giver.GetEntityId(),
			},
		},
	}); err != nil {
		driver.fail("accept", "send quest accept: %v", err)
		return 0, false
	}
	accepted, err := awaitQuestUpdate(client, connection, options.questID)
	if err != nil {
		driver.fail("accept", "%v", err)
		return 0, false
	}
	if accepted.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_NONE {
		driver.fail("accept", "%s refused: %s", options.questID, accepted.GetRefusal())
		return 0, false
	}
	driver.pass("accept", "%s from %s entity=%d, state=%s, %d objective(s)",
		options.questID, options.giverMob, giver.GetEntityId(),
		accepted.GetState(), len(accepted.GetObjectives()))
	if len(itemObjectiveIndexes) > 0 {
		progress, err := completedObjectiveProgress(accepted, itemObjectiveIndexes)
		if err != nil {
			driver.fail("item", "%v", err)
			return 0, false
		}
		driver.pass("item", "%s", progress)
	}
	return giver.GetEntityId(), true
}

func completedObjectiveProgress(update *sarnautv1.QuestStateUpdate, indexes []uint32) (string, error) {
	objectives := make(map[uint32]*sarnautv1.QuestObjectiveProgress, len(update.GetObjectives()))
	for _, objective := range update.GetObjectives() {
		objectives[objective.GetIndex()] = objective
	}
	progress := make([]string, 0, len(indexes))
	for _, index := range indexes {
		objective := objectives[index]
		if objective == nil {
			return "", fmt.Errorf("quest %s omitted item objective %d from its accepted state", update.GetQuestId(), index)
		}
		if objective.GetCounter() < objective.GetLimit() {
			return "", fmt.Errorf("quest %s item objective %d is %d/%d after inventory preload",
				update.GetQuestId(), index, objective.GetCounter(), objective.GetLimit())
		}
		progress = append(progress, fmt.Sprintf("objective %d=%d/%d",
			index, objective.GetCounter(), objective.GetLimit()))
	}
	return strings.Join(progress, ", ") + " from starting inventory", nil
}

// turnInQuest plays the other half: hand the quest back and read what the grant
// committed.
//
// The kill that satisfied the objective happened several steps ago, so the
// completion update is already in the stream ahead of the answer to this verb.
// What it waits for is the terminal state, not the next frame.
func (driver *driver) turnInQuest(
	client session.Client,
	connection transport.Connection,
	questID string,
	giverEntityID uint64,
) {
	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestTurnIn{
			QuestTurnIn: &sarnautv1.QuestTurnIn{QuestId: questID, FinisherEntityId: giverEntityID},
		},
	}); err != nil {
		driver.fail("turnin", "send quest turn in: %v", err)
		return
	}
	for attempt := 0; attempt < 64; attempt++ {
		update, err := awaitQuestUpdate(client, connection, questID)
		if err != nil {
			driver.fail("turnin", "%v", err)
			return
		}
		if update.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_NONE {
			driver.fail("turnin", "%s refused: %s", questID, update.GetRefusal())
			return
		}
		if update.GetState() != sarnautv1.QuestState_QUEST_STATE_TURNED_IN {
			continue
		}
		var granted int32
		for _, item := range update.GetItems() {
			granted += item.GetCount()
		}
		driver.pass("turnin", "%s at entity=%d: %d experience, %d money, %d honor, %d item unit(s)",
			questID, giverEntityID, update.GetExperience(), update.GetMoney(),
			update.GetHonor(), granted)
		return
	}
	driver.fail("turnin", "%s never reached turned-in", questID)
}

// awaitQuestUpdate reads the reliable stream until one quest is spoken about.
// Combat events, loot answers and inventory updates share the channel, and
// which arrives first is not something a smoke test should assert.
func awaitQuestUpdate(
	client session.Client,
	connection transport.Connection,
	questID string,
) (*sarnautv1.QuestStateUpdate, error) {
	for attempt := 0; attempt < 256; attempt++ {
		message, err := client.ReadReliableMessage(connection)
		if err != nil {
			return nil, fmt.Errorf("read quest update: %w", err)
		}
		if update := message.GetQuestStateUpdate(); update != nil && update.GetQuestId() == questID {
			return update, nil
		}
	}
	return nil, fmt.Errorf("no update for %s arrived on the reliable stream", questID)
}

// lootTheCorpse plays mechanics/loot.md rule 5.6 the way a client does: find
// the corpse container in a snapshot, ask what is on it, take it, and check
// that the inventory update agrees with the result.
//
// It asserts the shape of the answer, not a particular drop. The corpse's
// contents are a roll against a seed the shard chose, and a driver that
// insisted on one item would fail the day the seed or the table changed, which
// is exactly the kind of false alarm a smoke test must not produce.
func (driver *driver) lootTheCorpse(
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
	victimEntityID uint64,
) {
	corpse, ok := findCorpse(ctx, client, connection, victimEntityID)
	if !ok {
		// A table that rolled nothing stands no container up, which is correct
		// behaviour and not a failing step.
		driver.pass("loot", "the corpse of entity %d carried no drop; nothing to take", victimEntityID)
		return
	}

	offer, err := requestLootOffer(client, connection, corpse)
	if err != nil {
		driver.fail("loot", "%v", err)
		return
	}

	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: corpse},
		},
	}); err != nil {
		driver.fail("loot", "send loot take: %v", err)
		return
	}

	var (
		result *sarnautv1.LootResult
		update *sarnautv1.InventoryUpdate
	)
	for result == nil || update == nil {
		message, err := client.ReadReliableMessage(connection)
		if err != nil {
			driver.fail("loot", "read loot answer: %v", err)
			return
		}
		if seen := message.GetLootResult(); seen != nil {
			result = seen
			if result.GetRefusal() != sarnautv1.LootRefusal_LOOT_REFUSAL_NONE {
				driver.fail("loot", "take refused: %s", result.GetRefusal())
				return
			}
			continue
		}
		if seen := message.GetInventoryUpdate(); seen != nil {
			update = seen
		}
	}

	var taken, held int32
	heldByItem := make(map[string]int32)
	for _, item := range result.GetItems() {
		taken += item.GetCount()
	}
	for _, slot := range update.GetSlots() {
		held += slot.GetCount()
		heldByItem[slot.GetItemId()] += slot.GetCount()
	}
	for _, item := range result.GetItems() {
		if heldByItem[item.GetItemId()] < item.GetCount() {
			driver.fail("loot", "took %d of %s but the bag holds %d",
				item.GetCount(), item.GetItemId(), heldByItem[item.GetItemId()])
			return
		}
	}
	if int64(len(offer.GetItems())) != int64(len(result.GetItems())) {
		driver.fail("loot", "the corpse offered %d grants and the take produced %d",
			len(offer.GetItems()), len(result.GetItems()))
		return
	}
	if offer.GetMoney() != result.GetMoney() {
		driver.fail("loot", "the corpse offered %d money and the take produced %d",
			offer.GetMoney(), result.GetMoney())
		return
	}
	// The character started with an empty purse, so the credit and the balance
	// are the same number. This is the assertion that holds on every run: the
	// item grants of the M2 target's tree are chance-gated and the money leaf
	// is not, so a step that only checked items would sometimes check nothing.
	if update.GetCurrency() != result.GetMoney() {
		driver.fail("loot", "took %d money but the purse holds %d",
			result.GetMoney(), update.GetCurrency())
		return
	}
	if result.GetMoney() == 0 && len(result.GetItems()) == 0 {
		driver.fail("loot", "the corpse stood up holding nothing; an empty drop gets no container")
		return
	}
	driver.pass("loot", "corpse=%d money=%d grants=%d -> %d taken units, %d total units in %d bag slots, purse=%d",
		corpse, result.GetMoney(), len(result.GetItems()), taken, held,
		len(update.GetSlots()), update.GetCurrency())
}

// findCorpse waits for the loot module's container to appear in a snapshot. It
// is a new entity carrying the victim's content id, no health and not alive,
// which is what a client renders as a lootable corpse.
func findCorpse(
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
	victimEntityID uint64,
) (uint64, bool) {
	deadline := time.Now().Add(corpseSearchWindow)
	for time.Now().Before(deadline) {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			return 0, false
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetEntityId() <= victimEntityID || entity.GetAlive() || entity.GetMaxHealth() != 0 {
				continue
			}
			return entity.GetEntityId(), true
		}
	}
	return 0, false
}

// requestLootOffer asks what is on a corpse with the generic interact verb.
func requestLootOffer(
	client session.Client,
	connection transport.Connection,
	corpse uint64,
) (*sarnautv1.LootOffer, error) {
	if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{
			Interact: &sarnautv1.Interact{TargetEntityId: corpse},
		},
	}); err != nil {
		return nil, fmt.Errorf("send interact: %w", err)
	}
	for {
		message, err := client.ReadReliableMessage(connection)
		if err != nil {
			return nil, fmt.Errorf("read loot offer: %w", err)
		}
		if offer := message.GetLootOffer(); offer != nil {
			return offer, nil
		}
		if result := message.GetLootResult(); result != nil {
			return nil, fmt.Errorf("the corpse refused to open: %s", result.GetRefusal())
		}
	}
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
	for attempt := 0; attempt < 500; attempt++ {
		driver.abilitySeq++
		if err := client.SendCommand(connection, &sarnautv1.ClientMessage{
			ClientSeq: driver.abilitySeq,
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
				if death.GetVictimEntityId() != target.GetEntityId() {
					continue
				}
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
				if event.GetKillingBlow() {
					// The death event follows the killing combat event. Read it
					// before sending another cast, or that extra command leaves a
					// target-dead rejection queued for the next mob.
					continue
				}
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
	excludedEntityIDs map[uint64]struct{},
) (*sarnautv1.EntitySnapshot, error) {
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			return nil, fmt.Errorf("read snapshot: %w", err)
		}
		if entity := liveTarget(snapshot.GetEntities(), contentID, excludedEntityIDs); entity != nil {
			return entity, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no snapshot carried a live %s", contentID)
		}
	}
}

func liveTarget(
	entities []*sarnautv1.EntitySnapshot,
	contentID string,
	excludedEntityIDs map[uint64]struct{},
) *sarnautv1.EntitySnapshot {
	for _, entity := range entities {
		if entity.GetContentId() != contentID || !entity.GetAlive() {
			continue
		}
		if _, excluded := excludedEntityIDs[entity.GetEntityId()]; excluded {
			continue
		}
		return entity
	}
	return nil
}

type hostedShard struct {
	address              string
	zoneID               string
	packID               string
	killLimit            int
	itemObjectiveIndexes []uint32
	// ticket is the single-use shard ticket the in-process authority minted for
	// this run. The shard admits nobody without one (ADR 0030).
	ticket string
}

// startInProcessShard is the same composition as `cmd/shard`, minus the
// telemetry, the health endpoint and the infrastructure clients, and with the
// player spawned next to the target so the driver has something to cast at
// without a pathfinder.
func startInProcessShard(
	ctx context.Context,
	packPath, zoneID, targetMob, giverMob, questID string,
	skipUnsupportedQuests bool,
) (hostedShard, error) {
	content, err := pack.Load(packPath, pack.Options{})
	if err != nil {
		return hostedShard{}, fmt.Errorf("load content pack %q: %w", packPath, err)
	}
	anchor, err := anchorOf(content, targetMob)
	if err != nil {
		return hostedShard{}, err
	}
	definition, ok := content.Quest(questID)
	if !ok {
		return hostedShard{}, fmt.Errorf("content pack %s carries no quest %q", content.ID(), questID)
	}
	requirements, err := requirementsFor(definition, targetMob)
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
	spawns := content.NPCSpawns()
	if err := coLocateGiver(spawns, giverMob, anchor); err != nil {
		return hostedShard{}, err
	}
	spawns, err = coLocateTargets(spawns, targetMob, anchor, requirements.killLimit)
	if err != nil {
		return hostedShard{}, err
	}
	if err := combatModule.Populate(spawns); err != nil {
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
	//
	// One repository serves both the checkpoint path and the bag, exactly as
	// `cmd/shard` composes them: the loot award and the periodic save are two
	// writers on one character row, and running them against two stores would
	// hide the thing this driver is meant to smoke out.
	repository := charstore.NewMemory()
	worker := charstore.NewSaveWorker(repository, logger, 0, 0)
	characters := charstore.NewCharacterService(
		repository,
		sliceTemplates{spawn: charstore.Vec3{
			X: anchor.X + castDistance,
			Y: anchor.Y,
			Z: anchor.Z,
		}, inventory: requirements.inventory},
		worker,
		logger,
		0,
	)
	bags, err := charstore.NewInventoryService(repository, inventory.LimitsFromPack(content))
	if err != nil {
		return hostedShard{}, err
	}
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		return hostedShard{}, fmt.Errorf("read loot rules: %w", err)
	}
	lootModule := loot.New(logger, zone, lootRules, bags, loot.Options{WorldSeed: "m2-slice-driver"})
	// Quests read the same pack and grant through the same bag as loot. The
	// default fixture remains fail-fast; real packs skip future objective kinds
	// and log every omitted definition before the driver connects.
	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{
		SkipUnsupportedQuests: skipUnsupportedQuests,
		Logger:                logger,
	})
	if err != nil {
		return hostedShard{}, fmt.Errorf("read quests: %w", err)
	}
	questModule := quests.New(logger, zone, catalog, bags)
	authority := newSliceAuthority()

	binding := session.ZoneBinding{
		World:  zone,
		Combat: combatModule,
		Loot:   lootModule,
		Quests: questModule,
	}
	// One death, a corpse and a counter. The fan-out is the composition's, not
	// combat's: neither consumer knows the other exists.
	combatModule.SetKillSink(binding.KillSink())

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "m2-slice-driver",
		PackID:          content.ID(),
		Zones:           map[string]session.ZoneBinding{zone.ID(): binding},
		Authority:       authority,
		Characters:      characters,
		Logger:          logger,
	}

	go zone.Run(ctx)
	go combatModule.Run(ctx)
	go questModule.Run(ctx)
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
		address:              listener.Addr().String(),
		zoneID:               zone.ID(),
		packID:               content.ID(),
		ticket:               authority.ticket,
		killLimit:            requirements.killLimit,
		itemObjectiveIndexes: append([]uint32(nil), requirements.itemObjectiveIndexes...),
	}, nil
}

// sliceTemplates materializes the driver's character next to the target.
//
// `cmd/shard` reads the spawn out of the pack's chargen row; the driver
// overrides it for the same reason it overrides the zone's player spawn, which
// is that the worked example starts six metres from the mob and the driver has
// no pathfinder to walk there.
type sliceTemplates struct {
	spawn     charstore.Vec3
	inventory []charstore.InventoryItem
}

func (templates sliceTemplates) Template(string) (charstore.Snapshot, bool) {
	return charstore.Snapshot{
		State:     charstore.CharacterState{Position: templates.spawn, Level: 1, Health: 100},
		Inventory: append([]charstore.InventoryItem(nil), templates.inventory...),
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

// coLocateGiver places one authored giver within both interaction range of the
// player and replication range of the selected target. The driver already
// overrides the player spawn because it has no pathfinder; shifting this one
// copied spawn keeps that test-only geometry coherent after spatial AoI.
func coLocateGiver(spawns []pack.NPCSpawn, giverMob string, anchor world.Vec3) error {
	for index := range spawns {
		if spawns[index].MobID != giverMob {
			continue
		}
		spawns[index].Position = pack.Vec3{
			X: anchor.X + castDistance/2,
			Y: anchor.Y,
			Z: anchor.Z,
		}
		return nil
	}
	return fmt.Errorf("content pack has no live placement spawning quest giver %q", giverMob)
}

type sliceRequirements struct {
	killLimit            int
	inventory            []charstore.InventoryItem
	itemObjectiveIndexes []uint32
}

func requirementsFor(definition pack.Quest, targetMob string) (sliceRequirements, error) {
	requirements := sliceRequirements{}
	quantities := make(map[string]int32)
	for index, objective := range definition.Objectives {
		switch objective.Kind {
		case pack.QuestObjectiveCountKill:
			if containsID(objective.TargetIDs, targetMob) && int(objective.Limit) > requirements.killLimit {
				requirements.killLimit = int(objective.Limit)
			}
		case pack.QuestObjectiveCountItem:
			if objective.Limit > 0 && len(objective.TargetIDs) > 0 {
				quantities[objective.TargetIDs[0]] += objective.Limit
				requirements.itemObjectiveIndexes = append(
					requirements.itemObjectiveIndexes, uint32(index),
				)
			}
		}
	}
	if requirements.killLimit < 1 {
		return sliceRequirements{}, fmt.Errorf(
			"quest %q has no positive count-kill objective targeting %q",
			definition.ID, targetMob,
		)
	}
	itemIDs := make([]string, 0, len(quantities))
	for id := range quantities {
		itemIDs = append(itemIDs, id)
	}
	sort.Strings(itemIDs)
	for slot, id := range itemIDs {
		requirements.inventory = append(requirements.inventory, charstore.InventoryItem{
			Slot: int32(slot), ItemID: id, Quantity: quantities[id],
		})
	}
	return requirements, nil
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func coLocateTargets(
	spawns []pack.NPCSpawn,
	targetMob string,
	anchor world.Vec3,
	count int,
) ([]pack.NPCSpawn, error) {
	indices := make([]int, 0, count)
	for index := range spawns {
		if spawns[index].MobID == targetMob {
			indices = append(indices, index)
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("content pack has no live placement spawning target %q", targetMob)
	}
	base := spawns[indices[0]]
	for len(indices) < count {
		copy := base
		copy.PlacementID = fmt.Sprintf("%s.driver-%d", base.PlacementID, len(indices)+1)
		spawns = append(spawns, copy)
		indices = append(indices, len(spawns)-1)
	}
	for _, index := range indices[:count] {
		spawns[index].Position = pack.Vec3(anchor)
	}
	return spawns, nil
}
