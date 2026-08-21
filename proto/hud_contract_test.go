package proto_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestGameplayUIWireGolden(t *testing.T) {
	item := func(id uint64, product string, count uint32) *sarnautv1.ItemStackState {
		return &sarnautv1.ItemStackState{InstanceId: id, ProductItemId: product, StackCount: count}
	}
	inv := func(revision uint64) *sarnautv1.InventoryStateReplacement {
		return &sarnautv1.InventoryStateReplacement{
			Revision:          revision,
			LayoutId:          sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_18,
			Capacity:          18,
			Currency:          77,
			EquippedBagItemId: 201,
			PartitionSizes:    []uint32{12, 6},
			Slots: []*sarnautv1.InventorySlotState{{
				SlotIndex: 2,
				Item:      item(101, "item.heal-elixir", 3),
			}},
		}
	}

	tests := map[string]proto.Message{
		"client_quest_turn_in_reward": &sarnautv1.ClientMessage{
			ClientSeq: 40,
			Payload: &sarnautv1.ClientMessage_QuestTurnIn{QuestTurnIn: &sarnautv1.QuestTurnIn{
				QuestId: "quest.reward-choice", FinisherEntityId: 41, RewardIndex: 2, RequestId: 42, ExpectedRevision: 43,
			}},
		},
		"client_inventory_move": &sarnautv1.ClientMessage{
			ClientSeq: 1,
			Payload: &sarnautv1.ClientMessage_InventoryMove{InventoryMove: &sarnautv1.InventoryMove{
				RequestId:        2,
				ExpectedRevision: 3,
				FromSlot:         2,
				ToSlot:           17,
			}},
		},
		"client_loot_take_item_all": &sarnautv1.ClientMessage{
			ClientSeq: 4,
			Payload: &sarnautv1.ClientMessage_LootTakeItem{LootTakeItem: &sarnautv1.LootTakeItem{
				RequestId: 5, LootEntityId: 6, ExpectedRevision: 7, ItemIndex: -1,
			}},
		},
		"client_loot_take_money_all": &sarnautv1.ClientMessage{
			ClientSeq: 8,
			Payload: &sarnautv1.ClientMessage_LootTakeMoney{LootTakeMoney: &sarnautv1.LootTakeMoney{
				RequestId: 9, LootEntityId: 10, ExpectedRevision: 11,
			}},
		},
		"client_loot_take_all": &sarnautv1.ClientMessage{
			ClientSeq: 12,
			Payload: &sarnautv1.ClientMessage_LootTakeAll{LootTakeAll: &sarnautv1.LootTakeAll{
				RequestId: 13, LootEntityId: 14, ExpectedRevision: 15,
			}},
		},
		"client_loot_close": &sarnautv1.ClientMessage{
			ClientSeq: 16,
			Payload: &sarnautv1.ClientMessage_LootClose{LootClose: &sarnautv1.LootClose{
				RequestId: 17,
			}},
		},
		"client_quest_share": &sarnautv1.ClientMessage{
			ClientSeq: 20,
			Payload: &sarnautv1.ClientMessage_QuestShare{QuestShare: &sarnautv1.QuestShare{
				RequestId: 21, QuestId: "quest.rat-killer", ExpectedRevision: 22,
			}},
		},
		"client_quest_share_response": &sarnautv1.ClientMessage{
			ClientSeq: 22,
			Payload: &sarnautv1.ClientMessage_QuestShareResponse{QuestShareResponse: &sarnautv1.QuestShareResponse{
				RequestId: 23, InviteId: 24, Accept: true, ExpectedRevision: 25,
			}},
		},
		"client_target_select": &sarnautv1.ClientMessage{
			ClientSeq: 23,
			Payload: &sarnautv1.ClientMessage_TargetSelect{TargetSelect: &sarnautv1.TargetSelect{
				TargetEntityId: 99, RequestId: 100,
			}},
		},
		"client_activate_action": &sarnautv1.ClientMessage{
			ClientSeq: 24,
			Payload: &sarnautv1.ClientMessage_ActivateAction{ActivateAction: &sarnautv1.ActivateAction{
				RequestId: 25, SlotIndex: 35, ClientTick: 26, ExpectedRevision: 27,
			}},
		},
		"server_inventory_replacement": &sarnautv1.ServerMessage{
			ServerTick: 25,
			Payload:    &sarnautv1.ServerMessage_InventoryStateReplacement{InventoryStateReplacement: inv(26)},
		},
		"server_inventory_move_result": &sarnautv1.ServerMessage{
			ServerTick: 27,
			Payload: &sarnautv1.ServerMessage_InventoryMoveResult{InventoryMoveResult: &sarnautv1.InventoryMoveResult{
				RequestId: 28, Refusal: sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_NONE, Replacement: inv(29),
			}},
		},
		"server_inventory_slot_cooldown_update": &sarnautv1.ServerMessage{
			ServerTick: 45,
			Payload: &sarnautv1.ServerMessage_InventorySlotCooldownUpdate{InventorySlotCooldownUpdate: &sarnautv1.InventorySlotCooldownUpdate{
				InventoryRevision: 46, SlotIndex: 7, ItemInstanceId: 101,
				SpellCooldown: &sarnautv1.ItemSlotSpellCooldownState{ProductSpellId: "spell.item.heal", RemainingMilliseconds: 500, DurationMilliseconds: 1000},
			}},
		},
		"server_character_replacement": &sarnautv1.ServerMessage{
			ServerTick: 30,
			Payload: &sarnautv1.ServerMessage_CharacterStateReplacement{CharacterStateReplacement: &sarnautv1.CharacterStateReplacement{
				Revision: 31, CharacterEntityId: 32, Name: "Ayla", Level: 7,
				Equipment: []*sarnautv1.EquipmentSlotState{{Slot: sarnautv1.EquipmentSlotId_EQUIPMENT_SLOT_ID_HELM, Item: item(102, "item.leather-cap", 1)}},
				Bag:       item(103, "item.bag-12", 1),
				Stats:     []*sarnautv1.CharacterStatState{{Stat: sarnautv1.CharacterStatId_CHARACTER_STAT_ID_STRENGTH, Base: proto.Float32(44)}},
			}},
		},
		"server_loot_replacement": &sarnautv1.ServerMessage{
			ServerTick: 33,
			Payload: &sarnautv1.ServerMessage_LootStateReplacement{LootStateReplacement: &sarnautv1.LootStateReplacement{
				Revision: 34, RequestId: 35, LootEntityId: 36, Open: true,
				Refusal: sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_NONE, Money: 9,
				TotalCount: 1, PageSize: 4,
				Items: []*sarnautv1.LootItemState{{ItemIndex: 0, ProductItemId: "item.rat-tail", Count: 2, IsCursed: true}},
			}},
		},
		"server_target_replacement": &sarnautv1.ServerMessage{
			ServerTick: 42,
			Payload: &sarnautv1.ServerMessage_TargetStateReplacement{TargetStateReplacement: &sarnautv1.TargetStateReplacement{
				Revision: 43, RequestId: 44, HasAuthority: true, SelectedEntityId: 99,
				Refusal: sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NONE,
			}},
		},
		"server_action_bar_replacement": &sarnautv1.ServerMessage{
			ServerTick: 44,
			Payload: &sarnautv1.ServerMessage_ActionBarReplacement{ActionBarReplacement: &sarnautv1.ActionBarReplacement{
				Revision: 45, RequestId: 46, ActivationRefusal: sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_NONE,
				Slots: []*sarnautv1.ActionBarSlotState{{
					SlotIndex: 35,
					AbilityId: "ability.warrior.auto-attack", CooldownRemainingMilliseconds: 2, CooldownDurationMilliseconds: 5, Available: true,
					UnavailableReason: sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_NO_RESOURCE,
				}},
			}},
		},
		"server_combat_no_resource": &sarnautv1.ServerMessage{
			ServerTick: 47,
			Payload: &sarnautv1.ServerMessage_CombatEvent{CombatEvent: &sarnautv1.CombatEvent{
				CasterId: 1, TargetId: 2, AbilityId: "ability.resource-gated",
				Rejection: sarnautv1.AbilityRejection_ABILITY_REJECTION_NO_RESOURCE,
			}},
		},
		"server_quest_log_replacement": &sarnautv1.ServerMessage{
			ServerTick: 37,
			Payload: &sarnautv1.ServerMessage_QuestLogReplacement{QuestLogReplacement: &sarnautv1.QuestLogReplacement{
				Revision:         38,
				VisibleQuests:    []*sarnautv1.QuestLogEntry{{QuestId: "quest.rat-killer", State: sarnautv1.QuestUiState_QUEST_UI_STATE_READY_TO_RETURN, Name: "Rat Killer", Level: 4}},
				BookmarkQuestIds: []string{"quest.rat-killer"},
				ShareInvites:     []*sarnautv1.QuestShareInvite{{InviteId: 39, QuestId: "quest.shared", SenderEntityId: 40, SenderName: "Borin", RemainingMilliseconds: 10_000, OnStart: true}},
			}},
		},
		"server_quest_info_replacement": &sarnautv1.ServerMessage{
			ServerTick: 41,
			Payload: &sarnautv1.ServerMessage_QuestInfoReplacement{QuestInfoReplacement: &sarnautv1.QuestInfoReplacement{
				Revision: 42, RequestId: 43, RequestedQuestId: "quest.rat-killer",
				Mode: sarnautv1.QuestInfoMode_QUEST_INFO_MODE_TURN_IN, NpcEntityId: 77,
				Refusal:  sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_NONE,
				Info:     &sarnautv1.QuestInfo{Id: "quest.rat-killer", Name: "Rat Killer", Level: 4, Goal: "Defeat rats", CanCancel: true, RepeatPeriod: 21},
				Progress: &sarnautv1.QuestProgress{Id: "quest.rat-killer", State: sarnautv1.QuestUiState_QUEST_UI_STATE_IN_PROGRESS, Objectives: []*sarnautv1.QuestObjectiveState{{Name: "Rats", Progress: 2, Required: 3, Type: sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_KILL, ShowCounterValue: true}}},
				Reward:   &sarnautv1.QuestReward{Money: 5, Experience: 10, MandatoryItems: []*sarnautv1.QuestRewardItem{{ProductItemId: "item.reward", Count: 1}}, MandatoryItemsCount: 1},
			}},
		},
		"server_social_friends_replacement": &sarnautv1.ServerMessage{
			ServerTick: 48,
			Payload: &sarnautv1.ServerMessage_SocialFriendsReplacement{SocialFriendsReplacement: &sarnautv1.SocialFriendsReplacement{
				Revision: 49,
				Friends:  []*sarnautv1.SocialFriend{{CharacterId: "019200f0-0000-7000-8000-000000000032", DisplayName: "Ayla"}},
			}},
		},
	}

	golden := readHUDGolden(t)
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		message := tests[name]
		got, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		gotHex := hex.EncodeToString(got)
		if want := golden[name]; gotHex != want {
			t.Errorf("%s wire = %s, want %s", name, gotHex, want)
		}

		decoded := message.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(got, decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if !proto.Equal(message, decoded) {
			t.Errorf("%s round trip changed message\ngot:  %v\nwant: %v", name, decoded, message)
		}
	}
	if len(golden) != len(tests) {
		t.Fatalf("golden has %d entries, want exactly %d", len(golden), len(tests))
	}
}

