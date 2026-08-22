package itemactions_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/itemactions"
)

const (
	sword        = "item.sword"
	shield       = "item.shield"
	tonic        = "item.tonic"
	box          = "item.box"
	key          = "item.key"
	reward       = "item.reward"
	bag12        = "item.bag.12"
	potionAction = "item-action.item.tonic"
)

type catalog map[string]itemactions.Definition

func (catalog catalog) ItemDefinition(id string) (itemactions.Definition, bool) {
	definition, ok := catalog[id]
	return definition, ok
}

func fixtureCatalog(t *testing.T) catalog {
	t.Helper()
	layout12, ok := inventory.BagLayoutByID(inventory.BagLayout12ID)
	if !ok {
		t.Fatal("12-slot bag layout is absent")
	}
	return catalog{
		sword: {ItemID: sword, StackLimit: 1, Droppable: true,
			EquipSlots:  []itemactions.EquipmentSlot{itemactions.EquipmentMainhand},
			BindOnEquip: true, TriggerSlot: "MAINHAND"},
		shield: {ItemID: shield, StackLimit: 1, Droppable: true,
			EquipSlots: []itemactions.EquipmentSlot{itemactions.EquipmentOffhand}, TriggerSlot: "OFFHAND"},
		tonic: {ItemID: tonic, StackLimit: 20, Droppable: true,
			Use: &itemactions.UseDefinition{
				ProductActionID:  potionAction,
				AllowedLocations: []itemactions.LocationKind{itemactions.LocationItemBag},
			}},
		box: {ItemID: box, StackLimit: 20, Droppable: true,
			Box: &itemactions.BoxDefinition{KeyItemID: key, ConsumeKey: true,
				Rewards: []inventory.Grant{{ItemID: reward, Count: 2}}}},
		key:    {ItemID: key, StackLimit: 20, Droppable: true},
		reward: {ItemID: reward, StackLimit: 1, Droppable: true},
		bag12: {ItemID: bag12, StackLimit: 1, Droppable: true,
			EquipSlots: []itemactions.EquipmentSlot{itemactions.EquipmentBag}, BagLayout: &layout12},
	}
}

var errCommit = errors.New("injected commit failure")

type repository struct {
	mu         sync.Mutex
	state      itemactions.State
	failCommit bool
}

func (repository *repository) Update(
	ctx context.Context,
	_ uuid.UUID,
	expected int64,
	mutate func(itemactions.State) (itemactions.State, []itemactions.EquipChanged, error),
) (itemactions.State, []itemactions.EquipChanged, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return itemactions.State{}, nil, err
	}
	if expected != repository.state.Revision {
		return cloneState(repository.state), nil, itemactions.ErrStaleRevision
	}
	replacement, events, err := mutate(cloneState(repository.state))
	if err != nil {
		return cloneState(repository.state), nil, err
	}
	if replacement.Revision != repository.state.Revision+1 {
		return cloneState(repository.state), nil, errors.New("bad replacement revision")
	}
	if repository.failCommit {
		return cloneState(repository.state), nil, errCommit
	}
	repository.state = cloneState(replacement)
	return cloneState(repository.state), append([]itemactions.EquipChanged(nil), events...), nil
}

func (repository *repository) Read(
	ctx context.Context,
	_ uuid.UUID,
	expected int64,
) (itemactions.State, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return itemactions.State{}, err
	}
	state := cloneState(repository.state)
	if expected != state.Revision {
		return state, itemactions.ErrStaleRevision
	}
	return state, nil
}

func (repository *repository) snapshot() itemactions.State {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return cloneState(repository.state)
}

func cloneState(state itemactions.State) itemactions.State {
	cloned := state
	cloned.Inventory = append([]inventory.Stack(nil), state.Inventory...)
	cloned.Equipment = append([]itemactions.EquippedItem(nil), state.Equipment...)
	cloned.Layout = inventory.BagLayout{ID: state.Layout.ID, Partitions: append([]int32(nil), state.Layout.Partitions...)}
	if state.Bag != nil {
		bag := *state.Bag
		cloned.Bag = &bag
	}
	return cloned
}

