package session_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

// The quest slice's content. Every id here is a row in `data-schemas/demo`, and
// the placement geometry is authored too: the quest giver stands three metres
// from where a character materializes and six from the crab, so the accepted
// turn-in and the out-of-range refusal both come out of the fixture rather than
// out of a number in this file.
const (
	questGiverMob = "mob.paper-harbor.harbor-quartermaster"
	questTargetID = "mob.paper-harbor.tide-crab"
	questSliceID  = "quest.paper-harbor.tide-tally"
	questTicket   = "sarnaut_tk_quest_slice"
)

var questCharacter = uuid.MustParse("019200f0-0000-7000-8000-0000000d0001")

// TestQuestSliceOverQUIC is the whole server-side M2 chain through the wire:
// enter, interact with a quest giver, accept, kill the objective mob, loot it,
// turn in, and read the rewards.
//
// The unit tests in `internal/quests` call the module directly and know nothing
// about protobuf. This one is the only place the round trip is exercised — the
// dispatch of the three quest verbs, the mapping in both directions, the
// interact verb resolving to a quest giver rather than to a corpse, and the
// ordering that puts the committed state update ahead of the inventory update.
func TestQuestSliceOverQUIC(t *testing.T) {
	t.Parallel()

	fixture := newQuestFixture(t)
	defer fixture.stop()

	player := fixture.connect(t)
	giver := findEntityInSnapshots(t, fixture.ctx, player, questGiverMob)

	// interact resolves against a live NPC, and the answer is what this
	// character may take from it right now.
	offers := questInteract(t, player, giver.GetEntityId())
	offered, ok := offers[questSliceID]
	if !ok || offered.GetState() != sarnautv1.QuestState_QUEST_STATE_OFFERED {
		t.Fatalf("%s is %v, want offered", questSliceID, offered.GetState())
	}
	if vigil := offers["quest.paper-harbor.harbor-vigil"]; vigil.GetState() !=
		sarnautv1.QuestState_QUEST_STATE_UNAVAILABLE {
		t.Errorf("a level-5 quest is %v to a level-1 character, want unavailable", vigil.GetState())
	}

	// Accept.
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestAccept{
			QuestAccept: &sarnautv1.QuestAccept{QuestId: questSliceID, StarterEntityId: giver.GetEntityId()},
		},
	}); err != nil {
		t.Fatalf("SendCommand(quest_accept) error = %v", err)
	}
	accepted := awaitQuestUpdate(t, player, questSliceID)
	if accepted.GetState() != sarnautv1.QuestState_QUEST_STATE_ACCEPTED {
		t.Fatalf("state = %v after accepting, want accepted", accepted.GetState())
	}
	if len(accepted.GetObjectives()) != 1 || accepted.GetObjectives()[0].GetCounter() != 0 {
		t.Errorf("objectives = %+v, want one counter at zero", accepted.GetObjectives())
	}
	// The accept is committed before the client is told, so the store already
	// agrees (protocol/session.md rule 5.7.4's reasoning, applied to a quest).
	if state := fixture.storedQuestState(t, questSliceID); state != "accepted" {
		t.Errorf("stored state = %q, want accepted", state)
	}

	// Turning in early is refused, and refusing is ordinary play: the session
	// lives on and the instance is untouched.
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestTurnIn{
			QuestTurnIn: &sarnautv1.QuestTurnIn{QuestId: questSliceID, FinisherEntityId: giver.GetEntityId()},
		},
	}); err != nil {
		t.Fatalf("SendCommand(early quest_turn_in) error = %v", err)
	}
	early := awaitQuestRefusal(t, player, questSliceID)
	if early.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_NOT_COMPLETE {
		t.Fatalf("early turn-in answered %v, want NOT_COMPLETE", early.GetRefusal())
	}

	// Kill the objective mob. The credit arrives through combat's MobKilled
	// event.
	target := findEntityInSnapshots(t, fixture.ctx, player, questTargetID)
	death := castUntilDead(t, player, target)
	if death.GetKillerEntityId() != player.entityID {
		t.Fatalf("kill credit went to entity %d, want %d", death.GetKillerEntityId(), player.entityID)
	}

	// The counter update is pushed unasked, and it races the death event: the
	// two are published by independent fan-out goroutines and nothing orders
	// them, so a reader looking for a death can legitimately consume the quest
	// update on the way past. That the push happens at all is asserted in
	// `internal/quests`, against the module rather than against a scheduler.
	// What is deterministic here is asking, which is also what a client does
	// when it walks back to the giver.
	completed := askTheGiver(t, player, giver.GetEntityId(), questSliceID)
	if completed.GetState() != sarnautv1.QuestState_QUEST_STATE_COMPLETABLE {
		t.Fatalf("state = %v after the kill, want completable", completed.GetState())
	}
	if got := completed.GetObjectives()[0].GetCounter(); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}

	// Loot the corpse on the way past. The interact verb has to resolve it as a
	// corpse and not as the quest giver whose content id it carries.
	corpseID := findCorpseInSnapshots(t, fixture.ctx, player, target.GetEntityId(), questTargetID)
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{Interact: &sarnautv1.Interact{TargetEntityId: corpseID}},
	}); err != nil {
		t.Fatalf("SendCommand(interact corpse) error = %v", err)
	}
	if offer := awaitLootOffer(t, player); offer.GetCorpseEntityId() != corpseID {
		t.Fatalf("the corpse answered for entity %d, want %d", offer.GetCorpseEntityId(), corpseID)
	}
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_LootTake{LootTake: &sarnautv1.LootTake{CorpseEntityId: corpseID}},
	}); err != nil {
		t.Fatalf("SendCommand(loot_take) error = %v", err)
	}
	if result := awaitLootResult(t, player); result.GetRefusal() != sarnautv1.LootRefusal_LOOT_REFUSAL_NONE {
		t.Fatalf("loot take answered %v, want NONE", result.GetRefusal())
	}
	awaitInventoryUpdate(t, player)

	// Turn in.
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestTurnIn{
			QuestTurnIn: &sarnautv1.QuestTurnIn{QuestId: questSliceID, FinisherEntityId: giver.GetEntityId()},
		},
	}); err != nil {
		t.Fatalf("SendCommand(quest_turn_in) error = %v", err)
	}
	turnedIn := awaitQuestState(t, player, questSliceID, sarnautv1.QuestState_QUEST_STATE_TURNED_IN)
	if turnedIn.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_NONE {
		t.Fatalf("turn-in answered %v, want NONE", turnedIn.GetRefusal())
	}
	if turnedIn.GetExperience() == 0 || len(turnedIn.GetItems()) == 0 {
		t.Errorf("the turn-in reported experience %d and %d items, want the content's rewards",
			turnedIn.GetExperience(), len(turnedIn.GetItems()))
	}
	bag := awaitInventoryUpdate(t, player)

	// What the client was told is what the store already holds.
	state := fixture.storedCharacterState(t)
	if state.Experience != turnedIn.GetExperience() {
		t.Errorf("stored experience = %d, the client was told %d", state.Experience, turnedIn.GetExperience())
	}
	if state.Currency != bag.GetCurrency() {
		t.Errorf("stored currency = %d, the client was told %d", state.Currency, bag.GetCurrency())
	}
	if stored := fixture.storedQuestState(t, questSliceID); stored != "turned-in" {
		t.Errorf("stored quest state = %q, want turned-in", stored)
	}
	rewarded := map[string]int32{}
	for _, slot := range bag.GetSlots() {
		rewarded[slot.GetItemId()] += slot.GetCount()
	}
	for _, item := range turnedIn.GetItems() {
		if rewarded[item.GetItemId()] < item.GetCount() {
			t.Errorf("the bag holds %d of %s, want at least the %d granted",
				rewarded[item.GetItemId()], item.GetItemId(), item.GetCount())
		}
	}

	// A second turn-in grants nothing. The store is the assertion, not the
	// answer.
	if err := player.client.SendCommand(player.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_QuestTurnIn{
			QuestTurnIn: &sarnautv1.QuestTurnIn{QuestId: questSliceID, FinisherEntityId: giver.GetEntityId()},
		},
	}); err != nil {
		t.Fatalf("SendCommand(duplicate quest_turn_in) error = %v", err)
	}
	duplicate := awaitQuestRefusal(t, player, questSliceID)
	if duplicate.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_ALREADY_COMPLETE {
		t.Errorf("duplicate turn-in answered %v, want ALREADY_COMPLETE", duplicate.GetRefusal())
	}
	if after := fixture.storedCharacterState(t); after.Experience != state.Experience {
		t.Errorf("experience went from %d to %d across a duplicate turn-in",
			state.Experience, after.Experience)
	}
}

