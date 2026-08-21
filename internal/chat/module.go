package chat

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalidPresence = errors.New("chat: invalid authenticated presence")
	ErrAlreadyPresent  = errors.New("chat: character already present")
)

// Options supplies internal adapters. Clock is an internal test seam; nil uses
// time.Now. Directory is required for exact whisper missing/offline semantics.
type Options struct {
	Clock           func() time.Time
	Directory       Directory
	CurrencySpender CurrencySpender
	GroupAudience   GroupAudience
	RaidAudience    RaidAudience
	GuildAudience   GuildAudience
	SayAudience     SayAudience
}

// Module owns shard-wide presence, routing, authorization and message ids.
type Module struct {
	mu        sync.Mutex
	now       func() time.Time
	directory Directory
	currency  CurrencySpender
	group     GroupAudience
	raid      RaidAudience
	guild     GuildAudience
	say       SayAudience
	nextID    uint64
	live      map[uuid.UUID]*entry
}

type entry struct {
	presence Presence
	sink     Sink
	session  *Session
}

// Session binds all later operations to one authenticated identity.
type Session struct {
	module         *Module
	entry          *entry
	opMu           sync.Mutex
	closed         bool
	hasAccepted    bool
	lastAcceptedAt time.Time
}

// New constructs an empty shard-wide chat module.
func New(options Options) *Module {
	now := options.Clock
	if now == nil {
		now = time.Now
	}
	return &Module{
		now:       now,
		directory: options.Directory,
		currency:  options.CurrencySpender,
		group:     options.GroupAudience,
		raid:      options.RaidAudience,
		guild:     options.GuildAudience,
		say:       options.SayAudience,
		live:      make(map[uuid.UUID]*entry),
	}
}

// Join binds a delivery sink to one authenticated presence.
func (module *Module) Join(presence Presence, sink Sink) (*Session, error) {
	if module == nil || sink == nil || !validPresence(presence) {
		return nil, ErrInvalidPresence
	}
	module.mu.Lock()
	defer module.mu.Unlock()
	if _, exists := module.live[presence.CharacterID]; exists {
		return nil, ErrAlreadyPresent
	}
	current := &entry{presence: presence, sink: sink}
	session := &Session{module: module, entry: current}
	current.session = session
	module.live[presence.CharacterID] = current
	return session, nil
}

