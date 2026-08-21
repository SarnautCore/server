package loot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/pack"
)

const (
	partialTakeActor  = uint64(101)
	partialTakeCorpse = uint64(202)
)

var partialTakeOwner = uuid.MustParse("019200f0-0000-7000-8000-0000000f0202")

type lockedTestZone struct{ mutex sync.Mutex }

func (*lockedTestZone) ID() string { return "partial-take-test" }

func (zone *lockedTestZone) GameCommand(command func(gametypes.Tick) error) error {
	zone.mutex.Lock()
	defer zone.mutex.Unlock()
	return command(nil)
}

func (*lockedTestZone) GameAddSystem(gametypes.System) {}

type recordingAwarder struct {
	mutex   sync.Mutex
	awards  []inventory.Award
	err     error
	started chan struct{}
	release chan struct{}
}

func (awarder *recordingAwarder) Award(
	_ context.Context,
	_ uuid.UUID,
	award inventory.Award,
) (inventory.Result, error) {
	awarder.mutex.Lock()
	awarder.awards = append(awarder.awards, inventory.Award{
		Money:  award.Money,
		Grants: append([]inventory.Grant(nil), award.Grants...),
	})
	call := len(awarder.awards)
	awarder.mutex.Unlock()
	if call == 1 && awarder.started != nil {
		close(awarder.started)
		<-awarder.release
	}
	if awarder.err != nil {
		return inventory.Result{}, awarder.err
	}
	return inventory.Result{Currency: award.Money, SaveSeq: int64(call)}, nil
}

func (awarder *recordingAwarder) snapshot() []inventory.Award {
	awarder.mutex.Lock()
	defer awarder.mutex.Unlock()
	result := make([]inventory.Award, len(awarder.awards))
	for index, award := range awarder.awards {
		result[index] = inventory.Award{
			Money:  award.Money,
			Grants: append([]inventory.Grant(nil), award.Grants...),
		}
	}
	return result
}

func newPartialTakeModule(drop Drop, awarder inventory.Awarder) *Module {
	zone := &lockedTestZone{}
	module := New(slog.New(slog.NewTextHandler(io.Discard, nil)), zone, Rules{}, awarder, Options{})
	module.owners[partialTakeActor] = partialTakeOwner
	module.corpses[partialTakeCorpse] = &corpse{
		containerID: partialTakeCorpse,
		owner:       partialTakeOwner,
		tableID:     "loot.test.partial",
		drop:        drop.Clone(),
	}
	return module
}

func TestLootWindowConstantsAndEntryLimit(t *testing.T) {
	if MaxObservableLootEntries != 20 || LootPageSize != 4 {
		t.Fatalf("loot window = %d entries / %d per page, want 20 / 4",
			MaxObservableLootEntries, LootPageSize)
	}

	entries := make([]pack.LootNode, MaxObservableLootEntries)
	chances := make([]float64, MaxObservableLootEntries)
	for index := range entries {
		entries[index] = pack.LootNode{
			Kind: pack.LootNodeSingleItem, ItemID: "item.test", MinNumber: 1, MaxNumber: 1,
		}
		chances[index] = 1
	}
	table := pack.LootTable{ID: "loot.test.twenty", Root: pack.LootNode{
		Kind: pack.LootNodeAnd, Entries: entries, Chances: chances,
	}}
	drop, err := Evaluate(table, NewScriptedStream(0))
	if err != nil || len(drop.Items) != MaxObservableLootEntries {
		t.Fatalf("20-entry Evaluate() = %d items, %v", len(drop.Items), err)
	}

	table.Root.Entries = append(table.Root.Entries, entries[0])
	table.Root.Chances = append(table.Root.Chances, 1)
	if _, err := Evaluate(table, NewScriptedStream(0)); !errors.Is(err, ErrLootEntryLimit) {
		t.Fatalf("21-entry Evaluate() error = %v, want ErrLootEntryLimit", err)
	}
}