func TestGameplayUIEnvelopeCasesUseHUDRange(t *testing.T) {
	client := (&sarnautv1.ClientMessage{}).ProtoReflect().Descriptor()
	assertHUDFields(t, client, map[protoreflect.Name]fieldContract{
		"inventory_move":       {30, protoreflect.MessageKind},
		"loot_take_item":       {31, protoreflect.MessageKind},
		"loot_take_money":      {32, protoreflect.MessageKind},
		"loot_take_all":        {33, protoreflect.MessageKind},
		"loot_close":           {34, protoreflect.MessageKind},
		"quest_share":          {35, protoreflect.MessageKind},
		"quest_share_response": {36, protoreflect.MessageKind},
		"target_select":        {37, protoreflect.MessageKind},
		"activate_action":      {38, protoreflect.MessageKind},
	})
	server := (&sarnautv1.ServerMessage{}).ProtoReflect().Descriptor()
	assertHUDFields(t, server, map[protoreflect.Name]fieldContract{
		"inventory_state_replacement":    {30, protoreflect.MessageKind},
		"inventory_move_result":          {31, protoreflect.MessageKind},
		"character_state_replacement":    {32, protoreflect.MessageKind},
		"loot_state_replacement":         {33, protoreflect.MessageKind},
		"quest_log_replacement":          {34, protoreflect.MessageKind},
		"quest_info_replacement":         {35, protoreflect.MessageKind},
		"target_state_replacement":       {36, protoreflect.MessageKind},
		"action_bar_replacement":         {37, protoreflect.MessageKind},
		"inventory_slot_cooldown_update": {38, protoreflect.MessageKind},
		"social_friends_replacement":     {39, protoreflect.MessageKind},
	})

	// Chat and every older case retain their field numbers. The absent gaps are
	// intentionally left free for separately coordinated protocol additions.
	assertHUDField(t, client, "chat_send_request", 18, protoreflect.MessageKind)
	assertHUDField(t, server, "chat_delivery", 20, protoreflect.MessageKind)
	assertHUDField(t, server, "chat_rejection", 21, protoreflect.MessageKind)
	for number := protoreflect.FieldNumber(22); number <= 29; number++ {
		if client.Fields().ByNumber(number) != nil || server.Fields().ByNumber(number) != nil {
			t.Errorf("envelope field %d is occupied; want coordinated gap 22..29", number)
		}
	}
}

