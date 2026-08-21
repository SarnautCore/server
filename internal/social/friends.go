package social

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

var (
	ErrInvalidFriendOwner = errors.New("social friends: owner character id is required")
	ErrInvalidFriend      = errors.New("social friends: friend must be a distinct known character")
	ErrFriendRevision     = errors.New("social friends: revision exhausted")
)

// Friend is one canonical durable friend identity. DisplayName is copied from
// the authoritative character directory and is never accepted from a client.
type Friend struct {
	CharacterID uuid.UUID
	DisplayName string
}

// FriendsReplacement is a complete replacement, not a delta. Revision is
// monotonic for one owner and lets clients ignore an older concurrent send.
type FriendsReplacement struct {
	Revision uint64
	Friends  []Friend
}

// FriendsSink accepts full replacements after the authority lock is released.
type FriendsSink interface {
	OfferFriends(FriendsReplacement)
}

// FriendRepository owns durable relationships, canonical name resolution and
// revision allocation. Replace must commit all three atomically.
type FriendRepository interface {
	Friends(context.Context, uuid.UUID) (FriendsReplacement, error)
	ReplaceFriends(context.Context, uuid.UUID, []uuid.UUID) (FriendsReplacement, error)
}

// Friends is the authoritative replacement publisher. Expected persistence
// failures are returned to the mutation caller; a joined session receives only
// committed snapshots.
type Friends struct {
	sequenceMu sync.Mutex
	mu         sync.Mutex
	repo       FriendRepository
	sessions   map[uuid.UUID]map[*FriendsSession]struct{}
}

// FriendsSession is one authenticated recipient subscription.
type FriendsSession struct {
	authority *Friends
	owner     uuid.UUID
	sink      FriendsSink

	deliveryMu sync.Mutex
	delivered  bool
	revision   uint64
	closeOnce  sync.Once
}

func NewFriends(repository FriendRepository) (*Friends, error) {
	if repository == nil {
		return nil, errors.New("social friends: repository is required")
	}
	return &Friends{
		repo:     repository,
		sessions: make(map[uuid.UUID]map[*FriendsSession]struct{}),
	}, nil
}

// Join binds a sink to an authenticated owner and publishes the current full
// snapshot. Concurrent replacements may supersede that initial snapshot, but
// the sink observes only increasing revisions.
func (friends *Friends) Join(
	ctx context.Context,
	owner uuid.UUID,
	sink FriendsSink,
) (*FriendsSession, error) {
	if friends == nil || owner == uuid.Nil || sink == nil {
		return nil, ErrInvalidFriendOwner
	}
	friends.sequenceMu.Lock()
	defer friends.sequenceMu.Unlock()
	replacement, err := friends.repo.Friends(ctx, owner)
	if err != nil {
		return nil, err
	}
	session := &FriendsSession{authority: friends, owner: owner, sink: sink}
	friends.mu.Lock()
	owned := friends.sessions[owner]
	if owned == nil {
		owned = make(map[*FriendsSession]struct{})
		friends.sessions[owner] = owned
	}
	owned[session] = struct{}{}
	friends.mu.Unlock()
	session.publish(replacement)
	return session, nil
}

// Replace commits one complete relationship set and publishes it to every
// live session authenticated as owner.
func (friends *Friends) Replace(
	ctx context.Context,
	owner uuid.UUID,
	friendIDs []uuid.UUID,
) (FriendsReplacement, error) {
	if friends == nil || owner == uuid.Nil {
		return FriendsReplacement{}, ErrInvalidFriendOwner
	}
	friends.sequenceMu.Lock()
	replacement, err := friends.repo.ReplaceFriends(ctx, owner, append([]uuid.UUID(nil), friendIDs...))
	if err != nil {
		friends.sequenceMu.Unlock()
		return FriendsReplacement{}, err
	}
	friends.mu.Lock()
	owned := make([]*FriendsSession, 0, len(friends.sessions[owner]))
	for session := range friends.sessions[owner] {
		owned = append(owned, session)
	}
	friends.mu.Unlock()
	friends.sequenceMu.Unlock()
	for _, session := range owned {
		session.publish(replacement)
	}
	return cloneFriends(replacement), nil
}

// Close is idempotent. A publish already in progress may finish before Close
// returns; none can begin afterward.
func (session *FriendsSession) Close() {
	if session == nil || session.authority == nil {
		return
	}
	session.closeOnce.Do(func() {
		session.authority.mu.Lock()
		delete(session.authority.sessions[session.owner], session)
		if len(session.authority.sessions[session.owner]) == 0 {
			delete(session.authority.sessions, session.owner)
		}
		session.authority.mu.Unlock()
		session.deliveryMu.Lock()
		session.sink = nil
		session.deliveryMu.Unlock()
	})
}

func (session *FriendsSession) publish(replacement FriendsReplacement) {
	session.deliveryMu.Lock()
	defer session.deliveryMu.Unlock()
	if session.sink == nil || (session.delivered && replacement.Revision <= session.revision) {
		return
	}
	session.sink.OfferFriends(cloneFriends(replacement))
	session.delivered = true
	session.revision = replacement.Revision
}

func validateFriendIDs(owner uuid.UUID, friendIDs []uuid.UUID) error {
	if owner == uuid.Nil {
		return ErrInvalidFriendOwner
	}
	seen := make(map[uuid.UUID]struct{}, len(friendIDs))
	for _, friendID := range friendIDs {
		if friendID == uuid.Nil || friendID == owner {
			return ErrInvalidFriend
		}
		if _, duplicate := seen[friendID]; duplicate {
			return ErrInvalidFriend
		}
		seen[friendID] = struct{}{}
	}
	return nil
}

func cloneFriends(replacement FriendsReplacement) FriendsReplacement {
	result := replacement
	result.Friends = append([]Friend(nil), replacement.Friends...)
	return result
}