type eventSink struct {
	mu     sync.Mutex
	events []itemactions.EquipChanged
}

func (sink *eventSink) EnqueueEquipChanged(event itemactions.EquipChanged) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
}

type useAuthority struct {
	mu         sync.Mutex
	calls      int
	commits    int
	aborts     int
	lastID     uint64
	lastAction string
	resource   itemactions.ActionResource
	error      error
}

func (authority *useAuthority) PrepareItemAction(
	_ context.Context, _ uint64, instanceID uint64, actionID string, _ uint64,
) (itemactions.PreparedUse, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.calls++
	authority.lastID, authority.lastAction = instanceID, actionID
	if authority.error != nil {
		return nil, authority.error
	}
	return &preparedUse{authority: authority, result: itemactions.Cooldown{
		InstanceID: instanceID, ProductActionID: actionID,
		Remaining: time.Minute, Duration: time.Minute,
	}, resource: authority.resource}, nil
}

type preparedUse struct {
	authority *useAuthority
	result    itemactions.Cooldown
	resource  itemactions.ActionResource
	done      bool
}

func (prepared *preparedUse) Resource() itemactions.ActionResource { return prepared.resource }

func (prepared *preparedUse) Commit() itemactions.Cooldown {
	prepared.authority.mu.Lock()
	defer prepared.authority.mu.Unlock()
	if !prepared.done {
		prepared.authority.commits++
		prepared.done = true
	}
	return prepared.result
}

func (prepared *preparedUse) Abort() {
	prepared.authority.mu.Lock()
	defer prepared.authority.mu.Unlock()
	if !prepared.done {
		prepared.authority.aborts++
		prepared.done = true
	}
}

func actor() itemactions.Actor { return itemactions.Actor{CharacterID: uuid.New(), EntityID: 77} }

func baseState(items ...inventory.Stack) itemactions.State {
	return itemactions.State{Revision: 10, Inventory: items, Layout: inventory.DefaultBagLayout()}
}

func serviceFor(t *testing.T, repository *repository, use itemactions.UseAuthority, sink itemactions.EquipEventSink) *itemactions.Service {
	t.Helper()
	service, err := itemactions.NewService(repository, repository, fixtureCatalog(t), use, sink)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func TestSplitCreatesOneIdentityAndPreservesMutableState(t *testing.T) {
	removeTime := int64(1234)
	repository := &repository{state: baseState(inventory.Stack{
		Slot: 2, InstanceID: 41, ItemID: tonic, Count: 9, CounterValue: 7,
		Bound: true, QuestOperator: true, RemoveTime: &removeTime,
	})}
	repository.state.Equipment = []itemactions.EquippedItem{{
		Slot: itemactions.EquipmentOffhand,
		Item: inventory.Stack{Slot: -1, InstanceID: 100, ItemID: shield, Count: 1},
	}}
	result, err := serviceFor(t, repository, nil, nil).Split(context.Background(), actor(), itemactions.SplitCommand{
		RequestID: 1, ExpectedRevision: 10, MoveNoMore: 4, FromSlot: 2, ToSlot: 5,
	})
	if err != nil {
		t.Fatalf("Split() error = %v", err)
	}
	if result.State.Revision != 11 || len(result.State.Inventory) != 2 {
		t.Fatalf("Split() state = %+v", result.State)
	}
	left, right := result.State.Inventory[0], result.State.Inventory[1]
	if left.InstanceID != 41 || left.Count != 5 || right.InstanceID != 101 || right.Count != 4 {
		t.Fatalf("split stacks = %+v, %+v", left, right)
	}
	right.Slot, right.InstanceID, right.Count = left.Slot, left.InstanceID, left.Count
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("split lost mutable state: left %+v right %+v", left, right)
	}
}

