package quests_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
)

func TestTurnInWithChoiceCommitsMandatoryAndSelectedAlternative(t *testing.T) {
	t.Parallel()
	fixture := newRewardChoiceFixture(t, nil, []pack.QuestRewardItem{
		{ItemID: tonicItem, Count: 2},
		{ItemID: tonicItem, Count: 3},
	})

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	result, err := fixture.module.TurnInWithChoice(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, 1,
	)
	if err != nil {
		t.Fatalf("TurnInWithChoice() error = %v", err)
	}
	if !result.Committed {
		t.Fatal("TurnInWithChoice() did not report its transaction")
	}

	definition, _ := fixture.module.Catalog().Definition(tallyQuest)
	want := rewardCounts(definition.Rewards.MandatoryItems)
	want[tonicItem] += 3
	if got := itemCounts(result.Update.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("reported rewards = %v, want %v", got, want)
	}
	if got := inventoryCounts(fixture.storedInventory(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("committed rewards = %v, want %v", got, want)
	}
}

func TestTurnInDefaultsToTheFirstAlternative(t *testing.T) {
	t.Parallel()
	fixture := newRewardChoiceFixture(t, nil, []pack.QuestRewardItem{
		{ItemID: tonicItem, Count: 2},
		{ItemID: tonicItem, Count: 3},
	})

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	result, err := fixture.module.TurnIn(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity,
	)
	if err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
	definition, _ := fixture.module.Catalog().Definition(tallyQuest)
	want := rewardCounts(definition.Rewards.MandatoryItems)
	want[tonicItem] += 2
	if got := itemCounts(result.Update.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("reported rewards = %v, want first alternative %v", got, want)
	}
}

func TestTurnInWithoutAlternativesIgnoresTheChoiceIndex(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1, nil)

	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	result, err := fixture.module.TurnInWithChoice(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, -1,
	)
	if err != nil {
		t.Fatalf("TurnInWithChoice() error = %v for a quest with no alternatives", err)
	}
	definition, _ := fixture.module.Catalog().Definition(tallyQuest)
	if got, want := itemCounts(result.Update.Items), rewardCounts(definition.Rewards.MandatoryItems); !reflect.DeepEqual(got, want) {
		t.Errorf("reported rewards = %v, want mandatory rewards %v", got, want)
	}
}

func TestBadRewardChoicesCommitNothingAndLeaveTheQuestRetryable(t *testing.T) {
	t.Parallel()
	for _, rewardIndex := range []int32{-1, 2, math.MaxInt32} {
		rewardIndex := rewardIndex
		t.Run(stringIndex(rewardIndex), func(t *testing.T) {
			t.Parallel()
			fixture := newRewardChoiceFixture(t, nil, []pack.QuestRewardItem{
				{ItemID: tonicItem, Count: 2},
				{ItemID: tonicItem, Count: 3},
			})
			fixture.accept(t, tallyQuest)
			fixture.kill(fixture.entityID, crabMob)
			beforeState := fixture.storedCharacter(t)
			beforeBag := fixture.storedInventory(t)
			beforeGrants := fixture.granter.committed()

			result, err := fixture.module.TurnInWithChoice(
				context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, rewardIndex,
			)
			if !errors.Is(err, quests.ErrInvalidRewardChoice) {
				t.Fatalf("TurnInWithChoice(%d) error = %v, want ErrInvalidRewardChoice", rewardIndex, err)
			}
			if result.Update.Refusal != quests.RefusalInvalidRewardChoice {
				t.Errorf("refusal = %s, want invalid_reward_choice", result.Update.Refusal)
			}
			if result.Committed {
				t.Error("a bad reward choice reported a committed transaction")
			}
			if got := fixture.storedCharacter(t); !reflect.DeepEqual(got, beforeState) {
				t.Errorf("character changed on a bad reward choice: got %+v, want %+v", got, beforeState)
			}
			if got := fixture.storedInventory(t); !reflect.DeepEqual(got, beforeBag) {
				t.Errorf("inventory changed on a bad reward choice: got %+v, want %+v", got, beforeBag)
			}
			if got := fixture.granter.committed(); got != beforeGrants {
				t.Errorf("committed grants = %d, want unchanged %d", got, beforeGrants)
			}
			if got := fixture.journalState(t, tallyQuest); got != quests.StateCompletable {
				t.Errorf("journal state = %s, want completable", got)
			}

			if _, err := fixture.module.TurnInWithChoice(
				context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, 0,
			); err != nil {
				t.Fatalf("valid retry error = %v", err)
			}
		})
	}
}

