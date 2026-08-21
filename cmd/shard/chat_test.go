package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/party"
)

func TestCharacterDirectoryUsesAuthoritativeNameNormalizationAndDeletionState(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("019200f0-0000-7000-8000-00000000c091")
	repository := &fakeCharacterNames{character: charstore.Character{CharacterID: characterID, Name: "Obrien"}}
	directory := characterDirectory{characters: repository}
	character, found, err := directory.ResolveCharacterName(t.Context(), "O'-BRIEN")
	if err != nil || !found || character.CharacterID != characterID || character.Name != "Obrien" {
		t.Fatalf("ResolveCharacterName() = %+v/%v/%v", character, found, err)
	}
	if repository.gotName != "obrien" {
		t.Errorf("normalized lookup = %q, want obrien", repository.gotName)
	}

	deletedAt := time.Now()
	repository.character.DeletedAt = &deletedAt
	if _, found, err := directory.ResolveCharacterName(t.Context(), "Obrien"); err != nil || found {
		t.Fatalf("deleted resolve = found %v error %v, want not found", found, err)
	}
	repository.character.DeletedAt = nil
	repository.err = charstore.ErrNotFound
	if _, found, err := directory.ResolveCharacterName(t.Context(), "Nobody"); err != nil || found {
		t.Fatalf("missing resolve = found %v error %v, want not found", found, err)
	}
	repository.err = errors.New("database unavailable")
	if _, _, err := directory.ResolveCharacterName(t.Context(), "Obrien"); err == nil {
		t.Fatal("repository failure = nil, want propagated error")
	}
}

func TestPartyChatAudienceMapsAuthenticatedRemoteRecipientsAndRefusals(t *testing.T) {
	t.Parallel()

	authority := party.New()
	leader := party.Actor{CharacterID: uuid.New(), SessionID: uuid.New()}
	member := party.Actor{CharacterID: uuid.New(), SessionID: uuid.New()}
	if got := authority.Connect(leader); got != party.RefusalNone {
		t.Fatalf("Connect(leader) = %v", got)
	}
	if got := authority.Connect(member); got != party.RefusalNone {
		t.Fatalf("Connect(member) = %v", got)
	}
	adapter := partyChatAudience{parties: authority}
	if got, err := adapter.Audience(t.Context(), leader.CharacterID); err != nil || got.Refusal != chat.AudienceNotMember {
		t.Fatalf("solo Audience() = %+v, %v, want NotMember", got, err)
	}
	if got := authority.Invite(leader, member.CharacterID); got != party.RefusalNone {
		t.Fatalf("Invite() = %v", got)
	}
	if got := authority.Accept(member); got != party.RefusalNone {
		t.Fatalf("Accept() = %v", got)
	}
	got, err := adapter.Audience(t.Context(), leader.CharacterID)
	if err != nil || got.Refusal != chat.AudienceAllowed || len(got.RecipientCharacterIDs) != 1 || got.RecipientCharacterIDs[0] != member.CharacterID {
		t.Fatalf("formed Audience() = %+v, %v, want connected remote member", got, err)
	}
}

type fakeCharacterNames struct {
	character charstore.Character
	err       error
	gotName   string
}

func (repository *fakeCharacterNames) CharacterByNormalizedName(_ context.Context, name string) (charstore.Character, error) {
	repository.gotName = name
	return repository.character, repository.err
}
