package quests_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/world"
)

// The fixture pack's quest set. Every one of them is a row in
// `data-schemas/demo`, and the point of naming five is that the state machine
// below is exercised entirely from content: no test in this file knows what a
// quest requires, only which fixture asks the question it is about.
const (
	welcomeQuest = "quest.paper-harbor.harbor-welcome" // no objectives at all
	tallyQuest   = "quest.paper-harbor.tide-tally"     // one kill, two rewards
	deepQuest    = "quest.paper-harbor.deep-tally"     // gated on tallyQuest
	vigilQuest   = "quest.paper-harbor.harbor-vigil"   // required_level 5
	titheQuest   = "quest.paper-harbor.tonic-tithe"    // counts items, not kills

	// mossyQuest is the dataset's original quest: three sparrows, a starter and
	// a finisher that are different NPCs, and can_cancel false.
	mossyQuest = "quest.paper-harbor.mossy-gate"

	giverMob   = "mob.paper-harbor.harbor-quartermaster"
	crabMob    = "mob.paper-harbor.tide-crab"
	sparrowMob = "mob.paper-harbor.copper-sparrow"

	tonicItem = "item.consumable.harbor-tonic"
	scaleItem = "item.junk.brine-scale"
	tokenItem = "item.quest-rewards.tide-token"
)

// nearGiver and farGiver bracket TURN_IN_RANGE_M. Their exact values do not
// matter and which side of 5 metres they fall on does.
const (
	nearGiver = 3.0
	farGiver  = 12.0
)

// fixture is one zone, one quest module over the vendored fixture pack, and one
// character standing next to a quest giver.
//
// There is no combat module and no loot module here, and that is the assertion
// the file is built on: the quest module is driven by a [quests.Kill] value and
// a [quests.Granter], so a test needs neither of the packages that produce them.
type fixture struct {
	zone        *world.Zone
	module      *quests.Module
	repository  charstore.Repository
	granter     *bagGranter
	characterID uuid.UUID
	entityID    uint64
	giverEntity uint64
	farEntity   uint64
	updates     *recorder
}

func newFixture(t *testing.T, level uint32, inventory []charstore.InventoryItem) *fixture {
	t.Helper()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{})
	if err != nil {
		t.Fatalf("CatalogFromPack() error = %v", err)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "QuestTestZone",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}

	characterID := uuid.MustParse("019200f0-0000-7000-8000-0000000a1001")
	repository := charstore.NewMemory()
	granter := &bagGranter{
		repository: repository,
		capacity:   16,
		limits:     stackLimits(t, content),
	}
	seedCharacter(t, repository, characterID, int32(level), inventory)

	module := quests.New(slog.New(slog.DiscardHandler), zone, catalog, granter)
	entityID, _ := zone.JoinAt(world.Vec3{X: nearGiver}, 0)
	giverEntity := zone.SpawnNPC(world.NPCSpec{ContentID: giverMob, Level: 1, MaxHealth: 100})
	farEntity := zone.SpawnNPC(world.NPCSpec{
		ContentID: giverMob,
		Level:     1,
		MaxHealth: 100,
		Position:  world.Vec3{X: farGiver + nearGiver},
	})

	if err := module.Admit(entityID, characterID, quests.Character{
		Level:     level,
		Quests:    loadQuestRows(t, repository, characterID),
		Inventory: inventory,
	}); err != nil {
		t.Fatalf("Admit() error = %v", err)
	}
	updates := new(recorder)
	module.Subscribe(characterID, updates)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go module.Run(ctx)

	return &fixture{
		zone:        zone,
		module:      module,
		repository:  repository,
		granter:     granter,
		characterID: characterID,
		entityID:    entityID,
		giverEntity: giverEntity,
		farEntity:   farEntity,
		updates:     updates,
	}
}

// accept is the happy path every other test starts from.
func (fixture *fixture) accept(t *testing.T, questID string) quests.Result {
	t.Helper()
	result, err := fixture.module.Accept(context.Background(), fixture.entityID, questID, fixture.giverEntity)
	if err != nil {
		t.Fatalf("Accept(%s) error = %v", questID, err)
	}
	return result
}

// kill delivers one mechanics/combat.md rule 5.9.3 event, from inside a tick
// and under the zone lock, which is exactly where the combat module calls it.
func (fixture *fixture) kill(killerEntityID uint64, mobID string) {
	_ = fixture.zone.GameCommand(func(tick gametypes.Tick) error {
		fixture.module.CreditKill(tick, quests.Kill{
			KillerEntityID:  killerEntityID,
			VictimContentID: mobID,
			ServerTick:      tick.Number(),
		})
		return nil
	})
}

