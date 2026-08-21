package proto_test

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"unicode/utf16"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestChatV1WireGolden(t *testing.T) {
	golden := readChatGolden(t)
	utf16Text := "a😀b"
	if got, want := len(utf16.Encode([]rune(utf16Text))), 4; got != want {
		t.Fatalf("supplementary-plane golden text has %d UTF-16 code units, want %d", got, want)
	}

	tests := map[string]proto.Message{
		"legacy_client_move": &sarnautv1.ClientMessage{
			ClientSeq: 3,
			Payload: &sarnautv1.ClientMessage_MoveIntent{MoveIntent: &sarnautv1.ClientMoveIntent{
				Seq:   4,
				Input: &sarnautv1.Vec3{X: 1},
			}},
		},
		"legacy_server_error": &sarnautv1.ServerMessage{
			ServerTick: 5,
			Payload: &sarnautv1.ServerMessage_Error{Error: &sarnautv1.Error{
				Code:   sarnautv1.ErrorCode_ERROR_CODE_NOT_IN_ZONE,
				Detail: "zone",
			}},
		},
		"client_send": &sarnautv1.ClientMessage{
			ClientSeq: 7,
			Payload: &sarnautv1.ClientMessage_ChatSendRequest{ChatSendRequest: &sarnautv1.ChatSendRequest{
				RequestId: 42,
				Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER,
				Text:      "hello",
				Target: &sarnautv1.ChatSendRequest_WhisperCharacterName{
					WhisperCharacterName: "Ayla",
				},
			}},
		},
		"client_send_utf16": &sarnautv1.ClientMessage{
			ClientSeq: 8,
			Payload: &sarnautv1.ClientMessage_ChatSendRequest{ChatSendRequest: &sarnautv1.ChatSendRequest{
				RequestId: 43,
				Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_SAY,
				Text:      utf16Text,
			}},
		},
		"server_remote_delivery": &sarnautv1.ServerMessage{
			ServerTick: 900,
			Payload: &sarnautv1.ServerMessage_ChatDelivery{ChatDelivery: &sarnautv1.ChatDelivery{
				MessageId:              1001,
				Channel:                sarnautv1.ChatChannel_CHAT_CHANNEL_SAY,
				SentAtUnixMilliseconds: 1_700_000_000_123,
				SenderEntityId:         77,
				SenderName:             "Ayla",
				SenderAlive:            true,
				Body: &sarnautv1.ChatBody{Value: &sarnautv1.ChatBody_UserText{
					UserText: "hello",
				}},
			}},
		},
		"server_localized_delivery": &sarnautv1.ServerMessage{
			ServerTick: 902,
			Payload: &sarnautv1.ServerMessage_ChatDelivery{ChatDelivery: &sarnautv1.ChatDelivery{
				MessageId:              1002,
				Channel:                sarnautv1.ChatChannel_CHAT_CHANNEL_SAY,
				SentAtUnixMilliseconds: 1_700_000_000_124,
				SenderEntityId:         88,
				SenderName:             "Quartermaster",
				SenderAlive:            true,
				Body: &sarnautv1.ChatBody{Value: &sarnautv1.ChatBody_Localized{
					Localized: &sarnautv1.LocalizedChatBody{
						ProductLocalizationId: "chat.system.inventory_full",
						Arguments:             []string{"16"},
					},
				}},
			}},
		},
		"server_unreadable_delivery": &sarnautv1.ServerMessage{
			ServerTick: 903,
			Payload: &sarnautv1.ServerMessage_ChatDelivery{ChatDelivery: &sarnautv1.ChatDelivery{
				MessageId:              1003,
				Channel:                sarnautv1.ChatChannel_CHAT_CHANNEL_SAY,
				SentAtUnixMilliseconds: 1_700_000_000_125,
				SenderEntityId:         99,
				SenderName:             "Unknown",
				SenderAlive:            true,
				Body: &sarnautv1.ChatBody{Value: &sarnautv1.ChatBody_UnreadableFaction{
					UnreadableFaction: &sarnautv1.UnreadableFactionChatBody{
						FactionNameLocalizationId: "faction.league.name",
					},
				}},
			}},
		},
		"server_rejection": &sarnautv1.ServerMessage{
			ServerTick: 901,
			Payload: &sarnautv1.ServerMessage_ChatRejection{ChatRejection: &sarnautv1.ChatRejection{
				RequestId: 42,
				Channel:   sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER,
				Reason:    sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_TARGET_OFFLINE,
				Detail: &sarnautv1.LocalizedChatBody{
					ProductLocalizationId: "chat.error.target_offline",
					Arguments:             []string{"Borin"},
				},
			}},
		},
	}

	for name, message := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if want := golden[name]; hex.EncodeToString(got) != want {
				t.Fatalf("wire = %s, want %s", hex.EncodeToString(got), want)
			}

			decoded := message.ProtoReflect().New().Interface()
			if err := proto.Unmarshal(got, decoded); err != nil {
				t.Fatalf("unmarshal generated message: %v", err)
			}
			if !proto.Equal(message, decoded) {
				t.Fatalf("round trip changed message\ngot:  %v\nwant: %v", decoded, message)
			}
		})
	}
	if len(golden) != len(tests) {
		t.Fatalf("golden has %d entries, want exactly %d", len(golden), len(tests))
	}
}