func TestSplitZeroMeansMoveAllAndPartialCannotSwap(t *testing.T) {
	repository := &repository{state: baseState(
		inventory.Stack{Slot: 1, InstanceID: 1, ItemID: tonic, Count: 3},
		inventory.Stack{Slot: 2, InstanceID: 2, ItemID: sword, Count: 1},
	)}
	service := serviceFor(t, repository, nil, nil)
	_, err := service.Split(context.Background(), actor(), itemactions.SplitCommand{
		RequestID: 1, ExpectedRevision: 10, MoveNoMore: 2, FromSlot: 1, ToSlot: 2,
	})
	if !errors.Is(err, itemactions.ErrDestinationOccupied) {
		t.Fatalf("partial occupied Split() error = %v", err)
	}
	result, err := service.Split(context.Background(), actor(), itemactions.SplitCommand{
		RequestID: 2, ExpectedRevision: 10, MoveNoMore: 0, FromSlot: 1, ToSlot: 2,
	})
	if err != nil {
		t.Fatalf("full Split() error = %v", err)
	}
	if result.State.Inventory[0].ItemID != sword || result.State.Inventory[1].ItemID != tonic {
		t.Fatalf("full split did not swap: %+v", result.State.Inventory)
	}
}

func TestConcurrentSameRevisionCommitsExactlyOnce(t *testing.T) {
	repository := &repository{state: baseState(inventory.Stack{Slot: 0, InstanceID: 1, ItemID: tonic, Count: 10})}
	service := serviceFor(t, repository, nil, nil)
	const callers = 64
	var wait sync.WaitGroup
	results := make(chan error, callers)
	for index := range callers {
		wait.Add(1)
		go func(requestID uint64) {
			defer wait.Done()
			_, err := service.Split(context.Background(), actor(), itemactions.SplitCommand{
				RequestID: requestID + 1, ExpectedRevision: 10, MoveNoMore: 1, FromSlot: 0, ToSlot: int32(requestID%15 + 1),
			})
			results <- err
		}(uint64(index))
	}
	wait.Wait()
	close(results)
	succeeded, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, itemactions.ErrStaleRevision):
			stale++
		default:
			t.Fatalf("concurrent Split() error = %v", err)
		}
	}
	if succeeded != 1 || stale != callers-1 {
		t.Fatalf("concurrent outcomes success=%d stale=%d", succeeded, stale)
	}
	state := repository.snapshot()
	if state.Revision != 11 || totalCount(state.Inventory) != 10 {
		t.Fatalf("stored state after race = %+v", state)
	}
}

func TestDropRefusalAndCommitFailureLeaveStateUntouched(t *testing.T) {
	for _, test := range []struct {
		name string
		item inventory.Stack
		fail bool
		want error
	}{
		{name: "unsupported", item: inventory.Stack{Slot: 0, InstanceID: 1, ItemID: bag12, Count: 1}, want: itemactions.ErrUnsupportedAction},
		{name: "commit", item: inventory.Stack{Slot: 0, InstanceID: 1, ItemID: tonic, Count: 2}, fail: true, want: errCommit},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := fixtureCatalog(t)
			if test.name == "unsupported" {
				definition := catalog[bag12]
				definition.Droppable = false
				catalog[bag12] = definition
			}
			repository := &repository{state: baseState(test.item), failCommit: test.fail}
			service, err := itemactions.NewService(repository, repository, catalog, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := repository.snapshot()
			_, err = service.Drop(context.Background(), actor(), itemactions.DropCommand{
				RequestID: 1, ExpectedRevision: 10, Count: 1, Slot: 0,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Drop() error = %v, want %v", err, test.want)
			}
			if got := repository.snapshot(); !reflect.DeepEqual(got, before) {
				t.Fatalf("state changed after refusal: %+v", got)
			}
		})
	}
}

