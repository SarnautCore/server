package quests_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/scriptqueue"
	"github.com/SarnautCore/server/internal/world"
)

// startingTonics is the League warrior's chargen loadout. The item objective of
// `tonic-tithe` asks for two of them, so a fresh character accepts that quest
// already satisfied — which is what makes it the fixture for rule 5.5.3.
var startingTonics = []charstore.InventoryItem{
	{Slot: 0, ItemID: tonicItem, Quantity: 3},
}

func TestAcceptCommitsDeferredActivationWithQuestState(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)
	work := scriptqueue.Work{
		ID: "quest-accept|deferred|1", ZoneID: fixture.zone.ID(),
		ScopeKind: scriptqueue.ScopeActivation, ScopeID: "quest-accept",
		DueAtMS: 1_000, Payload: []byte(`{"schema":1}`),
	}
	result, err := fixture.module.AcceptWithDeferred(
		t.Context(), fixture.entityID, welcomeQuest, fixture.giverEntity, []scriptqueue.Work{work},
	)
	if err != nil {
		t.Fatalf("AcceptWithDeferred() error = %v", err)
	}
	if len(result.Deferred) != 1 || result.Deferred[0].Sequence == 0 {
		t.Fatalf("committed deferred rows = %#v, want one sequenced row", result.Deferred)
	}
	conflict := work
	conflict.Payload = []byte(`{"schema":2}`)
	if _, err := fixture.repository.EnqueueDeferredScript(t.Context(), conflict); !errors.Is(err, scriptqueue.ErrConflict) {
		t.Fatalf("conflicting outbox insert error = %v, want ErrConflict", err)
	}
}

// TestAcceptingWithAnUnfinishedPrerequisiteIsRefused is rule 5.3.3 and
// transition T4.
func TestAcceptingWithAnUnfinishedPrerequisiteIsRefused(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	result, err := fixture.module.Accept(context.Background(), fixture.entityID, deepQuest, fixture.giverEntity)
	if !errors.Is(err, quests.ErrQuestUnavailable) {
		t.Fatalf("Accept() error = %v, want ErrQuestUnavailable", err)
	}
	if result.Update.Refusal != quests.RefusalUnavailable {
		t.Errorf("refusal = %s, want unavailable", result.Update.Refusal)
	}
	if result.Committed {
		t.Error("a refused accept committed a transaction")
	}
	if state, ok := fixture.storedState(t, deepQuest); ok {
		t.Errorf("the store holds %s in state %q; a refused accept must write nothing", deepQuest, state)
	}

	// The gate opens exactly once its prerequisite reaches `turned-in`, and not
	// before: the same call, on the same character, after the same fixture has
	// finished the quest it waits on.
	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn(%s) error = %v", tallyQuest, err)
	}
	if _, err := fixture.module.Accept(
		context.Background(), fixture.entityID, deepQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("Accept(%s) after the prerequisite error = %v", deepQuest, err)
	}
}

// TestAQuestAboveTheCharactersLevelIsNotOffered is rule 5.3.2.
func TestAQuestAboveTheCharactersLevelIsNotOffered(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	updates, refusal := fixture.module.Interact(fixture.entityID, fixture.giverEntity)
	if refusal != quests.RefusalNone {
		t.Fatalf("Interact() refusal = %s, want none", refusal)
	}
	offered := map[string]quests.State{}
	for _, update := range updates {
		offered[update.QuestID] = update.State
	}
	if state := offered[vigilQuest]; state != quests.StateUnavailable {
		t.Errorf("%s is %s to a level 1 character, want unavailable", vigilQuest, state)
	}
	if state := offered[tallyQuest]; state != quests.StateOffered {
		t.Errorf("%s is %s, want offered; the level gate caught the wrong quest", tallyQuest, state)
	}

	if _, err := fixture.module.Accept(
		context.Background(), fixture.entityID, vigilQuest, fixture.giverEntity,
	); !errors.Is(err, quests.ErrQuestUnavailable) {
		t.Errorf("Accept(%s) error = %v, want ErrQuestUnavailable", vigilQuest, err)
	}

	// The same quest, the same content, a character five levels higher: the
	// gate is a number in the pack and nothing in this package.
	senior := newFixture(t, 5, nil)
	if _, err := senior.module.Accept(
		context.Background(), senior.entityID, vigilQuest, senior.giverEntity,
	); err != nil {
		t.Errorf("Accept(%s) at level 5 error = %v, want it offered", vigilQuest, err)
	}
}