func TestChatRequestCarriesNoServerAuthority(t *testing.T) {
	request := (&sarnautv1.ChatSendRequest{}).ProtoReflect().Descriptor()
	if request.Fields().Len() != 5 {
		t.Fatalf("ChatSendRequest has %d fields, want exactly 5", request.Fields().Len())
	}
	for _, forbidden := range []protoreflect.Name{
		"sender", "sender_id", "sender_entity_id", "sender_name", "sent_at_unix_milliseconds",
		"sender_alive", "is_echo", "body", "product_localization_id",
	} {
		if request.Fields().ByName(forbidden) != nil {
			t.Errorf("ChatSendRequest exposes server-authored field %q", forbidden)
		}
	}

	target := request.Oneofs().ByName("target")
	if target == nil || target.Fields().Len() != 2 {
		t.Fatalf("target oneof has %d fields, want 2", oneofFieldCount(target))
	}
	assertField(t, request, "request_id", 1, protoreflect.Uint64Kind)
	assertField(t, request, "channel", 2, protoreflect.EnumKind)
	assertField(t, request, "text", 3, protoreflect.StringKind)
	assertField(t, request, "whisper_character_name", 4, protoreflect.StringKind)
	assertField(t, request, "named_channel", 5, protoreflect.StringKind)
}

func TestChatDeliveryCarriesServerAuthority(t *testing.T) {
	delivery := (&sarnautv1.ChatDelivery{}).ProtoReflect().Descriptor()
	if delivery.Fields().Len() != 10 {
		t.Fatalf("ChatDelivery has %d fields, want exactly 10", delivery.Fields().Len())
	}
	assertField(t, delivery, "message_id", 1, protoreflect.Uint64Kind)
	assertField(t, delivery, "request_id", 2, protoreflect.Uint64Kind)
	assertField(t, delivery, "channel", 3, protoreflect.EnumKind)
	assertField(t, delivery, "sent_at_unix_milliseconds", 4, protoreflect.Int64Kind)
	assertField(t, delivery, "sender_entity_id", 5, protoreflect.Uint64Kind)
	assertField(t, delivery, "sender_name", 6, protoreflect.StringKind)
	assertField(t, delivery, "sender_alive", 7, protoreflect.BoolKind)
	assertField(t, delivery, "body", 8, protoreflect.MessageKind)
	assertField(t, delivery, "whisper_peer_name", 10, protoreflect.StringKind)
	assertField(t, delivery, "named_channel", 11, protoreflect.StringKind)
	if !delivery.ReservedRanges().Has(9) {
		t.Error("retired ChatDelivery field 9 is not reserved")
	}
	if !delivery.ReservedNames().Has("is_echo") {
		t.Error("retired ChatDelivery field name is_echo is not reserved")
	}
	context := delivery.Oneofs().ByName("context")
	if context == nil || context.Fields().Len() != 2 {
		t.Fatalf("ChatDelivery.context has %d fields, want 2", oneofFieldCount(context))
	}
}

