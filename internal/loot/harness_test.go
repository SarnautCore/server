package loot_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

// The loot target is the copper sparrow, and the choice is deliberate.
//
// Its table is `loot.fixture.m2-flat`, whose first entry has a chance of 1.0
// and a count range of 45 to 45. Every kill therefore drops exactly 45 harbor
// tonics whatever the seed does, which is what lets the tests below assert a
// bag layout instead of asserting "something dropped". The other two entries
// have chances of 0.00618751 and 0.0, so they almost never and never fire.
const (
	lootTargetMob = "mob.paper-harbor.copper-sparrow"
	// The exact drop is content: 45 units of a 20-limit item, worked example
	// 6.2 to the digit.
	dropCount = 45
)

const tickInterval = time.Second / 30

// harnessCharacters are the character ids this harness admits, in order. They
// are fixed because they seed the loot roll; see [harness.join].
var harnessCharacters = []uuid.UUID{
	uuid.MustParse("019200f0-0000-7000-8000-0000000f0001"),
	uuid.MustParse("019200f0-0000-7000-8000-0000000f0002"),
}

// harness is a zone with combat, loot, a bag service over an in-memory store,
// and two admitted players — the second one exists only so that rule 5.8.2 has
// somebody to refuse.
type harness struct {
	t          *testing.T
	zone       *world.Zone
	combat     *combat.Module
	loot       *loot.Module
	repository charstore.Repository

	ownerEntity    uint64
	ownerID        uuid.UUID
	strangerEntity uint64
	strangerID     uuid.UUID
	mobEntity      uint64
	// joined is how many characters this harness has admitted, and indexes
	// harnessCharacters.
	joined int
}

type harnessOptions struct {
	// slots is the bag capacity. Zero means inventory.DefaultSlots.
	slots int32
	// repository replaces the in-memory one, for the abort test.
	repository charstore.Repository
}

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	anchor, ok := anchorOf(content, lootTargetMob)
	if !ok {
		t.Fatalf("fixture pack has no live placement spawning %q", lootTargetMob)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "LootFixture",
		TickInterval:     tickInterval,
		SnapshotInterval: 2 * tickInterval,
		MaxMoveSpeed:     6,
		// Two metres from the anchor: inside every ability range the pack
		// carries, so no test has to walk anywhere.
		PlayerSpawn: anchor.Add(world.Vec3{X: 2}),
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

	repository := options.repository
	if repository == nil {
		repository = charstore.NewMemory()
	}
	bags, err := charstore.NewInventoryService(repository, inventory.LimitsFromPack(content), options.slots)
	if err != nil {
		t.Fatalf("inventory.NewService() error = %v", err)
	}
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("loot.RulesFromPack() error = %v", err)
	}
	lootModule := loot.New(slog.New(slog.DiscardHandler), zone, lootRules, bags, loot.Options{
		WorldSeed: "loot-fixture",
	})
	combatModule.SetKillSink(lootSink{module: lootModule})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go combatModule.Run(ctx)

	fixture := &harness{
		t:          t,
		zone:       zone,
		combat:     combatModule,
		loot:       lootModule,
		repository: repository,
	}
	fixture.ownerEntity, fixture.ownerID = fixture.join(repository)
	fixture.strangerEntity, fixture.strangerID = fixture.join(repository)
	fixture.mobEntity = fixture.findMob(lootTargetMob)
	return fixture
}

type lootSink struct{ module *loot.Module }

func (sink lootSink) MobKilled(tick gametypes.Tick, kill combat.Kill) {
	sink.module.MobKilled(tick, loot.Kill{
		VictimEntityID: kill.VictimEntityID, KillerEntityID: kill.KillerEntityID,
		VictimContentID: kill.VictimContentID, PlacementID: kill.PlacementID,
		LootTableID: kill.LootTableID, VictimLevel: kill.VictimLevel,
		Position: kill.Position, Heading: kill.Heading,
		DeathTick: kill.DeathTick, DespawnTick: kill.DespawnTick,
	})
}