// TestAcceptingOutOfRangeOrFromTheWrongNPCIsRefused is transition T4's two
// remaining guards.
func TestAcceptingOutOfRangeOrFromTheWrongNPCIsRefused(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	_, err := fixture.module.Accept(context.Background(), fixture.entityID, tallyQuest, fixture.farEntity)
	if !errors.Is(err, quests.ErrOutOfRange) {
		t.Errorf("Accept() at %.0f m error = %v, want ErrOutOfRange", farGiver, err)
	}

	// An entity that exists and is not this quest's starter.
	crab := fixture.zone.SpawnNPC(spawnAt(crabMob, 1))
	if _, err := fixture.module.Accept(
		context.Background(), fixture.entityID, tallyQuest, crab,
	); !errors.Is(err, quests.ErrWrongNPC) {
		t.Errorf("Accept() from the crab error = %v, want ErrWrongNPC", err)
	}

	if state, ok := fixture.storedState(t, tallyQuest); ok {
		t.Errorf("the store holds %s in state %q; neither refusal may create an instance", tallyQuest, state)
	}
	if _, err := fixture.module.Accept(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("Accept() in range from the starter error = %v", err)
	}
	if state, _ := fixture.storedState(t, tallyQuest); state != "accepted" {
		t.Errorf("stored state = %q, want accepted", state)
	}
}

// TestKillCreditGoesToTheKillerAndOnlyWhileAccepted is rule 5.4.2 and 5.4.3.
func TestKillCreditGoesToTheKillerAndOnlyWhileAccepted(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	// Before the quest is accepted the kill lands on nothing.
	fixture.kill(fixture.entityID, crabMob)

	fixture.accept(t, tallyQuest)
	if counters := fixture.counters(t, tallyQuest); counters[0] != 0 {
		t.Fatalf("counter = %d on acceptance, want 0; the pre-accept kill was counted", counters[0])
	}

	// A second entity in the same zone, belonging to nobody this module knows.
	stranger, _ := fixture.zone.JoinAt(zeroVec(), 0)
	fixture.kill(stranger, crabMob)
	if counters := fixture.counters(t, tallyQuest); counters[0] != 0 {
		t.Fatalf("counter = %d, want 0; somebody else's kill was credited", counters[0])
	}

	// A mob that is not this objective's target.
	fixture.kill(fixture.entityID, sparrowMob)
	if counters := fixture.counters(t, tallyQuest); counters[0] != 0 {
		t.Fatalf("counter = %d, want 0; the wrong mob advanced the objective", counters[0])
	}

	fixture.kill(fixture.entityID, crabMob)
	if counters := fixture.counters(t, tallyQuest); counters[0] != 1 {
		t.Fatalf("counter = %d, want 1", counters[0])
	}
	if state := fixture.journalState(t, tallyQuest); state != quests.StateCompletable {
		t.Errorf("state = %s after the last kill, want completable (T6)", state)
	}
	if update := fixture.updates.await(t, tallyQuest); update.State != quests.StateCompletable {
		t.Errorf("the published update says %s, want completable", update.State)
	}

	// Rule 5.4.3.2: a satisfied counter does not keep counting, and rule 5.4.5:
	// a kill that changes nothing publishes nothing. A client that received a
	// second "completable" for the same quest would redraw a completion it had
	// already drawn.
	published := fixture.updates.count(tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	fixture.kill(fixture.entityID, crabMob)
	if counters := fixture.counters(t, tallyQuest); counters[0] != 1 {
		t.Errorf("counter = %d after two more kills, want it clamped at the limit of 1", counters[0])
	}
	time.Sleep(50 * time.Millisecond)
	if got := fixture.updates.count(tallyQuest); got != published {
		t.Errorf("%d updates published, want the %d from before the clamped kills", got, published)
	}
}

// TestACounterStopsAtItsLimitAcrossSeveralKills is the same clamp over a limit
// above one, so that "clamped" is not indistinguishable from "set to one".
func TestACounterStopsAtItsLimitAcrossSeveralKills(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
	fixture.accept(t, deepQuest)

	// deep-tally asks for two sparrows. Five die.
	for range 5 {
		fixture.kill(fixture.entityID, sparrowMob)
	}
	counters := fixture.counters(t, deepQuest)
	if len(counters) != 1 || counters[0] != 2 {
		t.Fatalf("counters = %v, want [2]; the limit is content and it is 2", counters)
	}
	if state := fixture.journalState(t, deepQuest); state != quests.StateCompletable {
		t.Errorf("state = %s, want completable", state)
	}
}

// TestTurningInBeforeCompletionIsRefused is rule 5.7.1's first precondition.
func TestTurningInBeforeCompletionIsRefused(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	result, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity)
	if !errors.Is(err, quests.ErrQuestNotComplete) {
		t.Fatalf("TurnIn() error = %v, want ErrQuestNotComplete", err)
	}
	if result.Update.Refusal != quests.RefusalNotComplete {
		t.Errorf("refusal = %s, want not_complete", result.Update.Refusal)
	}
	if state := fixture.storedCharacter(t); state.Experience != 0 {
		t.Errorf("experience = %d after a refused turn-in, want 0", state.Experience)
	}
	if state, _ := fixture.storedState(t, tallyQuest); state != "accepted" {
		t.Errorf("stored state = %q, want it left at accepted", state)
	}
}