func TestTakeItemUsesOrderedIndexIdentityForDuplicateIDs(t *testing.T) {
	awarder := &recordingAwarder{}
	module := newPartialTakeModule(Drop{
		Money: 9,
		Items: []ItemGrant{
			{ItemID: "item.same", Count: 1, IsCursed: false},
			{ItemID: "item.same", Count: 7, IsCursed: false},
			{ItemID: "item.tail", Count: 3, IsCursed: false},
		},
	}, awarder)

	result, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 1)
	if err != nil {
		t.Fatalf("TakeItem() error = %v", err)
	}
	if want := []ItemGrant{{ItemID: "item.same", Count: 7, IsCursed: false}}; !reflect.DeepEqual(result.Items, want) {
		t.Fatalf("TakeItem() items = %+v, want %+v", result.Items, want)
	}
	if got, want := awarder.snapshot(), []inventory.Award{{
		Grants: []inventory.Grant{{ItemID: "item.same", Count: 7, Cursed: false}},
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("awards = %+v, want %+v", got, want)
	}

	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone {
		t.Fatalf("Look() refusal = %s", refusal)
	}
	wantRemaining := []ItemGrant{
		{ItemID: "item.same", Count: 1, IsCursed: false},
		{ItemID: "item.tail", Count: 3, IsCursed: false},
	}
	if offer.Money != 9 || !reflect.DeepEqual(offer.Items, wantRemaining) {
		t.Fatalf("remaining offer = money %d, items %+v; want 9, %+v", offer.Money, offer.Items, wantRemaining)
	}
}

func TestInvalidItemIndexIsTypedAndCannotMutate(t *testing.T) {
	awarder := &recordingAwarder{}
	original := Drop{Money: 5, Items: []ItemGrant{{ItemID: "item.one", Count: 2, IsCursed: false}}}
	module := newPartialTakeModule(original, awarder)

	for _, index := range []int32{-2, 1, 20} {
		result, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, index)
		if !errors.Is(err, ErrInvalidItemIndex) || result.Refusal != RefusalInvalidItemIndex {
			t.Errorf("TakeItem(%d) = refusal %s, error %v; want invalid_item_index", index, result.Refusal, err)
		}
	}
	if got := awarder.snapshot(); len(got) != 0 {
		t.Fatalf("invalid indices reached Award(): %+v", got)
	}
	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone || offer.Money != original.Money || !reflect.DeepEqual(offer.Items, original.Items) {
		t.Fatalf("invalid indices mutated corpse: offer=%+v refusal=%s", offer, refusal)
	}
}

func TestFailedPartialAwardReleasesTheWholeCorpseUnchanged(t *testing.T) {
	injected := errors.New("partial award failed")
	awarder := &recordingAwarder{err: injected}
	original := Drop{
		Money: 13,
		Items: []ItemGrant{
			{ItemID: "item.a", Count: 1, IsCursed: false},
			{ItemID: "item.b", Count: 2, IsCursed: true},
		},
	}
	module := newPartialTakeModule(original, awarder)

	result, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 1)
	if !errors.Is(err, injected) || result.Refusal != RefusalInternal {
		t.Fatalf("TakeItem() = refusal %s, error %v; want internal/injected", result.Refusal, err)
	}
	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone || offer.Money != original.Money || !reflect.DeepEqual(offer.Items, original.Items) {
		t.Fatalf("failed award changed corpse: offer=%+v refusal=%s", offer, refusal)
	}

	awarder.err = nil
	if _, err := module.TakeMoney(context.Background(), partialTakeActor, partialTakeCorpse); err != nil {
		t.Fatalf("TakeMoney() after release error = %v", err)
	}
}

