package party_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/party"
)

func actor() party.Actor {
	return party.Actor{CharacterID: uuid.New(), SessionID: uuid.New()}
}

func connect(t *testing.T, authority *party.Authority, actors ...party.Actor) {
	t.Helper()
	for _, current := range actors {
		if got := authority.Connect(current); got != party.RefusalNone {
			t.Fatalf("Connect(%v) refusal = %s", current.CharacterID, got)
		}
	}
}

func form(t *testing.T, authority *party.Authority, leader, member party.Actor) {
	t.Helper()
	if got := authority.Invite(leader, member.CharacterID); got != party.RefusalNone {
		t.Fatalf("Invite() refusal = %s", got)
	}
	if got := authority.Accept(member); got != party.RefusalNone {
		t.Fatalf("Accept() refusal = %s", got)
	}
}

func TestInviteAcceptDeclineAndBusyRules(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, first, second := actor(), actor(), actor()
	connect(t, authority, leader, first, second)

	if got := authority.Invite(leader, leader.CharacterID); got != party.RefusalSelfInvite {
		t.Fatalf("self invite refusal = %s", got)
	}
	if got := authority.Invite(leader, first.CharacterID); got != party.RefusalNone {
		t.Fatalf("first invite refusal = %s", got)
	}
	if got := authority.Invite(second, first.CharacterID); got != party.RefusalInviteeBusy {
		t.Fatalf("busy invite refusal = %s", got)
	}
	if got := authority.Decline(first); got != party.RefusalNone {
		t.Fatalf("Decline() refusal = %s", got)
	}
	if got := authority.Accept(first); got != party.RefusalNoInvitation {
		t.Fatalf("accept after decline refusal = %s", got)
	}

	form(t, authority, leader, first)
	if got := authority.Invite(second, first.CharacterID); got != party.RefusalAlreadyGrouped {
		t.Fatalf("invite grouped target refusal = %s", got)
	}
	if got := authority.Invite(first, second.CharacterID); got != party.RefusalNotLeader {
		t.Fatalf("non-leader invite refusal = %s", got)
	}
}

func TestRetailSixMemberCapacityIsCheckedAtInviteAndAccept(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader := actor()
	connect(t, authority, leader)
	members := make([]party.Actor, party.MaxMembers)
	for index := range members {
		members[index] = actor()
		connect(t, authority, members[index])
	}

	// Leave one invitation pending while four others make the party size five.
	if got := authority.Invite(leader, members[4].CharacterID); got != party.RefusalNone {
		t.Fatalf("pending invite refusal = %s", got)
	}
	for index := 0; index < 4; index++ {
		form(t, authority, leader, members[index])
	}
	form(t, authority, leader, members[5])
	if got := authority.Accept(members[4]); got != party.RefusalPartyFull {
		t.Fatalf("late accept refusal = %s, want party_full", got)
	}
	if got := authority.Invite(leader, members[4].CharacterID); got != party.RefusalPartyFull {
		t.Fatalf("full-party invite refusal = %s", got)
	}

	snapshot, refusal := authority.Membership(leader.CharacterID)
	if refusal != party.AudienceAllowed || len(snapshot.Members) != party.MaxMembers {
		t.Fatalf("membership = %+v, %s", snapshot, refusal)
	}
}

func TestLeadershipKickLeaveAndTwoMemberDissolution(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, first, second, outsider := actor(), actor(), actor(), actor()
	connect(t, authority, leader, first, second, outsider)
	form(t, authority, leader, first)
	form(t, authority, leader, second)

	if got := authority.Kick(first, second.CharacterID); got != party.RefusalNotLeader {
		t.Fatalf("non-leader kick refusal = %s", got)
	}
	if got := authority.Kick(leader, leader.CharacterID); got != party.RefusalCannotKickSelf {
		t.Fatalf("self kick refusal = %s", got)
	}
	if got := authority.TransferLeadership(leader, outsider.CharacterID); got != party.RefusalTargetNotMember {
		t.Fatalf("outsider transfer refusal = %s", got)
	}
	if got := authority.TransferLeadership(leader, first.CharacterID); got != party.RefusalNone {
		t.Fatalf("transfer refusal = %s", got)
	}
	if got := authority.Kick(first, second.CharacterID); got != party.RefusalNone {
		t.Fatalf("leader kick refusal = %s", got)
	}
	if got := authority.Leave(leader); got != party.RefusalNone {
		t.Fatalf("Leave() refusal = %s", got)
	}
	if _, refusal := authority.Membership(first.CharacterID); refusal != party.AudienceNoParty {
		t.Fatalf("remaining member refusal = %s, want no_party", refusal)
	}
}