// TestTurningInOutOfRangeOrAtTheWrongNPCIsRefused is transition T13.
func TestTurningInOutOfRangeOrAtTheWrongNPCIsRefused(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)

	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.farEntity,
	); !errors.Is(err, quests.ErrOutOfRange) {
		t.Errorf("TurnIn() at %.0f m error = %v, want ErrOutOfRange", farGiver, err)
	}
	crab := fixture.zone.SpawnNPC(spawnAt(crabMob, 1))
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, crab,
	); !errors.Is(err, quests.ErrWrongNPC) {
		t.Errorf("TurnIn() at the crab error = %v, want ErrWrongNPC", err)
	}
	if state := fixture.storedCharacter(t); state.Experience != 0 {
		t.Errorf("experience = %d, want 0: neither refusal grants anything", state.Experience)
	}
	if state := fixture.journalState(t, tallyQuest); state != quests.StateCompletable {
		t.Errorf("state = %s, want completable throughout", state)
	}
}

// TestASecondTurnInGrantsNothing is transition T17, asserted on persisted rows
// rather than on what the module answered.
func TestASecondTurnInGrantsNothing(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)

	first, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity)
	if err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
	if first.Update.State != quests.StateTurnedIn {
		t.Fatalf("state = %s, want turned-in", first.Update.State)
	}
	afterFirst := fixture.storedCharacter(t)
	inventoryAfterFirst := fixture.storedInventory(t)

	second, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity)
	if !errors.Is(err, quests.ErrQuestAlreadyComplete) {
		t.Fatalf("second TurnIn() error = %v, want ErrQuestAlreadyComplete", err)
	}
	if second.Committed {
		t.Error("the second turn-in committed a transaction")
	}

	afterSecond := fixture.storedCharacter(t)
	if afterSecond.Experience != afterFirst.Experience {
		t.Errorf("experience went from %d to %d across a duplicate turn-in",
			afterFirst.Experience, afterSecond.Experience)
	}
	if afterSecond.Currency != afterFirst.Currency {
		t.Errorf("currency went from %d to %d across a duplicate turn-in",
			afterFirst.Currency, afterSecond.Currency)
	}
	if got, want := len(fixture.storedInventory(t)), len(inventoryAfterFirst); got != want {
		t.Errorf("the bag holds %d slots after the duplicate, want %d", got, want)
	}
	for _, item := range fixture.storedInventory(t) {
		if item.ItemID == tokenItem && item.Quantity != 1 {
			t.Errorf("the bag holds %d tide tokens, want exactly one", item.Quantity)
		}
	}
	if granted := fixture.granter.committed(); granted != 2 {
		// One for the accept, one for the turn-in. A third means the duplicate
		// reached the transaction.
		t.Errorf("%d grants committed, want 2 (the accept and the one turn-in)", granted)
	}
}

