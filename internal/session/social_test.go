package session

import (
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/social"
)

func TestSocialFriendsReplacementMapsStableUUIDsCanonicalNamesAndRevision(t *testing.T) {
	t.Parallel()

	first := uuid.MustParse("019200f0-0000-7000-8000-00000000c0a1")
	second := uuid.MustParse("019200f0-0000-7000-8000-00000000c0a2")
	wire := socialFriendsReplacementMessage(social.FriendsReplacement{
		Revision: 42,
		Friends: []social.Friend{
			{CharacterID: first, DisplayName: "Éowyn"},
			{CharacterID: second, DisplayName: "Two  Spaces"},
		},
	}).GetSocialFriendsReplacement()
	if wire == nil || wire.GetRevision() != 42 || len(wire.GetFriends()) != 2 {
		t.Fatalf("wire replacement = %+v", wire)
	}
	if wire.GetFriends()[0].GetCharacterId() != first.String() || wire.GetFriends()[0].GetDisplayName() != "Éowyn" ||
		wire.GetFriends()[1].GetCharacterId() != second.String() || wire.GetFriends()[1].GetDisplayName() != "Two  Spaces" {
		t.Fatalf("wire friends = %+v, want stable identities and unmodified canonical names", wire.GetFriends())
	}
}
