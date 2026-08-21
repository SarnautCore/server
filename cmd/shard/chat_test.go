package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/chataudience"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/visibility"
	"github.com/SarnautCore/server/internal/world"
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

func TestWorldChatSessionsPublishAndRemoveCurrentZoneInfluence(t *testing.T) {
	t.Parallel()

	zone, err := world.NewZone(world.ZoneConfig{
		ID: "chat-world", TickInterval: time.Second, SnapshotInterval: time.Second, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	influence := visibility.NewInfluence()
	authority, err := chataudience.New(testFactions{
		"faction.league": {
			ID: "faction.league", NameKey: "faction.league.name", DefaultStance: gametypes.StanceFriendly,
		},
	}, influence, social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New audience error = %v", err)
	}
	sessions := newWorldChatSessions(authority, influence)
	aliceID, bobID := uuid.New(), uuid.New()
	aliceEntity, _ := zone.Join()
	bobEntity, _ := zone.Join()
	if err := zone.GameCommand(func(tick gametypes.Tick) error {
		tick.Entity(aliceEntity).Faction = "faction.league"
		tick.Entity(bobEntity).Faction = "faction.league"
		return nil
	}); err != nil {
		t.Fatalf("set player factions: %v", err)
	}
	alice, err := sessions.Admit(aliceID, aliceEntity, zone)
	if err != nil {
		t.Fatalf("Admit(Alice) error = %v", err)
	}
	defer alice.Close()
	bob, err := sessions.Admit(bobID, bobEntity, zone)
	if err != nil {
		t.Fatalf("Admit(Bob) error = %v", err)
	}

	recipients, err := authority.Audience(t.Context(), chat.SaySpeaker{
		CharacterID: aliceID, EntityID: aliceEntity, ZoneID: zone.ID(), Position: chat.Position{},
	})
	if err != nil || len(recipients) != 1 || recipients[0].CharacterID != bobID {
		t.Fatalf("live Say audience = %+v, %v, want Bob", recipients, err)
	}
	bob.Close()
	recipients, err = authority.Audience(t.Context(), chat.SaySpeaker{
		CharacterID: aliceID, EntityID: aliceEntity, ZoneID: zone.ID(), Position: chat.Position{},
	})
	if err != nil || len(recipients) != 0 {
		t.Fatalf("closed Say audience = %+v, %v, want empty", recipients, err)
	}
}

type testFactions map[string]gametypes.Faction

func (factions testFactions) Faction(id string) (gametypes.Faction, bool) {
	faction, ok := factions[id]
	return faction, ok
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
