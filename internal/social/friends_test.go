package social_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/social"
)

func TestFriendsPublishCanonicalFullReplacementsAndStopAfterClose(t *testing.T) {
	t.Parallel()

	owner, first, second := uuid.New(), uuid.New(), uuid.New()
	repository := social.NewMemoryFriendRepository(map[uuid.UUID]string{
		first:  "Éowyn",
		second: "Two  Spaces",
	})
	authority, err := social.NewFriends(repository)
	if err != nil {
		t.Fatal(err)
	}
	sink := newFriendSink()
	session, err := authority.Join(t.Context(), owner, sink)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.all(); len(got) != 1 || got[0].Revision != 0 || len(got[0].Friends) != 0 {
		t.Fatalf("initial replacements = %+v, want authoritative empty revision 0", got)
	}

	replacement, err := authority.Replace(t.Context(), owner, []uuid.UUID{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Revision != 1 || len(replacement.Friends) != 2 ||
		replacement.Friends[0].CharacterID != second || replacement.Friends[0].DisplayName != "Two  Spaces" ||
		replacement.Friends[1].DisplayName != "Éowyn" {
		t.Fatalf("replacement = %+v, want canonical names in relationship order", replacement)
	}

	session.Close()
	session.Close()
	if _, err := authority.Replace(t.Context(), owner, []uuid.UUID{first}); err != nil {
		t.Fatal(err)
	}
	if got := sink.all(); len(got) != 2 || got[1].Revision != 1 {
		t.Fatalf("closed sink replacements = %+v, want only revisions 0 and 1", got)
	}
}

func TestFriendsRejectForgedRelationshipsWithoutAdvancingRevision(t *testing.T) {
	t.Parallel()

	owner, known := uuid.New(), uuid.New()
	repository := social.NewMemoryFriendRepository(map[uuid.UUID]string{known: "Known"})
	authority, _ := social.NewFriends(repository)
	for _, relationships := range [][]uuid.UUID{
		{uuid.Nil},
		{owner},
		{known, known},
		{uuid.New()},
	} {
		if _, err := authority.Replace(t.Context(), owner, relationships); err == nil {
			t.Fatalf("Replace(%v) accepted", relationships)
		}
	}
	snapshot, err := repository.Friends(t.Context(), owner)
	if err != nil || snapshot.Revision != 0 || len(snapshot.Friends) != 0 {
		t.Fatalf("snapshot after rejects = %+v, %v", snapshot, err)
	}
}

func TestConcurrentFriendsReplacementsPublishOnlyIncreasingRevisions(t *testing.T) {
	t.Parallel()

	owner, friend := uuid.New(), uuid.New()
	repository := social.NewMemoryFriendRepository(map[uuid.UUID]string{friend: "Friend"})
	authority, _ := social.NewFriends(repository)
	sink := newFriendSink()
	session, err := authority.Join(t.Context(), owner, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	const replacements = 64
	var group sync.WaitGroup
	for index := 0; index < replacements; index++ {
		group.Add(1)
		go func(include bool) {
			defer group.Done()
			ids := []uuid.UUID(nil)
			if include {
				ids = []uuid.UUID{friend}
			}
			if _, err := authority.Replace(t.Context(), owner, ids); err != nil {
				t.Errorf("Replace() error = %v", err)
			}
		}(index%2 == 0)
	}
	group.Wait()

	deliveries := sink.all()
	for index := 1; index < len(deliveries); index++ {
		if deliveries[index].Revision <= deliveries[index-1].Revision {
			t.Fatalf("revisions are not increasing: %+v", deliveries)
		}
	}
	if got := deliveries[len(deliveries)-1].Revision; got != replacements {
		t.Fatalf("last revision = %d, want %d", got, replacements)
	}
}

type friendSink struct {
	mu           sync.Mutex
	replacements []social.FriendsReplacement
}

func newFriendSink() *friendSink { return new(friendSink) }

func (sink *friendSink) OfferFriends(replacement social.FriendsReplacement) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.replacements = append(sink.replacements, replacement)
}

func (sink *friendSink) all() []social.FriendsReplacement {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]social.FriendsReplacement(nil), sink.replacements...)
}
