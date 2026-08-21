package session_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

// The loot target is the copper sparrow. Its table's first entry has a chance
// of 1.0 and a count range of 45 to 45, so a kill always drops exactly 45
// harbor tonics and the assertions below can be about a bag layout rather than
// about "something arrived".
const (
	lootMobID    = "mob.paper-harbor.copper-sparrow"
	lootItemID   = "item.consumable.harbor-tonic"
	lootDropSize = 45
)

// TestLootSliceOverQUIC drives the loot verbs the way a client does: kill,
// interact to see the offer, take, and read the result and the inventory update
// off the reliable stream.
//
// The unit tests in `internal/loot` call the module directly. This one is the
// only place the wire round trip is exercised — the dispatch of `loot_take` and
// `interact`, the mapping in both directions, and the ordering that puts the
// committed result ahead of the inventory update.
func TestLootSliceOverQUIC(t *testing.T) {
	t.Parallel()

	fixture := newLootFixture(t)
	defer fixture.stop()

	owner := fixture.connect(t, ownerTicket)
	target := findLootTargetInSnapshots(t, fixture.ctx, owner.client, owner.connection)

	death := castUntilDead(t, owner, target)
	if death.GetKillerEntityId() != owner.entityID {
		t.Fatalf("kill credit went to entity %d, want this session's %d",
			death.GetKillerEntityId(), owner.entityID)
	}

	// The corpse container is a world entity of its own, standing where the mob
	// died. A client finds it the way it finds anything else: in a snapshot.
	corpseID := findCorpseInSnapshots(t, fixture.ctx, owner, target.GetEntityId(), lootMobID)

	// interact answers with what is on the corpse, before taking it.
	if err := owner.client.SendCommand(owner.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_Interact{
			Interact: &sarnautv1.Interact{TargetEntityId: corpseID},
		},
	}); err != nil {
		t.Fatalf("SendCommand(interact) error = %v", err)
	}
	offer := awaitLootOffer(t, owner)
	if offer.GetCorpseEntityId() != corpseID {
		t.Errorf("offer names corpse %d, want %d", offer.GetCorpseEntityId(), corpseID)
	}
	if len(offer.GetItems()) != 1 ||
		offer.GetItems()[0].GetItemId() != lootItemID ||
		offer.GetItems()[0].GetCount() != lootDropSize {
		t.Fatalf("offer.Items = %+v, want %d of %s", offer.GetItems(), lootDropSize, lootItemID)
	}

	// A second connected client cannot take the first player's loot.
	stranger := fixture.connect(t, strangerTicket)
	if err := stranger.client.SendCommand(stranger.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: corpseID},
		},
	}); err != nil {
		t.Fatalf("stranger SendCommand(loot_take) error = %v", err)
	}
	refused := awaitLootResult(t, stranger)
	if refused.GetRefusal() != sarnautv1.LootRefusal_LOOT_REFUSAL_NOT_YOUR_LOOT {
		t.Fatalf("stranger's take answered %s, want LOOT_REFUSAL_NOT_YOUR_LOOT", refused.GetRefusal())
	}
	if len(refused.GetItems()) != 0 || refused.GetMoney() != 0 {
		t.Errorf("a refusal carried %+v and %d money, want nothing", refused.GetItems(), refused.GetMoney())
	}
	// A refusal is ordinary play: the session is still up and still answering.
	if items, err := fixture.repository.LoadInventory(fixture.ctx, strangerCharacter); err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	} else if len(items) != 0 {
		t.Errorf("the stranger's bag holds %+v, want nothing", items)
	}

	// The owner's take succeeds and answers with the result and then the bag.
	if err := owner.client.SendCommand(owner.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: corpseID},
		},
	}); err != nil {
		t.Fatalf("SendCommand(loot_take) error = %v", err)
	}
	result := awaitLootResult(t, owner)
	if result.GetRefusal() != sarnautv1.LootRefusal_LOOT_REFUSAL_NONE {
		t.Fatalf("take answered %s, want LOOT_REFUSAL_NONE", result.GetRefusal())
	}
	if len(result.GetItems()) != 1 || result.GetItems()[0].GetCount() != lootDropSize {
		t.Errorf("result.Items = %+v, want %d of one item", result.GetItems(), lootDropSize)
	}

	update := awaitInventoryUpdate(t, owner)
	if len(update.GetSlots()) != 3 {
		t.Fatalf("inventory update carries %d slots, want 3 (worked example 6.2)", len(update.GetSlots()))
	}
	var total int32
	for _, slot := range update.GetSlots() {
		if slot.GetItemId() != lootItemID {
			t.Errorf("slot %d holds %q, want %q", slot.GetSlot(), slot.GetItemId(), lootItemID)
		}
		total += slot.GetCount()
	}
	if total != lootDropSize {
		t.Errorf("inventory update totals %d, want %d", total, lootDropSize)
	}

	// The award was committed before either answer was written, so the store
	// already agrees with what the client was told.
	stored, err := fixture.repository.LoadInventory(fixture.ctx, ownerCharacter)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(stored) != len(update.GetSlots()) {
		t.Fatalf("the store holds %d slots, the client was told %d", len(stored), len(update.GetSlots()))
	}
	for index, slot := range update.GetSlots() {
		if stored[index].Slot != slot.GetSlot() || stored[index].Quantity != slot.GetCount() {
			t.Errorf("slot %d: store has %+v, the client was told %+v", index, stored[index], slot)
		}
	}

	// Taking again is refused and yields nothing further.
	if err := owner.client.SendCommand(owner.connection, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: corpseID},
		},
	}); err != nil {
		t.Fatalf("SendCommand(second loot_take) error = %v", err)
	}
	second := awaitLootResult(t, owner)
	if second.GetRefusal() != sarnautv1.LootRefusal_LOOT_REFUSAL_ALREADY_LOOTED {
		t.Errorf("second take answered %s, want LOOT_REFUSAL_ALREADY_LOOTED", second.GetRefusal())
	}
	reloaded, err := fixture.repository.LoadInventory(fixture.ctx, ownerCharacter)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if got := lootUnits(reloaded); got != lootDropSize {
		t.Errorf("the bag holds %d after two takes, want exactly one drop of %d", got, lootDropSize)
	}
}

