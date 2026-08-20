package quests_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
)

// TestAPackWithAnUnsupportedObjectiveKindIsRefusedByName is rule 5.5.6.
//
// The pack it loads is a real one, compiled by `sarnaut-pack` from the
// `m2-quest-unsupported` overlay layer. That matters: the refusal has to happen
// against bytes a content author could actually produce, and a hand-built
// struct would prove only that a switch statement has a default branch.
//
// The pack itself is well formed and loads. What fails is the catalog, which is
// where the difference between "these bytes are broken" and "this build cannot
// play this quest" belongs.
func TestAPackWithAnUnsupportedObjectiveKindIsRefusedByName(t *testing.T) {
	t.Parallel()

	directory := filepath.Join("..", "..", "testdata", "packs", "demo-quest-unsupported")
	content, err := pack.Load(directory, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v; the pack's bytes are not what is under test", err)
	}

	_, err = quests.CatalogFromPack(content)
	if err == nil {
		t.Fatal("CatalogFromPack() error = nil; a count-special objective must stop the boot")
	}
	const questID = "quest.paper-harbor.gate-ritual"
	if !strings.Contains(err.Error(), questID) {
		t.Errorf("error = %q, want it to name %s: an operator has to know which row to fix", err, questID)
	}
	if !strings.Contains(err.Error(), "count-special") {
		t.Errorf("error = %q, want it to name the objective kind", err)
	}
	if !strings.Contains(err.Error(), content.ID()) {
		t.Errorf("error = %q, want it to name the pack it came from", err)
	}
}

// TestTheFixturePackLoadsEveryQuestItCarries is the other direction: the shard
// refuses the quest above and accepts everything the golden fixture carries.
func TestTheFixturePackLoadsEveryQuestItCarries(t *testing.T) {
	t.Parallel()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	catalog, err := quests.CatalogFromPack(content)
	if err != nil {
		t.Fatalf("CatalogFromPack() error = %v", err)
	}
	if catalog.Count() != len(content.QuestIDs()) {
		t.Errorf("the catalog holds %d of the pack's %d quests", catalog.Count(), len(content.QuestIDs()))
	}
	if edges := catalog.UnresolvedPrerequisites(); len(edges) != 0 {
		t.Errorf("unresolved prerequisites = %v, want none in a single-zone fixture", edges)
	}
	if starters := catalog.StartedBy(giverMob); len(starters) < 2 {
		t.Errorf("%s starts %v, want the several the dataset gives it", giverMob, starters)
	}
	if dependents := catalog.Dependents(tallyQuest); len(dependents) != 1 || dependents[0] != deepQuest {
		t.Errorf("Dependents(%s) = %v, want [%s]", tallyQuest, dependents, deepQuest)
	}
}

// TestTheCatalogRefusesDefinitionsItCannotEvaluate covers the checks that need
// no pack of their own, each one a shape the compiler will happily emit.
func TestTheCatalogRefusesDefinitionsItCannotEvaluate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		definition pack.Quest
		want       string
	}{
		{
			name: "a prerequisite status M2 does not implement",
			definition: pack.Quest{
				ID: "quest.demo.gated",
				Prerequisites: []pack.QuestPrerequisite{
					{QuestID: "quest.demo.first", RequiredStatus: "Started"},
				},
			},
			want: "Started",
		},
		{
			name: "an objective that names no target",
			definition: pack.Quest{
				ID: "quest.demo.aimless",
				Objectives: []pack.QuestObjective{
					{Kind: pack.QuestObjectiveCountKill, Limit: 3},
				},
			},
			want: "names no target",
		},
		{
			name: "an objective kind this build has never heard of",
			definition: pack.Quest{
				ID: "quest.demo.future",
				Objectives: []pack.QuestObjective{
					{Kind: pack.QuestObjectiveUnspecified, Limit: 1, TargetIDs: []string{"mob.demo.x"}},
				},
			},
			want: "quest.demo.future",
		},
		{
			name:       "the same quest twice",
			definition: pack.Quest{ID: "quest.demo.first"},
			want:       "defined twice",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			definitions := []pack.Quest{{ID: "quest.demo.first"}, testCase.definition}
			_, err := quests.NewCatalog(definitions, nil)
			if err == nil {
				t.Fatalf("NewCatalog() error = nil, want a refusal naming %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error = %q, want it to contain %q", err, testCase.want)
			}
		})
	}
}

// TestAPrerequisiteWithNoStatusIsTreatedAsFinished states the one leniency.
//
// The authored field is optional and `Finished` is the only status M2 knows, so
// an absent one cannot mean anything else. A status that is present and is
// something else is refused above, because treating an unimplemented gate as
// satisfied is how a quest chain silently unlocks out of order.
func TestAPrerequisiteWithNoStatusIsTreatedAsFinished(t *testing.T) {
	t.Parallel()

	_, err := quests.NewCatalog([]pack.Quest{
		{ID: "quest.demo.first"},
		{
			ID:            "quest.demo.second",
			Prerequisites: []pack.QuestPrerequisite{{QuestID: "quest.demo.first"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v, want an absent status to be accepted", err)
	}
}

// TestARewardNamingAnAbsentItemIsRefused is the load-time version of a bag
// insertion that could never succeed.
func TestARewardNamingAnAbsentItemIsRefused(t *testing.T) {
	t.Parallel()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	_, err = quests.NewCatalog([]pack.Quest{{
		ID: "quest.demo.generous",
		Rewards: pack.QuestRewards{
			MandatoryItems: []pack.QuestRewardItem{{ItemID: "item.demo.does-not-exist", Count: 1}},
		},
	}}, content)
	if err == nil {
		t.Fatal("NewCatalog() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "item.demo.does-not-exist") {
		t.Errorf("error = %q, want it to name the missing item", err)
	}
}

// TestAnUnresolvedPrerequisiteIsReportedNotRefused is the deliberate
// non-failure of a single-zone pack.
//
// A pack covers one zone (ADR 0029) and reference data gates quests on quests
// in the zone next door, so a dangling edge is ordinary. What it is not is
// invisible: every quest behind one is permanently unavailable, and that is
// worth a line in the boot log.
func TestAnUnresolvedPrerequisiteIsReportedNotRefused(t *testing.T) {
	t.Parallel()

	catalog, err := quests.NewCatalog([]pack.Quest{{
		ID:            "quest.demo.second",
		Prerequisites: []pack.QuestPrerequisite{{QuestID: "quest.other-zone.first", RequiredStatus: "Finished"}},
	}}, nil)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v, want a cross-zone prerequisite to load", err)
	}
	edges := catalog.UnresolvedPrerequisites()
	if len(edges) != 1 || !strings.Contains(edges[0], "quest.other-zone.first") {
		t.Errorf("UnresolvedPrerequisites() = %v, want the one dangling edge", edges)
	}
}
