package session

import (
	"context"
	"fmt"
	"math"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/transport"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// chatQueueDepth bounds one session's reliable chat backlog. Overflow closes
// that slow session rather than blocking every sender or silently leaving a
// live client with a hole in its ordered chat log.
const chatQueueDepth = 64

type chatSender struct {
	connection transport.Connection
	writer     *reliableWriter
	span       trace.Span
	queue      chan *sarnautv1.ServerMessage
}

func newChatSender(connection transport.Connection, writer *reliableWriter, span trace.Span) *chatSender {
	return &chatSender{
		connection: connection,
		writer:     writer,
		span:       span,
		queue:      make(chan *sarnautv1.ServerMessage, chatQueueDepth),
	}
}

func (sender *chatSender) OfferChat(delivery chat.Delivery) {
	sender.offer(chatDeliveryMessage(delivery))
}

func (sender *chatSender) offer(message *sarnautv1.ServerMessage) {
	select {
	case sender.queue <- message:
		return
	default:
	}
	if sender.span != nil {
		sender.span.AddEvent("session.chat_queue.overflow", trace.WithAttributes(
			attribute.Int("sarnaut.chat.queue_depth", chatQueueDepth),
		))
	}
	_ = sender.connection.Close()
}

func (sender *chatSender) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case message := <-sender.queue:
			if err := sender.writer.write(message); err != nil {
				return fmt.Errorf("write chat event: %w", err)
			}
		}
	}
}

func chatRequestFromProto(request *sarnautv1.ChatSendRequest) chat.Request {
	mapped := chat.Request{
		RequestID: request.GetRequestId(),
		Channel:   chat.Channel(request.GetChannel()),
		Text:      request.GetText(),
	}
	switch target := request.GetTarget().(type) {
	case *sarnautv1.ChatSendRequest_WhisperCharacterName:
		mapped.Target = chat.Target{Kind: chat.TargetWhisperCharacter, Value: target.WhisperCharacterName}
	case *sarnautv1.ChatSendRequest_NamedChannel:
		mapped.Target = chat.Target{Kind: chat.TargetNamedChannel, Value: target.NamedChannel}
	}
	return mapped
}

func chatDeliveryMessage(delivery chat.Delivery) *sarnautv1.ServerMessage {
	body := new(sarnautv1.ChatBody)
	switch delivery.Body.Kind {
	case chat.BodyUserText:
		body.Value = &sarnautv1.ChatBody_UserText{UserText: delivery.Body.UserText}
	case chat.BodyLocalized:
		body.Value = &sarnautv1.ChatBody_Localized{Localized: &sarnautv1.LocalizedChatBody{
			ProductLocalizationId: delivery.Body.Localized.ProductLocalizationID,
			Arguments:             append([]string(nil), delivery.Body.Localized.Arguments...),
		}}
	case chat.BodyUnreadableFaction:
		body.Value = &sarnautv1.ChatBody_UnreadableFaction{UnreadableFaction: &sarnautv1.UnreadableFactionChatBody{
			FactionNameLocalizationId: delivery.Body.FactionNameLocalizationID,
		}}
	default:
		body.Value = &sarnautv1.ChatBody_Localized{Localized: &sarnautv1.LocalizedChatBody{
			ProductLocalizationId: "chat.error.internal_error",
		}}
	}

	mapped := &sarnautv1.ChatDelivery{
		MessageId:              delivery.MessageID,
		Channel:                sarnautv1.ChatChannel(delivery.Channel),
		SentAtUnixMilliseconds: delivery.SentAt.UnixMilli(),
		SenderEntityId:         delivery.SenderEntityID,
		SenderName:             delivery.SenderName,
		SenderAlive:            delivery.SenderAlive,
		Body:                   body,
	}
	if delivery.WhisperPeerName != "" {
		mapped.Context = &sarnautv1.ChatDelivery_WhisperPeerName{WhisperPeerName: delivery.WhisperPeerName}
	} else if delivery.NamedChannel != "" {
		mapped.Context = &sarnautv1.ChatDelivery_NamedChannel{NamedChannel: delivery.NamedChannel}
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_ChatDelivery{ChatDelivery: mapped},
	}
}

func chatRejectionMessage(request chat.Request, result chat.Result) *sarnautv1.ServerMessage {
	arguments := []string(nil)
	if result.Rejection == chat.RejectionTargetNotFound || result.Rejection == chat.RejectionTargetOffline {
		arguments = []string{request.Target.Value}
	}
	rejection := &sarnautv1.ChatRejection{
		RequestId: request.RequestID,
		Channel:   sarnautv1.ChatChannel(request.Channel),
		Reason:    chatRejectionReason(result.Rejection),
		Detail: &sarnautv1.LocalizedChatBody{
			ProductLocalizationId: chatRejectionProductID(result.Rejection),
			Arguments:             arguments,
		},
		RetryAfterMilliseconds: retryAfterMilliseconds(result.RetryAfter),
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_ChatRejection{ChatRejection: rejection},
	}
}

func chatRejectionReason(rejection chat.Rejection) sarnautv1.ChatRejectionReason {
	if rejection < chat.RejectionMute || rejection > chat.RejectionEmpty {
		return sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_INTERNAL_ERROR
	}
	return sarnautv1.ChatRejectionReason(rejection)
}

func chatRejectionProductID(rejection chat.Rejection) string {
	ids := [...]string{
		"chat.error.mute",
		"chat.error.internal_error",
		"chat.error.silence",
		"chat.error.no_points",
		"chat.error.enemy_faction",
		"chat.error.ignored",
		"chat.error.dead",
		"chat.error.not_psionic",
		"chat.error.target_not_found",
		"chat.error.target_offline",
		"chat.error.rate_limited",
		"chat.error.too_long",
		"chat.error.not_member",
		"chat.error.not_authorized",
		"chat.error.unsupported_channel",
		"chat.error.empty",
	}
	if rejection < chat.RejectionMute || rejection > chat.RejectionEmpty {
		return "chat.error.internal_error"
	}
	return ids[int(rejection)]
}

func retryAfterMilliseconds(duration time.Duration) uint32 {
	if duration <= 0 {
		return 0
	}
	milliseconds := (duration + time.Millisecond - 1) / time.Millisecond
	if milliseconds > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(milliseconds)
}
