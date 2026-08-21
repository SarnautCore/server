// Package party owns retail group membership and leadership.
//
// The retail 1.1 group is shard-wide and session-aware. A disconnect clears a
// member's live address but does not remove the member. Death and zone changes
// do not touch the group. Explicit leave and kick do, and a non-raid group that
// falls to one member is dissolved. The shipped SocialRoot.xdb sets the member
// cap to six.
//
// Wire adapters must derive Actor from the authenticated session. CharacterID
// is never accepted from an untrusted command payload. Readers use Audience,
// which returns a copy of the connected member IDs and cannot expose mutable
// authority state to chat or quest callers.
package party

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// MaxMembers is Mechanics/GameRoot/SocialRoot.xdb maxGroupSize in retail 1.1.
const MaxMembers = 6

// Actor identifies one authenticated character session. SessionID prevents a
// late teardown from an evicted connection from disconnecting its replacement.
type Actor struct {
	CharacterID uuid.UUID
	SessionID   uuid.UUID
}

// AudienceRefusal is the ordinary-play result of resolving a group audience.
type AudienceRefusal uint8

const (
	AudienceAllowed AudienceRefusal = iota
	// AudienceNoParty means the authenticated character is connected but solo.
	AudienceNoParty
	// AudienceNotMember means the character has no current authenticated session.
	AudienceNotMember
	// AudienceUnavailable means the caller's request ended before resolution.
	AudienceUnavailable
)

func (refusal AudienceRefusal) String() string {
	switch refusal {
	case AudienceAllowed:
		return "allowed"
	case AudienceNoParty:
		return "no_party"
	case AudienceNotMember:
		return "not_member"
	case AudienceUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// Refusal names why a party mutation did not happen.
type Refusal uint8

const (
	RefusalNone Refusal = iota
	RefusalUnauthenticated
	RefusalStaleSession
	RefusalTargetUnavailable
	RefusalSelfInvite
	RefusalAlreadyGrouped
	RefusalInviteeBusy
	RefusalNotLeader
	RefusalPartyFull
	RefusalNoInvitation
	RefusalNoParty
	RefusalCannotKickSelf
	RefusalTargetNotMember
)

func (refusal Refusal) String() string {
	switch refusal {
	case RefusalNone:
		return "none"
	case RefusalUnauthenticated:
		return "unauthenticated"
	case RefusalStaleSession:
		return "stale_session"
	case RefusalTargetUnavailable:
		return "target_unavailable"
	case RefusalSelfInvite:
		return "self_invite"
	case RefusalAlreadyGrouped:
		return "already_grouped"
	case RefusalInviteeBusy:
		return "invitee_busy"
	case RefusalNotLeader:
		return "not_leader"
	case RefusalPartyFull:
		return "party_full"
	case RefusalNoInvitation:
		return "no_invitation"
	case RefusalNoParty:
		return "no_party"
	case RefusalCannotKickSelf:
		return "cannot_kick_self"
	case RefusalTargetNotMember:
		return "target_not_member"
	default:
		return "unknown"
	}
}

// Snapshot is an immutable view of one party. Members retain retail insertion
// order, which also makes leader election deterministic.
type Snapshot struct {
	ID       uuid.UUID
	LeaderID uuid.UUID
	Members  []uuid.UUID
}

// AudienceReader is the narrow seam used by group chat and quest sharing.
// The caller supplies the character ID already authenticated by its session.
type AudienceReader interface {
	Audience(ctx context.Context, characterID uuid.UUID) ([]uuid.UUID, AudienceRefusal)
}

// MembershipReader exposes the complete roster where a caller needs
// leadership or offline-member information. The returned slice is a copy.
type MembershipReader interface {
	Membership(characterID uuid.UUID) (Snapshot, AudienceRefusal)
}

type invitation struct {
	inviter uuid.UUID
	invitee uuid.UUID
}

type group struct {
	id       uuid.UUID
	leaderID uuid.UUID
	members  []uuid.UUID
}

// Authority serializes group mutations and returns copied read models. It is
// safe for shard sessions in different zones to call concurrently.
type Authority struct {
	mu sync.RWMutex

	sessions    map[uuid.UUID]uuid.UUID
	groups      map[uuid.UUID]*group
	membership  map[uuid.UUID]uuid.UUID
	invitations map[uuid.UUID]invitation
}

// New constructs an empty authority.
func New() *Authority {
	return &Authority{
		sessions:    make(map[uuid.UUID]uuid.UUID),
		groups:      make(map[uuid.UUID]*group),
		membership:  make(map[uuid.UUID]uuid.UUID),
		invitations: make(map[uuid.UUID]invitation),
	}
}

// Connect records the session that currently authenticates a character.
// Replacing a live session models retail address loss and cancels invitations
// authored by that address. Entering also clears any old incoming invitation.
func (authority *Authority) Connect(actor Actor) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if previous, exists := authority.sessions[actor.CharacterID]; exists && previous != actor.SessionID {
		authority.cancelInvitationsFrom(actor.CharacterID)
	}
	authority.sessions[actor.CharacterID] = actor.SessionID
	delete(authority.invitations, actor.CharacterID)

	if held := authority.groupFor(actor.CharacterID); held != nil && held.leaderID == uuid.Nil {
		held.leaderID = actor.CharacterID
	}
	return RefusalNone
}

// Disconnect clears live presence for the exact session. Membership survives.
// A stale session close cannot disconnect a replacement session.
func (authority *Authority) Disconnect(actor Actor) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if authority.sessions[actor.CharacterID] != actor.SessionID {
		return RefusalStaleSession
	}
	delete(authority.sessions, actor.CharacterID)
	authority.cancelInvitationsFrom(actor.CharacterID)

	held := authority.groupFor(actor.CharacterID)
	if held != nil && held.leaderID == actor.CharacterID {
		held.leaderID = authority.firstConnected(held)
	}
	return RefusalNone
}