// lootFixture is a zone with combat and loot behind a real QUIC listener, plus
// the in-memory repository the bag is committed to.
type lootFixture struct {
	ctx        context.Context
	cancel     context.CancelFunc
	address    string
	zoneID     string
	repository charstore.Repository
	serve      chan error
	listener   transport.Listener
}

const (
	ownerTicket    = "sarnaut_tk_loot_owner"
	strangerTicket = "sarnaut_tk_loot_stranger"
)

var (
	ownerCharacter    = uuid.MustParse("019200f0-0000-7000-8000-0000000c0001")
	strangerCharacter = uuid.MustParse("019200f0-0000-7000-8000-0000000c0002")
)

func newLootFixture(t *testing.T) *lootFixture {
	t.Helper()

	content := loadFixturePack(t)
	var anchor world.Vec3
	found := false
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID == lootMobID {
			anchor = world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the fixture pack has no placement for %q", lootMobID)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "LootSliceZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
		PlayerSpawn:      anchor.Add(world.Vec3{X: 4}),
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

	repository := charstore.NewMemory()
	bags, err := charstore.NewInventoryService(repository, inventory.LimitsFromPack(content), 0)
	if err != nil {
		t.Fatalf("inventory.NewService() error = %v", err)
	}
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("loot.RulesFromPack() error = %v", err)
	}
	lootModule := loot.New(slog.New(slog.DiscardHandler), zone, lootRules, bags, loot.Options{
		WorldSeed: "loot-slice-test",
	})
	combatModule.SetKillSink((session.ZoneBinding{Loot: lootModule}).KillSink())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	go zone.Run(ctx)
	go combatModule.Run(ctx)

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

	// The character service writes through the same repository the bag does, so
	// a checkpoint and a loot award are two writers on one row — which is the
	// composition `cmd/shard` uses and the one the save sequence has to survive.
	worker := charstore.NewSaveWorker(repository, slog.New(slog.DiscardHandler), 0, 0)
	go worker.Run(ctx)
	characters := charstore.NewCharacterService(
		repository,
		lootTemplates{spawn: charstore.Vec3{X: anchor.X + 4, Y: anchor.Y, Z: anchor.Z}},
		worker,
		slog.New(slog.DiscardHandler),
		0,
	)

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "loot-slice-test",
		Zones: map[string]session.ZoneBinding{
			zone.ID(): {World: zone, Combat: combatModule, Loot: lootModule},
		},
		Authority:  newLootAuthority(),
		Characters: characters,
		Logger:     slog.New(slog.DiscardHandler),
	}
	serve := make(chan error, 1)
	go func() { serve <- server.Serve(ctx, listener) }()

	return &lootFixture{
		ctx:        ctx,
		cancel:     cancel,
		address:    listener.Addr().String(),
		zoneID:     zone.ID(),
		repository: repository,
		serve:      serve,
		listener:   listener,
	}
}

func (fixture *lootFixture) stop() {
	fixture.cancel()
	_ = fixture.listener.Close()
	select {
	case <-fixture.serve:
	case <-time.After(5 * time.Second):
	}
}

// lootSession is one connected client and the entity it was admitted as.
type lootSession struct {
	client     session.Client
	connection transport.Connection
	entityID   uint64
}

