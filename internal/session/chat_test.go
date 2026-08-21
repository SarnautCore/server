package session

import (
	"testing"
	"time"

	"github.com/google/uuid"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/chat"
)

func TestChatWireMappingPreservesAuthoredAndServerFields(t *testing.T) {
	t.Parallel()

	request := chatRequestFromProto(&sarnautv1.ChatSendRequest{
		RequestId: 71,
		Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER,
		Text:      "raw  e\u0301  U0001f680",
		Target: &sarnautv1.ChatSendRequest_WhisperCharacterName{
			WhisperCharacterName: "Borin",
		},
	})
	if request.RequestID != 71 || request.Channel != chat.ChannelWhisper ||
		request.Text != "raw  e\u0301  U0001f680" || request.Target.Kind != chat.TargetWhisperCharacter ||
		request.Target.Value != "Borin" {
		t.Fatalf("chatRequestFromProto() = %+v", request)
	}

	sentAt := time.Date(2026, time.August, 21, 20, 45, 1, 234_000_000, time.UTC)
	delivery := chatDeliveryMessage(chat.Delivery{
		MessageID:         9001,
		Channel:           chat.ChannelWhisper,
		SentAt:            sentAt,
		SenderCharacterID: uuid.MustParse("019200f0-0000-7000-8000-00000000c071"),
		SenderEntityID:    707,
		SenderName:        "Alice",
		SenderAlive:       true,
		Body:              chat.Body{Kind: chat.BodyUserText, UserText: request.Text},
		WhisperPeerName:   "Borin",
	}).GetChatDelivery()
	if delivery.GetMessageId() != 9001 || delivery.GetRequestId() != 0 ||
		delivery.GetChannel() != sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER ||
		delivery.GetSentAtUnixMilliseconds() != sentAt.UnixMilli() || delivery.GetSenderEntityId() != 707 ||
		delivery.GetSenderName() != "Alice" || !delivery.GetSenderAlive() ||
		delivery.GetBody().GetUserText() != request.Text || delivery.GetWhisperPeerName() != "Borin" {
		t.Fatalf("chatDeliveryMessage() = %+v", delivery)
	}
}

func TestChatBodyMapsOnlyProductLocalizationIdentifiers(t *testing.T) {
	t.Parallel()

	localized := chatDeliveryMessage(chat.Delivery{Body: chat.Body{
		Kind: chat.BodyLocalized,
		Localized: chat.LocalizedBody{
			ProductLocalizationID: "chat.system.zone_restart",
			Arguments:             []string{"15"},
		},
	}}).GetChatDelivery().GetBody().GetLocalized()
	if localized.GetProductLocalizationId() != "chat.system.zone_restart" || len(localized.GetArguments()) != 1 || localized.GetArguments()[0] != "15" {
		t.Fatalf("localized body = %+v", localized)
	}

	unreadable := chatDeliveryMessage(chat.Delivery{Body: chat.Body{
		Kind:                      chat.BodyUnreadableFaction,
		FactionNameLocalizationID: "faction.league.name",
	}}).GetChatDelivery().GetBody().GetUnreadableFaction()
	if unreadable.GetFactionNameLocalizationId() != "faction.league.name" {
		t.Fatalf("unreadable faction body = %+v", unreadable)
	}
}

func TestEveryChatRefusalMapsToTypedWireReasonAndProductText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rejection chat.Rejection
		wire      sarnautv1.ChatRejectionReason
		productID string
	}{
		{chat.RejectionMute, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_MUTE, "chat.error.mute"},
		{chat.RejectionInternalError, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_INTERNAL_ERROR, "chat.error.internal_error"},
		{chat.RejectionSilence, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_SILENCE, "chat.error.silence"},
		{chat.RejectionNoPoints, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_NO_POINTS, "chat.error.no_points"},
		{chat.RejectionEnemyFaction, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_ENEMY_FACTION, "chat.error.enemy_faction"},
		{chat.RejectionIgnored, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_IGNORED, "chat.error.ignored"},
		{chat.RejectionDead, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_DEAD, "chat.error.dead"},
		{chat.RejectionNotPsionic, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_NOT_PSIONIC, "chat.error.not_psionic"},
		{chat.RejectionTargetNotFound, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_TARGET_NOT_FOUND, "chat.error.target_not_found"},
		{chat.RejectionTargetOffline, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_TARGET_OFFLINE, "chat.error.target_offline"},
		{chat.RejectionRateLimited, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_RATE_LIMITED, "chat.error.rate_limited"},
		{chat.RejectionTooLong, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_TOO_LONG, "chat.error.too_long"},
		{chat.RejectionNotMember, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_NOT_MEMBER, "chat.error.not_member"},
		{chat.RejectionNotAuthorized, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_NOT_AUTHORIZED, "chat.error.not_authorized"},
		{chat.RejectionUnsupportedChannel, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_UNSUPPORTED_CHANNEL, "chat.error.unsupported_channel"},
		{chat.RejectionEmpty, sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_EMPTY, "chat.error.empty"},
	}
	for _, test := range tests {
		t.Run(test.productID, func(t *testing.T) {
			message := chatRejectionMessage(
				chat.Request{RequestID: 72, Channel: chat.ChannelWhisper, Target: chat.Target{Kind: chat.TargetWhisperCharacter, Value: "Borin"}},
				chat.Result{Rejection: test.rejection, RetryAfter: 1_500*time.Millisecond + time.Nanosecond},
			).GetChatRejection()
			if message.GetRequestId() != 72 || message.GetChannel() != sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER ||
				message.GetReason() != test.wire || message.GetDetail().GetProductLocalizationId() != test.productID {
				t.Fatalf("chatRejectionMessage() = %+v", message)
			}
			if message.GetRetryAfterMilliseconds() != 1501 {
				t.Errorf("retry_after_ms = %d, want ceiling 1501", message.GetRetryAfterMilliseconds())
			}
			if (test.rejection == chat.RejectionTargetNotFound || test.rejection == chat.RejectionTargetOffline) &&
				(len(message.GetDetail().GetArguments()) != 1 || message.GetDetail().GetArguments()[0] != "Borin") {
				t.Errorf("target refusal arguments = %v, want Borin", message.GetDetail().GetArguments())
			}
		})
	}
}