func TestAFullBagRollsBackTheWholeSelectedReward(t *testing.T) {
	t.Parallel()
	mandatoryFits := make([]charstore.InventoryItem, 0, 14)
	for slot := range int32(14) {
		mandatoryFits = append(mandatoryFits, charstore.InventoryItem{
			Slot:       slot,
			InstanceID: uint64(slot) + 1,
			ItemID:     scaleItem,
			Quantity:   1,
		})
	}
	control := newFixture(t, 1, mandatoryFits)
	control.accept(t, tallyQuest)
	control.kill(control.entityID, crabMob)
	if _, err := control.module.TurnIn(
		context.Background(), control.entityID, tallyQuest, control.giverEntity,
	); err != nil {
		t.Fatalf("mandatory-only control TurnIn() error = %v", err)
	}

	fixture := newRewardChoiceFixture(t, mandatoryFits, []pack.QuestRewardItem{
		{ItemID: tonicItem, Count: 2},
		{ItemID: tonicItem, Count: 3},
	})
	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	beforeState := fixture.storedCharacter(t)
	beforeBag := fixture.storedInventory(t)
	beforeGrants := fixture.granter.committed()

	result, err := fixture.module.TurnInWithChoice(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, 1,
	)
	if !errors.Is(err, quests.ErrBagFull) {
		t.Fatalf("TurnInWithChoice() error = %v, want ErrBagFull", err)
	}
	if result.Update.Refusal != quests.RefusalBagFull {
		t.Errorf("refusal = %s, want bag_full", result.Update.Refusal)
	}
	if result.Committed || len(result.Update.Items) != 0 {
		t.Errorf("refused result = %+v, want no transaction and no rewards", result)
	}
	if got := fixture.storedCharacter(t); !reflect.DeepEqual(got, beforeState) {
		t.Errorf("character changed on full-bag refusal: got %+v, want %+v", got, beforeState)
	}
	if got := fixture.storedInventory(t); !reflect.DeepEqual(got, beforeBag) {
		t.Errorf("inventory changed on full-bag refusal: got %+v, want %+v", got, beforeBag)
	}
	if got := fixture.granter.committed(); got != beforeGrants {
		t.Errorf("committed grants = %d, want unchanged %d", got, beforeGrants)
	}
	if got := fixture.journalState(t, tallyQuest); got != quests.StateCompletable {
		t.Errorf("journal state = %s, want completable", got)
	}
}

func TestConcurrentRewardChoicesCommitExactlyOneSelection(t *testing.T) {
	t.Parallel()
	fixture := newRewardChoiceFixture(t, nil, []pack.QuestRewardItem{
		{ItemID: tonicItem, Count: 2},
		{ItemID: tonicItem, Count: 3},
	})
	fixture.accept(t, tallyQuest)
	fixture.kill(fixture.entityID, crabMob)
	entered, release := fixture.granter.blockNextGrant()
	defer release()

	var first quests.Result
	var firstErr error
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		first, firstErr = fixture.module.TurnInWithChoice(
			context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, 0,
		)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first turn-in did not reach the blocked grant")
	}

	second, secondErr := fixture.module.TurnInWithChoice(
		context.Background(), fixture.entityID, tallyQuest, fixture.giverEntity, 1,
	)
	if !errors.Is(secondErr, quests.ErrQuestAlreadyComplete) {
		t.Errorf("concurrent choice error = %v, want ErrQuestAlreadyComplete", secondErr)
	}
	if second.Committed {
		t.Error("the concurrent choice reported a committed transaction")
	}
	release()
	group.Wait()
	if firstErr != nil || !first.Committed {
		t.Fatalf("first choice result = %+v, error = %v", first, firstErr)
	}
	if got := fixture.granter.committed(); got != 2 {
		t.Errorf("committed grants = %d, want accept plus one turn-in", got)
	}
	if got := fixture.journalState(t, tallyQuest); got != quests.StateTurnedIn {
		t.Errorf("journal state = %s, want turned-in", got)
	}

	definition, _ := fixture.module.Catalog().Definition(tallyQuest)
	want := rewardCounts(definition.Rewards.MandatoryItems)
	want[tonicItem] += definition.Rewards.AlternativeItems[0].Count
	if got := inventoryCounts(fixture.storedInventory(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("committed rewards = %v, want winning selection %v", got, want)
	}
}

func newRewardChoiceFixture(
	t *testing.T,
	inventory []charstore.InventoryItem,
	alternatives []pack.QuestRewardItem,
) *fixture {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	definitions := make([]pack.Quest, 0, len(content.QuestIDs()))
	for _, questID := range content.QuestIDs() {
		definition, ok := content.Quest(questID)
		if !ok {
			continue
		}
		if questID == tallyQuest {
			definition.Rewards.AlternativeItems = append([]pack.QuestRewardItem(nil), alternatives...)
		}
		definitions = append(definitions, definition)
	}
	catalog, err := quests.NewCatalog(definitions, content)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return newFixtureWithCatalog(t, 1, inventory, content, catalog)
}

func rewardCounts(items []pack.QuestRewardItem) map[string]int32 {
	counts := make(map[string]int32, len(items))
	for _, item := range items {
		if item.Count > 0 {
			counts[item.ItemID] += item.Count
		}
	}
	return counts
}

func itemCounts(items []quests.ItemCount) map[string]int32 {
	counts := make(map[string]int32, len(items))
	for _, item := range items {
		counts[item.ItemID] += item.Count
	}
	return counts
}

func inventoryCounts(items []charstore.InventoryItem) map[string]int32 {
	counts := make(map[string]int32, len(items))
	for _, item := range items {
		counts[item.ItemID] += item.Quantity
	}
	return counts
}

func stringIndex(index int32) string {
	if index < 0 {
		return "negative"
	}
	if index == math.MaxInt32 {
		return "max-int32"
	}
	return "past-end"
}
