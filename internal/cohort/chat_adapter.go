package cohort

import (
	"context"
	"errors"

	"github.com/SarnautCore/server/internal/chat"
	"github.com/google/uuid"
)

type GuildChatAudience struct {
	authority *Authority
}

func NewGuildChatAudience(authority *Authority) (*GuildChatAudience, error) {
	if authority == nil {
		return nil, errors.New("guild chat audience: authority is required")
	}
	return &GuildChatAudience{authority: authority}, nil
}

func (adapter *GuildChatAudience) Audience(
	ctx context.Context,
	characterID uuid.UUID,
	officersOnly bool,
) (chat.Audience, error) {
	audience, err := adapter.authority.GuildAudience(ctx, characterID, officersOnly)
	if err != nil {
		return chat.Audience{}, err
	}
	return toChatAudience(audience), nil
}

type RaidChatAudience struct {
	authority *Authority
}

func NewRaidChatAudience(authority *Authority) (*RaidChatAudience, error) {
	if authority == nil {
		return nil, errors.New("raid chat audience: authority is required")
	}
	return &RaidChatAudience{authority: authority}, nil
}

func (adapter *RaidChatAudience) Audience(
	ctx context.Context,
	characterID uuid.UUID,
) (chat.Audience, error) {
	audience, err := adapter.authority.RaidAudience(ctx, characterID)
	if err != nil {
		return chat.Audience{}, err
	}
	return toChatAudience(audience), nil
}

func toChatAudience(audience Audience) chat.Audience {
	refusal := chat.AudienceAllowed
	switch audience.Refusal {
	case Allowed:
	case NotMember:
		refusal = chat.AudienceNotMember
	case NotAuthorized:
		refusal = chat.AudienceNotAuthorized
	default:
		refusal = chat.AudienceNotAuthorized
	}
	return chat.Audience{
		RecipientCharacterIDs: append([]uuid.UUID(nil), audience.RecipientCharacterIDs...),
		Refusal:               refusal,
	}
}

var (
	_ chat.GuildAudience = (*GuildChatAudience)(nil)
	_ chat.RaidAudience  = (*RaidChatAudience)(nil)
)