func TestChatChannelsPreserveRetailNumbers(t *testing.T) {
	channel := sarnautv1.ChatChannel_CHAT_CHANNEL_WHISPER.Descriptor()
	wantValues := map[protoreflect.Name]protoreflect.EnumNumber{
		"CHAT_CHANNEL_WHISPER":       0,
		"CHAT_CHANNEL_GROUP":         1,
		"CHAT_CHANNEL_SAY":           2,
		"CHAT_CHANNEL_ZONE":          4,
		"CHAT_CHANNEL_ZONE_SPECIAL":  5,
		"CHAT_CHANNEL_WORLD":         6,
		"CHAT_CHANNEL_GUILD":         9,
		"CHAT_CHANNEL_GUILD_OFFICER": 10,
		"CHAT_CHANNEL_RAID":          11,
	}
	if channel.Values().Len() != len(wantValues) {
		t.Fatalf("generated channel count = %d, want %d", channel.Values().Len(), len(wantValues))
	}
	for name, number := range wantValues {
		value := channel.Values().ByName(name)
		if value == nil || value.Number() != number {
			t.Errorf("channel %s = %v, want %d", name, value, number)
		}
	}

	for number, name := range map[protoreflect.EnumNumber]protoreflect.Name{
		3:  "CHAT_CHANNEL_YELL",
		7:  "CHAT_CHANNEL_EMOTE",
		8:  "CHAT_CHANNEL_NAMED",
		12: "CHAT_CHANNEL_PSIONIC",
	} {
		if !channel.ReservedRanges().Has(number) {
			t.Errorf("channel number %d is not reserved", number)
		}
		if !channel.ReservedNames().Has(name) {
			t.Errorf("channel name %q is not reserved", name)
		}
		if channel.Values().ByNumber(number) != nil {
			t.Errorf("reserved channel number %d generated a usable enum value", number)
		}
	}
}

func TestChatRejectionsPreserveRetailNumbers(t *testing.T) {
	reason := sarnautv1.ChatRejectionReason_CHAT_REJECTION_REASON_MUTE.Descriptor()
	want := []string{
		"CHAT_REJECTION_REASON_MUTE",
		"CHAT_REJECTION_REASON_INTERNAL_ERROR",
		"CHAT_REJECTION_REASON_SILENCE",
		"CHAT_REJECTION_REASON_NO_POINTS",
		"CHAT_REJECTION_REASON_ENEMY_FACTION",
		"CHAT_REJECTION_REASON_IGNORED",
		"CHAT_REJECTION_REASON_DEAD",
		"CHAT_REJECTION_REASON_NOT_PSIONIC",
		"CHAT_REJECTION_REASON_TARGET_NOT_FOUND",
		"CHAT_REJECTION_REASON_TARGET_OFFLINE",
		"CHAT_REJECTION_REASON_RATE_LIMITED",
		"CHAT_REJECTION_REASON_TOO_LONG",
		"CHAT_REJECTION_REASON_NOT_MEMBER",
		"CHAT_REJECTION_REASON_NOT_AUTHORIZED",
		"CHAT_REJECTION_REASON_UNSUPPORTED_CHANNEL",
		"CHAT_REJECTION_REASON_EMPTY",
	}
	if reason.Values().Len() != len(want) {
		t.Fatalf("generated rejection count = %d, want %d", reason.Values().Len(), len(want))
	}
	for number, name := range want {
		value := reason.Values().ByNumber(protoreflect.EnumNumber(number))
		if value == nil || string(value.Name()) != name {
			t.Errorf("rejection %d = %v, want %s", number, value, name)
		}
	}
}

func TestLocalizedChatBodyUsesProductCatalogID(t *testing.T) {
	localizedMessages := []protoreflect.MessageDescriptor{
		(&sarnautv1.LocalizedChatBody{}).ProtoReflect().Descriptor(),
		(&sarnautv1.UnreadableFactionChatBody{}).ProtoReflect().Descriptor(),
	}
	assertField(t, localizedMessages[0], "product_localization_id", 1, protoreflect.StringKind)
	assertField(t, localizedMessages[0], "arguments", 2, protoreflect.StringKind)
	assertField(t, localizedMessages[1], "faction_name_localization_id", 1, protoreflect.StringKind)
	for _, message := range localizedMessages {
		for i := 0; i < message.Fields().Len(); i++ {
			name := strings.ToLower(string(message.Fields().Get(i).Name()))
			if strings.Contains(name, "xdb") || strings.Contains(name, "path") || strings.Contains(name, "resource") {
				t.Errorf("%s exposes source identifier field %q", message.FullName(), name)
			}
		}
	}
}