func TestOpenBoxConsumesBoxAndKeyAndAllocatesRewardIdentities(t *testing.T) {
	repository := &repository{state: baseState(
		inventory.Stack{Slot: 0, InstanceID: 8, ItemID: box, Count: 2},
		inventory.Stack{Slot: 1, InstanceID: 9, ItemID: key, Count: 1},
	)}
	result, err := serviceFor(t, repository, nil, nil).OpenBox(context.Background(), actor(), itemactions.OpenBoxCommand{
		RequestID: 1, ExpectedRevision: 10, BoxSlot: 0, KeySlot: 1,
	})
	if err != nil {
		t.Fatalf("OpenBox() error = %v", err)
	}
	if result.State.Revision != 11 || len(result.State.Inventory) != 3 {
		t.Fatalf("OpenBox() state = %+v", result.State)
	}
	if result.State.Inventory[0].InstanceID != 8 || result.State.Inventory[0].Count != 1 {
		t.Fatalf("box identity/count changed incorrectly: %+v", result.State.Inventory[0])
	}
	for _, stack := range result.State.Inventory[1:] {
		if stack.ItemID != reward || stack.Count != 1 || stack.InstanceID < 10 {
			t.Fatalf("reward stack = %+v", stack)
		}
	}
}

func TestOpenBoxRollsBackWhenAllRewardsWillNotFit(t *testing.T) {
	items := []inventory.Stack{
		{Slot: 0, InstanceID: 1, ItemID: box, Count: 1},
		{Slot: 1, InstanceID: 2, ItemID: key, Count: 1},
	}
	for slot := int32(2); slot < 16; slot++ {
		items = append(items, inventory.Stack{Slot: slot, InstanceID: uint64(slot + 1), ItemID: sword, Count: 1})
	}
	repository := &repository{state: baseState(items...)}
	catalog := fixtureCatalog(t)
	definition := catalog[box]
	definition.Box = &itemactions.BoxDefinition{
		KeyItemID: key, ConsumeKey: true,
		Rewards: []inventory.Grant{{ItemID: reward, Count: 3}},
	}
	catalog[box] = definition
	service, err := itemactions.NewService(repository, repository, catalog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := repository.snapshot()
	_, err = service.OpenBox(context.Background(), actor(), itemactions.OpenBoxCommand{
		RequestID: 1, ExpectedRevision: 10, BoxSlot: 0, KeySlot: 1,
	})
	if !errors.Is(err, inventory.ErrBagFull) {
		t.Fatalf("OpenBox() error = %v", err)
	}
	if got := repository.snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed OpenBox() changed state: %+v", got)
	}
}

func TestOpenBoxDoesNotMergeDefaultRewardsIntoMutableInstance(t *testing.T) {
	repository := &repository{state: baseState(
		inventory.Stack{Slot: 0, InstanceID: 1, ItemID: box, Count: 1},
		inventory.Stack{Slot: 1, InstanceID: 2, ItemID: key, Count: 1},
		inventory.Stack{Slot: 2, InstanceID: 3, ItemID: reward, Count: 1, Bound: true},
	)}
	result, err := serviceFor(t, repository, nil, nil).OpenBox(context.Background(), actor(), itemactions.OpenBoxCommand{
		RequestID: 1, ExpectedRevision: 10, BoxSlot: 0, KeySlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.State.Inventory) != 3 || result.State.Inventory[2].InstanceID != 3 ||
		result.State.Inventory[2].Count != 1 || !result.State.Inventory[2].Bound {
		t.Fatalf("mutable reward instance changed or absorbed defaults: %+v", result.State.Inventory)
	}
}

func TestEquippingSmallerBagRefusesToStrandOccupiedSlots(t *testing.T) {
	oldBag := inventory.Stack{Slot: -1, InstanceID: 40, ItemID: "item.bag.16", Count: 1}
	repository := &repository{state: baseState(
		inventory.Stack{Slot: 0, InstanceID: 41, ItemID: bag12, Count: 1},
		inventory.Stack{Slot: 15, InstanceID: 42, ItemID: sword, Count: 1},
	)}
	repository.state.Bag = &oldBag
	before := repository.snapshot()
	_, err := serviceFor(t, repository, nil, nil).Equip(context.Background(), actor(), itemactions.EquipCommand{
		RequestID: 1, ExpectedRevision: 10, BagSlot: 0, DressSlot: itemactions.EquipmentBag,
	})
	if !errors.Is(err, itemactions.ErrBagWouldShrink) {
		t.Fatalf("Equip(smaller bag) error = %v", err)
	}
	if got := repository.snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("smaller bag refusal changed state: %+v", got)
	}
}