func TestTakeMoneyCreditsOnlyMoneyAndLastComponentMarksLooted(t *testing.T) {
	awarder := &recordingAwarder{}
	module := newPartialTakeModule(Drop{
		Money: 17,
		Items: []ItemGrant{{ItemID: "item.left", Count: 4, IsCursed: false}},
	}, awarder)

	money, err := module.TakeMoney(context.Background(), partialTakeActor, partialTakeCorpse)
	if err != nil {
		t.Fatalf("TakeMoney() error = %v", err)
	}
	if money.Money != 17 || len(money.Items) != 0 {
		t.Fatalf("TakeMoney() result = money %d, items %+v", money.Money, money.Items)
	}
	if got, want := awarder.snapshot(), []inventory.Award{{Money: 17}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("money awards = %+v, want %+v", got, want)
	}
	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone || offer.Money != 0 || len(offer.Items) != 1 {
		t.Fatalf("offer after money = %+v, refusal %s", offer, refusal)
	}

	if _, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 0); err != nil {
		t.Fatalf("taking last item error = %v", err)
	}
	if _, refusal := module.Look(partialTakeActor, partialTakeCorpse); refusal != RefusalAlreadyLooted {
		t.Fatalf("Look() after last component = %s, want already_looted", refusal)
	}
}

func TestEveryConcurrentTakeVerbRefusesWhileAwardIsInProgress(t *testing.T) {
	awarder := &recordingAwarder{started: make(chan struct{}), release: make(chan struct{})}
	module := newPartialTakeModule(Drop{
		Money: 23,
		Items: []ItemGrant{
			{ItemID: "item.a", Count: 1, IsCursed: false},
			{ItemID: "item.b", Count: 2, IsCursed: false},
		},
	}, awarder)

	done := make(chan error, 1)
	go func() {
		_, err := module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 0)
		done <- err
	}()
	<-awarder.started

	operations := []struct {
		name string
		take func() (Result, error)
	}{
		{"item", func() (Result, error) {
			return module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, 1)
		}},
		{"money", func() (Result, error) {
			return module.TakeMoney(context.Background(), partialTakeActor, partialTakeCorpse)
		}},
		{"all", func() (Result, error) {
			return module.TakeAll(context.Background(), partialTakeActor, partialTakeCorpse)
		}},
	}
	for _, operation := range operations {
		result, err := operation.take()
		if !errors.Is(err, ErrTakeInProgress) || result.Refusal != RefusalInProgress {
			t.Errorf("concurrent %s = refusal %s, error %v; want in_progress", operation.name, result.Refusal, err)
		}
	}

	close(awarder.release)
	if err := <-done; err != nil {
		t.Fatalf("reserved TakeItem() error = %v", err)
	}
	if got := awarder.snapshot(); len(got) != 1 {
		t.Fatalf("Award() calls = %d, want only the reserved operation", len(got))
	}
	offer, refusal := module.Look(partialTakeActor, partialTakeCorpse)
	if refusal != RefusalNone || offer.Money != 23 || !reflect.DeepEqual(offer.Items,
		[]ItemGrant{{ItemID: "item.b", Count: 2, IsCursed: false}}) {
		t.Fatalf("remaining offer = %+v, refusal %s", offer, refusal)
	}
}

func TestTakeAndMinusOneSelectorRemainTakeAllCompatible(t *testing.T) {
	for name, take := range map[string]func(*Module) (Result, error){
		"Take": func(module *Module) (Result, error) {
			return module.Take(context.Background(), partialTakeActor, partialTakeCorpse)
		},
		"TakeItem(-1)": func(module *Module) (Result, error) {
			return module.TakeItem(context.Background(), partialTakeActor, partialTakeCorpse, -1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			awarder := &recordingAwarder{}
			original := Drop{Money: 3, Items: []ItemGrant{{ItemID: "item.all", Count: 2, IsCursed: false}}}
			module := newPartialTakeModule(original, awarder)
			result, err := take(module)
			if err != nil || result.Money != original.Money || !reflect.DeepEqual(result.Items, original.Items) {
				t.Fatalf("take-all result = %+v, error %v", result, err)
			}
			if _, refusal := module.Look(partialTakeActor, partialTakeCorpse); refusal != RefusalAlreadyLooted {
				t.Fatalf("Look() after take-all = %s", refusal)
			}
		})
	}
}