// questFixture is a zone with combat, loot and quests behind a real QUIC
// listener, composed exactly the way `cmd/shard` composes it — including the
// kill fan-out, which is the one piece of wiring that has to exist for a kill to
// reach both a corpse and a counter.
type questFixture struct {
	ctx        context.Context
	cancel     context.CancelFunc
	address    string
	zoneID     string
	repository store.Repository
	serve      chan error
	listener   transport.Listener
}

func newQuestFixture(t *testing.T) *questFixture {
	t.Helper()

	content := loadFixturePack(t)
	anchor, ok := placementOf(content, questTargetID)
	if !ok {
		t.Fatalf("the fixture pack has no placement for %q", questTargetID)
	}
	spawn := anchor.Add(world.Vec3{X: 6})

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "QuestSliceZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
		PlayerSpawn:      spawn,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	combatRules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("combat.RulesFromPack() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, combatRules, combat.Options{})
	if err := combatModule.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	repository := store.NewMemory()
	bags, err := inventory.NewService(repository, inventory.LimitsFromPack(content), 0)
	if err != nil {
		t.Fatalf("inventory.NewService() error = %v", err)
	}
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("loot.RulesFromPack() error = %v", err)
	}
	lootModule := loot.New(slog.New(slog.DiscardHandler), zone, lootRules, bags, loot.Options{
		WorldSeed: "quest-slice-test",
	})
	catalog, err := quests.CatalogFromPack(content)
	if err != nil {
		t.Fatalf("quests.CatalogFromPack() error = %v", err)
	}
	questModule := quests.New(slog.New(slog.DiscardHandler), zone, catalog, bags)

	binding := session.ZoneBinding{
		World:  zone,
		Combat: combatModule,
		Loot:   lootModule,
		Quests: questModule,
	}
	combatModule.SetKillSink(binding.KillSink())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	go zone.Run(ctx)
	go combatModule.Run(ctx)
	go questModule.Run(ctx)

	serverTLS, err := transport.NewDevServerTLSConfig()
	if err != nil {
		cancel()
		t.Fatalf("NewDevServerTLSConfig() error = %v", err)
	}
	listener, err := transport.ListenQUIC("127.0.0.1:0", serverTLS)
	if err != nil {
		cancel()
		t.Fatalf("ListenQUIC() error = %v", err)
	}

	worker := store.NewSaveWorker(repository, slog.New(slog.DiscardHandler), 0, 0)
	go worker.Run(ctx)
	characters := store.NewCharacterService(
		repository,
		questTemplates{spawn: store.Vec3{X: spawn.X, Y: spawn.Y, Z: spawn.Z}},
		worker,
		slog.New(slog.DiscardHandler),
		0,
	)

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "quest-slice-test",
		Zones:           map[string]session.ZoneBinding{zone.ID(): binding},
		Authority:       &questAuthority{},
		Characters:      characters,
		Logger:          slog.New(slog.DiscardHandler),
	}
	serve := make(chan error, 1)
	go func() { serve <- server.Serve(ctx, listener) }()

	return &questFixture{
		ctx:        ctx,
		cancel:     cancel,
		address:    listener.Addr().String(),
		zoneID:     zone.ID(),
		repository: repository,
		serve:      serve,
		listener:   listener,
	}
}