// join admits one player and seeds the character row checkpoint L1 would have
// written, because the award reads the stored state to credit the purse.
//
// The character id is drawn from a fixed list rather than generated. It is one
// of the four seed inputs of rule 5.2.4, so a fresh id per run means a fresh
// drop per run: the chance-gated entries of the flat tree appear on roughly one
// run in a hundred and fail whichever assertion counted the grants. Reproducible
// rolls are the whole point of the seed, and a harness that randomised one was
// the only thing in this package not taking it seriously.
func (fixture *harness) join(repository charstore.Repository) (uint64, uuid.UUID) {
	fixture.t.Helper()
	entityID, _ := fixture.zone.Join()
	if err := fixture.combat.Admit(entityID); err != nil {
		fixture.t.Fatalf("combat.Admit() error = %v", err)
	}
	if err := fixture.zone.Subscribe(entityID, discardSnapshots{}); err != nil {
		fixture.t.Fatalf("Subscribe() error = %v", err)
	}
	if fixture.joined >= len(harnessCharacters) {
		fixture.t.Fatalf("the harness admits %d characters, not more", len(harnessCharacters))
	}
	characterID := harnessCharacters[fixture.joined]
	fixture.joined++
	err := charstore.SaveCharacter(context.Background(), repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      fixture.zone.ID(),
			Level:       1,
			Health:      100,
			SaveSeq:     1,
		},
	})
	if err != nil {
		fixture.t.Fatalf("seed character state: %v", err)
	}
	fixture.loot.Admit(entityID, characterID)
	return entityID, characterID
}

func (fixture *harness) findMob(contentID string) uint64 {
	fixture.t.Helper()
	var found uint64
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		tick.Each(func(entity *world.Entity) bool {
			if entity.ContentID == contentID && entity.Alive {
				found = entity.ID
				return false
			}
			return true
		})
		return nil
	})
	if found == 0 {
		fixture.t.Fatalf("no spawned entity carries content id %q", contentID)
	}
	return found
}

// kill casts at the mob until it dies, stepping ticks through the cooldown the
// pack decides. It returns the corpse container the loot module stood up.
func (fixture *harness) kill() uint64 {
	fixture.t.Helper()
	for attempt := 0; attempt < 4000; attempt++ {
		_, err := fixture.combat.UseAbility(fixture.ownerEntity, combat.AbilityRequest{
			TargetID: fixture.mobEntity,
		})
		if err != nil && !errors.Is(err, combat.ErrOnCooldown) {
			fixture.t.Fatalf("UseAbility() rejected with %v", err)
		}
		if containerID, ok := fixture.loot.CorpseFor(fixture.mobEntity); ok {
			return containerID
		}
		fixture.zone.Step()
	}
	fixture.t.Fatal("the mob never died")
	return 0
}

func (fixture *harness) step(count int) {
	for index := 0; index < count; index++ {
		fixture.zone.Step()
	}
}

func (fixture *harness) inventoryOf(characterID uuid.UUID) []charstore.InventoryItem {
	fixture.t.Helper()
	items, err := fixture.repository.LoadInventory(context.Background(), characterID)
	if err != nil {
		fixture.t.Fatalf("LoadInventory() error = %v", err)
	}
	return items
}

// unitsOf totals the quantity of one item across every slot, which is the
// number that has to be exactly one drop's worth after a double take.
func unitsOf(items []charstore.InventoryItem, itemID string) int32 {
	var total int32
	for _, item := range items {
		if item.ItemID == itemID {
			total += item.Quantity
		}
	}
	return total
}

func anchorOf(content *pack.Pack, mobID string) (world.Vec3, bool) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID != mobID {
			continue
		}
		return world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}, true
	}
	return world.Vec3{}, false
}

type discardSnapshots struct{}

func (discardSnapshots) OfferSnapshot(world.Snapshot) {}