// UpdatePresence replaces mutable routing state while keeping the authenticated
// character identity fixed. It is totally ordered with Send and Close on this
// Session; a Send observes either the complete old value or the complete new
// one, never a mix.
func (session *Session) UpdatePresence(presence Presence) Result {
	if session == nil || session.module == nil {
		return Result{Rejection: RejectionInternalError}
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.closed || !validPresence(presence) {
		return Result{Rejection: RejectionInternalError}
	}
	if presence.CharacterID != session.entry.presence.CharacterID || presence.Name != session.entry.presence.Name {
		return Result{Rejection: RejectionNotAuthorized}
	}
	session.module.mu.Lock()
	defer session.module.mu.Unlock()
	if session.module.live[presence.CharacterID] != session.entry {
		return Result{Rejection: RejectionInternalError}
	}
	session.entry.presence = presence
	return Result{Accepted: true}
}

// Close removes this presence. It is idempotent and ordered against Send and
// UpdatePresence by the session operation mutex.
func (session *Session) Close() {
	if session == nil || session.module == nil {
		return
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.closed {
		return
	}
	session.closed = true
	session.module.mu.Lock()
	if session.module.live[session.entry.presence.CharacterID] == session.entry {
		delete(session.module.live, session.entry.presence.CharacterID)
	}
	session.module.mu.Unlock()
}

// Send validates and routes one request. Operations on one Session are totally
// ordered. Sinks are invoked only after the module routing lock is released.
func (session *Session) Send(ctx context.Context, request Request) Result {
	if session == nil || session.module == nil {
		return Result{Rejection: RejectionInternalError}
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.closed {
		return Result{Rejection: RejectionInternalError}
	}
	if !utf8.ValidString(request.Text) {
		// Protobuf rejects this before routing. Keep the domain interface
		// fail-closed as well for adapters that do not use protobuf.
		return Result{Rejection: RejectionInternalError}
	}
	if request.Text == "" {
		return Result{Rejection: RejectionEmpty}
	}
	if utf16Units(request.Text) > MaxTextUTF16Units {
		return Result{Rejection: RejectionTooLong}
	}
	now := session.module.now()
	if session.hasAccepted {
		elapsed := now.Sub(session.lastAcceptedAt)
		if elapsed < MinimumAcceptedSendInterval {
			retryAfter := MinimumAcceptedSendInterval - elapsed
			if elapsed < 0 {
				retryAfter = MinimumAcceptedSendInterval
			}
			return Result{Rejection: RejectionRateLimited, RetryAfter: retryAfter}
		}
	}
	module := session.module
	module.mu.Lock()
	if module.live[session.entry.presence.CharacterID] != session.entry {
		module.mu.Unlock()
		return Result{Rejection: RejectionInternalError}
	}
	sender := session.entry.presence
	module.mu.Unlock()
	observation, ok := sender.Observe()
	if !ok {
		return Result{Rejection: RejectionInternalError}
	}
	if !observation.Alive {
		return Result{Rejection: RejectionDead}
	}
	var whisperTarget Character
	var audience Audience
	var sayRecipients []SayRecipient
	switch request.Channel {
	case ChannelWhisper:
		if request.Target.Kind != TargetWhisperCharacter || request.Target.Value == "" {
			return Result{Rejection: RejectionTargetNotFound}
		}
		if session.module.directory == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		var found bool
		var err error
		whisperTarget, found, err = session.module.directory.ResolveCharacterName(ctx, request.Target.Value)
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
		if !found {
			return Result{Rejection: RejectionTargetNotFound}
		}
	case ChannelZone:
		if request.Target.Kind != TargetNone {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
	case ChannelSay:
		if request.Target.Kind != TargetNone || module.say == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		var err error
		sayRecipients, err = module.say.Audience(ctx, SaySpeaker{
			CharacterID: sender.CharacterID,
			EntityID:    sender.EntityID,
			ZoneID:      sender.ZoneID,
			Position:    observation.Position,
		})
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
	case ChannelGroup:
		if request.Target.Kind != TargetNone || module.group == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		var err error
		audience, err = module.group.Audience(ctx, sender.CharacterID)
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
	case ChannelRaid:
		if request.Target.Kind != TargetNone || module.raid == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		var err error
		audience, err = module.raid.Audience(ctx, sender.CharacterID)
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
	case ChannelGuild, ChannelGuildOfficer:
		if request.Target.Kind != TargetNone || module.guild == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		var err error
		audience, err = module.guild.Audience(ctx, sender.CharacterID, request.Channel == ChannelGuildOfficer)
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
	case ChannelZoneSpecial, ChannelWorld:
		if request.Target.Kind != TargetNone || session.module.currency == nil {
			return Result{Rejection: RejectionUnsupportedChannel}
		}
		currency := AlternativeCurrency{
			ResourceID: WorldChatCurrencyResourceID,
			SysName:    WorldChatCurrencySysName,
		}
		if request.Channel == ChannelZoneSpecial {
			currency = AlternativeCurrency{
				ResourceID: ZoneSpecialCurrencyResourceID,
				SysName:    ZoneSpecialCurrencySysName,
			}
		}
		spent, err := session.module.currency.Spend(ctx, sender.CharacterID, currency, 1)
		if err != nil {
			return Result{Rejection: RejectionInternalError}
		}
		if !spent {
			return Result{Rejection: RejectionNoPoints}
		}
	default:
		return Result{Rejection: RejectionUnsupportedChannel}
	}
	if audience.Refusal != AudienceAllowed {
		switch audience.Refusal {
		case AudienceNotMember:
			return Result{Rejection: RejectionNotMember}
		case AudienceNotAuthorized:
			return Result{Rejection: RejectionNotAuthorized}
		default:
			return Result{Rejection: RejectionInternalError}
		}
	}

	module.mu.Lock()
	if module.live[session.entry.presence.CharacterID] != session.entry {
		module.mu.Unlock()
		return Result{Rejection: RejectionInternalError}
	}
	targets := make([]*entry, 0, len(module.live))
	readability := make(map[uuid.UUID]string, len(sayRecipients))
	seen := make(map[uuid.UUID]struct{}, len(module.live))
	switch request.Channel {
	case ChannelWhisper:
		targetEntry := module.live[whisperTarget.CharacterID]
		if targetEntry == nil {
			module.mu.Unlock()
			return Result{Rejection: RejectionTargetOffline}
		}
		// Retail has no self-whisper suppression. A target resolving to the
		// speaker still receives the server copy in addition to local echo.
		targets = append(targets, targetEntry)
	case ChannelZone, ChannelZoneSpecial:
		targets = appendMatching(targets, module.live, session.entry, func(candidate Presence) bool {
			return candidate.ZoneID == sender.ZoneID
		})
	case ChannelWorld:
		targets = appendMatching(targets, module.live, session.entry, func(Presence) bool { return true })
	case ChannelSay:
		for _, recipient := range sayRecipients {
			if recipient.CharacterID == sender.CharacterID {
				continue
			}
			if _, duplicate := seen[recipient.CharacterID]; duplicate {
				continue
			}
			if target := module.live[recipient.CharacterID]; target != nil {
				targets = append(targets, target)
				seen[recipient.CharacterID] = struct{}{}
				readability[recipient.CharacterID] = recipient.UnreadableFactionLocalizationID
			}
		}
	case ChannelGroup, ChannelRaid, ChannelGuild, ChannelGuildOfficer:
		for _, recipientID := range audience.RecipientCharacterIDs {
			if recipientID == sender.CharacterID {
				continue
			}
			if _, duplicate := seen[recipientID]; duplicate {
				continue
			}
			if target := module.live[recipientID]; target != nil {
				targets = append(targets, target)
				seen[recipientID] = struct{}{}
			}
		}
	}
	module.nextID++
	messageID := module.nextID
	sentAt := now
	session.hasAccepted = true
	session.lastAcceptedAt = now
	sort.SliceStable(targets, func(left, right int) bool {
		if targets[left] == session.entry {
			return true
		}
		if targets[right] == session.entry {
			return false
		}
		return targets[left].presence.CharacterID.String() < targets[right].presence.CharacterID.String()
	})
	module.mu.Unlock()

	for _, recipient := range targets {
		delivery := Delivery{
			MessageID:         messageID,
			Channel:           request.Channel,
			SentAt:            sentAt,
			SenderCharacterID: sender.CharacterID,
			SenderEntityID:    sender.EntityID,
			SenderName:        sender.Name,
			SenderAlive:       observation.Alive,
			Body:              Body{Kind: BodyUserText, UserText: request.Text},
		}
		if factionID := readability[recipient.presence.CharacterID]; factionID != "" {
			delivery.Body = Body{
				Kind:                      BodyUnreadableFaction,
				FactionNameLocalizationID: factionID,
			}
		}
		if request.Channel == ChannelWhisper {
			delivery.WhisperPeerName = sender.Name
		}
		recipient.sink.OfferChat(delivery)
	}
	return Result{Accepted: true}
}

func validPresence(presence Presence) bool {
	return presence.CharacterID != uuid.Nil && presence.EntityID != 0 && presence.Name != "" &&
		presence.ZoneID != "" && presence.Observe != nil
}

func appendMatching(
	targets []*entry,
	live map[uuid.UUID]*entry,
	sender *entry,
	matches func(Presence) bool,
) []*entry {
	for _, candidate := range live {
		if candidate != sender && matches(candidate.presence) {
			targets = append(targets, candidate)
		}
	}
	return targets
}

func utf16Units(text string) int {
	units := 0
	for _, character := range text {
		if character <= 0xffff {
			units++
		} else {
			units += 2
		}
	}
	return units
}