func (fixture *questFixture) stop() {
	fixture.cancel()
	_ = fixture.listener.Close()
	select {
	case <-fixture.serve:
	case <-time.After(5 * time.Second):
	}
}

func (fixture *questFixture) connect(t *testing.T) *lootSession {
	t.Helper()
	connection, err := transport.DialQUIC(fixture.ctx, fixture.address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "quest-slice-client",
		Ticket:          questTicket,
	}
	if _, err := client.Handshake(fixture.ctx, connection); err != nil {
		t.Fatalf("Handshake() error = %v", err)
	}
	entered, err := client.EnterZone(connection, fixture.zoneID)
	if err != nil {
		t.Fatalf("EnterZone() error = %v", err)
	}
	return &lootSession{client: client, connection: connection, entityID: entered.GetOwnEntityId()}
}

func (fixture *questFixture) storedQuestState(t *testing.T, questID string) string {
	t.Helper()
	rows, err := fixture.repository.LoadQuestStates(fixture.ctx, questCharacter)
	if err != nil {
		t.Fatalf("LoadQuestStates() error = %v", err)
	}
	for _, row := range rows {
		if row.QuestID == questID {
			return row.State
		}
	}
	return ""
}

func (fixture *questFixture) storedCharacterState(t *testing.T) store.CharacterState {
	t.Helper()
	state, err := fixture.repository.LoadCharacterState(fixture.ctx, questCharacter)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	return state
}

// questInteract sends one interact and collects every quest update that comes
// back, keyed by quest id. The NPC offers several, and which arrives first is
// not something to assert on.
func questInteract(t *testing.T, actor *lootSession, targetEntityID uint64) map[string]*sarnautv1.QuestStateUpdate {
	t.Helper()
	if err := actor.client.SendCommand(actor.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{Interact: &sarnautv1.Interact{TargetEntityId: targetEntityID}},
	}); err != nil {
		t.Fatalf("SendCommand(interact) error = %v", err)
	}
	seen := make(map[string]*sarnautv1.QuestStateUpdate)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message, err := actor.client.ReadReliableMessage(actor.connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		update := message.GetQuestStateUpdate()
		if update == nil {
			continue
		}
		seen[update.GetQuestId()] = update
		if len(seen) >= 5 {
			return seen
		}
	}
	if len(seen) == 0 {
		t.Fatal("the quest giver answered nothing")
	}
	return seen
}

