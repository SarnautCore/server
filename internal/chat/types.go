// Package chat routes authenticated player chat within one shard process.
package chat

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MaxTextUTF16Units matches the retail Windows edit control. Counting UTF-16
// units means a supplementary-plane scalar counts as two, while combining
// marks remain separate units. Accepted text is never rewritten.
const MaxTextUTF16Units = 300

// MinimumAcceptedSendInterval is the retail one-send-per-1000ms rule. Only an
// accepted send advances it.
const MinimumAcceptedSendInterval = time.Second

// Channel preserves the retail chat-type numbers carried by protocol v1.
type Channel int32

const (
	ChannelWhisper      Channel = 0
	ChannelGroup        Channel = 1
	ChannelSay          Channel = 2
	ChannelZone         Channel = 4
	ChannelZoneSpecial  Channel = 5
	ChannelWorld        Channel = 6
	ChannelGuild        Channel = 9
	ChannelGuildOfficer Channel = 10
	ChannelRaid         Channel = 11
)

// Rejection preserves the retail refusal numbers followed by the project v1
// extensions. The zero value is a real Mute rejection; [Result.Accepted]
// distinguishes success from refusal.
type Rejection int32

const (
	RejectionMute               Rejection = 0
	RejectionInternalError      Rejection = 1
	RejectionSilence            Rejection = 2
	RejectionNoPoints           Rejection = 3
	RejectionEnemyFaction       Rejection = 4
	RejectionIgnored            Rejection = 5
	RejectionDead               Rejection = 6
	RejectionNotPsionic         Rejection = 7
	RejectionTargetNotFound     Rejection = 8
	RejectionTargetOffline      Rejection = 9
	RejectionRateLimited        Rejection = 10
	RejectionTooLong            Rejection = 11
	RejectionNotMember          Rejection = 12
	RejectionNotAuthorized      Rejection = 13
	RejectionUnsupportedChannel Rejection = 14
	RejectionEmpty              Rejection = 15
)

// TargetKind names the channel-specific target shape.
type TargetKind uint8

const (
	TargetNone TargetKind = iota
	TargetWhisperCharacter
	TargetNamedChannel
)

// Target is the client-authored channel target. The sender is deliberately
// absent from [Request] and comes only from [Presence].
type Target struct {
	Kind  TargetKind
	Value string
}

// Request is the complete domain form of one client chat request.
type Request struct {
	RequestID uint64
	Channel   Channel
	Text      string
	Target    Target
}

// Result reports whether a send or presence update committed. RetryAfter is
// set only for a rate-limit refusal.
type Result struct {
	Accepted   bool
	Rejection  Rejection
	RetryAfter time.Duration
}

// Position is the current authored-world position used by configured local
// chat. It stays independent of the world package so chat cannot mutate a zone.
type Position struct {
	X float32
	Y float32
	Z float32
}

// Observation is the current mutable entity state read at send time.
type Observation struct {
	Position Position
	Alive    bool
}

// Presence is server-authored routing state for one authenticated character.
// CharacterID and Name are immutable for the life of a Session. The remaining
// fields may change through [Session.UpdatePresence].
type Presence struct {
	CharacterID uuid.UUID
	EntityID    uint64
	Name        string
	ZoneID      string
	Observe     func() (Observation, bool)
}

// Character is the directory's canonical identity for one target name.
type Character struct {
	CharacterID uuid.UUID
	Name        string
}

// Directory resolves a player-authored whisper name against the authoritative
// character directory. A found character can still be offline.
type Directory interface {
	ResolveCharacterName(context.Context, string) (Character, bool, error)
}

// PaidChannels atomically authorizes and debits a channel whose authored cost
// is not part of protocol v1. With no adapter the module refuses the channel;
// it never guesses an amount.
type AlternativeCurrency struct {
	ResourceID uint32
	SysName    string
}

const (
	WorldChatCurrencyResourceID   uint32 = 455213071
	WorldChatCurrencySysName             = "world_chat"
	ZoneSpecialCurrencyResourceID uint32 = 455213063
	ZoneSpecialCurrencySysName           = "zone_chat_special"
)

// CurrencySpender atomically debits an authored alternative currency. Chat
// owns the resource and amount selection; the adapter owns durable atomicity.
type CurrencySpender interface {
	Spend(context.Context, uuid.UUID, AlternativeCurrency, uint64) (bool, error)
}

// AudienceRefusal is the only membership decision an audience authority may
// return. It is mapped to the corresponding typed chat rejection.
type AudienceRefusal uint8

const (
	AudienceAllowed AudienceRefusal = iota
	AudienceNotMember
	AudienceNotAuthorized
)

// Audience is an immutable connected-recipient snapshot. Authorities return
// other members only because the retail client projects its own local echo.
type Audience struct {
	RecipientCharacterIDs []uuid.UUID
	Refusal               AudienceRefusal
}

// GroupAudience resolves the authoritative connected party cohort across
// zones. Chat never infers party membership from mutable presence fields.
type GroupAudience interface {
	Audience(context.Context, uuid.UUID) (Audience, error)
}

// RaidAudience resolves the authoritative connected raid cohort across zones.
type RaidAudience interface {
	Audience(context.Context, uuid.UUID) (Audience, error)
}

// GuildAudience resolves the authoritative connected guild cohort and rights.
// officersOnly requires both sender and recipients to have officer-chat rights.
type GuildAudience interface {
	Audience(context.Context, uuid.UUID, bool) (Audience, error)
}

// SaySpeaker is the authenticated input to the world-owned local-chat query.
type SaySpeaker struct {
	CharacterID uuid.UUID
	EntityID    uint64
	ZoneID      string
	Position    Position
}

// SayRecipient is one world-authorized Say recipient. UnreadableFaction is
// set for a nonfriend recipient and carries the baked faction localization id.
type SayRecipient struct {
	CharacterID                     uuid.UUID
	UnreadableFactionLocalizationID string
}

// SayAudience implements the recovered retail topology: avatar recipients in
// the same scanner/mission, inclusive 3D distance squared <= 10,000,
// canInfluence, reverse-ignore filtering, and friend-faction readability.
type SayAudience interface {
	Audience(context.Context, SaySpeaker) ([]SayRecipient, error)
}

// BodyKind distinguishes user text from server-authored product content.
type BodyKind uint8

const (
	BodyUserText BodyKind = iota
	BodyLocalized
	BodyUnreadableFaction
)

// LocalizedBody refers only to a baked product localization id.
type LocalizedBody struct {
	ProductLocalizationID string
	Arguments             []string
}

// Body is one delivery body. A client request can create only BodyUserText.
type Body struct {
	Kind                      BodyKind
	UserText                  string
	Localized                 LocalizedBody
	FactionNameLocalizationID string
}

// Delivery is one recipient's server-authored view of an accepted message.
type Delivery struct {
	MessageID uint64
	Channel   Channel
	SentAt    time.Time
	// SpamWeight is the authoritative source weight consumed by client bubble
	// anti-spam. Zero means the authority assigned zero; clients never infer it
	// from text or channel.
	SpamWeight        uint32
	SenderCharacterID uuid.UUID
	SenderEntityID    uint64
	SenderName        string
	SenderAlive       bool
	Body              Body
	WhisperPeerName   string
	NamedChannel      string
}

// Sink accepts deliveries after the module has released its routing lock.
type Sink interface {
	OfferChat(Delivery)
}
