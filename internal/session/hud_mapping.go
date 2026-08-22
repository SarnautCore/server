package session

import (
	"fmt"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/loot"
	"github.com/SarnautCore/server/internal/quests"
)

// questLogReplacementToProto projects the complete visible quest book. Share
// invitations and command results are session-owned and stay absent here.
func questLogReplacementToProto(
	revision uint64,
	book quests.HUDQuestBook,
	progressByQuest map[string]quests.HUDQuestProgress,
) (*sarnautv1.ServerMessage, error) {
	if len(book.Quests) > quests.HUDVisibleQuestLimit {
		return nil, fmt.Errorf("project quest log: %d visible quests exceed %d",
			len(book.Quests), quests.HUDVisibleQuestLimit)
	}
	if len(book.Bookmarks) > quests.HUDBookmarkLimit {
		return nil, fmt.Errorf("project quest log: %d bookmarks exceed %d",
			len(book.Bookmarks), quests.HUDBookmarkLimit)
	}
	replacement := &sarnautv1.QuestLogReplacement{Revision: revision}
	for _, summary := range book.Quests {
		state, err := questUIStateToProto(summary.State)
		if err != nil {
			return nil, fmt.Errorf("project quest log entry %q: %w", summary.ID, err)
		}
		progress, ok := progressByQuest[summary.ID]
		if !ok {
			return nil, fmt.Errorf("project quest log entry %q: progress is absent", summary.ID)
		}
		if progress.ID != summary.ID || progress.State != summary.State {
			return nil, fmt.Errorf(
				"project quest log entry %q: summary and progress disagree", summary.ID,
			)
		}
		if len(progress.Objectives) > quests.HUDDetailObjectiveLimit {
			return nil, fmt.Errorf("project quest log entry %q: %d objectives exceed %d",
				summary.ID, len(progress.Objectives), quests.HUDDetailObjectiveLimit)
		}
		entry := &sarnautv1.QuestLogEntry{
			QuestId: summary.ID,
			State:   state,
			Name:    summary.Name.LocalizationKey,
			Level:   summary.Level,
		}
		if summary.HideLevel != nil {
			entry.IsHideLevel = *summary.HideLevel
		}
		for _, objective := range progress.Objectives {
			projected, err := questObjectiveToProto(objective)
			if err != nil {
				return nil, fmt.Errorf("project quest log entry %q: %w", summary.ID, err)
			}
			entry.Objectives = append(entry.Objectives, projected)
		}
		replacement.VisibleQuests = append(replacement.VisibleQuests, entry)
	}
	for _, bookmark := range book.Bookmarks {
		replacement.BookmarkQuestIds = append(replacement.BookmarkQuestIds, bookmark.ID)
	}
	if book.Selected != nil {
		replacement.SelectedQuestId = book.Selected.Info.ID
	}
	if book.DailyCount != nil {
		replacement.DailyCount = *book.DailyCount
	}
	if book.DailyLimit != nil {
		replacement.DailyLimit = *book.DailyLimit
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_QuestLogReplacement{
			QuestLogReplacement: replacement,
		},
	}, nil
}

// questDetailReplacementToProto projects the selected journal detail. Mode
// NONE distinguishes it from an NPC offer or turn-in panel.
func questDetailReplacementToProto(
	revision uint64,
	requestID uint64,
	detail quests.HUDQuestDetail,
) (*sarnautv1.ServerMessage, error) {
	if len(detail.Progress.Objectives) > quests.HUDDetailObjectiveLimit {
		return nil, fmt.Errorf("project selected quest %q: %d objectives exceed %d",
			detail.Info.ID, len(detail.Progress.Objectives), quests.HUDDetailObjectiveLimit)
	}
	info, err := questInfoToProto(detail.Info)
	if err != nil {
		return nil, err
	}
	progress, err := questProgressToProto(detail.Progress)
	if err != nil {
		return nil, err
	}
	reward, err := questRewardToProto(detail.Rewards)
	if err != nil {
		return nil, err
	}
	return questInfoServerMessage(&sarnautv1.QuestInfoReplacement{
		Revision:         revision,
		RequestId:        requestID,
		RequestedQuestId: detail.Info.ID,
		Mode:             sarnautv1.QuestInfoMode_QUEST_INFO_MODE_NONE,
		Refusal:          sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_NONE,
		Info:             info,
		Progress:         progress,
		Reward:           reward,
	}), nil
}