func TestEquipSwapBindsAndQueuesExactEvents(t *testing.T) {
	sink := &eventSink{}
	repository := &repository{state: baseState(inventory.Stack{Slot: 3, InstanceID: 11, ItemID: sword, Count: 1})}
	repository.state.Equipment = []itemactions.EquippedItem{{
		Slot: itemactions.EquipmentMainhand,
		Item: inventory.Stack{Slot: -1, InstanceID: 12, ItemID: sword, Count: 1},
	}}
	result, err := serviceFor(t, repository, nil, sink).Equip(context.Background(), actor(), itemactions.EquipCommand{
		RequestID: 99, ExpectedRevision: 10, BagSlot: 3, DressSlot: itemactions.EquipmentMainhand,
	})
	if err != nil {
		t.Fatalf("Equip() error = %v", err)
	}
	if got := result.State.Equipment[0].Item; got.InstanceID != 11 || !got.Bound {
		t.Fatalf("equipped item = %+v", got)
	}
	if got := result.State.Inventory[0]; got.InstanceID != 12 || got.Slot != 3 {
		t.Fatalf("returned item = %+v", got)
	}
	if len(result.Events) != 2 || result.Events[0].Equipped || !result.Events[1].Equipped ||
		result.Events[1].TriggerSlot != "MAINHAND" {
		t.Fatalf("events = %+v", result.Events)
	}
	if !reflect.DeepEqual(sink.events, result.Events) {
		t.Fatalf("queued events = %+v, want %+v", sink.events, result.Events)
	}
}

func TestEquipCannotReplaceCursedItem(t *testing.T) {
	repository := &repository{state: baseState(inventory.Stack{Slot: 3, InstanceID: 11, ItemID: sword, Count: 1})}
	repository.state.Equipment = []itemactions.EquippedItem{{
		Slot: itemactions.EquipmentMainhand,
		Item: inventory.Stack{Slot: -1, InstanceID: 12, ItemID: sword, Count: 1, Cursed: true},
	}}
	before := repository.snapshot()
	_, err := serviceFor(t, repository, nil, nil).Equip(context.Background(), actor(), itemactions.EquipCommand{
		RequestID: 1, ExpectedRevision: 10, BagSlot: 3, DressSlot: itemactions.EquipmentMainhand,
	})
	if !errors.Is(err, itemactions.ErrCursed) {
		t.Fatalf("Equip() error = %v", err)
	}
	if got := repository.snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("cursed replacement changed state: %+v", got)
	}
}

func TestUnequipAutoDestinationAndCursedRefusal(t *testing.T) {
	for _, cursed := range []bool{false, true} {
		repository := &repository{state: baseState(inventory.Stack{Slot: 0, InstanceID: 1, ItemID: tonic, Count: 1})}
		repository.state.Equipment = []itemactions.EquippedItem{{
			Slot: itemactions.EquipmentOffhand,
			Item: inventory.Stack{Slot: -1, InstanceID: 2, ItemID: shield, Count: 1, Cursed: cursed},
		}}
		result, err := serviceFor(t, repository, nil, nil).Unequip(context.Background(), actor(), itemactions.UnequipCommand{
			RequestID: 1, ExpectedRevision: 10, BagSlot: -1, DressSlot: itemactions.EquipmentOffhand,
		})
		if cursed {
			if !errors.Is(err, itemactions.ErrCursed) || repository.snapshot().Revision != 10 {
				t.Fatalf("cursed Unequip() error/state = %v %+v", err, repository.snapshot())
			}
			continue
		}
		if err != nil {
			t.Fatalf("Unequip() error = %v", err)
		}
		if len(result.State.Equipment) != 0 || result.State.Inventory[1].Slot != 1 || result.State.Inventory[1].InstanceID != 2 {
			t.Fatalf("Unequip() state = %+v", result.State)
		}
	}
}