func TestSocialFriendsCarryOnlyStableIdentityAndName(t *testing.T) {
	friend := (&sarnautv1.SocialFriend{}).ProtoReflect().Descriptor()
	if friend.Fields().Len() != 2 {
		t.Fatalf("SocialFriend fields = %d, want 2", friend.Fields().Len())
	}
	assertHUDField(t, friend, "character_id", 1, protoreflect.StringKind)
	assertHUDField(t, friend, "display_name", 2, protoreflect.StringKind)

	replacement := (&sarnautv1.SocialFriendsReplacement{}).ProtoReflect().Descriptor()
	assertHUDField(t, replacement, "revision", 1, protoreflect.Uint64Kind)
	assertHUDField(t, replacement, "friends", 2, protoreflect.MessageKind)
	options, ok := replacement.Fields().ByName("friends").Options().(*descriptorpb.FieldOptions)
	if !ok {
		t.Fatal("SocialFriendsReplacement.friends options are missing")
	}
	if proto.HasExtension(options, sarnautv1.E_MaxCount) {
		t.Error("SocialFriendsReplacement.friends invents an unproven maximum count")
	}
}

func TestInventoryLayoutIDsAreRetailCapacities(t *testing.T) {
	descriptor := sarnautv1.InventoryLayoutId_INVENTORY_LAYOUT_ID_UNSPECIFIED.Descriptor()
	want := map[protoreflect.Name]protoreflect.EnumNumber{
		"INVENTORY_LAYOUT_ID_UNSPECIFIED": 0,
		"INVENTORY_LAYOUT_ID_12":          12,
		"INVENTORY_LAYOUT_ID_16":          16,
		"INVENTORY_LAYOUT_ID_18":          18,
		"INVENTORY_LAYOUT_ID_24":          24,
		"INVENTORY_LAYOUT_ID_30":          30,
		"INVENTORY_LAYOUT_ID_36":          36,
		"INVENTORY_LAYOUT_ID_42":          42,
		"INVENTORY_LAYOUT_ID_48":          48,
		"INVENTORY_LAYOUT_ID_54":          54,
		"INVENTORY_LAYOUT_ID_60":          60,
	}
	assertEnumValues(t, descriptor, want)
	wantPartitions := map[protoreflect.EnumNumber][]uint32{
		0: {}, 12: {12}, 16: {16}, 18: {12, 6}, 24: {16, 8}, 30: {30},
		36: {8, 8, 8, 6, 6}, 42: {30, 12}, 48: {12, 12, 12, 12},
		54: {30, 12, 12}, 60: {30, 30},
	}
	for number, wantCapacities := range wantPartitions {
		value := descriptor.Values().ByNumber(number)
		if value == nil {
			t.Fatalf("layout %d is missing", number)
		}
		options, ok := value.Options().(*descriptorpb.EnumValueOptions)
		if !ok || !proto.HasExtension(options, sarnautv1.E_InventoryPartitionLayout) {
			t.Fatalf("layout %d lacks partition contract", number)
		}
		contract, ok := proto.GetExtension(options, sarnautv1.E_InventoryPartitionLayout).(*sarnautv1.InventoryPartitionLayout)
		if !ok {
			t.Fatalf("layout %d partition option has wrong type", number)
		}
		if got := fmt.Sprint(contract.GetCapacities()); got != fmt.Sprint(wantCapacities) {
			t.Errorf("layout %d partitions = %v, want %v", number, contract.GetCapacities(), wantCapacities)
		}
	}

	replacement := (&sarnautv1.InventoryStateReplacement{}).ProtoReflect().Descriptor()
	assertHUDMessageOption(t, replacement, sarnautv1.E_UniqueNonzeroInstanceIds, true)
	assertHUDMessageOption(t, replacement, sarnautv1.E_EquippedBagIsReference, true)
	assertHUDField(t, replacement, "revision", 1, protoreflect.Uint64Kind)
	assertHUDField(t, replacement, "layout_id", 2, protoreflect.EnumKind)
	assertHUDFieldOption(t, replacement.Fields().ByName("capacity"), sarnautv1.E_MaxUint, uint64(60))
	assertHUDField(t, replacement, "currency", 4, protoreflect.Int64Kind)
	assertHUDField(t, replacement, "equipped_bag_item_id", 5, protoreflect.Uint64Kind)
	assertHUDFieldOption(t, replacement.Fields().ByName("partition_sizes"), sarnautv1.E_MaxCount, uint32(5))
	assertHUDFieldOption(t, replacement.Fields().ByName("slots"), sarnautv1.E_MaxCount, uint32(60))
}

