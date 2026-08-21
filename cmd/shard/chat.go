package main

import (
	"context"
	"errors"
	"sync"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/chataudience"
	"github.com/SarnautCore/server/internal/currency"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/visibility"
	"github.com/google/uuid"
)

type characterNames interface {
	CharacterByNormalizedName(context.Context, string) (charstore.Character, error)
}

type paidChatCurrencies struct {
	ledger *currency.Ledger
}

func (currencies paidChatCurrencies) Spend(
	ctx context.Context,
	characterID uuid.UUID,
	resource chat.AlternativeCurrency,
	amount uint64,
) (bool, error) {
	if currencies.ledger == nil {
		return false, errors.New("paid chat currency ledger is unavailable")
	}
	return currencies.ledger.SpendProductIdentity(
		ctx,
		characterID,
		resource.ResourceID,
		resource.SysName,
		amount,
	)
}

// worldChatSessions publishes the current world implementation's visibility
// topology. A hosted Zone is one scanner and one mission instance; every live
// player pair in that instance can influence each other. The audience still
// applies the exact 100m, faction, avatar, and reverse-ignore rules at send.
type worldChatSessions struct {
	mu        sync.Mutex
	audience  *chataudience.Authority
	influence *visibility.Influence
	live      map[uuid.UUID]worldChatPresence
}

type worldChatPresence struct {
	entityID uint64
	zoneID   string
}

type worldChatSession struct {
	owner     *worldChatSessions
	character uuid.UUID
	audience  *chataudience.Session
	once      sync.Once
}

func newWorldChatSessions(authority *chataudience.Authority, influence *visibility.Influence) *worldChatSessions {
	return &worldChatSessions{
		audience:  authority,
		influence: influence,
		live:      make(map[uuid.UUID]worldChatPresence),
	}
}

func (sessions *worldChatSessions) Admit(
	characterID uuid.UUID,
	entityID uint64,
	zone gametypes.Zone,
) (session.LocalChatSession, error) {
	if sessions == nil || sessions.audience == nil || sessions.influence == nil || zone == nil {
		return nil, errors.New("local chat world authority is unavailable")
	}
	// The current world has one scanner and mission per Zone instance. Keeping
	// both explicit prevents a future instancing implementation from silently
	// broadening Say when it introduces narrower identifiers.
	zoneID := zone.ID()
	membership, err := sessions.audience.Admit(chataudience.WorldAvatar(
		characterID,
		entityID,
		zoneID,
		zoneID,
		zone,
	))
	if err != nil {
		return nil, err
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	for _, candidate := range sessions.live {
		if candidate.zoneID != zoneID {
			continue
		}
		sessions.influence.SetCanInfluence(entityID, candidate.entityID, true)
		sessions.influence.SetCanInfluence(candidate.entityID, entityID, true)
	}
	sessions.live[characterID] = worldChatPresence{entityID: entityID, zoneID: zoneID}
	return &worldChatSession{owner: sessions, character: characterID, audience: membership}, nil
}

func (session *worldChatSession) Close() {
	if session == nil {
		return
	}
	session.once.Do(func() {
		session.owner.mu.Lock()
		presence, exists := session.owner.live[session.character]
		if exists {
			delete(session.owner.live, session.character)
			session.owner.influence.Forget(presence.entityID)
		}
		session.owner.mu.Unlock()
		session.audience.Close()
	})
}

// partyChatAudience translates the party module's session-aware read model to
// chat's immutable remote-recipient contract.
type partyChatAudience struct {
	parties party.AudienceReader
}

func (audience partyChatAudience) Audience(ctx context.Context, characterID uuid.UUID) (chat.Audience, error) {
	recipients, refusal := audience.parties.Audience(ctx, characterID)
	switch refusal {
	case party.AudienceAllowed:
		return chat.Audience{RecipientCharacterIDs: recipients}, nil
	case party.AudienceNoParty, party.AudienceNotMember:
		return chat.Audience{Refusal: chat.AudienceNotMember}, nil
	case party.AudienceUnavailable:
		return chat.Audience{}, ctx.Err()
	default:
		return chat.Audience{}, errors.New("unknown party audience refusal")
	}
}

// characterDirectory is the production whisper-name adapter. It resolves
// against auth.characters, where normalized names are authoritative, while
// keeping persistence types out of the chat module.
type characterDirectory struct {
	characters characterNames
}

func (directory characterDirectory) ResolveCharacterName(
	ctx context.Context,
	name string,
) (chat.Character, bool, error) {
	normalized := charstore.NormalizeCharacterName(name)
	if normalized == "" {
		return chat.Character{}, false, nil
	}
	character, err := directory.characters.CharacterByNormalizedName(ctx, normalized)
	if errors.Is(err, charstore.ErrNotFound) {
		return chat.Character{}, false, nil
	}
	if err != nil {
		return chat.Character{}, false, err
	}
	if character.DeletedAt != nil {
		return chat.Character{}, false, nil
	}
	return chat.Character{CharacterID: character.CharacterID, Name: character.Name}, true, nil
}