// questNPCInfoReplacementToProto projects one NPC panel. The caller supplies
// OFFER or TURN_IN because proximity to an NPC, not the catalog record, owns
// that decision. A nil info preserves a refused response without fabricated
// blank detail records.
func questNPCInfoReplacementToProto(
	revision uint64,
	requestID uint64,
	requestedQuestID string,
	npcEntityID uint64,
	mode sarnautv1.QuestInfoMode,
	info *quests.HUDNPCQuestInfo,
	refusal quests.Refusal,
) (*sarnautv1.ServerMessage, error) {
	if mode != sarnautv1.QuestInfoMode_QUEST_INFO_MODE_NONE &&
		mode != sarnautv1.QuestInfoMode_QUEST_INFO_MODE_OFFER &&
		mode != sarnautv1.QuestInfoMode_QUEST_INFO_MODE_TURN_IN {
		return nil, fmt.Errorf("quest info mode %d has no wire value", mode)
	}
	wireRefusal, err := questInfoRefusalToProto(refusal)
	if err != nil {
		return nil, err
	}
	replacement := &sarnautv1.QuestInfoReplacement{
		Revision:         revision,
		RequestId:        requestID,
		RequestedQuestId: requestedQuestID,
		Mode:             mode,
		NpcEntityId:      npcEntityID,
		Refusal:          wireRefusal,
	}
	if info == nil {
		return questInfoServerMessage(replacement), nil
	}
	if len(info.Objectives) > quests.HUDNPCInfoObjectiveLimit {
		return nil, fmt.Errorf("project NPC quest %q: %d objectives exceed %d",
			info.Info.ID, len(info.Objectives), quests.HUDNPCInfoObjectiveLimit)
	}
	replacement.Info, err = questInfoToProto(info.Info)
	if err != nil {
		return nil, err
	}
	replacement.Progress, err = questNPCProgressToProto(info.Info.ID, info.State, info.Objectives)
	if err != nil {
		return nil, err
	}
	replacement.Reward, err = questRewardToProto(info.Rewards)
	if err != nil {
		return nil, err
	}
	return questInfoServerMessage(replacement), nil
}

func questInfoServerMessage(replacement *sarnautv1.QuestInfoReplacement) *sarnautv1.ServerMessage {
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_QuestInfoReplacement{
			QuestInfoReplacement: replacement,
		},
	}
}

func questInfoToProto(info quests.HUDQuestInfo) (*sarnautv1.QuestInfo, error) {
	wire := &sarnautv1.QuestInfo{
		Id:            info.ID,
		Name:          info.Name.LocalizationKey,
		Level:         info.Level,
		RequiredLevel: info.RequiredLevel,
		Goal:          info.Goal.LocalizationKey,
		StartText:     info.StartText.LocalizationKey,
		CheckText:     info.CheckText.LocalizationKey,
		FinishText:    info.FinishText.LocalizationKey,
		CanCancel:     info.CanCancel,
		Type:          info.Type,
	}
	wire.DebugName = cloneString(info.DebugName)
	wire.ZonesMapId = cloneString(info.ZonesMapID)
	if info.HideLevel != nil {
		wire.IsHideLevel = *info.HideLevel
	}
	if info.KickText != nil {
		wire.KickText = info.KickText.LocalizationKey
	}
	if info.PlotLine != nil {
		wire.PlotLine = *info.PlotLine
	}
	if info.Shared != nil {
		wire.Shared = *info.Shared
	}
	if info.IsPvP != nil {
		wire.IsPvp = *info.IsPvP
	}
	if info.IsInSecretSequence != nil {
		wire.IsInSecretSequence = *info.IsInSecretSequence
	}
	if info.IsTutorial != nil {
		wire.IsTutorial = *info.IsTutorial
	}
	if info.IsRepeatable != nil {
		wire.IsRepeatable = *info.IsRepeatable
	}
	if info.CanRepeat != nil {
		wire.CanRepeat = *info.CanRepeat
	}
	if info.RepeatPeriod != nil {
		wire.RepeatPeriod = *info.RepeatPeriod
	}
	if info.IsSecret != nil {
		wire.IsSecret = *info.IsSecret
	}
	if info.ZoneName != nil {
		wire.ZoneName = info.ZoneName.LocalizationKey
	}
	if info.GoalLocation != nil || info.ReturnLocation != nil {
		return nil, fmt.Errorf(
			"quest %q goal or return position has no authored map-id pair", info.ID,
		)
	}
	for _, location := range info.AdditionalLocations {
		wire.AdditionalLocations = append(wire.AdditionalLocations,
			questPositionToProto(location.ZonesMapID, location.Position))
	}
	return wire, nil
}