func (fixture *lootFixture) connect(t *testing.T, ticket string) *lootSession {
	t.Helper()
	connection, err := transport.DialQUIC(fixture.ctx, fixture.address, transport.NewDevClientTLSConfig())
	if err != nil {
		t.Fatalf("DialQUIC() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         "loot-slice-client",
		Ticket:          ticket,
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

func castUntilDead(t *testing.T, actor *lootSession, target *sarnautv1.EntitySnapshot) *sarnautv1.DeathEvent {
	t.Helper()
	var seq uint64
	var last *sarnautv1.CombatEvent
	for attempt := 0; attempt < 200; attempt++ {
		seq++
		if err := actor.client.SendCommand(actor.connection, &sarnautv1.ClientMessage{
			ClientSeq: seq,
			Payload: &sarnautv1.ClientMessage_AbilityUse{
				AbilityUse: &sarnautv1.AbilityUse{TargetId: target.GetEntityId()},
			},
		}); err != nil {
			t.Fatalf("SendCommand(ability_use) error = %v", err)
		}
		for {
			message, err := actor.client.ReadReliableMessage(actor.connection)
			if err != nil {
				t.Fatalf("ReadReliableMessage() error = %v", err)
			}
			if death := message.GetDeathEvent(); death != nil {
				return death
			}
			event := message.GetCombatEvent()
			if event == nil {
				continue
			}
			if event.GetRejection() == sarnautv1.AbilityRejection_ABILITY_REJECTION_ON_COOLDOWN {
				time.Sleep(100 * time.Millisecond)
			}
			last = event
			break
		}
	}
	t.Fatalf("the mob never died; last combat event = %v", last)
	return nil
}

func findLootTargetInSnapshots(
	t *testing.T,
	ctx context.Context,
	client session.Client,
	connection transport.Connection,
) *sarnautv1.EntitySnapshot {
	t.Helper()
	for {
		snapshot, err := client.ReadSnapshot(ctx, connection)
		if err != nil {
			t.Fatalf("ReadSnapshot() error = %v", err)
		}
		for _, entity := range snapshot.GetEntities() {
			if entity.GetContentId() == lootMobID && entity.GetAlive() {
				return entity
			}
		}
	}
}

func awaitLootOffer(t *testing.T, actor *lootSession) *sarnautv1.LootOffer {
	t.Helper()
	for {
		message, err := actor.client.ReadReliableMessage(actor.connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if offer := message.GetLootOffer(); offer != nil {
			return offer
		}
	}
}

func awaitLootResult(t *testing.T, actor *lootSession) *sarnautv1.LootResult {
	t.Helper()
	for {
		message, err := actor.client.ReadReliableMessage(actor.connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if result := message.GetLootResult(); result != nil {
			return result
		}
	}
}

func awaitInventoryUpdate(t *testing.T, actor *lootSession) *sarnautv1.InventoryUpdate {
	t.Helper()
	for {
		message, err := actor.client.ReadReliableMessage(actor.connection)
		if err != nil {
			t.Fatalf("ReadReliableMessage() error = %v", err)
		}
		if update := message.GetInventoryUpdate(); update != nil {
			return update
		}
	}
}

func lootUnits(items []charstore.InventoryItem) int32 {
	var total int32
	for _, item := range items {
		if item.ItemID == lootItemID {
			total += item.Quantity
		}
	}
	return total
}

// lootAuthority redeems two tickets, one per character, so the ownership rule
// has a second connected client to refuse.
type lootAuthority struct {
	mu       sync.Mutex
	redeemed map[string]bool
}

func newLootAuthority() *lootAuthority {
	return &lootAuthority{redeemed: make(map[string]bool)}
}

func (authority *lootAuthority) RedeemTicket(_ context.Context, ticket string) (session.Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.redeemed[ticket] {
		return session.Admission{}, &session.Refusal{Reason: session.ReasonUnknownTicket}
	}
	switch ticket {
	case ownerTicket:
		authority.redeemed[ticket] = true
		return session.Admission{
			AccountID:       uuid.MustParse("019200f0-0000-7000-8000-0000000a0001"),
			CharacterID:     ownerCharacter,
			CharacterName:   "Owner",
			ChargenOptionID: "chargen.league.warrior",
		}, nil
	case strangerTicket:
		authority.redeemed[ticket] = true
		return session.Admission{
			AccountID:       uuid.MustParse("019200f0-0000-7000-8000-0000000a0002"),
			CharacterID:     strangerCharacter,
			CharacterName:   "Stranger",
			ChargenOptionID: "chargen.league.warrior",
		}, nil
	default:
		return session.Admission{}, &session.Refusal{Reason: session.ReasonUnknownTicket}
	}
}

func (authority *lootAuthority) RenewPlayLock(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func (authority *lootAuthority) ReleasePlayLock(context.Context, uuid.UUID) error { return nil }

// lootTemplates materializes both characters next to the target.
type lootTemplates struct {
	spawn charstore.Vec3
}

func (templates lootTemplates) Template(string) (charstore.Snapshot, bool) {
	return integrationTemplate(templates.spawn), true
}