func TestInventoryMoveIsRevisionedAndReturnsFullReplacement(t *testing.T) {
	move := (&sarnautv1.InventoryMove{}).ProtoReflect().Descriptor()
	assertHUDFields(t, move, map[protoreflect.Name]fieldContract{
		"request_id":        {1, protoreflect.Uint64Kind},
		"expected_revision": {2, protoreflect.Uint64Kind},
		"from_slot":         {3, protoreflect.Uint32Kind},
		"to_slot":           {4, protoreflect.Uint32Kind},
	})
	result := (&sarnautv1.InventoryMoveResult{}).ProtoReflect().Descriptor()
	assertHUDFields(t, result, map[protoreflect.Name]fieldContract{
		"request_id":  {1, protoreflect.Uint64Kind},
		"refusal":     {2, protoreflect.EnumKind},
		"replacement": {3, protoreflect.MessageKind},
	})
	if got := result.Fields().ByName("replacement").Message().FullName(); got != "sarnaut.v1.InventoryStateReplacement" {
		t.Errorf("move result replacement type = %s", got)
	}
	assertEnumValues(t, sarnautv1.InventoryMoveRefusal_INVENTORY_MOVE_REFUSAL_UNSPECIFIED.Descriptor(), map[protoreflect.Name]protoreflect.EnumNumber{
		"INVENTORY_MOVE_REFUSAL_UNSPECIFIED":         0,
		"INVENTORY_MOVE_REFUSAL_NONE":                1,
		"INVENTORY_MOVE_REFUSAL_STALE_REVISION":      2,
		"INVENTORY_MOVE_REFUSAL_INVALID_SOURCE":      3,
		"INVENTORY_MOVE_REFUSAL_INVALID_DESTINATION": 4,
		"INVENTORY_MOVE_REFUSAL_EMPTY_SOURCE":        5,
		"INVENTORY_MOVE_REFUSAL_SAME_SLOT":           6,
		"INVENTORY_MOVE_REFUSAL_STACK_FULL":          7,
		"INVENTORY_MOVE_REFUSAL_INVALID_STATE":       8,
		"INVENTORY_MOVE_REFUSAL_INTERNAL":            9,
	})
}

func TestInventorySlotCooldownIsItemAndRevisionGuarded(t *testing.T) {
	slot := (&sarnautv1.InventorySlotState{}).ProtoReflect().Descriptor()
	assertHUDField(t, slot, "spell_cooldown", 3, protoreflect.MessageKind)
	cooldown := (&sarnautv1.ItemSlotSpellCooldownState{}).ProtoReflect().Descriptor()
	assertHUDFields(t, cooldown, map[protoreflect.Name]fieldContract{
		"product_spell_id":       {1, protoreflect.StringKind},
		"remaining_milliseconds": {2, protoreflect.Int64Kind},
		"duration_milliseconds":  {3, protoreflect.Int64Kind},
	})
	assertHUDFieldOption(t, cooldown.Fields().ByName("remaining_milliseconds"), sarnautv1.E_MinSint, int64(0))
	assertHUDFieldOption(t, cooldown.Fields().ByName("duration_milliseconds"), sarnautv1.E_MinSint, int64(0))
	update := (&sarnautv1.InventorySlotCooldownUpdate{}).ProtoReflect().Descriptor()
	assertHUDFields(t, update, map[protoreflect.Name]fieldContract{
		"inventory_revision": {1, protoreflect.Uint64Kind},
		"slot_index":         {2, protoreflect.Uint32Kind},
		"item_instance_id":   {3, protoreflect.Uint64Kind},
		"spell_cooldown":     {4, protoreflect.MessageKind},
	})
	assertHUDFieldOption(t, update.Fields().ByName("slot_index"), sarnautv1.E_MaxUint, uint64(59))
}

func TestCharacterPanelPreservesRetailOrdinals(t *testing.T) {
	equipment := sarnautv1.EquipmentSlotId_EQUIPMENT_SLOT_ID_HELM.Descriptor()
	wantEquipment := map[protoreflect.Name]protoreflect.EnumNumber{
		"EQUIPMENT_SLOT_ID_HELM": 0, "EQUIPMENT_SLOT_ID_ARMOR": 1,
		"EQUIPMENT_SLOT_ID_PANTS": 2, "EQUIPMENT_SLOT_ID_BOOTS": 3,
		"EQUIPMENT_SLOT_ID_MANTLE": 4, "EQUIPMENT_SLOT_ID_GLOVES": 5,
		"EQUIPMENT_SLOT_ID_BRACERS": 6, "EQUIPMENT_SLOT_ID_BELT": 7,
		"EQUIPMENT_SLOT_ID_RING_1": 8, "EQUIPMENT_SLOT_ID_RING_2": 9,
		"EQUIPMENT_SLOT_ID_EARRINGS": 10, "EQUIPMENT_SLOT_ID_NECKLACE": 11,
		"EQUIPMENT_SLOT_ID_CLOAK": 12, "EQUIPMENT_SLOT_ID_SHIRT": 13,
		"EQUIPMENT_SLOT_ID_MAINHAND": 14, "EQUIPMENT_SLOT_ID_OFFHAND": 15,
		"EQUIPMENT_SLOT_ID_RANGED": 16, "EQUIPMENT_SLOT_ID_TABARD": 18,
		"EQUIPMENT_SLOT_ID_TRINKET": 19, "EQUIPMENT_SLOT_ID_DEATH_INSURANCE": 21,
	}
	assertEnumValues(t, equipment, wantEquipment)
	for number, name := range map[protoreflect.EnumNumber]protoreflect.Name{17: "EQUIPMENT_SLOT_ID_AMMO", 20: "EQUIPMENT_SLOT_ID_BAG"} {
		if !equipment.ReservedRanges().Has(number) || !equipment.ReservedNames().Has(name) {
			t.Errorf("equipment ordinal %d and name %s are not both reserved", number, name)
		}
	}

	stats := sarnautv1.CharacterStatId_CHARACTER_STAT_ID_STRENGTH.Descriptor()
	statNames := []string{"STRENGTH", "MIGHT", "DEXTERITY", "AGILITY", "STAMINA", "PRECISION", "HARDINESS", "INTELLECT", "INTUITION", "SPIRIT", "WILL", "RESOLVE", "WISDOM", "LETHALITY"}
	wantStats := make(map[protoreflect.Name]protoreflect.EnumNumber, len(statNames))
	for ordinal, name := range statNames {
		wantStats[protoreflect.Name("CHARACTER_STAT_ID_"+name)] = protoreflect.EnumNumber(ordinal)
	}
	assertEnumValues(t, stats, wantStats)

	character := (&sarnautv1.CharacterStateReplacement{}).ProtoReflect().Descriptor()
	assertHUDMessageOption(t, character, sarnautv1.E_UniqueNonzeroInstanceIds, true)
	assertHUDField(t, character, "name", 3, protoreflect.StringKind)
	assertHUDField(t, character, "level", 4, protoreflect.Uint32Kind)
	assertHUDFieldOption(t, character.Fields().ByName("equipment"), sarnautv1.E_ExactCount, uint32(20))
	assertHUDField(t, character, "bag", 6, protoreflect.MessageKind)
	assertHUDFieldOption(t, character.Fields().ByName("stats"), sarnautv1.E_ExactCount, uint32(14))
	stat := (&sarnautv1.CharacterStatState{}).ProtoReflect().Descriptor()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{"base": 2, "effective": 3, "long_term": 4} {
		assertHUDField(t, stat, name, number, protoreflect.FloatKind)
		if !stat.Fields().ByName(name).HasPresence() {
			t.Errorf("CharacterStatState.%s lost optional presence", name)
		}
	}
}