func questPositionToProto(mapID string, position quests.HUDPosition) *sarnautv1.QuestLocation {
	return &sarnautv1.QuestLocation{
		ZonesMapId: mapID,
		Position: &sarnautv1.Vec3{
			X: position.X,
			Y: position.Y,
			Z: position.Z,
		},
	}
}

func questProgressToProto(progress quests.HUDQuestProgress) (*sarnautv1.QuestProgress, error) {
	state, err := questUIStateToProto(progress.State)
	if err != nil {
		return nil, fmt.Errorf("project quest progress %q: %w", progress.ID, err)
	}
	wire := &sarnautv1.QuestProgress{Id: progress.ID, State: state}
	wire.TimerDurationMilliseconds, err = optionalNonnegativeUint64(
		progress.TimerDurationMS, "timer duration", progress.ID,
	)
	if err != nil {
		return nil, err
	}
	wire.TimerTimeLeftMilliseconds, err = optionalNonnegativeUint64(
		progress.TimerTimeLeftMS, "timer time left", progress.ID,
	)
	if err != nil {
		return nil, err
	}
	for _, objective := range progress.Objectives {
		projected, err := questObjectiveToProto(objective)
		if err != nil {
			return nil, fmt.Errorf("project quest progress %q: %w", progress.ID, err)
		}
		wire.Objectives = append(wire.Objectives, projected)
	}
	return wire, nil
}

func questNPCProgressToProto(
	questID string,
	state *quests.HUDQuestState,
	objectives []quests.HUDQuestObjective,
) (*sarnautv1.QuestProgress, error) {
	wire := &sarnautv1.QuestProgress{Id: questID}
	if state != nil {
		projected, err := questUIStateToProto(*state)
		if err != nil {
			return nil, fmt.Errorf("project NPC quest progress %q: %w", questID, err)
		}
		wire.State = projected
	}
	for _, objective := range objectives {
		projected, err := questObjectiveToProto(objective)
		if err != nil {
			return nil, fmt.Errorf("project NPC quest progress %q: %w", questID, err)
		}
		wire.Objectives = append(wire.Objectives, projected)
	}
	return wire, nil
}

func questObjectiveToProto(objective quests.HUDQuestObjective) (*sarnautv1.QuestObjectiveState, error) {
	if objective.IsInternal {
		return nil, fmt.Errorf("internal quest objective reached the visible HUD projection")
	}
	kind, err := questObjectiveTypeToProto(objective.Type)
	if err != nil {
		return nil, err
	}
	wire := &sarnautv1.QuestObjectiveState{
		Name:             objective.Name.LocalizationKey,
		Required:         int64(objective.Required),
		IsInternal:       objective.IsInternal,
		Type:             kind,
		ShowCounterValue: objective.ShowCounterValue,
	}
	wire.SysDebugName = cloneString(objective.SysDebugName)
	if objective.Progress != nil {
		wire.Progress = int64(*objective.Progress)
	}
	for _, item := range objective.Items {
		wire.Items = append(wire.Items, &sarnautv1.QuestObjectiveItem{
			ProductItemId: item.ItemID,
		})
	}
	return wire, nil
}