// state is the persisted state of one quest, read back out of the repository
// rather than out of the module. Every assertion about what a grant committed
// goes through here: what the module believes and what survives a restart are
// different questions.
func (fixture *fixture) storedState(t *testing.T, questID string) (string, bool) {
	t.Helper()
	rows, err := fixture.repository.LoadQuestStates(context.Background(), fixture.characterID)
	if err != nil {
		t.Fatalf("LoadQuestStates() error = %v", err)
	}
	for _, row := range rows {
		if row.QuestID == questID {
			return row.State, true
		}
	}
	return "", false
}

func (fixture *fixture) storedCharacter(t *testing.T) charstore.CharacterState {
	t.Helper()
	state, err := fixture.repository.LoadCharacterState(context.Background(), fixture.characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	return state
}

func (fixture *fixture) storedInventory(t *testing.T) []charstore.InventoryItem {
	t.Helper()
	items, err := fixture.repository.LoadInventory(context.Background(), fixture.characterID)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	return items
}

// counters reads one quest's live counters out of the module's own journal.
func (fixture *fixture) counters(t *testing.T, questID string) []int32 {
	t.Helper()
	for _, update := range fixture.module.Log(fixture.characterID) {
		if update.QuestID != questID {
			continue
		}
		values := make([]int32, 0, len(update.Objectives))
		for _, objective := range update.Objectives {
			values = append(values, objective.Counter)
		}
		return values
	}
	t.Fatalf("the journal holds no %s", questID)
	return nil
}

func (fixture *fixture) journalState(t *testing.T, questID string) quests.State {
	t.Helper()
	for _, update := range fixture.module.Log(fixture.characterID) {
		if update.QuestID == questID {
			return update.State
		}
	}
	t.Fatalf("the journal holds no %s", questID)
	return quests.StateUnspecified
}

// recorder collects the updates the module fans out, which is the only way a
// caller learns about progress it did not ask for.
type recorder struct {
	mu      sync.Mutex
	updates []quests.Update
}

func (sink *recorder) OfferQuestUpdate(update quests.Update) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.updates = append(sink.updates, update)
}

// await waits for an update about one quest and returns the last one seen.
func (sink *recorder) await(t *testing.T, questID string) quests.Update {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		var found *quests.Update
		for index := range sink.updates {
			if sink.updates[index].QuestID == questID {
				found = &sink.updates[index]
			}
		}
		sink.mu.Unlock()
		if found != nil {
			return *found
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no update arrived for %s", questID)
	return quests.Update{}
}

func (sink *recorder) count(questID string) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var seen int
	for _, update := range sink.updates {
		if update.QuestID == questID {
			seen++
		}
	}
	return seen
}

// bagGranter is the test double for the seam `internal/inventory` implements.
//
// It is a real transaction over a real repository: the removals, the
// insertions, the currencies and the quest row all commit together or not at
// all, which is the half of rule 5.7 the quest module depends on. The bag
// arithmetic is deliberately simpler than the production one — this package may
// not import `internal/inventory` (see boundary_test.go), and what these tests
// are about is the state machine, not stack splitting. The production grant is
// held to the same contract by `internal/inventory`'s own tests and by the
// end-to-end run in `internal/session`.
type bagGranter struct {
	repository charstore.Repository
	capacity   int32
	limits     map[string]int32

	mu sync.Mutex
	// failWith, when set, fails every grant. It is how the
	// transaction-died-halfway path of rule 5.7.6 is reached without a database
	// that can be made to fail on demand.
	failWith error
	// grants counts committed transactions, so "exactly once" is a number and
	// not an inference from the resulting rows.
	grants int
}

func (granter *bagGranter) fail(err error) {
	granter.mu.Lock()
	defer granter.mu.Unlock()
	granter.failWith = err
}

func (granter *bagGranter) committed() int {
	granter.mu.Lock()
	defer granter.mu.Unlock()
	return granter.grants
}