// Invite records one pending invitation for a connected target. Retail keeps
// no invitation timer here. An invitation lasts until response, a conflicting
// login, or inviter address loss.
func (authority *Authority) Invite(actor Actor, targetID uuid.UUID) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	if targetID == uuid.Nil || !authority.connected(targetID) {
		return RefusalTargetUnavailable
	}
	if targetID == actor.CharacterID {
		return RefusalSelfInvite
	}
	if authority.groupFor(targetID) != nil {
		return RefusalAlreadyGrouped
	}
	if _, busy := authority.invitations[targetID]; busy {
		return RefusalInviteeBusy
	}
	if held := authority.groupFor(actor.CharacterID); held != nil {
		if held.leaderID != actor.CharacterID {
			return RefusalNotLeader
		}
		if len(held.members) >= MaxMembers {
			return RefusalPartyFull
		}
	}
	authority.invitations[targetID] = invitation{inviter: actor.CharacterID, invitee: targetID}
	return RefusalNone
}

// Accept consumes the authenticated character's pending invitation. The
// invitation is consumed even if leadership or capacity changed after it was
// sent, matching retail CrowdService.accept.
func (authority *Authority) Accept(actor Actor) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	invite, ok := authority.invitations[actor.CharacterID]
	if !ok {
		return RefusalNoInvitation
	}
	delete(authority.invitations, actor.CharacterID)

	if authority.groupFor(actor.CharacterID) != nil {
		return RefusalAlreadyGrouped
	}
	if !authority.connected(invite.inviter) {
		return RefusalNoInvitation
	}
	held := authority.groupFor(invite.inviter)
	if held == nil {
		held = &group{
			id:       uuid.New(),
			leaderID: invite.inviter,
			members:  []uuid.UUID{invite.inviter, invite.invitee},
		}
		authority.groups[held.id] = held
		authority.membership[invite.inviter] = held.id
		authority.membership[invite.invitee] = held.id
		return RefusalNone
	}
	if held.leaderID != invite.inviter {
		return RefusalNotLeader
	}
	if len(held.members) >= MaxMembers {
		return RefusalPartyFull
	}
	held.members = append(held.members, invite.invitee)
	authority.membership[invite.invitee] = held.id
	return RefusalNone
}

// Decline consumes the authenticated character's pending invitation.
func (authority *Authority) Decline(actor Actor) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	if _, ok := authority.invitations[actor.CharacterID]; !ok {
		return RefusalNoInvitation
	}
	delete(authority.invitations, actor.CharacterID)
	return RefusalNone
}

// Leave removes the authenticated character. Reducing a party to one member
// dissolves it and returns the remaining character to solo play.
func (authority *Authority) Leave(actor Actor) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	if authority.groupFor(actor.CharacterID) == nil {
		return RefusalNoParty
	}
	authority.removeMember(actor.CharacterID)
	authority.cancelInvitationsFrom(actor.CharacterID)
	return RefusalNone
}

// Kick removes targetID. Only the connected leader can kick and retail refuses
// the leader's attempt to kick itself as a distinct operation from leave.
func (authority *Authority) Kick(actor Actor, targetID uuid.UUID) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	held := authority.groupFor(actor.CharacterID)
	if held == nil {
		return RefusalNoParty
	}
	if targetID == actor.CharacterID {
		return RefusalCannotKickSelf
	}
	if held.leaderID != actor.CharacterID {
		return RefusalNotLeader
	}
	if authority.membership[targetID] != held.id {
		return RefusalTargetNotMember
	}
	authority.removeMember(targetID)
	authority.cancelInvitationsFrom(targetID)
	return RefusalNone
}