func questRewardToProto(reward quests.HUDQuestRewards) (*sarnautv1.QuestReward, error) {
	for name, count := range map[string]int{
		"mandatory items":   visibleRewardCount(reward.MandatoryItems),
		"alternative items": visibleRewardCount(reward.AlternativeItems),
		"reputations":       len(reward.Reputations),
		"currencies":        len(reward.Currencies),
	} {
		if count > 5 {
			return nil, fmt.Errorf("quest reward %s: %d visible rows exceed 5", name, count)
		}
	}
	wire := &sarnautv1.QuestReward{
		Money:               reward.Money,
		Experience:          reward.Experience,
		Honor:               reward.Honor,
		MandatoryItemsCount: reward.MandatoryItemsCount,
	}
	for _, item := range reward.MandatoryItems {
		if item.Hidden {
			continue
		}
		projected, err := questRewardItemToProto(item)
		if err != nil {
			return nil, err
		}
		wire.MandatoryItems = append(wire.MandatoryItems, projected)
	}
	for _, item := range reward.AlternativeItems {
		if item.Hidden {
			continue
		}
		projected, err := questRewardItemToProto(item)
		if err != nil {
			return nil, err
		}
		wire.AlternativeItems = append(wire.AlternativeItems, projected)
	}
	for _, reputation := range reward.Reputations {
		wire.Reputations = append(wire.Reputations, &sarnautv1.QuestReputationReward{
			Faction: reputation.FactionID,
			Value:   reputation.Value,
		})
	}
	for _, currency := range reward.Currencies {
		wire.Currencies = append(wire.Currencies, &sarnautv1.QuestCurrencyReward{
			CurrencyId: currency.CurrencyID,
			Value:      currency.Value,
		})
	}
	return wire, nil
}

func visibleRewardCount(items []quests.HUDRewardItem) int {
	count := 0
	for _, item := range items {
		if !item.Hidden {
			count++
		}
	}
	return count
}

func questRewardItemToProto(item quests.HUDRewardItem) (*sarnautv1.QuestRewardItem, error) {
	if item.Count < 0 {
		return nil, fmt.Errorf("quest reward item %q has negative count %d", item.ItemID, item.Count)
	}
	return &sarnautv1.QuestRewardItem{
		ProductItemId: item.ItemID,
		Count:         uint32(item.Count),
	}, nil
}

func questUIStateToProto(state quests.HUDQuestState) (sarnautv1.QuestUiState, error) {
	switch state {
	case quests.HUDQuestStateInProgress:
		return sarnautv1.QuestUiState_QUEST_UI_STATE_IN_PROGRESS, nil
	case quests.HUDQuestStateReadyToReturn:
		return sarnautv1.QuestUiState_QUEST_UI_STATE_READY_TO_RETURN, nil
	case quests.HUDQuestStateCompleted:
		return sarnautv1.QuestUiState_QUEST_UI_STATE_COMPLETED, nil
	case quests.HUDQuestStateFailed:
		return sarnautv1.QuestUiState_QUEST_UI_STATE_FAILED, nil
	default:
		return 0, fmt.Errorf("quest HUD state %q has no wire value", state)
	}
}

func questObjectiveTypeToProto(kind quests.HUDQuestObjectiveType) (sarnautv1.QuestObjectiveType, error) {
	switch kind {
	case quests.HUDQuestObjectiveKill:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_KILL, nil
	case quests.HUDQuestObjectiveItem:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_ITEM, nil
	case quests.HUDQuestObjectiveSpecial:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_SPECIAL, nil
	case quests.HUDQuestObjectiveHonor:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_HONOR, nil
	case quests.HUDQuestObjectiveKillAvatar:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_KILL_AVATAR, nil
	case quests.HUDQuestObjectiveMoney:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_MONEY, nil
	case quests.HUDQuestObjectiveShipUpgradeMoney:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_SHIP_UPGRADE_MONEY, nil
	case quests.HUDQuestObjectiveUpgradableShip:
		return sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_UPGRADABLE_SHIP, nil
	default:
		return 0, fmt.Errorf("quest objective type %q has no wire value", kind)
	}
}

func questInfoRefusalToProto(refusal quests.Refusal) (sarnautv1.QuestInfoRefusal, error) {
	switch refusal {
	case quests.RefusalNone:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_NONE, nil
	case quests.RefusalUnknownQuest:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_UNKNOWN_QUEST, nil
	case quests.RefusalUnavailable:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_UNAVAILABLE, nil
	case quests.RefusalLogFull:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_LOG_FULL, nil
	case quests.RefusalOutOfRange:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_OUT_OF_RANGE, nil
	case quests.RefusalWrongNPC:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_WRONG_NPC, nil
	case quests.RefusalNotComplete:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_NOT_COMPLETE, nil
	case quests.RefusalAlreadyComplete:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_ALREADY_COMPLETE, nil
	case quests.RefusalBagFull:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_BAG_FULL, nil
	case quests.RefusalCannotCancel:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_CANNOT_CANCEL, nil
	case quests.RefusalInternal, quests.RefusalNotAQuestGiver:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_INTERNAL, nil
	case quests.RefusalInvalidRewardChoice:
		return sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_INVALID_REWARD_CHOICE, nil
	default:
		return 0, fmt.Errorf("quest refusal %d has no HUD wire value", uint8(refusal))
	}
}

