//go:build integration

package session_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/store"
	"github.com/SarnautCore/server/internal/world"
)

// postgresDSNEnvironment is the same variable every service reads.
const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

// TestQuestProgressSurvivesADisconnectAndReconnect is the end-to-end
// persistence claim for mechanics/quests.md, and it needs a real database
// rather than the in-memory store.
//
// What the in-memory store cannot prove is that the turn-in's item write,
// currency credit and quest row are one PostgreSQL transaction, that
// `shard.character_quests`' primary key holds one row per character and quest,
// that the jsonb counters round-trip, and that the `honor` column added by
// migration 00003 survives a reload. So: accept, kill, turn in, drop every
// in-memory structure the session had, and read the quest back through a fresh
// repository.
//
// It composes the modules directly rather than over QUIC. The wire round trip
// is `TestQuestSliceOverQUIC`'s job; what this one is about is what is left in
// the database afterwards.
func TestQuestProgressSurvivesADisconnectAndReconnect(t *testing.T) {
	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("skipping database-backed quest test: %s is not set", postgresDSNEnvironment)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	migrator, err := store.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator() error = %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}
	repository, err := store.NewPostgres(pool)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}

	content := loadFixturePack(t)
	anchor, ok := placementOf(content, questTargetID)
	if !ok {
		t.Fatalf("the fixture pack has no placement for %q", questTargetID)
	}
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "QuestIntegration",
		TickInterval:     time.Second / 30,
		SnapshotInterval: time.Second / 15,
		MaxMoveSpeed:     6,
		PlayerSpawn:      anchor.Add(world.Vec3{X: 6}),
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
	bags, err := inventory.NewService(repository, inventory.LimitsFromPack(content), 0)
	if err != nil {
		t.Fatalf("inventory.NewService() error = %v", err)
	}
	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{})
	if err != nil {
		t.Fatalf("quests.CatalogFromPack() error = %v", err)
	}
	questModule := quests.New(slog.New(slog.DiscardHandler), zone, catalog, bags)
	binding := session.ZoneBinding{World: zone, Combat: combatModule, Quests: questModule}
	combatModule.SetKillSink(binding.KillSink())

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go combatModule.Run(runCtx)
	go questModule.Run(runCtx)

	// Checkpoint L1, as the session would have written it.
	characterID := uuid.New()
	err = store.SaveCharacter(ctx, repository, store.Snapshot{
		State: store.CharacterState{
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
	if err := combatModule.Admit(playerID); err != nil {
		t.Fatalf("combat.Admit() error = %v", err)
	}
	// Replication is what tells the simulation this entity is really here: a
	// mob drops an aggro target that is not replicated and walks home
	// untargetable (mechanics/combat.md rule 5.8.6), so without this the crab
	// would never be attackable a second time.
	if err := zone.Subscribe(playerID, discardSnapshots{}); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := questModule.Admit(playerID, characterID, quests.Character{Level: 1}); err != nil {
		t.Fatalf("quests.Admit() error = %v", err)
	}

	giverID, targetID := findEntities(t, zone, questGiverMob, questTargetID)
	if _, err := questModule.Accept(ctx, playerID, questSliceID, giverID); err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the objective mob never died")
		}
		// A refusal costs nothing (mechanics/combat.md section 6.2), so the loop
		// casts as fast as the server will let it, exactly as a client does.
		_, _ = combatModule.UseAbility(playerID, combat.AbilityRequest{TargetID: targetID})
		zone.Step()
		if questStateOf(t, questModule, characterID, questSliceID) == quests.StateCompletable {
			break
		}
	}

	result, err := questModule.TurnIn(ctx, playerID, questSliceID, giverID)
	if err != nil {
		t.Fatalf("TurnIn() error = %v", err)
	}
	if result.Update.State != quests.StateTurnedIn {
		t.Fatalf("state = %s, want turned-in", result.Update.State)
	}

	// Disconnect. The entity, the module's log and every in-memory view go away.
	combatModule.Release(playerID)
	questModule.Release(playerID)
	zone.Leave(playerID)
	cancel()

	// Reconnect through a repository built from scratch, so nothing the first
	// half of this test held in memory can answer the question.
	fresh, err := store.NewPostgres(pool)
	if err != nil {
		t.Fatalf("second NewPostgres() error = %v", err)
	}
	rows, err := fresh.LoadQuestStates(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadQuestStates() after reconnect error = %v", err)
	}
	var reloaded *store.QuestState
	for index := range rows {
		if rows[index].QuestID == questSliceID {
			reloaded = &rows[index]
		}
	}
	if reloaded == nil {
		t.Fatalf("the database holds no row for %s; the turn-in did not persist", questSliceID)
	}
	if reloaded.State != "turned-in" {
		t.Errorf("reloaded state = %q, want turned-in", reloaded.State)
	}
	if len(reloaded.Objectives) == 0 {
		t.Error("the jsonb counters came back empty")
	}

	state, err := fresh.LoadCharacterState(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() after reconnect error = %v", err)
	}
	definition, _ := catalog.Definition(questSliceID)
	if state.Experience != definition.Rewards.Experience {
		t.Errorf("reloaded experience = %d, want the content's %d",
			state.Experience, definition.Rewards.Experience)
	}
	if state.Currency != definition.Rewards.Money {
		t.Errorf("reloaded currency = %d, want the content's %d", state.Currency, definition.Rewards.Money)
	}
	if state.Honor != definition.Rewards.Honor {
		t.Errorf("reloaded honor = %d, want the content's %d", state.Honor, definition.Rewards.Honor)
	}
	items, err := fresh.LoadInventory(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadInventory() after reconnect error = %v", err)
	}
	held := map[string]int32{}
	for _, item := range items {
		held[item.ItemID] += item.Quantity
	}
	for _, reward := range definition.Rewards.MandatoryItems {
		if held[reward.ItemID] != reward.Count {
			t.Errorf("the reloaded bag holds %d of %s, want %d",
				held[reward.ItemID], reward.ItemID, reward.Count)
		}
	}

	// A second session, loading what the first one left, sees the quest as
	// turned in — and is refused a second turn-in on that basis alone.
	secondEntity, _ := zone.Join()
	if err := questModule.Admit(secondEntity, characterID, quests.Character{
		Level:     1,
		Quests:    rows,
		Inventory: items,
	}); err != nil {
		t.Fatalf("second Admit() error = %v", err)
	}
	if got := questStateOf(t, questModule, characterID, questSliceID); got != quests.StateTurnedIn {
		t.Errorf("the reloaded journal says %s, want turned-in", got)
	}
	if _, err := questModule.TurnIn(ctx, secondEntity, questSliceID, giverID); err == nil {
		t.Error("a reconnected session turned the same quest in again")
	}
}

// discardSnapshots is a replication sink that keeps nothing. What it is for is
// the side effect of subscribing, not the frames.
type discardSnapshots struct{}

func (discardSnapshots) OfferSnapshot(world.Snapshot) {}

func findEntities(t *testing.T, zone *world.Zone, giverMob, targetMob string) (uint64, uint64) {
	t.Helper()
	var giverID, targetID uint64
	_ = zone.Command(func(tick *world.Tick) error {
		tick.Each(func(entity *world.Entity) bool {
			switch {
			case entity.ContentID == giverMob && entity.Alive && giverID == 0:
				giverID = entity.ID
			case entity.ContentID == targetMob && entity.Alive && targetID == 0:
				targetID = entity.ID
			}
			return true
		})
		return nil
	})
	if giverID == 0 || targetID == 0 {
		t.Fatalf("the zone holds giver %d and target %d; both must be live", giverID, targetID)
	}
	return giverID, targetID
}

func questStateOf(t *testing.T, module *quests.Module, characterID uuid.UUID, questID string) quests.State {
	t.Helper()
	for _, update := range module.Log(characterID) {
		if update.QuestID == questID {
			return update.State
		}
	}
	return quests.StateUnspecified
}
