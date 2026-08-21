package session

import (
	"encoding/json"
	"strings"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/quests"
	"google.golang.org/protobuf/proto"
)

func TestQuestLogReplacementProjectionGolden(t *testing.T) {
	hideLevel := true
	progress := int32(2)
	dailyCount, dailyLimit := uint32(3), uint32(10)
	book := quests.HUDQuestBook{
		Quests: []quests.HUDQuestSummary{
			{
				ID: "quest.alpha", Name: quests.HUDLocalizedText{LocalizationKey: "quest.alpha.name"},
				Level: 7, HideLevel: &hideLevel, State: quests.HUDQuestStateInProgress,
			},
			{
				ID: "quest.beta", Name: quests.HUDLocalizedText{LocalizationKey: "quest.beta.name"},
				Level: 8, State: quests.HUDQuestStateReadyToReturn,
			},
		},
		Bookmarks:  []quests.HUDQuestSummary{{ID: "quest.beta"}, {ID: "quest.alpha"}},
		Selected:   &quests.HUDQuestDetail{Info: quests.HUDQuestInfo{ID: "quest.beta"}},
		DailyCount: &dailyCount,
		DailyLimit: &dailyLimit,
	}
	progressByQuest := map[string]quests.HUDQuestProgress{
		"quest.alpha": {
			ID: "quest.alpha", State: quests.HUDQuestStateInProgress,
			Objectives: []quests.HUDQuestObjective{{
				Name:     quests.HUDLocalizedText{LocalizationKey: "quest.alpha.counter"},
				Progress: &progress, Required: 4, Type: quests.HUDQuestObjectiveItem,
				ShowCounterValue: true,
				Items:            []quests.HUDQuestObjectiveItemRef{{ItemID: "item.quest.alpha"}},
			}},
		},
		"quest.beta": {ID: "quest.beta", State: quests.HUDQuestStateReadyToReturn},
	}
	bookBefore := mustJSON(t, book)
	progressBefore := mustJSON(t, progressByQuest)

	message, err := questLogReplacementToProto(71, book, progressByQuest)
	if err != nil {
		t.Fatalf("questLogReplacementToProto() error = %v", err)
	}
	want := &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_QuestLogReplacement{
			QuestLogReplacement: &sarnautv1.QuestLogReplacement{
				Revision: 71,
				VisibleQuests: []*sarnautv1.QuestLogEntry{
					{
						QuestId: "quest.alpha", State: sarnautv1.QuestUiState_QUEST_UI_STATE_IN_PROGRESS,
						Name: "quest.alpha.name", Level: 7, IsHideLevel: true,
						Objectives: []*sarnautv1.QuestObjectiveState{{
							Name: "quest.alpha.counter", Progress: 2, Required: 4,
							Type:             sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_ITEM,
							ShowCounterValue: true,
							Items:            []*sarnautv1.QuestObjectiveItem{{ProductItemId: "item.quest.alpha"}},
						}},
					},
					{
						QuestId: "quest.beta", State: sarnautv1.QuestUiState_QUEST_UI_STATE_READY_TO_RETURN,
						Name: "quest.beta.name", Level: 8,
					},
				},
				BookmarkQuestIds: []string{"quest.beta", "quest.alpha"},
				SelectedQuestId:  "quest.beta",
				DailyCount:       3,
				DailyLimit:       10,
			},
		},
	}
	if !proto.Equal(message, want) {
		t.Fatalf("quest log replacement =\n%v\nwant\n%v", message, want)
	}
	if got := mustJSON(t, book); got != bookBefore {
		t.Fatal("quest log projection mutated the book")
	}
	if got := mustJSON(t, progressByQuest); got != progressBefore {
		t.Fatal("quest log projection mutated progress")
	}
}

func TestQuestLogReplacementRequiresExactProgressForEveryVisibleRow(t *testing.T) {
	book := quests.HUDQuestBook{Quests: []quests.HUDQuestSummary{{
		ID: "quest.one", State: quests.HUDQuestStateInProgress,
	}}}
	if _, err := questLogReplacementToProto(1, book, nil); err == nil ||
		!strings.Contains(err.Error(), "progress is absent") {
		t.Fatalf("missing progress error = %v", err)
	}
	wrong := map[string]quests.HUDQuestProgress{"quest.one": {
		ID: "quest.two", State: quests.HUDQuestStateInProgress,
	}}
	if _, err := questLogReplacementToProto(1, book, wrong); err == nil ||
		!strings.Contains(err.Error(), "disagree") {
		t.Fatalf("mismatched progress error = %v", err)
	}
}