func actionBarReplacementToProto(
	revision uint64,
	requestID uint64,
	bar combat.ActionBar,
	rejection combat.ActionRejection,
) (*sarnautv1.ServerMessage, error) {
	refusal, err := actionActivationRefusalToProto(rejection)
	if err != nil {
		return nil, err
	}
	replacement := &sarnautv1.ActionBarReplacement{
		Revision:          revision,
		RequestId:         requestID,
		ActivationRefusal: refusal,
		Slots:             make([]*sarnautv1.ActionBarSlotState, 0, combat.ActionBarSlotCount),
	}
	for index, slot := range bar.Slots {
		if slot.SlotIndex != uint32(index) {
			return nil, fmt.Errorf(
				"action bar entry %d carries slot index %d", index, slot.SlotIndex,
			)
		}
		if slot.CooldownRemainingMS < 0 || slot.CooldownDurationMS < 0 {
			return nil, fmt.Errorf("action bar slot %d carries a negative cooldown", index)
		}
		if slot.Available != (slot.UnavailableReason == combat.ActionAvailable) {
			return nil, fmt.Errorf("action bar slot %d availability and reason disagree", index)
		}
		reason, err := actionUnavailableReasonToProto(slot.UnavailableReason)
		if err != nil {
			return nil, fmt.Errorf("action bar slot %d: %w", index, err)
		}
		replacement.Slots = append(replacement.Slots, &sarnautv1.ActionBarSlotState{
			SlotIndex:                     slot.SlotIndex,
			AbilityId:                     slot.AbilityID,
			CooldownRemainingMilliseconds: slot.CooldownRemainingMS,
			CooldownDurationMilliseconds:  slot.CooldownDurationMS,
			Available:                     slot.Available,
			UnavailableReason:             reason,
		})
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_ActionBarReplacement{
			ActionBarReplacement: replacement,
		},
	}, nil
}

func actionUnavailableReasonToProto(
	reason combat.ActionUnavailableReason,
) (sarnautv1.ActionUnavailableReason, error) {
	switch reason {
	case combat.ActionUnavailableUnspecified:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_UNSPECIFIED, nil
	case combat.ActionAvailable:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_NONE, nil
	case combat.ActionUnavailableEmptySlot:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_EMPTY_SLOT, nil
	case combat.ActionUnavailableOnCooldown:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_ON_COOLDOWN, nil
	case combat.ActionUnavailableNoTarget:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_NO_TARGET, nil
	case combat.ActionUnavailableInvalidTarget:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_INVALID_TARGET, nil
	case combat.ActionUnavailableOutOfRange:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_OUT_OF_RANGE, nil
	case combat.ActionUnavailableActorDead:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_ACTOR_DEAD, nil
	case combat.ActionUnavailableDisabled:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_DISABLED, nil
	case combat.ActionUnavailableNoResource:
		return sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_NO_RESOURCE, nil
	default:
		return 0, fmt.Errorf("action unavailable reason %d has no wire value", uint8(reason))
	}
}

func actionActivationRefusalToProto(
	rejection combat.ActionRejection,
) (sarnautv1.ActionActivationRefusal, error) {
	switch rejection {
	case combat.ActionRejectionNone:
		return sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_NONE, nil
	case combat.ActionRejectionInvalidSlot:
		return sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_INVALID_SLOT, nil
	case combat.ActionRejectionEmptySlot:
		return sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_EMPTY_SLOT, nil
	default:
		return 0, fmt.Errorf("action rejection %d has no wire value", uint8(rejection))
	}
}

func targetStateReplacementToProto(
	revision uint64,
	requestID uint64,
	hasAuthority bool,
	selectedEntityID uint64,
	rejection combat.Rejection,
) (*sarnautv1.ServerMessage, error) {
	refusal, err := targetSelectRefusalToProto(rejection)
	if err != nil {
		return nil, err
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_TargetStateReplacement{
			TargetStateReplacement: &sarnautv1.TargetStateReplacement{
				Revision:         revision,
				RequestId:        requestID,
				HasAuthority:     hasAuthority,
				SelectedEntityId: selectedEntityID,
				Refusal:          refusal,
			},
		},
	}, nil
}

