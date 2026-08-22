package pack

import (
	"errors"
	"testing"
	"time"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

func TestPlayerProgressionCarriesAuthoredCurveImpactsAndLifecycle(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	row := fixturePlayerProgression()
	replaceCompiledTable(t, directory, tablePlayerProgression,
		contentv1.RowType_ROW_TYPE_PLAYER_PROGRESSION,
		[]compiledRow{{id: row.GetId(), message: row}},
	)
	reseal(t, directory)

	content, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	rules, ok := content.PlayerProgression()
	if !ok {
		t.Fatal("PlayerProgression() reports no authored row")
	}
	if rules.ID() != row.GetId() || rules.MaxLevel() != 3 {
		t.Fatalf("player progression = %q max %d", rules.ID(), rules.MaxLevel())
	}
	if threshold, ok := rules.CumulativeExperience(3); !ok || threshold != 300 {
		t.Fatalf("CumulativeExperience(3) = %d, %v", threshold, ok)
	}
	if experience, err := rules.ExperienceForMobs(4, 2); err != nil || experience != 240 {
		t.Fatalf("ExperienceForMobs(4, 2) = %d, %v", experience, err)
	}
	if _, err := rules.ExperienceForMobs(1, 1); err == nil {
		t.Fatal("ExperienceForMobs() invented an absent authored result")
	}
	if rules.RespawnDelay() != 3*time.Second ||
		rules.ResurrectionSicknessDuration() != 20*time.Second {
		t.Fatalf("lifecycle = %v, %v", rules.RespawnDelay(), rules.ResurrectionSicknessDuration())
	}
}

func TestPlayerProgressionRejectsMalformedRuntimeRows(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*contentv1.PlayerProgression){
		"threshold gap": func(row *contentv1.PlayerProgression) {
			row.Thresholds[1].Level = 3
		},
		"non-increasing threshold": func(row *contentv1.PlayerProgression) {
			row.Thresholds[2].CumulativeExperience = 100
		},
		"duplicate impact inputs": func(row *contentv1.PlayerProgression) {
			row.ExperienceImpacts = append(row.ExperienceImpacts, &contentv1.ExperienceImpact{
				Id: "impact.fixture.duplicate", MobCount: 4, MobLevel: 2, ResolvedExperience: 241,
			})
		},
		"zero lifecycle": func(row *contentv1.PlayerProgression) {
			row.RespawnDelayMs = 0
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := copyFixture(t)
			row := fixturePlayerProgression()
			mutate(row)
			replaceCompiledTable(t, directory, tablePlayerProgression,
				contentv1.RowType_ROW_TYPE_PLAYER_PROGRESSION,
				[]compiledRow{{id: row.GetId(), message: row}},
			)
			reseal(t, directory)
			if _, err := Load(directory, Options{}); !errors.Is(err, ErrMalformedTable) {
				t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
			}
		})
	}
}

func TestChargenCarriesAuthoredHealthAndActionLoadout(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	slot := uint32(0)
	row := &contentv1.ChargenOption{
		Id: "chargen.fixture.native", StartingLevel: 1,
		Stats: fixtureStartingCombatStats(),
		StartingActions: []*contentv1.StartingAction{
			{SlotIndex: &slot, ActionId: fixtureNativeActionID},
		},
		PassiveAbilityIds: []string{"passive.warrior.parry"},
	}
	replaceCompiledTable(t, directory, tableChargen, contentv1.RowType_ROW_TYPE_CHARGEN_OPTION,
		[]compiledRow{{id: row.GetId(), message: row}},
	)
	action := fixtureNativeAction()
	replaceCompiledTable(t, directory, tableNativeActions, contentv1.RowType_ROW_TYPE_NATIVE_ACTION,
		[]compiledRow{{id: action.GetId(), message: action}},
	)
	reseal(t, directory)

	content, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	option := content.ChargenOptions()[0]
	if option.StartingHealth != 146 || option.StartingMaxHealth != 146 ||
		len(option.StartingActions) != 1 || option.StartingActions[0].SlotIndex == nil ||
		*option.StartingActions[0].SlotIndex != 0 || len(option.PassiveAbilityIDs) != 1 {
		t.Fatalf("native chargen option = %#v", option)
	}
	option.StartingActions[0].ActionID = "scribbled-over"
	*option.StartingActions[0].SlotIndex = 99
	if fresh := content.ChargenOptions()[0]; fresh.StartingActions[0].ActionID == "scribbled-over" ||
		*fresh.StartingActions[0].SlotIndex == 99 {
		t.Fatal("ChargenOptions() exposed its stored action slice")
	}
}

func fixturePlayerProgression() *contentv1.PlayerProgression {
	return &contentv1.PlayerProgression{
		Id: "progression.fixture.avatar", MaxLevel: 3,
		Thresholds: []*contentv1.PlayerLevelThreshold{
			{Level: 1, CumulativeExperience: 0},
			{Level: 2, CumulativeExperience: 100},
			{Level: 3, CumulativeExperience: 300},
		},
		ExperienceImpacts: []*contentv1.ExperienceImpact{{
			Id: "impact.fixture.mob-pack", MobCount: 4, MobLevel: 2, ResolvedExperience: 240,
		}},
		RespawnDelayMs: 3000, ResurrectionSicknessDurationMs: 20000,
	}
}