// TestTheRewardsCommittedAreTheOnesTheContentDeclares keeps the grant honest
// about where its numbers come from.
func TestTheRewardsCommittedAreTheOnesTheContentDeclares(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	definition, ok := fixture.module.Catalog().Definition(tallyQuest)
	if !ok {
		t.Fatalf("the catalog has no %s", tallyQuest)
	}
	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	result, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity)
	if err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}

	state := fixture.storedCharacter(t)
	if state.Experience != definition.Rewards.Experience {
		t.Errorf("experience = %d, want the content's %d", state.Experience, definition.Rewards.Experience)
	}
	if state.Currency != definition.Rewards.Money {
		t.Errorf("currency = %d, want the content's %d", state.Currency, definition.Rewards.Money)
	}
	if state.Honor != definition.Rewards.Honor {
		t.Errorf("honor = %d, want the content's %d", state.Honor, definition.Rewards.Honor)
	}
	if len(result.Update.Items) != len(definition.Rewards.MandatoryItems) {
		t.Fatalf("the update reports %d reward items, want the content's %d",
			len(result.Update.Items), len(definition.Rewards.MandatoryItems))
	}
	held := map[string]int32{}
	for _, item := range fixture.storedInventory(t) {
		held[item.ItemID] += item.Quantity
	}
	for _, reward := range definition.Rewards.MandatoryItems {
		if held[reward.ItemID] != reward.Count {
			t.Errorf("the bag holds %d of %s, want the content's %d",
				held[reward.ItemID], reward.ItemID, reward.Count)
		}
	}
}

// TestAGrantThatDoesNotFitAbortsTheWholeTurnIn is rule 5.7.3 and 5.7.6, which
// is transition T12.
func TestAGrantThatDoesNotFitAbortsTheWholeTurnIn(t *testing.T) {
	t.Parallel()
	// A bag with exactly one free slot, against a reward of two unstackable
	// items. The arithmetic is content's: two kinds at stack_limit 1.
	full := make([]charstore.InventoryItem, 0, 15)
	for slot := range int32(15) {
		full = append(full, charstore.InventoryItem{Slot: slot, ItemID: scaleItem, Quantity: 1})
	}
	fixture := newFixture(t, 1, full)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	before := fixture.storedCharacter(t)

	result, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity)
	if !errors.Is(err, quests.ErrBagFull) {
		t.Fatalf("TurnIn() error = %v, want ErrBagFull", err)
	}
	if result.Update.Refusal != quests.RefusalBagFull {
		t.Errorf("refusal = %s, want bag_full", result.Update.Refusal)
	}

	after := fixture.storedCharacter(t)
	if after.Experience != before.Experience {
		t.Errorf("experience went from %d to %d on a refused grant", before.Experience, after.Experience)
	}
	if after.Currency != before.Currency {
		t.Errorf("currency went from %d to %d on a refused grant", before.Currency, after.Currency)
	}
	// The stored row is whatever the accept wrote: counters ride the session's
	// periodic checkpoint, and a refused turn-in writes nothing at all. What
	// matters is that it is not turned in, and that the live instance still is
	// completable so the player can free a slot and come back.
	if state, _ := fixture.storedState(t, tallyQuest); state == "turned-in" {
		t.Error("the stored row says turned-in after a refused grant")
	}
	if state := fixture.journalState(t, tallyQuest); state != quests.StateCompletable {
		t.Errorf("journal state = %s, want completable", state)
	}
	for _, item := range fixture.storedInventory(t) {
		if item.ItemID == tokenItem {
			t.Error("a reward item reached the bag on a refused grant")
		}
	}

	// Free a slot and the same turn-in succeeds. That is the other half of the
	// arithmetic: the requirement was one slot, not two.
	trimmed := full[:14]
	if err := charstore.SaveCharacter(context.Background(), fixture.repository, charstore.Snapshot{
		State:     bumped(after),
		Inventory: trimmed,
	}); err != nil {
		t.Fatalf("SaveCharacter() error = %v", err)
	}
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn() after freeing a slot error = %v", err)
	}
	if state, _ := fixture.storedState(t, tallyQuest); state != "turned-in" {
		t.Errorf("stored state = %q, want turned-in", state)
	}
}

// TestAFailedGrantLeavesTheInstanceUntouched is rule 5.7.6 for the other
// failure: the transaction itself dies.
func TestAFailedGrantLeavesTheInstanceUntouched(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)

	fixture.granter.fail(errors.New("the database went away"))
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err == nil {
		t.Fatal("TurnIn() error = nil, want the granter's failure")
	}
	if state := fixture.journalState(t, tallyQuest); state != quests.StateCompletable {
		t.Errorf("state = %s after a failed grant, want completable", state)
	}
	if state := fixture.storedCharacter(t); state.Experience != 0 {
		t.Errorf("experience = %d after a failed grant, want 0", state.Experience)
	}

	// The instance is not wedged: the flight marker was cleared, so the player
	// can walk away and try again.
	fixture.granter.fail(nil)
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn() after the database came back error = %v", err)
	}
	if state, _ := fixture.storedState(t, tallyQuest); state != "turned-in" {
		t.Errorf("stored state = %q, want turned-in", state)
	}
}