func TestUseConsumesOnlyAfterPreparationAndCommitsCooldown(t *testing.T) {
	repository := &repository{state: baseState(inventory.Stack{Slot: 4, InstanceID: 55, ItemID: tonic, Count: 3})}
	authority := &useAuthority{resource: itemactions.ActionResource{Kind: itemactions.ActionResourceActiveItem, Count: 1}}
	result, cooldown, err := serviceFor(t, repository, authority, nil).Use(context.Background(), actor(), itemactions.UseCommand{
		RequestID: 2, ExpectedRevision: 10,
		Location: itemactions.ItemLocation{HasSlot: true, Slot: 4, Kind: itemactions.LocationItemBag},
	})
	if err != nil {
		t.Fatalf("Use() error = %v", err)
	}
	if result.State.Revision != 11 || repository.snapshot().Revision != 11 ||
		result.State.Inventory[0].Count != 2 || cooldown == nil ||
		authority.lastID != 55 || authority.lastAction != potionAction ||
		authority.commits != 1 || authority.aborts != 0 {
		t.Fatalf("Use() result=%+v cooldown=%+v authority=%+v", result, cooldown, authority)
	}
	_, _, err = serviceFor(t, repository, authority, nil).Use(context.Background(), actor(), itemactions.UseCommand{
		RequestID: 3, ExpectedRevision: 11,
		Location: itemactions.ItemLocation{HasSlot: true, Slot: 0, Kind: itemactions.LocationDeposit},
	})
	if !errors.Is(err, itemactions.ErrUnsupportedAction) {
		t.Fatalf("deposit Use() error = %v", err)
	}
}

func TestUseAbortsPreparedWorldActionWhenPersistenceFails(t *testing.T) {
	repository := &repository{
		state:      baseState(inventory.Stack{Slot: 4, InstanceID: 55, ItemID: tonic, Count: 3}),
		failCommit: true,
	}
	authority := &useAuthority{resource: itemactions.ActionResource{Kind: itemactions.ActionResourceActiveItem, Count: 1}}
	before := repository.snapshot()
	_, cooldown, err := serviceFor(t, repository, authority, nil).Use(context.Background(), actor(), itemactions.UseCommand{
		RequestID: 2, ExpectedRevision: 10,
		Location: itemactions.ItemLocation{HasSlot: true, Slot: 4, Kind: itemactions.LocationItemBag},
	})
	if !errors.Is(err, errCommit) || cooldown != nil {
		t.Fatalf("Use() error/cooldown = %v %+v", err, cooldown)
	}
	if got := repository.snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed Use() changed persistence: %+v", got)
	}
	if authority.commits != 0 || authority.aborts != 1 {
		t.Fatalf("prepared action commits=%d aborts=%d", authority.commits, authority.aborts)
	}
}

func TestNonConsumingUseLeavesRevisionAndInstanceUntouched(t *testing.T) {
	repository := &repository{state: baseState(inventory.Stack{Slot: 4, InstanceID: 55, ItemID: tonic, Count: 1})}
	authority := &useAuthority{resource: itemactions.ActionResource{Kind: itemactions.ActionResourceNone}}
	result, cooldown, err := serviceFor(t, repository, authority, nil).Use(context.Background(), actor(), itemactions.UseCommand{
		RequestID: 2, ExpectedRevision: 10,
		Location: itemactions.ItemLocation{HasSlot: true, Slot: 4, Kind: itemactions.LocationItemBag},
	})
	if err != nil || cooldown == nil {
		t.Fatalf("Use() = %+v, %+v, %v", result, cooldown, err)
	}
	if result.State.Revision != 10 || result.State.Inventory[0].InstanceID != 55 ||
		result.State.Inventory[0].Count != 1 || authority.commits != 1 {
		t.Fatalf("non-consuming Use() state=%+v authority=%+v", result.State, authority)
	}
}

func totalCount(stacks []inventory.Stack) int32 {
	var result int32
	for _, stack := range stacks {
		result += stack.Count
	}
	return result
}
