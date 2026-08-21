package main

import (
	"context"
	"errors"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/party"
	"github.com/google/uuid"
)

type characterNames interface {
	CharacterByNormalizedName(context.Context, string) (charstore.Character, error)
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