// TestAQuestWithNoObjectivesIsCompletableOnAcceptance is rule 5.6 and
// transition T7.
func TestAQuestWithNoObjectivesIsCompletableOnAcceptance(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	result := fixture.accept(t, welcomeQuest)
	if result.Update.State != quests.StateCompletable {
		t.Fatalf("state = %s on acceptance, want completable", result.Update.State)
	}
	if state, _ := fixture.storedState(t, welcomeQuest); state != "completable" {
		t.Errorf("stored state = %q, want completable: T7 commits inside T3", state)
	}
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, welcomeQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
}

// TestAnItemCounterTracksTheBagAndRegresses is rules 5.5.2 to 5.5.4, which is
// transition T10.
func TestAnItemCounterTracksTheBagAndRegresses(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, startingTonics)

	result := fixture.accept(t, titheQuest)
	if result.Update.State != quests.StateCompletable {
		t.Fatalf("state = %s, want completable: three tonics satisfy a limit of two",
			result.Update.State)
	}

	// The player sells one. Two are left, which is still the limit.
	fixture.module.InventoryChanged(fixture.characterID, []charstore.InventoryItem{
		{Slot: 0, ItemID: tonicItem, Quantity: 2},
	})
	if state := fixture.journalState(t, titheQuest); state != quests.StateCompletable {
		t.Errorf("state = %s at two of two, want completable", state)
	}

	// The player sells another. The counter regresses and so does the state.
	fixture.module.InventoryChanged(fixture.characterID, []charstore.InventoryItem{
		{Slot: 0, ItemID: tonicItem, Quantity: 1},
	})
	if counters := fixture.counters(t, titheQuest); counters[0] != 1 {
		t.Errorf("counter = %d, want 1", counters[0])
	}
	if state := fixture.journalState(t, titheQuest); state != quests.StateInProgress {
		t.Errorf("state = %s after selling below the limit, want in-progress (T10)", state)
	}
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, titheQuest, fixture.giverEntity,
	); !errors.Is(err, quests.ErrQuestNotComplete) {
		t.Errorf("TurnIn() error = %v, want ErrQuestNotComplete", err)
	}

	// Reacquired, and it is completable again.
	fixture.module.InventoryChanged(fixture.characterID, []charstore.InventoryItem{
		{Slot: 0, ItemID: tonicItem, Quantity: 4},
	})
	if state := fixture.journalState(t, titheQuest); state != quests.StateCompletable {
		t.Errorf("state = %s after reacquiring, want completable", state)
	}
}

// TestTurningInAnItemQuestConsumesTheItems is rule 5.7.4's first clause.
func TestTurningInAnItemQuestConsumesTheItems(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, startingTonics)

	fixture.accept(t, titheQuest)
	if _, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, titheQuest, fixture.giverEntity,
	); err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
	held := map[string]int32{}
	for _, item := range fixture.storedInventory(t) {
		held[item.ItemID] += item.Quantity
	}
	if held[tonicItem] != 1 {
		t.Errorf("the bag holds %d tonics, want 1: two of three were consumed", held[tonicItem])
	}
	if held[tokenItem] != 1 {
		t.Errorf("the bag holds %d tide tokens, want the one reward", held[tokenItem])
	}
}

