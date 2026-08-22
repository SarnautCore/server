//go:build integration

package loot_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

// postgresDSNEnvironment is the same variable every service reads.
const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

// TestLootSurvivesADisconnectAndReconnect is the end-to-end persistence claim,
// and it needs a real database rather than the in-memory store.
//
// What the in-memory store cannot prove is that the award's bag write and purse
// credit are one PostgreSQL transaction, that `shard.character_inventory`'s
// primary key holds the slot layout, and that the `currency` column added by
// migration 00002 survives a reload. So: kill, loot, drop every in-memory
// structure the session had, and read the bag back through a fresh repository.
func TestLootSurvivesADisconnectAndReconnect(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("skipping database-backed loot test: %s is not set", postgresDSNEnvironment)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator() error = %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	anchor, ok := anchorOf(content, lootTargetMob)
	if !ok {
		t.Fatalf("fixture pack has no live placement spawning %q", lootTargetMob)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "LootIntegration",
		TickInterval:     tickInterval,
		SnapshotInterval: 2 * tickInterval,
		MaxMoveSpeed:     6,
		PlayerSpawn:      anchor.Add(world.Vec3{X: 2}),
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
	bags, err := charstore.NewInventoryService(repository, inventory.LimitsFromPack(content), 0)
	if err != nil {
		t.Fatalf("inventory.NewService() error = %v", err)
	}
	lootRules, err := loot.RulesFromPack(content)
	if err != nil {
		t.Fatalf("loot.RulesFromPack() error = %v", err)
	}
	lootModule := loot.New(slog.New(slog.DiscardHandler), zone, lootRules, bags, loot.Options{
		WorldSeed: "loot-integration",
	})
	combatModule.SetKillSink(lootSink{module: lootModule})

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go combatModule.Run(runCtx)

	// Checkpoint L1, as the session would have written it.
	characterID := uuid.New()
	err = charstore.SaveCharacter(ctx, repository, charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      zone.ID(),
			Level:       1,
			Health:      100,
			SaveSeq:     1,
		},
	})
	if err != nil {
		t.Fatalf("seed character state: %v", err)
	}

	playerID, _ := zone.Join()
	if err := combatModule.Admit(playerID, combat.PlayerAdmission{
		Level: 1, Health: combat.MaxHealth(1, 1), MaxHealth: combat.MaxHealth(1, 1),
		AbilityIDs: combatRules.AbilityIDs(),
	}); err != nil {
		t.Fatalf("combat.Admit() error = %v", err)
	}
	if err := zone.Subscribe(playerID, discardSnapshots{}); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	lootModule.Admit(playerID, characterID)

	var mobID uint64
	_ = zone.Command(func(tick *world.Tick) error {
		tick.Each(func(entity *world.Entity) bool {
			if entity.ContentID == lootTargetMob && entity.Alive {
				mobID = entity.ID
				return false
			}
			return true
		})
		return nil
	})
	if mobID == 0 {
		t.Fatalf("no live %s in the zone", lootTargetMob)
	}

	var containerID uint64
	deadline := time.Now().Add(30 * time.Second)
	for containerID == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the mob never died")
		}
		_, _ = combatModule.UseAbility(playerID, combat.AbilityRequest{TargetID: mobID})
		if found, ok := lootModule.CorpseFor(mobID); ok {
			containerID = found
			break
		}
		zone.Step()
	}

	result, err := lootModule.Take(ctx, playerID, containerID)
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	if result.Refusal != loot.RefusalNone {
		t.Fatalf("Take() refused with %s", result.Refusal)
	}

	// Disconnect. The entity, the session and every in-memory view go away.
	combatModule.Release(playerID)
	lootModule.Release(playerID)
	zone.Leave(playerID)
	cancel()

	// Reconnect through a repository built from scratch, so nothing the first
	// half of this test held in memory can answer the question.
	fresh, err := charstore.NewPostgres(pool)
	if err != nil {
		t.Fatalf("second NewPostgres() error = %v", err)
	}
	reloaded, err := fresh.LoadInventory(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadInventory() after reconnect error = %v", err)
	}

	if len(reloaded) != len(result.Slots) {
		t.Fatalf("reloaded %d slots, the award committed %d", len(reloaded), len(result.Slots))
	}
	for index, stack := range result.Slots {
		got := reloaded[index]
		if got.Slot != stack.Slot || got.ItemID != stack.ItemID || got.Quantity != stack.Count {
			t.Errorf("slot %d reloaded as %+v, want %+v", index, got, stack)
		}
	}
	if got := unitsOf(reloaded, tonicItemID); got != dropCount {
		t.Errorf("reloaded bag holds %d of %s, want %d", got, tonicItemID, dropCount)
	}

	state, err := fresh.LoadCharacterState(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() after reconnect error = %v", err)
	}
	if state.Currency != result.Currency {
		t.Errorf("reloaded purse = %d, the award committed %d", state.Currency, result.Currency)
	}
}
