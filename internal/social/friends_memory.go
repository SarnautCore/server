package social

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/google/uuid"
)

// MemoryFriendRepository applies the same replacement and revision rules as
// PostgreSQL. CharacterNames is an authoritative test/development directory.
type MemoryFriendRepository struct {
	mu             sync.Mutex
	characterNames map[uuid.UUID]string
	sets           map[uuid.UUID]FriendsReplacement
}

func NewMemoryFriendRepository(characterNames map[uuid.UUID]string) *MemoryFriendRepository {
	names := make(map[uuid.UUID]string, len(characterNames))
	for characterID, name := range characterNames {
		names[characterID] = name
	}
	return &MemoryFriendRepository{
		characterNames: names,
		sets:           make(map[uuid.UUID]FriendsReplacement),
	}
}

func (repository *MemoryFriendRepository) Friends(
	ctx context.Context,
	owner uuid.UUID,
) (FriendsReplacement, error) {
	if err := ctx.Err(); err != nil {
		return FriendsReplacement{}, err
	}
	if owner == uuid.Nil {
		return FriendsReplacement{}, ErrInvalidFriendOwner
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return cloneFriends(repository.sets[owner]), nil
}

func (repository *MemoryFriendRepository) ReplaceFriends(
	ctx context.Context,
	owner uuid.UUID,
	friendIDs []uuid.UUID,
) (FriendsReplacement, error) {
	if err := ctx.Err(); err != nil {
		return FriendsReplacement{}, err
	}
	if err := validateFriendIDs(owner, friendIDs); err != nil {
		return FriendsReplacement{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	current := repository.sets[owner]
	if current.Revision == math.MaxInt64 {
		return FriendsReplacement{}, ErrFriendRevision
	}
	next := FriendsReplacement{Revision: current.Revision + 1, Friends: make([]Friend, 0, len(friendIDs))}
	for _, friendID := range friendIDs {
		name := repository.characterNames[friendID]
		if name == "" {
			return FriendsReplacement{}, fmt.Errorf("%w: %s", ErrInvalidFriend, friendID)
		}
		next.Friends = append(next.Friends, Friend{CharacterID: friendID, DisplayName: name})
	}
	repository.sets[owner] = cloneFriends(next)
	return next, nil
}

var _ FriendRepository = (*MemoryFriendRepository)(nil)