func TestQuestDetailProjectionPreservesPresenceAndVisibleRewards(t *testing.T) {
	debugName := "alpha debug"
	mapID := "map.league.01"
	duration, remaining := int64(60_000), int64(14_500)
	detail := quests.HUDQuestDetail{
		Info: quests.HUDQuestInfo{
			ID: "quest.alpha", Name: quests.HUDLocalizedText{LocalizationKey: "quest.alpha.name"},
			DebugName: &debugName, ZonesMapID: &mapID,
		},
		Progress: quests.HUDQuestProgress{
			ID: "quest.alpha", State: quests.HUDQuestStateInProgress,
			TimerDurationMS: &duration, TimerTimeLeftMS: &remaining,
		},
		Rewards: quests.HUDQuestRewards{
			MandatoryItems: []quests.HUDRewardItem{
				{ItemID: "item.visible", Count: 2},
				{ItemID: "item.hidden", Count: 1, Hidden: true},
			},
			MandatoryItemsCount: 9,
		},
	}

	message, err := questDetailReplacementToProto(4, 5, detail)
	if err != nil {
		t.Fatalf("questDetailReplacementToProto() error = %v", err)
	}
	got := message.GetQuestInfoReplacement()
	if got.GetMode() != sarnautv1.QuestInfoMode_QUEST_INFO_MODE_NONE ||
		got.GetRequestedQuestId() != "quest.alpha" {
		t.Fatalf("detail envelope = %+v", got)
	}
	if got.Info.DebugName == nil || *got.Info.DebugName != debugName ||
		got.Info.ZonesMapId == nil || *got.Info.ZonesMapId != mapID {
		t.Fatalf("optional quest info fields = %+v", got.Info)
	}
	if got.Progress.TimerDurationMilliseconds == nil ||
		*got.Progress.TimerDurationMilliseconds != uint64(duration) ||
		got.Progress.TimerTimeLeftMilliseconds == nil ||
		*got.Progress.TimerTimeLeftMilliseconds != uint64(remaining) {
		t.Fatalf("optional timers = %+v", got.Progress)
	}
	if len(got.Reward.MandatoryItems) != 1 ||
		got.Reward.MandatoryItems[0].GetProductItemId() != "item.visible" ||
		got.Reward.GetMandatoryItemsCount() != 9 {
		t.Fatalf("visible reward projection = %+v", got.Reward)
	}
}

func TestQuestNPCOfferCarriesObjectivesWithoutInventingInstanceState(t *testing.T) {
	info := &quests.HUDNPCQuestInfo{
		Info: quests.HUDQuestInfo{ID: "quest.offer"},
		Objectives: []quests.HUDQuestObjective{{
			Name:     quests.HUDLocalizedText{LocalizationKey: "quest.offer.counter"},
			Required: 3, Type: quests.HUDQuestObjectiveKill,
		}},
	}
	message, err := questNPCInfoReplacementToProto(
		9, 10, "quest.offer", 11,
		sarnautv1.QuestInfoMode_QUEST_INFO_MODE_OFFER,
		info, quests.RefusalNone,
	)
	if err != nil {
		t.Fatalf("questNPCInfoReplacementToProto() error = %v", err)
	}
	got := message.GetQuestInfoReplacement()
	if got.Progress == nil || len(got.Progress.Objectives) != 1 {
		t.Fatalf("offer progress = %+v", got.Progress)
	}
	if got.Progress.GetState() != sarnautv1.QuestUiState_QUEST_UI_STATE_IN_PROGRESS {
		t.Fatalf("offer default state = %s", got.Progress.GetState())
	}
	if got.Progress.Objectives[0].GetProgress() != 0 {
		t.Fatalf("offer invented objective progress: %+v", got.Progress.Objectives[0])
	}

	refused, err := questNPCInfoReplacementToProto(
		12, 13, "quest.missing", 14,
		sarnautv1.QuestInfoMode_QUEST_INFO_MODE_NONE,
		nil, quests.RefusalUnknownQuest,
	)
	if err != nil {
		t.Fatalf("refused quest info error = %v", err)
	}
	refusal := refused.GetQuestInfoReplacement()
	if refusal.Info != nil || refusal.Progress != nil || refusal.Reward != nil ||
		refusal.GetRefusal() != sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_UNKNOWN_QUEST {
		t.Fatalf("refused quest info fabricated nested state: %+v", refusal)
	}
}

