package loot_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
)

// The fixture tree of mechanics/loot.md section 6.1, transcribed node for node.
//
// It is hand-built rather than loaded from the pack on purpose. The spec calls
// it a curated test fixture and states every number in it; building it here
// means the assertion below is against the spec's tree and not against whatever
// the content pack happens to hold today, and the separate fixture-pack test
// covers the other direction.
func nestedFixture() pack.LootTable {
	return pack.LootTable{
		ID:       "loot.fixture.m2-nested",
		MaxDepth: 3,
		Root: pack.LootNode{
			Kind:    pack.LootNodeAnd,
			Chances: []float64{1.00, 0.50, 0.25},
			Entries: []pack.LootNode{
				{Kind: pack.LootNodeMoney, MinNumber: 2, MaxNumber: 4},
				{
					Kind:    pack.LootNodeOr,
					Chances: []float64{0.30, 0.70},
					Entries: []pack.LootNode{
						{Kind: pack.LootNodeSingleItem, ItemID: "trash-hoof", MinNumber: 1, MaxNumber: 3},
						{
							Kind:    pack.LootNodeAnd,
							Chances: []float64{1.00, 0.40},
							Entries: []pack.LootNode{
								{Kind: pack.LootNodeSingleItem, ItemID: "heal-elixir", MinNumber: 1, MaxNumber: 1},
								{Kind: pack.LootNodeMoney, MinNumber: 10, MaxNumber: 10},
							},
						},
					},
				},
				{Kind: pack.LootNodeSingleItem, ItemID: "trash-hoof", MinNumber: 1, MaxNumber: 1},
			},
		},
	}
}

// TestWorkedExampleSection61 is the unit test mechanics/loot.md section 6.1
// asks for, and it asserts all three things the spec says to assert.
//
// The draw count is the one that earns its keep. The money and the item list
// alone would pass for several wrong implementations — one that draws for a
// skipped subtree, one that skips the draw when min equals max — and the count
// is what tells them apart.
func TestWorkedExampleSection61(t *testing.T) {
	stream := loot.NewScriptedStream(0.10, 0.90, 0.42, 0.55, 0.05, 0.33, 0.80, 0.99)

	drop, err := loot.Evaluate(nestedFixture(), stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if drop.Money != 4 {
		t.Errorf("drop.Money = %d, want 4", drop.Money)
	}
	want := []loot.ItemGrant{{ItemID: "heal-elixir", Count: 1}}
	if len(drop.Items) != len(want) || drop.Items[0] != want[0] {
		t.Errorf("drop.Items = %+v, want %+v", drop.Items, want)
	}
	if stream.Draws() != 8 {
		t.Errorf("stream.Draws() = %d, want 8", stream.Draws())
	}
}

// TestSeedPinnedRoll is the companion test of section 6.1: the scripted-stream
// test pins the evaluator, and this one pins the PCG64 wiring of rule 5.2 that
// the scripted stream deliberately bypasses.
//
// The golden values below were recorded on the first run of this test and are
// frozen. A change to SEED_HASH, to the field order of rule 5.2.2, or to the
// word extraction of rule 5.2.3 fails here instead of quietly reshuffling every
// drop in the game.
//
// The spec's tuple gives `killer_character_id = 1`. Character ids are UUIDs
// here, so it is written as the UUID whose value is one; the seed hashes the
// sixteen bytes either way.
func TestSeedPinnedRoll(t *testing.T) {
	seed := loot.Seed{
		WorldSeed:         "sarnautcore-m2-test",
		ZoneID:            "inst-league1",
		SpawnSlotID:       "spawn.inst-league1.placement.000-020.1-2-server-objects.4",
		DeathServerTick:   150,
		KillerCharacterID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
	}

	const goldenDigest = "066f0cdf736ea88dcd471f0167e62550984169647ee0f579190472b7c11d0b03"
	if got := seed.Hex(); got != goldenDigest {
		t.Errorf("seed.Hex() = %q, want the frozen %q", got, goldenDigest)
	}

	stream := loot.NewStream(seed)
	drop, err := loot.Evaluate(nestedFixture(), stream)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	// This roll happens to produce the same drop the scripted trace does, which
	// is a coincidence of the seed and not a property worth relying on. The
	// digest above is the assertion that pins the wiring; these three pin the
	// arithmetic that the real stream feeds.
	const (
		goldenMoney = int64(4)
		goldenDraws = 8
	)
	if drop.Money != goldenMoney {
		t.Errorf("drop.Money = %d, want the frozen %d", drop.Money, goldenMoney)
	}
	goldenItems := []loot.ItemGrant{{ItemID: "heal-elixir", Count: 1}}
	if len(drop.Items) != len(goldenItems) || drop.Items[0] != goldenItems[0] {
		t.Errorf("drop.Items = %+v, want the frozen %+v", drop.Items, goldenItems)
	}
	if stream.Draws() != goldenDraws {
		t.Errorf("stream.Draws() = %d, want the frozen %d", stream.Draws(), goldenDraws)
	}
}

// TestSeedIsLengthPrefixed is rule 5.2.2's reason for length prefixing, stated
// as a test: without it ("ab", "c") and ("a", "bc") hash to the same seed, and
// two unrelated corpses roll the same drop whenever their ids line up that way.
func TestSeedIsLengthPrefixed(t *testing.T) {
	left := loot.Seed{WorldSeed: "ab", ZoneID: "c"}
	right := loot.Seed{WorldSeed: "a", ZoneID: "bc"}
	if left.Hex() == right.Hex() {
		t.Fatalf("(%q,%q) and (%q,%q) hash to the same seed %s",
			left.WorldSeed, left.ZoneID, right.WorldSeed, right.ZoneID, left.Hex())
	}
}

// TestEverySeedFieldChangesTheSeed guards the field order and the field set: a
// tuple member that stopped being hashed would still pass every other test in
// this package.
func TestEverySeedFieldChangesTheSeed(t *testing.T) {
	base := loot.Seed{
		WorldSeed:         "world",
		ZoneID:            "zone",
		SpawnSlotID:       "slot",
		DeathServerTick:   7,
		KillerCharacterID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"),
	}
	mutations := map[string]loot.Seed{
		"world_seed":          {WorldSeed: "other", ZoneID: "zone", SpawnSlotID: "slot", DeathServerTick: 7, KillerCharacterID: base.KillerCharacterID},
		"zone_id":             {WorldSeed: "world", ZoneID: "other", SpawnSlotID: "slot", DeathServerTick: 7, KillerCharacterID: base.KillerCharacterID},
		"spawn_slot_id":       {WorldSeed: "world", ZoneID: "zone", SpawnSlotID: "other", DeathServerTick: 7, KillerCharacterID: base.KillerCharacterID},
		"death_server_tick":   {WorldSeed: "world", ZoneID: "zone", SpawnSlotID: "slot", DeathServerTick: 8, KillerCharacterID: base.KillerCharacterID},
		"killer_character_id": {WorldSeed: "world", ZoneID: "zone", SpawnSlotID: "slot", DeathServerTick: 7, KillerCharacterID: uuid.MustParse("00000000-0000-0000-0000-00000000000b")},
	}
	for field, mutated := range mutations {
		if mutated.Hex() == base.Hex() {
			t.Errorf("changing %s left the seed at %s", field, base.Hex())
		}
	}
}