// askTheGiver interacts with an NPC and returns what it says about one quest.
//
// It is a request and a response, which is what makes it usable as an
// assertion: the pushed updates a session also receives are ordered against
// nothing.
func askTheGiver(
	t *testing.T,
	actor *lootSession,
	giverEntityID uint64,
	questID string,
) *sarnautv1.QuestStateUpdate {
	t.Helper()
	if err := actor.client.SendCommand(actor.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{
			Interact: &sarnautv1.Interact{TargetEntityId: giverEntityID},
		},
	}); err != nil {
		t.Fatalf("SendCommand(interact) error = %v", err)
	}
	return awaitQuestUpdate(t, actor, questID)
}

func awaitQuestUpdate(t *testing.T, actor *lootSession, questID string) *sarnautv1.QuestStateUpdate {
	t.Helper()
	for {
		message, err := actor.client.ReadReliableMessage(actor.connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if update := message.GetQuestStateUpdate(); update != nil && update.GetQuestId() == questID {
			return update
		}
	}
}

// awaitQuestRefusal drains until the named quest is refused something. A push
// that was still in flight is not an answer to the verb just sent.
func awaitQuestRefusal(t *testing.T, actor *lootSession, questID string) *sarnautv1.QuestStateUpdate {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		update := awaitQuestUpdate(t, actor, questID)
		if update.GetRefusal() != sarnautv1.QuestRefusal_QUEST_REFUSAL_NONE {
			return update
		}
	}
	t.Fatalf("%s was never refused", questID)
	return nil
}

// awaitQuestState drains until the named quest reports the state asked for. A
// counter update and a completion can arrive as two frames, and which one a
// caller sees first is a scheduling detail.
func awaitQuestState(
	t *testing.T,
	actor *lootSession,
	questID string,
	want sarnautv1.QuestState,
) *sarnautv1.QuestStateUpdate {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		update := awaitQuestUpdate(t, actor, questID)
		if update.GetState() == want {
			return update
		}
	}
	t.Fatalf("%s never reached %v", questID, want)
	return nil
}

func findEntityInSnapshots(
	t *testing.T,
	ctx context.Context,
	actor *lootSession,
	contentID string,
) *sarnautv1.EntitySnapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := actor.client.ReadSnapshot(ctx, actor.connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetContentId() == contentID && entity.GetAlive() {
				return entity
			}
		}
	}
	t.Fatalf("no snapshot carried a live %s", contentID)
	return nil
}

func placementOf(content *pack.Pack, mobID string) (world.Vec3, bool) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID == mobID {
			return world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}, true
		}
	}
	return world.Vec3{}, false
}

// findCorpseInSnapshots waits for the container the loot module stood up over
// one dead entity: a new entity carrying the victim's content id, not alive and
// with no health.
func findCorpseInSnapshots(
	t *testing.T,
	ctx context.Context,
	actor *lootSession,
	victimID uint64,
	contentID string,
) uint64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := actor.client.ReadSnapshot(ctx, actor.connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetEntityId() == victimID || entity.GetContentId() != contentID {
				continue
			}
			if entity.GetMaxHealth() == 0 && !entity.GetAlive() {
				return entity.GetEntityId()
			}
		}
	}
	t.Fatal("no corpse container appeared in a snapshot")
	return 0
}

// questAuthority redeems the one ticket this test presents, twice: the
// persistence case reconnects.
type questAuthority struct {
	mu       sync.Mutex
	redeemed int
}

func (authority *questAuthority) RedeemTicket(_ context.Context, ticket string) (session.Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if ticket != questTicket {
		return session.Admission{}, &session.Refusal{Reason: session.ReasonUnknownTicket}
	}
	authority.redeemed++
	return session.Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-0000000b0001"),
		CharacterID:     questCharacter,
		CharacterName:   "Quester",
		ChargenOptionID: "chargen.league.warrior",
	}, nil
}

func (authority *questAuthority) RenewPlayLock(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func (authority *questAuthority) ReleasePlayLock(context.Context, uuid.UUID) error { return nil }

// questTemplates materializes the character six metres from the crab, which
// puts it three from the quest giver.
type questTemplates struct {
	spawn store.Vec3
}

func (templates questTemplates) Template(string) (store.Snapshot, bool) {
	return store.Snapshot{
		State: store.CharacterState{Position: templates.spawn, Level: 1, Health: 100},
	}, true
}
