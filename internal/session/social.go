package session

import (
	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/social"
)

// socialFriendsSink shares the session's ordered reliable event queue. Friend
// replacements and chat deliveries cannot race two independent writers on the
// same stream.
type socialFriendsSink struct {
	events *chatSender
}

func (sink socialFriendsSink) OfferFriends(replacement social.FriendsReplacement) {
	if sink.events != nil {
		sink.events.offer(socialFriendsReplacementMessage(replacement))
	}
}

func socialFriendsReplacementMessage(replacement social.FriendsReplacement) *sarnautv1.ServerMessage {
	wire := &sarnautv1.SocialFriendsReplacement{Revision: replacement.Revision}
	for _, friend := range replacement.Friends {
		wire.Friends = append(wire.Friends, &sarnautv1.SocialFriend{
			CharacterId: friend.CharacterID.String(),
			DisplayName: friend.DisplayName,
		})
	}
	return &sarnautv1.ServerMessage{
		Payload: &sarnautv1.ServerMessage_SocialFriendsReplacement{
			SocialFriendsReplacement: wire,
		},
	}
}