func TestDisconnectPreservesMembershipAndElectsConnectedLeader(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, first, second := actor(), actor(), actor()
	connect(t, authority, leader, first, second)
	form(t, authority, leader, first)
	form(t, authority, leader, second)

	if got := authority.Disconnect(leader); got != party.RefusalNone {
		t.Fatalf("leader Disconnect() refusal = %s", got)
	}
	snapshot, refusal := authority.Membership(first.CharacterID)
	if refusal != party.AudienceAllowed || snapshot.LeaderID != first.CharacterID || len(snapshot.Members) != 3 {
		t.Fatalf("after disconnect membership = %+v, %s", snapshot, refusal)
	}
	wantAudience := []uuid.UUID{second.CharacterID}
	if got, refusal := authority.Audience(context.Background(), first.CharacterID); refusal != party.AudienceAllowed || !reflect.DeepEqual(got, wantAudience) {
		t.Fatalf("Audience() = %v, %s, want %v", got, refusal, wantAudience)
	}

	replacement := leader
	replacement.SessionID = uuid.New()
	if got := authority.Connect(replacement); got != party.RefusalNone {
		t.Fatalf("reconnect refusal = %s", got)
	}
	if got := authority.Disconnect(leader); got != party.RefusalStaleSession {
		t.Fatalf("stale disconnect refusal = %s", got)
	}
	snapshot, refusal = authority.Membership(replacement.CharacterID)
	if refusal != party.AudienceAllowed || snapshot.LeaderID != first.CharacterID {
		t.Fatalf("reconnected membership = %+v, %s", snapshot, refusal)
	}
}

func TestAllOfflinePartySurvivesAndFirstReconnectBecomesLeader(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, member := actor(), actor()
	connect(t, authority, leader, member)
	form(t, authority, leader, member)
	if got := authority.Disconnect(leader); got != party.RefusalNone {
		t.Fatal(got)
	}
	if got := authority.Disconnect(member); got != party.RefusalNone {
		t.Fatal(got)
	}

	reconnected := member
	reconnected.SessionID = uuid.New()
	connect(t, authority, reconnected)
	snapshot, refusal := authority.Membership(reconnected.CharacterID)
	if refusal != party.AudienceAllowed || snapshot.LeaderID != member.CharacterID || len(snapshot.Members) != 2 {
		t.Fatalf("reconnected membership = %+v, %s", snapshot, refusal)
	}
}

func TestInvitationsFollowRetailSessionLifetime(t *testing.T) {
	t.Parallel()
	authority := party.New()
	inviter, invitee := actor(), actor()
	connect(t, authority, inviter, invitee)
	if got := authority.Invite(inviter, invitee.CharacterID); got != party.RefusalNone {
		t.Fatal(got)
	}
	if got := authority.Disconnect(inviter); got != party.RefusalNone {
		t.Fatal(got)
	}
	if got := authority.Accept(invitee); got != party.RefusalNoInvitation {
		t.Fatalf("accept after inviter loss refusal = %s", got)
	}

	inviter.SessionID = uuid.New()
	connect(t, authority, inviter)
	if got := authority.Invite(inviter, invitee.CharacterID); got != party.RefusalNone {
		t.Fatal(got)
	}
	invitee.SessionID = uuid.New()
	connect(t, authority, invitee)
	if got := authority.Accept(invitee); got != party.RefusalNoInvitation {
		t.Fatalf("accept after invitee reconnect refusal = %s", got)
	}
}