// TestAbandonHonoursTheContentsCancelFlag is transitions T14 and T15.
func TestAbandonHonoursTheContentsCancelFlag(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	if _, err := fixture.module.Abandon(context.Background(), fixture.entityID, tallyQuest); err != nil {
		t.Fatalf("Abandon() error = %v", err)
	}
	if state, _ := fixture.storedState(t, tallyQuest); state != "abandoned" {
		t.Errorf("stored state = %q, want abandoned", state)
	}
	// T16: the quest can be taken again from scratch.
	if _, err := fixture.module.Accept(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	); err != nil {
		t.Errorf("Accept() after abandoning error = %v, want it offered again", err)
	}

	// mossy-gate is the fixture's uncancellable quest, and the flag is content.
	uncancellable := newFixture(t, 1, nil)
	sparrow := uncancellable.zone.SpawnNPC(spawnAt(sparrowMob, 1))
	if _, err := uncancellable.module.Accept(
		context.Background(), uncancellable.entityID, mossyQuest, sparrow,
	); err != nil {
		t.Fatalf("Accept(%s) error = %v", mossyQuest, err)
	}
	if _, err := uncancellable.module.Abandon(
		context.Background(), uncancellable.entityID, mossyQuest,
	); !errors.Is(err, quests.ErrQuestCannotBeCancelled) {
		t.Errorf("Abandon() error = %v, want ErrQuestCannotBeCancelled", err)
	}
	if state, _ := uncancellable.storedState(t, mossyQuest); state != "accepted" {
		t.Errorf("stored state = %q, want it left accepted", state)
	}
}

// TestInteractingWithSomethingThatGivesNoQuestIsNotAnError keeps the generic
// verb generic: a corpse arrives on the same case.
func TestInteractingWithSomethingThatGivesNoQuestIsNotAnError(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	// A mob that starts and finishes nothing.
	bystander := fixture.zone.SpawnNPC(spawnAt("mob.paper-harbor.nobody-in-particular", 1))
	updates, refusal := fixture.module.Interact(fixture.entityID, bystander)
	if refusal != quests.RefusalNotAQuestGiver {
		t.Errorf("refusal = %s, want not_a_quest_giver", refusal)
	}
	if len(updates) != 0 {
		t.Errorf("updates = %v, want none", updates)
	}

	// A corpse container carries its victim's content id, and the tide crab is
	// a real finisher. Without the liveness check this would answer as a quest
	// giver and the interact verb would never reach the loot handler.
	corpse := fixture.zone.SpawnNPC(spawnAt(crabMob, 1))
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		entity := tick.Entity(corpse)
		entity.Alive = false
		entity.MaxHealth = 0
		entity.Health = 0
		return nil
	})
	if _, refusal := fixture.module.Interact(fixture.entityID, corpse); refusal != quests.RefusalNotAQuestGiver {
		t.Errorf("a corpse answered %s, want not_a_quest_giver", refusal)
	}

	_, refusal = fixture.module.Interact(fixture.entityID, fixture.farEntity)
	if refusal != quests.RefusalOutOfRange {
		t.Errorf("refusal = %s at %.0f m, want out_of_range", refusal, farGiver)
	}
}

// TestASecondQuestNeedsNoGoChange is the claim the whole design is for.
//
// Nothing below names a rule. It walks every definition the pack carries and
// asserts that each one can be driven through the state machine by content
// alone: the gates it declares, the objectives it declares, the rewards it
// declares. A sixth quest added to `data-schemas/demo` is covered by this test
// the moment it is compiled in.
func TestASecondQuestNeedsNoGoChange(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 9, startingTonics)

	catalog := fixture.module.Catalog()
	if catalog.Count() < 2 {
		t.Fatalf("the fixture pack carries %d quests; this test needs at least two", catalog.Count())
	}
	var completed int
	for _, questID := range catalog.IDs() {
		definition, _ := catalog.Definition(questID)
		giver := fixture.giverEntity
		if definition.StarterID != giverMob {
			giver = fixture.zone.SpawnNPC(spawnAt(definition.StarterID, 1))
		}
		if _, err := fixture.module.Accept(context.Background(), fixture.entityID, questID, giver); err != nil {
			// A quest gated on one that is not finished yet is refused, which is
			// the gate working. The loop is not a script.
			continue
		}
		for _, objective := range definition.Objectives {
			for range objective.Limit {
				for _, target := range objective.TargetIDs {
					fixture.kill(fixture.entityID, target)
				}
			}
		}
		if fixture.journalState(t, questID) != quests.StateCompletable {
			continue
		}
		finisher := fixture.giverEntity
		if definition.FinisherID != giverMob {
			finisher = fixture.zone.SpawnNPC(spawnAt(definition.FinisherID, 1))
		}
		if _, err := fixture.module.TurnIn(context.Background(), fixture.entityID, questID, finisher); err != nil {
			t.Errorf("TurnIn(%s) error = %v", questID, err)
			continue
		}
		completed++
	}
	if completed < 2 {
		t.Errorf("%d quests were completed end to end, want at least two", completed)
	}
}
