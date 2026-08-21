package cohort

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type Authority struct {
	repository Repository
	presence   PresenceReader
}

func NewAuthority(repository Repository, presence PresenceReader) (*Authority, error) {
	if repository == nil {
		return nil, errors.New("cohort authority: repository is required")
	}
	if presence == nil {
		return nil, errors.New("cohort authority: presence reader is required")
	}
	return &Authority{repository: repository, presence: presence}, nil
}

// GuildAudience checks the sender's recovered retail chat right, then returns
// every other connected roster member who has the same right.
func (authority *Authority) GuildAudience(
	ctx context.Context,
	senderID uuid.UUID,
	officersOnly bool,
) (Audience, error) {
	roster, err := authority.repository.GuildByMember(ctx, senderID)
	if errors.Is(err, ErrNotFound) {
		return Audience{Refusal: NotMember}, nil
	}
	if err != nil {
		return Audience{}, fmt.Errorf("load guild audience: %w", err)
	}

	required := GMRChat
	if officersOnly {
		required = GMROfficerChat
	}
	senderFound := false
	for _, member := range roster.Members {
		if member.CharacterID == senderID {
			senderFound = true
			if !member.Rights.Has(required) {
				return Audience{Refusal: NotAuthorized}, nil
			}
			break
		}
	}
	if !senderFound {
		return Audience{Refusal: NotMember}, nil
	}

	recipients := make([]uuid.UUID, 0, len(roster.Members)-1)
	for _, member := range roster.Members {
		if member.CharacterID == senderID || !member.Rights.Has(required) {
			continue
		}
		if authority.presence.Connected(member.CharacterID) {
			recipients = append(recipients, member.CharacterID)
		}
	}
	return Audience{RecipientCharacterIDs: recipients, Refusal: Allowed}, nil
}

// RaidAudience returns every other connected member in durable roster order.
func (authority *Authority) RaidAudience(ctx context.Context, senderID uuid.UUID) (Audience, error) {
	roster, err := authority.repository.RaidByMember(ctx, senderID)
	if errors.Is(err, ErrNotFound) {
		return Audience{Refusal: NotMember}, nil
	}
	if err != nil {
		return Audience{}, fmt.Errorf("load raid audience: %w", err)
	}

	senderFound := false
	for _, memberID := range roster.Members {
		if memberID == senderID {
			senderFound = true
			break
		}
	}
	if !senderFound {
		return Audience{Refusal: NotMember}, nil
	}

	recipients := make([]uuid.UUID, 0, len(roster.Members)-1)
	for _, memberID := range roster.Members {
		if memberID != senderID && authority.presence.Connected(memberID) {
			recipients = append(recipients, memberID)
		}
	}
	return Audience{RecipientCharacterIDs: recipients, Refusal: Allowed}, nil
}

func (authority *Authority) ReplaceGuild(ctx context.Context, roster GuildRoster) error {
	return authority.repository.ReplaceGuild(ctx, cloneGuild(roster))
}

func (authority *Authority) ReplaceRaid(ctx context.Context, roster RaidRoster) error {
	return authority.repository.ReplaceRaid(ctx, cloneRaid(roster))
}