func targetSelectRefusalToProto(rejection combat.Rejection) (sarnautv1.TargetSelectRefusal, error) {
	switch rejection {
	case combat.RejectionNone:
		return sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NONE, nil
	case combat.RejectionNoTarget:
		return sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NO_TARGET, nil
	case combat.RejectionInvalidTarget:
		return sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_INVALID_TARGET, nil
	case combat.RejectionTargetDead:
		return sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_TARGET_DEAD, nil
	default:
		return 0, fmt.Errorf("combat rejection %d has no target-selection wire value", uint8(rejection))
	}
}

// lootStateReplacementToProto maps the current complete corpse view. A nil
// offer emits open=false and no entity id, which is also the exact response to
// the bodyless LootClose request. A stale command preserves the supplied open
// view and reports the session-owned revision refusal.
func lootStateReplacementToProto(
	revision uint64,
	requestID uint64,
	open bool,
	offer *loot.Offer,
	refusal loot.Refusal,
	stale bool,
) (*sarnautv1.ServerMessage, error) {
	if open != (offer != nil) {
		return nil, fmt.Errorf("loot open flag and current offer disagree")
	}
	wireRefusal, err := lootUIRefusalToProto(refusal, stale)
	if err != nil {
		return nil, err
	}
	replacement := &sarnautv1.LootStateReplacement{
		Revision:  revision,
		RequestId: requestID,
		Refusal:   wireRefusal,
		PageSize:  loot.LootPageSize,
	}
	if open {
		if offer.CorpseEntityID == 0 {
			return nil, fmt.Errorf("open loot offer has no corpse entity id")
		}
		if offer.Money < 0 {
			return nil, fmt.Errorf("loot offer carries negative money %d", offer.Money)
		}
		if len(offer.Items) > loot.MaxObservableLootEntries {
			return nil, fmt.Errorf(
				"loot offer has %d items, maximum is %d",
				len(offer.Items), loot.MaxObservableLootEntries,
			)
		}
		replacement.Open = true
		replacement.LootEntityId = offer.CorpseEntityID
		replacement.Money = offer.Money
		replacement.TotalCount = uint32(len(offer.Items))
		for index, grant := range offer.Items {
			if grant.Count <= 0 {
				return nil, fmt.Errorf("loot item %d has nonpositive count %d", index, grant.Count)
			}
			replacement.Items = append(replacement.Items, &sarnautv1.LootItemState{
				ItemIndex:     int32(index),
				ProductItemId: grant.ItemID,
				Count:         uint32(grant.Count),
				IsCursed:      grant.IsCursed,
			})
		}
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_LootStateReplacement{
			LootStateReplacement: replacement,
		},
	}, nil
}

func lootUIRefusalToProto(refusal loot.Refusal, stale bool) (sarnautv1.LootUiRefusal, error) {
	if stale {
		if refusal != loot.RefusalNone {
			return 0, fmt.Errorf("stale loot state also carries domain refusal %s", refusal)
		}
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_STALE_REVISION, nil
	}
	switch refusal {
	case loot.RefusalNone:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_NONE, nil
	case loot.RefusalNoCorpse:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_NO_CORPSE, nil
	case loot.RefusalNotYourLoot:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_NOT_YOUR_LOOT, nil
	case loot.RefusalAlreadyLooted:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_ALREADY_LOOTED, nil
	case loot.RefusalBagFull:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_BAG_FULL, nil
	case loot.RefusalInProgress:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_IN_PROGRESS, nil
	case loot.RefusalInternal:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_INTERNAL, nil
	case loot.RefusalInvalidItemIndex:
		return sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_INVALID_INDEX, nil
	default:
		return 0, fmt.Errorf("loot refusal %d has no HUD wire value", uint8(refusal))
	}
}

func optionalNonnegativeUint64(value *int64, field, questID string) (*uint64, error) {
	if value == nil {
		return nil, nil
	}
	if *value < 0 {
		return nil, fmt.Errorf("quest %q %s is negative: %d", questID, field, *value)
	}
	converted := uint64(*value)
	return &converted, nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
