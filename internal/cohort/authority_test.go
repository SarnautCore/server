package cohort_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/SarnautCore/server/internal/cohort"
	"github.com/google/uuid"
)

func TestRecoveredGuildChatRightOrdinals(t *testing.T) {
	t.Parallel()

	if cohort.GMRChat != 0 || cohort.GMROfficerChat != 1 {
		t.Fatalf("guild chat right ordinals = %d, %d", cohort.GMRChat, cohort.GMROfficerChat)
	}
	if _, err := cohort.Rights(cohort.GuildMemberRight(2)); !errors.Is(err, cohort.ErrInvalidRoster) {
		t.Fatalf("unrecovered right error = %v", err)
	}
}

func TestGuildAudienceEnforcesRecoveredRightsAndConnectedRoster(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	presence := cohort.NewPresenceRegistry()
	authority, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	chat := cohort.MustRights(cohort.GMRChat)
	officer := cohort.MustRights(cohort.GMRChat, cohort.GMROfficerChat)
	noRights := cohort.MustRights()
	characters := ids(5)
	sender, member, officerMember, muted, offline := characters[0], characters[1], characters[2], characters[3], characters[4]
	if err := authority.ReplaceGuild(t.Context(), cohort.GuildRoster{
		GuildID: uuid.New(),
		Members: []cohort.GuildMember{
			{CharacterID: sender, Rights: officer},
			{CharacterID: member, Rights: chat},
			{CharacterID: officerMember, Rights: officer},
			{CharacterID: muted, Rights: noRights},
			{CharacterID: offline, Rights: officer},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connect(t, presence, sender, member, officerMember, muted)

	regular, err := authority.GuildAudience(t.Context(), sender, false)
	if err != nil {
		t.Fatal(err)
	}
	if regular.Refusal != cohort.Allowed {
		t.Fatalf("regular refusal = %s", regular.Refusal)
	}
	assertIDs(t, regular.RecipientCharacterIDs, member, officerMember)

	officers, err := authority.GuildAudience(t.Context(), sender, true)
	if err != nil {
		t.Fatal(err)
	}
	if officers.Refusal != cohort.Allowed {
		t.Fatalf("officer refusal = %s", officers.Refusal)
	}
	assertIDs(t, officers.RecipientCharacterIDs, officerMember)

	unauthorized, err := authority.GuildAudience(t.Context(), member, true)
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.Refusal != cohort.NotAuthorized || unauthorized.RecipientCharacterIDs != nil {
		t.Fatalf("unauthorized audience = %+v", unauthorized)
	}

	mutedAudience, err := authority.GuildAudience(t.Context(), muted, false)
	if err != nil {
		t.Fatal(err)
	}
	if mutedAudience.Refusal != cohort.NotAuthorized {
		t.Fatalf("muted refusal = %s", mutedAudience.Refusal)
	}
}

func TestGuildAndRaidReturnNotMember(t *testing.T) {
	t.Parallel()

	authority, err := cohort.NewAuthority(cohort.NewMemoryRepository(), cohort.NewPresenceRegistry())
	if err != nil {
		t.Fatal(err)
	}
	characterID := uuid.New()
	guild, err := authority.GuildAudience(t.Context(), characterID, false)
	if err != nil {
		t.Fatal(err)
	}
	if guild.Refusal != cohort.NotMember {
		t.Fatalf("guild refusal = %s", guild.Refusal)
	}
	raid, err := authority.RaidAudience(t.Context(), characterID)
	if err != nil {
		t.Fatal(err)
	}
	if raid.Refusal != cohort.NotMember {
		t.Fatalf("raid refusal = %s", raid.Refusal)
	}
}

func TestRaidAudiencePreservesRosterOrderAndExcludesSender(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	presence := cohort.NewPresenceRegistry()
	authority, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	characters := ids(4)
	first, sender, offline, last := characters[0], characters[1], characters[2], characters[3]
	if err := authority.ReplaceRaid(t.Context(), cohort.RaidRoster{
		RaidID:  uuid.New(),
		Members: []uuid.UUID{first, sender, offline, last},
	}); err != nil {
		t.Fatal(err)
	}
	connect(t, presence, first, sender, last)
	audience, err := authority.RaidAudience(t.Context(), sender)
	if err != nil {
		t.Fatal(err)
	}
	if audience.Refusal != cohort.Allowed {
		t.Fatalf("refusal = %s", audience.Refusal)
	}
	assertIDs(t, audience.RecipientCharacterIDs, first, last)
}

func TestStaleDisconnectCannotEraseReconnect(t *testing.T) {
	t.Parallel()

	presence := cohort.NewPresenceRegistry()
	characterID := uuid.New()
	oldRelease, err := presence.Connect(characterID)
	if err != nil {
		t.Fatal(err)
	}
	newRelease, err := presence.Connect(characterID)
	if err != nil {
		t.Fatal(err)
	}
	oldRelease()
	if !presence.Connected(characterID) {
		t.Fatal("stale disconnect erased the reconnect")
	}
	newRelease()
	newRelease()
	if presence.Connected(characterID) {
		t.Fatal("winning session remained connected after release")
	}
}

func TestMembershipPersistsAcrossAuthorityReconstruction(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	presence := cohort.NewPresenceRegistry()
	first, second := uuid.New(), uuid.New()
	rights := cohort.MustRights(cohort.GMRChat)
	original, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.ReplaceGuild(t.Context(), cohort.GuildRoster{
		GuildID: uuid.New(),
		Members: []cohort.GuildMember{
			{CharacterID: first, Rights: rights},
			{CharacterID: second, Rights: rights},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := original.ReplaceRaid(t.Context(), cohort.RaidRoster{
		RaidID: uuid.New(), Members: []uuid.UUID{first, second},
	}); err != nil {
		t.Fatal(err)
	}

	reconstructed, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	connect(t, presence, first, second)
	guild, err := reconstructed.GuildAudience(t.Context(), first, false)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, guild.RecipientCharacterIDs, second)
	raid, err := reconstructed.RaidAudience(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, raid.RecipientCharacterIDs, second)
}

func TestReplacingRosterMovesMembersAtomically(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	rights := cohort.MustRights(cohort.GMRChat)
	firstGuild, secondGuild := uuid.New(), uuid.New()
	moved, stays := uuid.New(), uuid.New()
	if err := store.ReplaceGuild(t.Context(), cohort.GuildRoster{
		GuildID: firstGuild,
		Members: []cohort.GuildMember{
			{CharacterID: moved, Rights: rights},
			{CharacterID: stays, Rights: rights},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceGuild(t.Context(), cohort.GuildRoster{
		GuildID: secondGuild,
		Members: []cohort.GuildMember{{CharacterID: moved, Rights: rights}},
	}); err != nil {
		t.Fatal(err)
	}
	movedRoster, err := store.GuildByMember(t.Context(), moved)
	if err != nil {
		t.Fatal(err)
	}
	if movedRoster.GuildID != secondGuild {
		t.Fatalf("moved member guild = %s", movedRoster.GuildID)
	}
	stayingRoster, err := store.GuildByMember(t.Context(), stays)
	if err != nil {
		t.Fatal(err)
	}
	if stayingRoster.GuildID != firstGuild || len(stayingRoster.Members) != 1 {
		t.Fatalf("old guild roster = %+v", stayingRoster)
	}
}

func TestInvalidRostersAreRejectedWithoutMutation(t *testing.T) {
	t.Parallel()

	store := cohort.NewMemoryRepository()
	member := uuid.New()
	rights := cohort.MustRights(cohort.GMRChat)
	valid := cohort.GuildRoster{
		GuildID: uuid.New(),
		Members: []cohort.GuildMember{{CharacterID: member, Rights: rights}},
	}
	if err := store.ReplaceGuild(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Members = append(invalid.Members, invalid.Members[0])
	if err := store.ReplaceGuild(t.Context(), invalid); !errors.Is(err, cohort.ErrInvalidRoster) {
		t.Fatalf("invalid roster error = %v", err)
	}
	stored, err := store.GuildByMember(t.Context(), member)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, valid) {
		t.Fatalf("stored roster mutated: %+v", stored)
	}
}

func TestConcurrentReadsPresenceAndRosterReplacement(t *testing.T) {
	store := cohort.NewMemoryRepository()
	presence := cohort.NewPresenceRegistry()
	authority, err := cohort.NewAuthority(store, presence)
	if err != nil {
		t.Fatal(err)
	}
	sender, recipient := uuid.New(), uuid.New()
	rights := cohort.MustRights(cohort.GMRChat)
	roster := cohort.GuildRoster{
		GuildID: uuid.New(),
		Members: []cohort.GuildMember{
			{CharacterID: sender, Rights: rights},
			{CharacterID: recipient, Rights: rights},
		},
	}
	if err := authority.ReplaceGuild(t.Context(), roster); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < 250; iteration++ {
				release, _ := presence.Connect(recipient)
				_, _ = authority.GuildAudience(context.Background(), sender, false)
				release()
				if worker == 0 {
					_ = authority.ReplaceGuild(context.Background(), roster)
				}
			}
		}(worker)
	}
	wait.Wait()
}

func connect(t *testing.T, presence *cohort.PresenceRegistry, characterIDs ...uuid.UUID) {
	t.Helper()
	for _, characterID := range characterIDs {
		if _, err := presence.Connect(characterID); err != nil {
			t.Fatal(err)
		}
	}
}

func ids(count int) []uuid.UUID {
	result := make([]uuid.UUID, count)
	for index := range result {
		result[index] = uuid.New()
	}
	return result
}

func assertIDs(t *testing.T, actual []uuid.UUID, expected ...uuid.UUID) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("recipient IDs = %v, want %v", actual, expected)
	}
}