func TestChatBodyHasOnlyServerSelectedRepresentations(t *testing.T) {
	body := (&sarnautv1.ChatBody{}).ProtoReflect().Descriptor()
	value := body.Oneofs().ByName("value")
	if value == nil {
		t.Fatal("ChatBody.value oneof is missing")
	}
	want := map[protoreflect.Name]protoreflect.FieldNumber{
		"user_text":          1,
		"localized":          2,
		"unreadable_faction": 3,
	}
	if value.Fields().Len() != len(want) {
		t.Fatalf("ChatBody.value has %d fields, want %d", value.Fields().Len(), len(want))
	}
	for name, number := range want {
		field := value.Fields().ByName(name)
		if field == nil || field.Number() != number {
			t.Errorf("ChatBody.%s = %v, want field %d", name, field, number)
		}
	}
}

func TestChatRequestRejectsServerFieldsAndReservedNamesInJSON(t *testing.T) {
	tests := map[string]string{
		"sender":          `{"requestId":"1","channel":"CHAT_CHANNEL_SAY","text":"hello","senderName":"Mallory"}`,
		"delivery_body":   `{"requestId":"1","channel":"CHAT_CHANNEL_SAY","text":"hello","body":{"userText":"forged"}}`,
		"named_channel":   `{"requestId":"1","channel":"CHAT_CHANNEL_NAMED","text":"hello","namedChannel":"trade"}`,
		"psionic_channel": `{"requestId":"1","channel":"CHAT_CHANNEL_PSIONIC","text":"hello"}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if err := protojson.Unmarshal([]byte(input), &sarnautv1.ChatSendRequest{}); err == nil {
				t.Fatalf("accepted unsupported JSON input %s", input)
			}
		})
	}
}

func TestChatRequestRejectsInvalidUnicode(t *testing.T) {
	// ED A0 80 is the UTF-8 encoding a permissive codec might assign to the
	// unpaired UTF-16 high surrogate D800. Protobuf strings require valid UTF-8.
	invalid := string([]byte{0xed, 0xa0, 0x80})
	_, err := proto.Marshal(&sarnautv1.ChatSendRequest{Text: invalid})
	if err == nil {
		t.Fatal("generated protobuf accepted an unpaired-surrogate encoding")
	}
}

func TestChatGeneratedDescriptorIdentity(t *testing.T) {
	file := (&sarnautv1.ChatSendRequest{}).ProtoReflect().Descriptor().ParentFile()
	if got, want := file.Path(), "sarnaut/v1/chat.proto"; got != want {
		t.Errorf("descriptor path = %q, want %q", got, want)
	}
	if got, want := string(file.Package()), "sarnaut.v1"; got != want {
		t.Errorf("descriptor package = %q, want %q", got, want)
	}
	options, ok := file.Options().(*descriptorpb.FileOptions)
	if !ok {
		t.Fatal("chat descriptor options are missing")
	}
	if got, want := options.GetGoPackage(), "github.com/SarnautCore/server/gen/sarnaut/v1;sarnautv1"; got != want {
		t.Errorf("go_package = %q, want %q", got, want)
	}
	if got, want := options.GetCsharpNamespace(), "Sarnaut.Protocol.V1"; got != want {
		t.Errorf("csharp_namespace = %q, want %q", got, want)
	}
}

func TestChatEnvelopeCasesAreAdditive(t *testing.T) {
	client := (&sarnautv1.ClientMessage{}).ProtoReflect().Descriptor()
	assertField(t, client, "chat_send_request", 18, protoreflect.MessageKind)

	server := (&sarnautv1.ServerMessage{}).ProtoReflect().Descriptor()
	assertField(t, server, "chat_delivery", 20, protoreflect.MessageKind)
	assertField(t, server, "chat_rejection", 21, protoreflect.MessageKind)
}

func readChatGolden(t *testing.T) map[string]string {
	t.Helper()
	contents, err := os.ReadFile("testdata/chat-v1-wire.golden")
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

func assertField(t *testing.T, message protoreflect.MessageDescriptor, name protoreflect.Name, number protoreflect.FieldNumber, kind protoreflect.Kind) {
	t.Helper()
	field := message.Fields().ByName(name)
	if field == nil {
		t.Fatalf("%s.%s is missing", message.FullName(), name)
	}
	if field.Number() != number || field.Kind() != kind {
		t.Errorf("%s.%s = field %d %s, want field %d %s", message.FullName(), name, field.Number(), field.Kind(), number, kind)
	}
}

func oneofFieldCount(oneof protoreflect.OneofDescriptor) int {
	if oneof == nil {
		return 0
	}
	return oneof.Fields().Len()
}
