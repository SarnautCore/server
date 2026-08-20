package main

import (
	"path/filepath"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

func TestOnlyRealPackRunsSkipUnsupportedQuests(t *testing.T) {
	t.Parallel()

	if skipUnsupportedQuestsFor(filepath.Join("testdata", "packs", "demo")) {
		t.Fatal("the default fixture enabled unsupported-quest skipping")
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "packs", "demo"))
	if err != nil {
		t.Fatalf("filepath.Abs(default fixture) error = %v", err)
	}
	if skipUnsupportedQuestsFor(fixture) {
		t.Fatal("the absolute default fixture path enabled unsupported-quest skipping")
	}
	if !skipUnsupportedQuestsFor(filepath.Join("..", "data", "packs", "inst-league1")) {
		t.Fatal("a real-pack run left unsupported-quest skipping disabled")
	}
}

func TestCoLocateGiverMovesOneCopiedSpawnBesideTheTarget(t *testing.T) {
	t.Parallel()

	spawns := []pack.NPCSpawn{
		{MobID: "mob.demo.target", Position: pack.Vec3{X: 100, Y: 200, Z: 3}},
		{MobID: "mob.demo.giver", Position: pack.Vec3{X: -900, Y: -900, Z: -9}},
	}
	anchor := world.Vec3{X: 100, Y: 200, Z: 3}
	if err := coLocateGiver(spawns, "mob.demo.giver", anchor); err != nil {
		t.Fatalf("coLocateGiver() error = %v", err)
	}
	want := pack.Vec3{X: 103, Y: 200, Z: 3}
	if spawns[1].Position != want {
		t.Errorf("giver position = %+v, want %+v", spawns[1].Position, want)
	}
}

func TestRequirementsForCarriesKillAndItemObjectivesIntoTheHarness(t *testing.T) {
	t.Parallel()

	definition := pack.Quest{
		ID: "quest.demo.mixed",
		Objectives: []pack.QuestObjective{
			{Kind: pack.QuestObjectiveCountKill, Limit: 3, TargetIDs: []string{"mob.demo.target"}},
			{Kind: pack.QuestObjectiveCountItem, Limit: 2, TargetIDs: []string{"item.demo.key"}},
		},
	}
	requirements, err := requirementsFor(definition, "mob.demo.target")
	if err != nil {
		t.Fatalf("requirementsFor() error = %v", err)
	}
	if requirements.killLimit != 3 {
		t.Errorf("kill limit = %d, want 3", requirements.killLimit)
	}
	if len(requirements.inventory) != 1 ||
		requirements.inventory[0].ItemID != "item.demo.key" ||
		requirements.inventory[0].Quantity != 2 {
		t.Errorf("inventory = %+v, want two item.demo.key units", requirements.inventory)
	}
	if len(requirements.itemObjectiveIndexes) != 1 || requirements.itemObjectiveIndexes[0] != 1 {
		t.Errorf("item objective indexes = %v, want [1]", requirements.itemObjectiveIndexes)
	}
}

func TestCoLocateTargetsAddsEnoughDistinctTargets(t *testing.T) {
	t.Parallel()

	spawns := []pack.NPCSpawn{
		{PlacementID: "placement.target", MobID: "mob.demo.target"},
		{PlacementID: "placement.other", MobID: "mob.demo.other"},
	}
	anchor := world.Vec3{X: 10, Y: 20, Z: 3}
	got, err := coLocateTargets(spawns, "mob.demo.target", anchor, 3)
	if err != nil {
		t.Fatalf("coLocateTargets() error = %v", err)
	}
	seen := make(map[string]struct{})
	for _, spawn := range got {
		if spawn.MobID != "mob.demo.target" {
			continue
		}
		if spawn.Position != (pack.Vec3{X: 10, Y: 20, Z: 3}) {
			t.Errorf("target %q position = %+v, want anchor", spawn.PlacementID, spawn.Position)
		}
		seen[spawn.PlacementID] = struct{}{}
	}
	if len(seen) != 3 {
		t.Errorf("distinct colocated targets = %d, want 3: %+v", len(seen), got)
	}
}

func TestFindTargetSkipsAnAlreadyKilledEntityFromAStaleSnapshot(t *testing.T) {
	t.Parallel()

	entities := []*sarnautv1.EntitySnapshot{
		{EntityId: 10, ContentId: "mob.demo.target", Alive: true},
		{EntityId: 11, ContentId: "mob.demo.target", Alive: true},
	}
	excluded := map[uint64]struct{}{10: {}}

	selected := liveTarget(entities, "mob.demo.target", excluded)
	if selected == nil || selected.GetEntityId() != 11 {
		t.Fatalf("selected target = %+v, want entity 11", selected)
	}
}

func TestCompletedObjectiveProgressRequiresEveryItemCounterAtItsLimit(t *testing.T) {
	t.Parallel()

	update := &sarnautv1.QuestStateUpdate{
		QuestId: "quest.demo.mixed",
		Objectives: []*sarnautv1.QuestObjectiveProgress{
			{Index: 0, Counter: 0, Limit: 3},
			{Index: 1, Counter: 2, Limit: 2},
		},
	}
	progress, err := completedObjectiveProgress(update, []uint32{1})
	if err != nil {
		t.Fatalf("completedObjectiveProgress() error = %v", err)
	}
	if progress != "objective 1=2/2 from starting inventory" {
		t.Errorf("progress = %q", progress)
	}
	if _, err := completedObjectiveProgress(update, []uint32{0}); err == nil {
		t.Fatal("an incomplete objective was reported complete")
	}
}