func TestAudienceAndMembershipReturnImmutableCopies(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, member := actor(), actor()
	connect(t, authority, leader, member)
	form(t, authority, leader, member)

	audience, refusal := authority.Audience(context.Background(), leader.CharacterID)
	if refusal != party.AudienceAllowed {
		t.Fatal(refusal)
	}
	audience[0] = uuid.Nil
	snapshot, refusal := authority.Membership(leader.CharacterID)
	if refusal != party.AudienceAllowed {
		t.Fatal(refusal)
	}
	snapshot.Members[0] = uuid.Nil

	again, refusal := authority.Audience(context.Background(), leader.CharacterID)
	if refusal != party.AudienceAllowed || again[0] != member.CharacterID {
		t.Fatalf("mutated audience leaked into authority: %v, %s", again, refusal)
	}
	againSnapshot, refusal := authority.Membership(leader.CharacterID)
	if refusal != party.AudienceAllowed || againSnapshot.Members[0] != leader.CharacterID {
		t.Fatalf("mutated snapshot leaked into authority: %+v, %s", againSnapshot, refusal)
	}
}

func TestDeleteCharacterDiffersFromDisconnectAndDeath(t *testing.T) {
	t.Parallel()
	authority := party.New()
	leader, member := actor(), actor()
	connect(t, authority, leader, member)
	form(t, authority, leader, member)

	// Combat death has no party mutation. The roster remains authoritative.
	before, refusal := authority.Membership(leader.CharacterID)
	if refusal != party.AudienceAllowed {
		t.Fatal(refusal)
	}
	after, refusal := authority.Membership(leader.CharacterID)
	if refusal != party.AudienceAllowed || !reflect.DeepEqual(after, before) {
		t.Fatalf("death-independent membership changed: before=%+v after=%+v", before, after)
	}

	authority.DeleteCharacter(member.CharacterID)
	if _, refusal := authority.Membership(leader.CharacterID); refusal != party.AudienceNoParty {
		t.Fatalf("leader after permanent member deletion refusal = %s", refusal)
	}
}

func TestAudienceRejectsSoloUnknownAndDisconnectedCharacters(t *testing.T) {
	t.Parallel()
	authority := party.New()
	solo := actor()
	connect(t, authority, solo)
	if got, refusal := authority.Audience(context.Background(), solo.CharacterID); refusal != party.AudienceNoParty || got != nil {
		t.Fatalf("solo audience = %v, %s", got, refusal)
	}
	if got, refusal := authority.Audience(context.Background(), uuid.New()); refusal != party.AudienceNotMember || got != nil {
		t.Fatalf("unknown audience = %v, %s", got, refusal)
	}
	if got := authority.Disconnect(solo); got != party.RefusalNone {
		t.Fatal(got)
	}
	if _, refusal := authority.Audience(context.Background(), solo.CharacterID); refusal != party.AudienceNotMember {
		t.Fatalf("disconnected audience refusal = %s", refusal)
	}
}

func TestAudienceHonoursCancelledRequest(t *testing.T) {
	t.Parallel()
	authority := party.New()
	connected := actor()
	connect(t, authority, connected)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, refusal := authority.Audience(ctx, connected.CharacterID); refusal != party.AudienceUnavailable || got != nil {
		t.Fatalf("cancelled audience = %v, %s", got, refusal)
	}
}

func TestConcurrentSessionAndAudienceOperationsAreRaceFree(t *testing.T) {
	authority := party.New()
	leader, member := actor(), actor()
	connect(t, authority, leader, member)
	form(t, authority, leader, member)

	const workers = 16
	const iterations = 500
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(index int) {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				_, _ = authority.Audience(context.Background(), leader.CharacterID)
				_, _ = authority.Membership(member.CharacterID)
				if index%4 == 0 {
					_ = authority.TransferLeadership(leader, leader.CharacterID)
				}
			}
		}(worker)
	}
	wait.Wait()
}

var (
	_ party.AudienceReader   = (*party.Authority)(nil)
	_ party.MembershipReader = (*party.Authority)(nil)
)