func (granter *bagGranter) GrantQuestReward(
	ctx context.Context,
	grant charstore.QuestGrant,
) (charstore.QuestGrantResult, error) {
	granter.mu.Lock()
	forced := granter.failWith
	granter.mu.Unlock()
	if forced != nil {
		return charstore.QuestGrantResult{}, forced
	}

	var result charstore.QuestGrantResult
	err := granter.repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		state, err := tx.LoadCharacterState(ctx, grant.CharacterID)
		if err != nil {
			return err
		}
		items, err := tx.LoadInventory(ctx, grant.CharacterID)
		if err != nil {
			return err
		}
		held := map[string]int32{}
		for _, item := range items {
			held[item.ItemID] += item.Quantity
		}
		for _, consumed := range grant.Consume {
			if held[consumed.ItemID] < consumed.Count {
				return errors.New("quest grant consumes more than the bag holds")
			}
			held[consumed.ItemID] -= consumed.Count
		}
		for _, given := range grant.Grants {
			held[given.ItemID] += given.Count
		}
		if granter.slotsFor(held) > granter.capacity {
			return charstore.ErrGrantWouldNotFit
		}
		if err := tx.ReplaceInventory(ctx, grant.CharacterID, granter.pack(held)); err != nil {
			return err
		}

		state.Experience += grant.Experience
		state.Currency += grant.Money
		state.Honor += grant.Honor
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return err
		}
		if err := tx.UpsertQuestState(ctx, grant.CharacterID, grant.Quest); err != nil {
			return err
		}
		result = charstore.QuestGrantResult{
			Inventory:  granter.pack(held),
			Currency:   state.Currency,
			Experience: state.Experience,
			Honor:      state.Honor,
			SaveSeq:    state.SaveSeq,
		}
		return nil
	})
	if err != nil {
		return charstore.QuestGrantResult{}, err
	}
	granter.mu.Lock()
	granter.grants++
	granter.mu.Unlock()
	return result, nil
}

func (granter *bagGranter) slotsFor(held map[string]int32) int32 {
	var slots int32
	for itemID, count := range held {
		if count <= 0 {
			continue
		}
		limit := granter.limits[itemID]
		if limit < 1 {
			limit = 1
		}
		slots += (count + limit - 1) / limit
	}
	return slots
}

func (granter *bagGranter) pack(held map[string]int32) []charstore.InventoryItem {
	ids := make([]string, 0, len(held))
	for itemID, count := range held {
		if count > 0 {
			ids = append(ids, itemID)
		}
	}
	sort.Strings(ids)

	var (
		items []charstore.InventoryItem
		slot  int32
	)
	for _, itemID := range ids {
		limit := granter.limits[itemID]
		if limit < 1 {
			limit = 1
		}
		for remaining := held[itemID]; remaining > 0; {
			count := limit
			if count > remaining {
				count = remaining
			}
			items = append(items, charstore.InventoryItem{Slot: slot, ItemID: itemID, Quantity: count})
			remaining -= count
			slot++
		}
	}
	return items
}

// spawnAt places one NPC where the character can reach it. Everything these
// tests need from a world entity is its content id and its distance, so the
// spec is that and nothing else.
func spawnAt(contentID string, level uint32) world.NPCSpec {
	return world.NPCSpec{ContentID: contentID, Level: level, MaxHealth: 100}
}

func zeroVec() world.Vec3 { return world.Vec3{} }

// bumped advances a state's save sequence so a test can write over what a grant
// just committed. The anti-clobber rule of ADR 0031 §6 rejects a save that does
// not advance, which is the behaviour under test everywhere else.
func bumped(state charstore.CharacterState) charstore.CharacterState {
	state.SaveSeq++
	return state
}

func stackLimits(t *testing.T, content *pack.Pack) map[string]int32 {
	t.Helper()
	limits := map[string]int32{}
	for _, itemID := range []string{tonicItem, scaleItem, tokenItem} {
		item, ok := content.Item(itemID)
		if !ok {
			t.Fatalf("the fixture pack does not carry %s", itemID)
		}
		limits[itemID] = item.Stack()
	}
	return limits
}

func seedCharacter(
	t *testing.T,
	repository charstore.Repository,
	characterID uuid.UUID,
	level int32,
	items []charstore.InventoryItem,
) {
	t.Helper()
	ctx := context.Background()
	err := charstore.SaveCharacter(ctx, repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "QuestTestZone",
			Level:       level,
			Health:      100,
			SaveSeq:     1,
		},
		Inventory: items,
	})
	if err != nil {
		t.Fatalf("SaveCharacter() error = %v", err)
	}
}

func loadQuestRows(t *testing.T, repository charstore.Repository, characterID uuid.UUID) []charstore.QuestState {
	t.Helper()
	rows, err := repository.LoadQuestStates(context.Background(), characterID)
	if err != nil {
		t.Fatalf("LoadQuestStates() error = %v", err)
	}
	return rows
}