func TestItemStateCarriesOnlyProductIdentityAndMutableAuthority(t *testing.T) {
	item := (&sarnautv1.ItemStackState{}).ProtoReflect().Descriptor()
	assertHUDFields(t, item, map[protoreflect.Name]fieldContract{
		"instance_id":                   {1, protoreflect.Uint64Kind},
		"product_item_id":               {2, protoreflect.StringKind},
		"stack_count":                   {3, protoreflect.Uint32Kind},
		"counter_value":                 {4, protoreflect.Int32Kind},
		"is_bound":                      {5, protoreflect.BoolKind},
		"is_cursed":                     {6, protoreflect.BoolKind},
		"is_quest_operator":             {7, protoreflect.BoolKind},
		"remove_time":                   {8, protoreflect.Int64Kind},
		"rune_product_resource_id":      {9, protoreflect.StringKind},
		"rune_slot_product_resource_id": {10, protoreflect.StringKind},
	})
	for _, name := range []protoreflect.Name{"rune_product_resource_id", "rune_slot_product_resource_id"} {
		if !item.Fields().ByName(name).HasPresence() {
			t.Errorf("ItemStackState.%s lost optional presence", name)
		}
	}
	for _, forbidden := range []protoreflect.Name{"item_id", "name", "localized_name", "icon", "quality", "category", "prepared", "cooldown_remaining", "cooldown_duration"} {
		if item.Fields().ByName(forbidden) != nil {
			t.Errorf("ItemStackState exposes client-catalog field %q", forbidden)
		}
	}
}

func TestLootContractPreservesRetailPagingAndSelectors(t *testing.T) {
	closeRequest := (&sarnautv1.LootClose{}).ProtoReflect().Descriptor()
	if closeRequest.Fields().Len() != 1 {
		t.Fatalf("LootClose fields = %d, want request_id only", closeRequest.Fields().Len())
	}
	assertHUDField(t, closeRequest, "request_id", 1, protoreflect.Uint64Kind)
	for _, forbidden := range []protoreflect.Name{"loot_entity_id", "expected_revision"} {
		if closeRequest.Fields().ByName(forbidden) != nil {
			t.Errorf("LootClose exposes non-session-local field %q", forbidden)
		}
	}

	takeItem := (&sarnautv1.LootTakeItem{}).ProtoReflect().Descriptor()
	index := takeItem.Fields().ByName("item_index")
	assertHUDFieldOption(t, index, sarnautv1.E_MinSint, int64(-1))
	assertHUDFieldOption(t, index, sarnautv1.E_MaxSint, int64(19))
	if takeItem.Fields().ByName("item_id") != nil {
		t.Fatal("LootTakeItem must use retail item_index, not item_id")
	}
	takeMoney := (&sarnautv1.LootTakeMoney{}).ProtoReflect().Descriptor()
	if takeMoney.Fields().ByName("amount") != nil {
		t.Error("LootTakeMoney exposes an unimplemented partial-money amount")
	}

	replacement := (&sarnautv1.LootStateReplacement{}).ProtoReflect().Descriptor()
	assertHUDField(t, replacement, "open", 4, protoreflect.BoolKind)
	if replacement.Fields().ByName("do_not_disturb") != nil {
		t.Error("loot DND was misread as do-not-disturb; drag-and-drop needs no state field")
	}
	assertHUDField(t, replacement, "refusal", 5, protoreflect.EnumKind)
	assertHUDFieldOption(t, replacement.Fields().ByName("total_count"), sarnautv1.E_MaxUint, uint64(20))
	assertHUDFieldOption(t, replacement.Fields().ByName("page_size"), sarnautv1.E_FixedUint, uint64(4))
	assertHUDFieldOption(t, replacement.Fields().ByName("items"), sarnautv1.E_MaxCount, uint32(20))
	lootItem := (&sarnautv1.LootItemState{}).ProtoReflect().Descriptor()
	assertHUDFields(t, lootItem, map[protoreflect.Name]fieldContract{
		"item_index":      {1, protoreflect.Int32Kind},
		"product_item_id": {2, protoreflect.StringKind},
		"count":           {3, protoreflect.Uint32Kind},
		"is_cursed":       {4, protoreflect.BoolKind},
	})
	for _, forbidden := range []protoreflect.Name{"item", "instance_id", "is_bound", "is_quest_operator"} {
		if lootItem.Fields().ByName(forbidden) != nil {
			t.Errorf("LootItemState exposes inventory-instance field %q", forbidden)
		}
	}
	lootOptions, ok := lootItem.Options().(*descriptorpb.MessageOptions)
	if !ok {
		t.Fatal("LootItemState descriptor options are missing")
	}
	if proto.HasExtension(lootOptions, sarnautv1.E_UniqueNonzeroInstanceIds) {
		t.Error("LootItemState incorrectly applies inventory instance-id uniqueness")
	}

	legacy := sarnautv1.LootRefusal_LOOT_REFUSAL_UNSPECIFIED.Descriptor()
	hud := sarnautv1.LootUiRefusal_LOOT_UI_REFUSAL_UNSPECIFIED.Descriptor()
	assertMirroredEnumPrefix(t, legacy, hud)
	invalid := hud.Values().ByName("LOOT_UI_REFUSAL_INVALID_INDEX")
	if invalid == nil || invalid.Number() != 8 {
		t.Errorf("invalid-index refusal = %v, want 8", invalid)
	}
	stale := hud.Values().ByName("LOOT_UI_REFUSAL_STALE_REVISION")
	if stale == nil || stale.Number() != 9 {
		t.Errorf("stale-revision refusal = %v, want 9", stale)
	}
}