// TransferLeadership assigns leadership to any current member, including an
// offline member. Retail validates membership, not connectivity.
func (authority *Authority) TransferLeadership(actor Actor, targetID uuid.UUID) Refusal {
	if !validActor(actor) {
		return RefusalUnauthenticated
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if refusal := authority.authenticate(actor); refusal != RefusalNone {
		return refusal
	}
	held := authority.groupFor(actor.CharacterID)
	if held == nil {
		return RefusalNoParty
	}
	if held.leaderID != actor.CharacterID {
		return RefusalNotLeader
	}
	if authority.membership[targetID] != held.id {
		return RefusalTargetNotMember
	}
	held.leaderID = targetID
	return RefusalNone
}

// DeleteCharacter applies retail's permanent-avatar-removal rule. Unlike a
// disconnect or death, deletion removes membership and pending invitations.
func (authority *Authority) DeleteCharacter(characterID uuid.UUID) {
	if characterID == uuid.Nil {
		return
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()

	delete(authority.sessions, characterID)
	delete(authority.invitations, characterID)
	authority.cancelInvitationsFrom(characterID)
	if authority.groupFor(characterID) != nil {
		authority.removeMember(characterID)
	}
}

// Audience returns every other connected party member in retail insertion
// order. Retail clients echo the sender's group chat locally, so remote chat
// fan-out excludes it. Quest sharing starts from the same set and applies live
// range, death, and quest eligibility itself.
func (authority *Authority) Audience(ctx context.Context, characterID uuid.UUID) ([]uuid.UUID, AudienceRefusal) {
	if contextDone(ctx) {
		return nil, AudienceUnavailable
	}
	if authority == nil || characterID == uuid.Nil {
		return nil, AudienceNotMember
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()

	if !authority.connected(characterID) {
		return nil, AudienceNotMember
	}
	held := authority.groupFor(characterID)
	if held == nil {
		return nil, AudienceNoParty
	}
	members := make([]uuid.UUID, 0, len(held.members))
	for _, memberID := range held.members {
		if memberID != characterID && authority.connected(memberID) {
			members = append(members, memberID)
		}
	}
	if contextDone(ctx) {
		return nil, AudienceUnavailable
	}
	return members, AudienceAllowed
}

// Membership returns the complete roster for a connected member.
func (authority *Authority) Membership(characterID uuid.UUID) (Snapshot, AudienceRefusal) {
	if authority == nil || characterID == uuid.Nil {
		return Snapshot{}, AudienceNotMember
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()

	if !authority.connected(characterID) {
		return Snapshot{}, AudienceNotMember
	}
	held := authority.groupFor(characterID)
	if held == nil {
		return Snapshot{}, AudienceNoParty
	}
	return Snapshot{
		ID:       held.id,
		LeaderID: held.leaderID,
		Members:  append([]uuid.UUID(nil), held.members...),
	}, AudienceAllowed
}

func validActor(actor Actor) bool {
	return actor.CharacterID != uuid.Nil && actor.SessionID != uuid.Nil
}

func contextDone(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func (authority *Authority) authenticate(actor Actor) Refusal {
	sessionID, ok := authority.sessions[actor.CharacterID]
	if !ok {
		return RefusalUnauthenticated
	}
	if sessionID != actor.SessionID {
		return RefusalStaleSession
	}
	return RefusalNone
}

func (authority *Authority) connected(characterID uuid.UUID) bool {
	_, ok := authority.sessions[characterID]
	return ok
}

func (authority *Authority) groupFor(characterID uuid.UUID) *group {
	return authority.groups[authority.membership[characterID]]
}

func (authority *Authority) cancelInvitationsFrom(inviterID uuid.UUID) {
	for inviteeID, invite := range authority.invitations {
		if invite.inviter == inviterID {
			delete(authority.invitations, inviteeID)
		}
	}
}

func (authority *Authority) firstConnected(held *group) uuid.UUID {
	for _, memberID := range held.members {
		if authority.connected(memberID) {
			return memberID
		}
	}
	return uuid.Nil
}

func (authority *Authority) removeMember(characterID uuid.UUID) {
	held := authority.groupFor(characterID)
	if held == nil {
		return
	}
	for index, memberID := range held.members {
		if memberID == characterID {
			held.members = append(held.members[:index], held.members[index+1:]...)
			break
		}
	}
	delete(authority.membership, characterID)
	if held.leaderID == characterID {
		held.leaderID = authority.firstConnected(held)
	}
	if len(held.members) > 1 {
		return
	}
	for _, memberID := range held.members {
		delete(authority.membership, memberID)
	}
	delete(authority.groups, held.id)
}