func TestQuestProjectionRejectsLossyOrMalformedAuthority(t *testing.T) {
	negativeTimer := int64(-1)
	period := int64(86_400_000)
	position := quests.HUDPosition{X: 1}
	tests := []struct {
		name string
		run  func() error
	}{
		{"negative timer", func() error {
			_, err := questProgressToProto(quests.HUDQuestProgress{
				ID: "quest.timer", State: quests.HUDQuestStateInProgress,
				TimerDurationMS: &negativeTimer,
			})
			return err
		}},
		{"negative reward", func() error {
			_, err := questRewardToProto(quests.HUDQuestRewards{
				MandatoryItems: []quests.HUDRewardItem{{ItemID: "item.bad", Count: -1}},
			})
			return err
		}},
		{"repeat period type mismatch", func() error {
			_, err := questInfoToProto(quests.HUDQuestInfo{ID: "quest.period", RepeatPeriodMS: &period})
			return err
		}},
		{"unknown state", func() error {
			_, err := questUIStateToProto(quests.HUDQuestState("MISSING"))
			return err
		}},
		{"unknown objective", func() error {
			_, err := questObjectiveTypeToProto(quests.HUDQuestObjectiveType("MISSING"))
			return err
		}},
		{"internal objective", func() error {
			_, err := questObjectiveToProto(quests.HUDQuestObjective{
				IsInternal: true, Type: quests.HUDQuestObjectiveSpecial,
			})
			return err
		}},
		{"unpaired quest position", func() error {
			_, err := questInfoToProto(quests.HUDQuestInfo{
				ID: "quest.position", GoalLocation: &position,
			})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("projection accepted authority with no exact wire value")
			}
		})
	}
}

func TestActionBarReplacementProjectsAllAuthoredSlots(t *testing.T) {
	var bar combat.ActionBar
	for index := range bar.Slots {
		bar.Slots[index] = combat.ActionSlotState{
			SlotIndex: uint32(index), UnavailableReason: combat.ActionUnavailableEmptySlot,
		}
	}
	bar.Slots[4] = combat.ActionSlotState{
		SlotIndex: 4, AbilityID: "ability.warrior.attack",
		CooldownRemainingMS: 750, CooldownDurationMS: 1_500,
		UnavailableReason: combat.ActionUnavailableOnCooldown,
	}

	message, err := actionBarReplacementToProto(21, 22, bar, combat.ActionRejectionNone)
	if err != nil {
		t.Fatalf("actionBarReplacementToProto() error = %v", err)
	}
	got := message.GetActionBarReplacement()
	if len(got.Slots) != combat.ActionBarSlotCount || got.Slots[35].GetSlotIndex() != 35 {
		t.Fatalf("action slot shape = %d slots, last %+v", len(got.Slots), got.Slots[35])
	}
	if slot := got.Slots[4]; slot.GetAbilityId() != "ability.warrior.attack" ||
		slot.GetCooldownRemainingMilliseconds() != 750 ||
		slot.GetUnavailableReason() != sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_ON_COOLDOWN {
		t.Fatalf("action slot 4 = %+v", slot)
	}
}

func TestActionBarProjectionRejectsUnrepresentableState(t *testing.T) {
	var bar combat.ActionBar
	for index := range bar.Slots {
		bar.Slots[index] = combat.ActionSlotState{
			SlotIndex: uint32(index), UnavailableReason: combat.ActionUnavailableEmptySlot,
		}
	}
	bar.Slots[0].UnavailableReason = combat.ActionUnavailableNoResource
	if _, err := actionBarReplacementToProto(1, 2, bar, combat.ActionRejectionNone); err == nil ||
		!strings.Contains(err.Error(), "no_resource") {
		t.Fatalf("no-resource projection error = %v", err)
	}
	bar.Slots[0].UnavailableReason = combat.ActionUnavailableEmptySlot
	bar.Slots[17].SlotIndex = 18
	if _, err := actionBarReplacementToProto(1, 2, bar, combat.ActionRejectionNone); err == nil ||
		!strings.Contains(err.Error(), "carries slot index") {
		t.Fatalf("out-of-order projection error = %v", err)
	}
}

func TestTargetReplacementPreservesAuthorityAndExactRefusals(t *testing.T) {
	tests := []struct {
		domain combat.Rejection
		wire   sarnautv1.TargetSelectRefusal
	}{
		{combat.RejectionNone, sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NONE},
		{combat.RejectionNoTarget, sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NO_TARGET},
		{combat.RejectionInvalidTarget, sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_INVALID_TARGET},
		{combat.RejectionTargetDead, sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_TARGET_DEAD},
	}
	for _, test := range tests {
		message, err := targetStateReplacementToProto(31, 32, true, 99, test.domain)
		if err != nil {
			t.Fatalf("targetStateReplacementToProto(%d) error = %v", test.domain, err)
		}
		got := message.GetTargetStateReplacement()
		if got.GetRevision() != 31 || got.GetRequestId() != 32 || !got.GetHasAuthority() ||
			got.GetSelectedEntityId() != 99 || got.GetRefusal() != test.wire {
			t.Errorf("target replacement for %d = %+v", test.domain, got)
		}
	}
	if _, err := targetStateReplacementToProto(
		1, 2, true, 3, combat.RejectionOutOfRange,
	); err == nil {
		t.Fatal("target projection accepted non-selection combat rejection")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(encoded)
}