func TestQuestDetailAndCardinalitiesAreFrozen(t *testing.T) {
	questInfo := (&sarnautv1.QuestInfo{}).ProtoReflect().Descriptor()
	if questInfo.Fields().Len() != 27 {
		t.Fatalf("QuestInfo fields = %d, want 27", questInfo.Fields().Len())
	}
	assertHUDField(t, questInfo, "repeat_period", 21, protoreflect.Int32Kind)
	for _, name := range []protoreflect.Name{"debug_name", "zones_map_id"} {
		if !questInfo.Fields().ByName(name).HasPresence() {
			t.Errorf("QuestInfo.%s lost optional presence", name)
		}
	}
	assertHUDField(t, questInfo, "is_in_secret_sequence", 17, protoreflect.BoolKind)
	assertHUDField(t, questInfo, "is_secret", 22, protoreflect.BoolKind)
	for i := 0; i < questInfo.Fields().Len(); i++ {
		field := questInfo.Fields().Get(i)
		if field.Cardinality() == protoreflect.Repeated && strings.Contains(string(field.Name()), "secret") {
			t.Errorf("unproven secret presentation collection leaked into wire as %s", field.Name())
		}
	}

	progress := (&sarnautv1.QuestProgress{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, progress.Fields().ByName("objectives"), sarnautv1.E_MaxCount, uint32(6))
	for _, name := range []protoreflect.Name{"timer_duration_milliseconds", "timer_time_left_milliseconds"} {
		if !progress.Fields().ByName(name).HasPresence() {
			t.Errorf("QuestProgress.%s lost optional presence", name)
		}
	}

	reward := (&sarnautv1.QuestReward{}).ProtoReflect().Descriptor()
	for _, name := range []protoreflect.Name{"mandatory_items", "alternative_items", "reputations", "currencies"} {
		assertHUDFieldOption(t, reward.Fields().ByName(name), sarnautv1.E_MaxCount, uint32(5))
	}
	if proto.HasExtension(reward.Fields().ByName("mandatory_items_count").Options(), sarnautv1.E_MaxUint) {
		t.Error("mandatory_items_count incorrectly capped to visible reward group size")
	}

	log := (&sarnautv1.QuestLogReplacement{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, log.Fields().ByName("visible_quests"), sarnautv1.E_MaxCount, uint32(20))
	assertHUDFieldOption(t, log.Fields().ByName("bookmark_quest_ids"), sarnautv1.E_MaxCount, uint32(3))
	assertHUDField(t, log, "share_invites", 4, protoreflect.MessageKind)
	assertHUDField(t, log, "share_results", 5, protoreflect.MessageKind)
	assertHUDField(t, log, "selected_quest_id", 6, protoreflect.StringKind)
	assertHUDField(t, log, "daily_count", 7, protoreflect.Uint32Kind)
	assertHUDField(t, log, "daily_limit", 8, protoreflect.Uint32Kind)
	assertHUDField(t, log, "request_id", 9, protoreflect.Uint64Kind)
	assertHUDField(t, log, "command_quest_id", 10, protoreflect.StringKind)
	assertHUDField(t, log, "command_refusal", 11, protoreflect.EnumKind)
	logEntry := (&sarnautv1.QuestLogEntry{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, logEntry.Fields().ByName("objectives"), sarnautv1.E_MaxCount, uint32(5))
	assertHUDField(t, logEntry, "name", 4, protoreflect.StringKind)
	assertHUDField(t, logEntry, "level", 5, protoreflect.Uint32Kind)
	assertHUDField(t, logEntry, "is_hide_level", 6, protoreflect.BoolKind)

	wantStates := map[protoreflect.Name]protoreflect.EnumNumber{
		"QUEST_UI_STATE_IN_PROGRESS": 0, "QUEST_UI_STATE_READY_TO_RETURN": 1,
		"QUEST_UI_STATE_COMPLETED": 2, "QUEST_UI_STATE_FAILED": 3,
	}
	assertEnumValues(t, sarnautv1.QuestUiState_QUEST_UI_STATE_IN_PROGRESS.Descriptor(), wantStates)
	wantObjectives := []string{"KILL", "ITEM", "SPECIAL", "HONOR", "KILL_AVATAR", "MONEY", "SHIP_UPGRADE_MONEY", "UPGRADABLE_SHIP"}
	wantObjectiveValues := make(map[protoreflect.Name]protoreflect.EnumNumber, len(wantObjectives))
	for ordinal, name := range wantObjectives {
		wantObjectiveValues[protoreflect.Name("QUEST_OBJECTIVE_TYPE_"+name)] = protoreflect.EnumNumber(ordinal)
	}
	assertEnumValues(t, sarnautv1.QuestObjectiveType_QUEST_OBJECTIVE_TYPE_KILL.Descriptor(), wantObjectiveValues)

	legacy := sarnautv1.QuestRefusal_QUEST_REFUSAL_UNSPECIFIED.Descriptor()
	hud := sarnautv1.QuestInfoRefusal_QUEST_INFO_REFUSAL_UNSPECIFIED.Descriptor()
	assertMirroredEnumPrefix(t, legacy, hud)
	for name, number := range map[protoreflect.Name]protoreflect.EnumNumber{
		"QUEST_INFO_REFUSAL_STALE_REVISION":        12,
		"QUEST_INFO_REFUSAL_INVALID_REWARD_CHOICE": 13,
	} {
		value := hud.Values().ByName(name)
		if value == nil || value.Number() != number {
			t.Errorf("%s = %v, want %d", name, value, number)
		}
	}
	share := sarnautv1.QuestShareRefusal_QUEST_SHARE_REFUSAL_UNSPECIFIED.Descriptor()
	noParty := share.Values().ByName("QUEST_SHARE_REFUSAL_NO_PARTY")
	if noParty == nil || noParty.Number() != 4 {
		t.Errorf("quest share no-party refusal = %v, want 4", noParty)
	}
	staleShare := share.Values().ByName("QUEST_SHARE_REFUSAL_STALE_REVISION")
	if staleShare == nil || staleShare.Number() != 10 {
		t.Errorf("quest share stale-revision refusal = %v, want 10", staleShare)
	}

	infoReplacement := (&sarnautv1.QuestInfoReplacement{}).ProtoReflect().Descriptor()
	assertHUDField(t, infoReplacement, "mode", 4, protoreflect.EnumKind)
	assertHUDField(t, infoReplacement, "npc_entity_id", 5, protoreflect.Uint64Kind)
	assertHUDField(t, infoReplacement, "refusal", 6, protoreflect.EnumKind)
	assertEnumValues(t, sarnautv1.QuestInfoMode_QUEST_INFO_MODE_NONE.Descriptor(), map[protoreflect.Name]protoreflect.EnumNumber{
		"QUEST_INFO_MODE_NONE": 0, "QUEST_INFO_MODE_OFFER": 1, "QUEST_INFO_MODE_TURN_IN": 2,
	})

	turnIn := (&sarnautv1.QuestTurnIn{}).ProtoReflect().Descriptor()
	assertHUDField(t, turnIn, "reward_index", 3, protoreflect.Uint32Kind)
	assertHUDField(t, turnIn, "request_id", 4, protoreflect.Uint64Kind)
	assertHUDField(t, turnIn, "expected_revision", 5, protoreflect.Uint64Kind)
	accept := (&sarnautv1.QuestAccept{}).ProtoReflect().Descriptor()
	assertHUDField(t, accept, "request_id", 3, protoreflect.Uint64Kind)
	assertHUDField(t, accept, "expected_revision", 4, protoreflect.Uint64Kind)
	abandon := (&sarnautv1.QuestAbandon{}).ProtoReflect().Descriptor()
	assertHUDField(t, abandon, "request_id", 2, protoreflect.Uint64Kind)
	assertHUDField(t, abandon, "expected_revision", 3, protoreflect.Uint64Kind)
	shareCommand := (&sarnautv1.QuestShare{}).ProtoReflect().Descriptor()
	assertHUDField(t, shareCommand, "expected_revision", 3, protoreflect.Uint64Kind)
	shareResponse := (&sarnautv1.QuestShareResponse{}).ProtoReflect().Descriptor()
	assertHUDField(t, shareResponse, "expected_revision", 4, protoreflect.Uint64Kind)
	shareInvite := (&sarnautv1.QuestShareInvite{}).ProtoReflect().Descriptor()
	assertHUDField(t, shareInvite, "remaining_milliseconds", 5, protoreflect.Uint64Kind)
	assertHUDField(t, shareInvite, "on_start", 6, protoreflect.BoolKind)
	shareResult := (&sarnautv1.QuestShareResult{}).ProtoReflect().Descriptor()
	assertHUDField(t, shareResult, "recipient_entity_id", 5, protoreflect.Uint64Kind)
	assertHUDField(t, shareResult, "recipient_name", 6, protoreflect.StringKind)
}

func TestGameplayUIDescriptorIdentity(t *testing.T) {
	file := (&sarnautv1.InventoryMove{}).ProtoReflect().Descriptor().ParentFile()
	if got, want := file.Path(), "sarnaut/v1/hud.proto"; got != want {
		t.Errorf("descriptor path = %q, want %q", got, want)
	}
	options, ok := file.Options().(*descriptorpb.FileOptions)
	if !ok {
		t.Fatal("hud descriptor options are missing")
	}
	if got, want := options.GetGoPackage(), "github.com/SarnautCore/server/gen/sarnaut/v1;sarnautv1"; got != want {
		t.Errorf("go_package = %q, want %q", got, want)
	}
	if got, want := options.GetCsharpNamespace(), "Sarnaut.Protocol.V1"; got != want {
		t.Errorf("csharp_namespace = %q, want %q", got, want)
	}
}

func TestTargetAndActionBarAreAuthoritative(t *testing.T) {
	targetSelect := (&sarnautv1.TargetSelect{}).ProtoReflect().Descriptor()
	assertHUDField(t, targetSelect, "target_entity_id", 1, protoreflect.Uint64Kind)
	assertHUDField(t, targetSelect, "request_id", 2, protoreflect.Uint64Kind)
	target := (&sarnautv1.TargetStateReplacement{}).ProtoReflect().Descriptor()
	assertHUDFields(t, target, map[protoreflect.Name]fieldContract{
		"revision":           {1, protoreflect.Uint64Kind},
		"request_id":         {2, protoreflect.Uint64Kind},
		"has_authority":      {3, protoreflect.BoolKind},
		"selected_entity_id": {4, protoreflect.Uint64Kind},
		"refusal":            {5, protoreflect.EnumKind},
	})
	for _, forbidden := range []protoreflect.Name{"target", "hostility", "health", "alive", "name"} {
		if target.Fields().ByName(forbidden) != nil {
			t.Errorf("TargetStateReplacement duplicates replication field %q", forbidden)
		}
	}

	activate := (&sarnautv1.ActivateAction{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, activate.Fields().ByName("slot_index"), sarnautv1.E_MaxUint, uint64(35))
	assertHUDField(t, activate, "expected_revision", 4, protoreflect.Uint64Kind)
	if activate.Fields().ByName("target_entity_id") != nil {
		t.Error("ActivateAction may not nominate a target outside selected-target authority")
	}

	slot := (&sarnautv1.ActionBarSlotState{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, slot.Fields().ByName("slot_index"), sarnautv1.E_MaxUint, uint64(35))
	assertHUDField(t, slot, "ability_id", 2, protoreflect.StringKind)
	assertHUDField(t, slot, "cooldown_remaining_milliseconds", 3, protoreflect.Int64Kind)
	assertHUDField(t, slot, "cooldown_duration_milliseconds", 4, protoreflect.Int64Kind)
	assertHUDFieldOption(t, slot.Fields().ByName("cooldown_remaining_milliseconds"), sarnautv1.E_MinSint, int64(0))
	assertHUDFieldOption(t, slot.Fields().ByName("cooldown_duration_milliseconds"), sarnautv1.E_MinSint, int64(0))
	assertHUDField(t, slot, "available", 5, protoreflect.BoolKind)
	assertHUDField(t, slot, "unavailable_reason", 6, protoreflect.EnumKind)
	for _, forbidden := range []protoreflect.Name{"action_product_id", "cooldown_remaining", "cooldown_duration"} {
		if slot.Fields().ByName(forbidden) != nil {
			t.Errorf("ActionBarSlotState exposes unproven field %q", forbidden)
		}
	}
	bar := (&sarnautv1.ActionBarReplacement{}).ProtoReflect().Descriptor()
	assertHUDFieldOption(t, bar.Fields().ByName("slots"), sarnautv1.E_ExactCount, uint32(36))
	assertHUDField(t, bar, "activation_refusal", 3, protoreflect.EnumKind)
	refusal := sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_UNSPECIFIED.Descriptor()
	stale := refusal.Values().ByName("ACTION_ACTIVATION_REFUSAL_STALE_REVISION")
	if stale == nil || stale.Number() != 2 {
		t.Errorf("action stale-revision refusal = %v, want 2", stale)
	}
	unavailable := sarnautv1.ActionUnavailableReason_ACTION_UNAVAILABLE_REASON_UNSPECIFIED.Descriptor()
	noResource := unavailable.Values().ByName("ACTION_UNAVAILABLE_REASON_NO_RESOURCE")
	if noResource == nil || noResource.Number() != 10 {
		t.Errorf("action no-resource reason = %v, want 10", noResource)
	}
	ability := sarnautv1.AbilityRejection_ABILITY_REJECTION_UNSPECIFIED.Descriptor()
	abilityNoResource := ability.Values().ByName("ABILITY_REJECTION_NO_RESOURCE")
	if abilityNoResource == nil || abilityNoResource.Number() != 8 {
		t.Errorf("ability no-resource rejection = %v, want 8", abilityNoResource)
	}
}

type fieldContract struct {
	number protoreflect.FieldNumber
	kind   protoreflect.Kind
}

func assertHUDFields(t *testing.T, message protoreflect.MessageDescriptor, fields map[protoreflect.Name]fieldContract) {
	t.Helper()
	for name, contract := range fields {
		assertHUDField(t, message, name, contract.number, contract.kind)
	}
}

func assertHUDField(t *testing.T, message protoreflect.MessageDescriptor, name protoreflect.Name, number protoreflect.FieldNumber, kind protoreflect.Kind) {
	t.Helper()
	field := message.Fields().ByName(name)
	if field == nil {
		t.Fatalf("%s.%s is missing", message.FullName(), name)
	}
	if field.Number() != number || field.Kind() != kind {
		t.Errorf("%s.%s = field %d %s, want field %d %s", message.FullName(), name, field.Number(), field.Kind(), number, kind)
	}
}

func assertHUDFieldOption(t *testing.T, field protoreflect.FieldDescriptor, extension protoreflect.ExtensionType, want any) {
	t.Helper()
	if field == nil {
		t.Fatal("cannot inspect option on missing field")
	}
	options, ok := field.Options().(*descriptorpb.FieldOptions)
	if !ok {
		t.Fatalf("%s has no field options", field.FullName())
	}
	if !proto.HasExtension(options, extension) {
		t.Fatalf("%s lacks option %s", field.FullName(), extension.TypeDescriptor().FullName())
	}
	got := proto.GetExtension(options, extension)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s option %s = %v, want %v", field.FullName(), extension.TypeDescriptor().FullName(), got, want)
	}
}

func assertHUDMessageOption(t *testing.T, message protoreflect.MessageDescriptor, extension protoreflect.ExtensionType, want any) {
	t.Helper()
	options, ok := message.Options().(*descriptorpb.MessageOptions)
	if !ok {
		t.Fatalf("%s has no message options", message.FullName())
	}
	if !proto.HasExtension(options, extension) {
		t.Fatalf("%s lacks option %s", message.FullName(), extension.TypeDescriptor().FullName())
	}
	got := proto.GetExtension(options, extension)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s option %s = %v, want %v", message.FullName(), extension.TypeDescriptor().FullName(), got, want)
	}
}

func assertEnumValues(t *testing.T, descriptor protoreflect.EnumDescriptor, want map[protoreflect.Name]protoreflect.EnumNumber) {
	t.Helper()
	if descriptor.Values().Len() != len(want) {
		t.Fatalf("%s values = %d, want %d", descriptor.FullName(), descriptor.Values().Len(), len(want))
	}
	for name, number := range want {
		value := descriptor.Values().ByName(name)
		if value == nil || value.Number() != number {
			t.Errorf("%s.%s = %v, want %d", descriptor.FullName(), name, value, number)
		}
	}
}

func assertMirroredEnumNumbers(t *testing.T, legacy, hud protoreflect.EnumDescriptor) {
	t.Helper()
	if legacy.Values().Len() != hud.Values().Len() {
		t.Fatalf("%s has %d values, %s has %d", legacy.FullName(), legacy.Values().Len(), hud.FullName(), hud.Values().Len())
	}
	for i := 0; i < legacy.Values().Len(); i++ {
		if got, want := hud.Values().Get(i).Number(), legacy.Values().Get(i).Number(); got != want {
			t.Errorf("%s value %d = %d, want legacy number %d", hud.FullName(), i, got, want)
		}
	}
}

func assertMirroredEnumPrefix(t *testing.T, legacy, hud protoreflect.EnumDescriptor) {
	t.Helper()
	if hud.Values().Len() < legacy.Values().Len() {
		t.Fatalf("%s has %d values, fewer than %s with %d", hud.FullName(), hud.Values().Len(), legacy.FullName(), legacy.Values().Len())
	}
	for i := 0; i < legacy.Values().Len(); i++ {
		if got, want := hud.Values().Get(i).Number(), legacy.Values().Get(i).Number(); got != want {
			t.Errorf("%s value %d = %d, want legacy number %d", hud.FullName(), i, got, want)
		}
	}
}

func readHUDGolden(t *testing.T) map[string]string {
	t.Helper()
	contents, err := os.ReadFile("testdata/gameplay-ui-v1-wire.golden")
	if err != nil {
		t.Fatalf("read wire golden: %v", err)
	}
	entries := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(string(contents)), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name == "" || value == "" {
			t.Fatalf("invalid wire golden line %q", line)
		}
		if _, err := hex.DecodeString(value); err != nil {
			t.Fatalf("invalid hex for %s: %v", name, err)
		}
		entries[name] = value
	}
	return entries
}
