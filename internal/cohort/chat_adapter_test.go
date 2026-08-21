package cohort_test

import (
	"testing"

	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/cohort"
	"github.com/google/uuid"
)

func TestChatAdaptersMapTypedAudienceResults(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	presence := cohort.NewPresenceRegistry()
	authority, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	guildAdapter, err := cohort.NewGuildChatAudience(authority)
	if err != nil {
		t.Fatal(err)
	}
	raidAdapter, err := cohort.NewRaidChatAudience(authority)
	if err != nil {
		t.Fatal(err)
	}

	sender, recipient := uuid.New(), uuid.New()
	chatRight := cohort.MustRights(cohort.GMRChat)
	if err := authority.ReplaceGuild(t.Context(), cohort.GuildRoster{
		GuildID: uuid.New(),
		Members: []cohort.GuildMember{
			{CharacterID: sender, Rights: chatRight},
			{CharacterID: recipient, Rights: chatRight},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.ReplaceRaid(t.Context(), cohort.RaidRoster{
		RaidID: uuid.New(), Members: []uuid.UUID{sender, recipient},
	}); err != nil {
		t.Fatal(err)
	}
	connect(t, presence, sender, recipient)

	guild, err := guildAdapter.Audience(t.Context(), sender, false)
	if err != nil {
		t.Fatal(err)
	}
	if guild.Refusal != chat.AudienceAllowed {
		t.Fatalf("guild refusal = %d", guild.Refusal)
	}
	assertIDs(t, guild.RecipientCharacterIDs, recipient)

	officer, err := guildAdapter.Audience(t.Context(), sender, true)
	if err != nil {
		t.Fatal(err)
	}
	if officer.Refusal != chat.AudienceNotAuthorized {
		t.Fatalf("officer refusal = %d", officer.Refusal)
	}

	raid, err := raidAdapter.Audience(t.Context(), sender)
	if err != nil {
		t.Fatal(err)
	}
	if raid.Refusal != chat.AudienceAllowed {
		t.Fatalf("raid refusal = %d", raid.Refusal)
	}
	assertIDs(t, raid.RecipientCharacterIDs, recipient)

	unknown, err := raidAdapter.Audience(t.Context(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Refusal != chat.AudienceNotMember {
		t.Fatalf("unknown refusal = %d", unknown.Refusal)
	}
}
