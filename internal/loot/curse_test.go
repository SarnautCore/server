package loot

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/pack"
)

type curseItemSource map[string]pack.Item

func (source curseItemSource) Item(id string) (pack.Item, bool) {
	item, ok := source[id]
	return item, ok
}

func curseItem(id string) pack.LootNode {
	return pack.LootNode{
		Kind:      pack.LootNodeSingleItem,
		ItemID:    id,
		MinNumber: 1,
		MaxNumber: 1,
	}
}

func TestCurseRollRerunsTheWholeTablePastAnOrdinaryDuplicate(t *testing.T) {
	table := pack.LootTable{ID: "loot.curse.reroll", Root: pack.LootNode{
		Kind:    pack.LootNodeOr,
		Entries: []pack.LootNode{curseItem("item.a"), curseItem("item.b")},
		Chances: []float64{0.5, 0.5},
	}}
	items := curseItemSource{
		"item.a": {ID: "item.a", CurseEligible: true},
		"item.b": {ID: "item.b", CurseEligible: true},
	}
	stream := NewScriptedStream(
		0.1, 0, // ordinary item.a
		0.1, 0, // attempt one duplicates item.a
		0.9, 0, // attempt two selects item.b
		0.1, // item.b passes the curse probability
	)

	drop, err := EvaluateWithCurses(table, items, stream)
	if err != nil {
		t.Fatalf("EvaluateWithCurses() error = %v", err)
	}
	want := []ItemGrant{
		{ItemID: "item.a", Count: 1, IsCursed: false},
		{ItemID: "item.b", Count: 1, IsCursed: true},
	}
	if !reflect.DeepEqual(drop.Items, want) {
		t.Fatalf("EvaluateWithCurses() items = %+v, want %+v", drop.Items, want)
	}
	if stream.Draws() != 7 {
		t.Errorf("EvaluateWithCurses() draws = %d, want 7", stream.Draws())
	}
}

func TestFinalCurseRollDropsOnlyOrdinaryDuplicates(t *testing.T) {
	table := pack.LootTable{ID: "loot.curse.final", Root: pack.LootNode{
		Kind:    pack.LootNodeAnd,
		Entries: []pack.LootNode{curseItem("item.a"), curseItem("item.b")},
		Chances: []float64{0.5, 0.5},
	}}
	items := curseItemSource{
		"item.a": {ID: "item.a", CurseEligible: true},
		"item.b": {ID: "item.b", CurseEligible: true},
	}
	stream := NewScriptedStream(
		0, 0, 1, // ordinary item.a only
		0, 0, 0, 0, // attempt one: item.a and item.b
		0, 0, 0, 0, // attempt two: item.a and item.b
		0, 0, 0, 0, // attempt three: item.a and item.b
		0, // item.b passes; duplicate item.a was removed
	)

	drop, err := EvaluateWithCurses(table, items, stream)
	if err != nil {
		t.Fatalf("EvaluateWithCurses() error = %v", err)
	}
	want := []ItemGrant{
		{ItemID: "item.a", Count: 1, IsCursed: false},
		{ItemID: "item.b", Count: 1, IsCursed: true},
	}
	if !reflect.DeepEqual(drop.Items, want) {
		t.Fatalf("EvaluateWithCurses() items = %+v, want %+v", drop.Items, want)
	}
	if stream.Draws() != 16 {
		t.Errorf("EvaluateWithCurses() draws = %d, want 16", stream.Draws())
	}
}

func TestCurseCandidateBagChecksEachProductOnce(t *testing.T) {
	table := pack.LootTable{ID: "loot.curse.unique-products", Root: pack.LootNode{
		Kind: pack.LootNodeAnd,
		Entries: []pack.LootNode{
			curseItem("item.a"),
			curseItem("item.b"),
			curseItem("item.b"),
		},
		Chances: []float64{0.5, 0.5, 0.5},
	}}
	items := curseItemSource{
		"item.a": {ID: "item.a", CurseEligible: true},
		"item.b": {ID: "item.b", CurseEligible: true},
	}
	stream := NewScriptedStream(
		0, 0, 1, 1, // ordinary item.a only
		1, 0, 0, 0, 0, // candidate has item.b twice
		0, // one probability check for the unique item.b product
	)

	drop, err := EvaluateWithCurses(table, items, stream)
	if err != nil {
		t.Fatalf("EvaluateWithCurses() error = %v", err)
	}
	want := []ItemGrant{
		{ItemID: "item.a", Count: 1, IsCursed: false},
		{ItemID: "item.b", Count: 1, IsCursed: true},
	}
	if !reflect.DeepEqual(drop.Items, want) {
		t.Fatalf("EvaluateWithCurses() items = %+v, want one cursed item.b", drop.Items)
	}
	if stream.Draws() != 10 {
		t.Errorf("EvaluateWithCurses() draws = %d, want 10", stream.Draws())
	}
}

func TestCurseProbabilityMatchesTheAuthoredThreshold(t *testing.T) {
	const samples = 10_000
	hits := 0
	for index := 0; index < samples; index++ {
		value := (float64(index) + 0.5) / samples
		if curseProbabilityHit(value) {
			hits++
		}
	}
	if hits != 3_333 {
		t.Fatalf("curse hits = %d of %d, want 3333", hits, samples)
	}
	if !curseProbabilityHit(0.333299999) || curseProbabilityHit(0.3333) {
		t.Error("curse probability boundary is not [0, 0.3333)")
	}
}

func TestSeededCurseRollIsReplayable(t *testing.T) {
	table := pack.LootTable{ID: "loot.curse.seeded", Root: pack.LootNode{
		Kind:    pack.LootNodeOr,
		Entries: []pack.LootNode{curseItem("item.a"), curseItem("item.b")},
		Chances: []float64{0.5, 0.5},
	}}
	items := curseItemSource{
		"item.a": {ID: "item.a", CurseEligible: true},
		"item.b": {ID: "item.b", CurseEligible: true},
	}
	seed := Seed{
		WorldSeed:         "curse-replay",
		ZoneID:            "paper-harbor",
		SpawnSlotID:       "spawn.curse.1",
		DeathServerTick:   991,
		KillerCharacterID: uuid.MustParse("019200f0-0000-7000-8000-00000000c001"),
	}

	firstStream := NewStream(seed)
	first, err := EvaluateWithCurses(table, items, firstStream)
	if err != nil {
		t.Fatalf("first EvaluateWithCurses() error = %v", err)
	}
	secondStream := NewStream(seed)
	second, err := EvaluateWithCurses(table, items, secondStream)
	if err != nil {
		t.Fatalf("second EvaluateWithCurses() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) || firstStream.Draws() != secondStream.Draws() {
		t.Fatalf("seed replay = %+v/%d then %+v/%d", first, firstStream.Draws(), second, secondStream.Draws())
	}
}
