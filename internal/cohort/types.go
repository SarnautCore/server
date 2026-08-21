// Package cohort owns durable guild and raid membership plus live-session
// filtering. It does not know about chat messages or transports.
package cohort

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("cohort: membership not found")
	ErrInvalidRoster = errors.New("cohort: invalid roster")
)

// GuildMemberRight keeps the two recovered Allods 1.1 right ordinals used by
// chat. Other guild gameplay rights stay out of this package until recovered.
type GuildMemberRight uint8

const (
	GMRChat GuildMemberRight = iota
	GMROfficerChat
)

// GuildRights is a compact set of recovered guild rights.
type GuildRights uint8

const knownGuildRights GuildRights = 1<<GMRChat | 1<<GMROfficerChat

// Rights constructs a validated guild-right set.
func Rights(rights ...GuildMemberRight) (GuildRights, error) {
	var result GuildRights
	for _, right := range rights {
		if right > GMROfficerChat {
			return 0, fmt.Errorf("%w: unknown guild right %d", ErrInvalidRoster, right)
		}
		result |= 1 << right
	}
	return result, nil
}

// MustRights is intended for static configuration and tests.
func MustRights(rights ...GuildMemberRight) GuildRights {
	result, err := Rights(rights...)
	if err != nil {
		panic(err)
	}
	return result
}

// Has reports whether this set grants right.
func (rights GuildRights) Has(right GuildMemberRight) bool {
	return right <= GMROfficerChat && rights&(1<<right) != 0
}

func (rights GuildRights) valid() bool {
	return rights&^knownGuildRights == 0
}

type GuildMember struct {
	CharacterID uuid.UUID
	Rights      GuildRights
}

type GuildRoster struct {
	GuildID uuid.UUID
	Members []GuildMember
}

type RaidRoster struct {
	RaidID  uuid.UUID
	Members []uuid.UUID
}

// Refusal is a normal authoritative outcome, not an infrastructure error.
type Refusal uint8

const (
	Allowed Refusal = iota
	NotMember
	NotAuthorized
)

func (refusal Refusal) String() string {
	switch refusal {
	case Allowed:
		return "allowed"
	case NotMember:
		return "not_member"
	case NotAuthorized:
		return "not_authorized"
	default:
		return fmt.Sprintf("refusal(%d)", refusal)
	}
}

// Audience is an immutable snapshot of connected recipients. The sender is
// never present in RecipientCharacterIDs.
type Audience struct {
	RecipientCharacterIDs []uuid.UUID
	Refusal               Refusal
}

func validateGuild(roster GuildRoster) error {
	if roster.GuildID == uuid.Nil {
		return fmt.Errorf("%w: nil guild ID", ErrInvalidRoster)
	}
	seen := make(map[uuid.UUID]struct{}, len(roster.Members))
	for _, member := range roster.Members {
		if member.CharacterID == uuid.Nil {
			return fmt.Errorf("%w: nil guild member ID", ErrInvalidRoster)
		}
		if !member.Rights.valid() {
			return fmt.Errorf("%w: unknown rights %#x", ErrInvalidRoster, member.Rights)
		}
		if _, duplicate := seen[member.CharacterID]; duplicate {
			return fmt.Errorf("%w: duplicate guild member %s", ErrInvalidRoster, member.CharacterID)
		}
		seen[member.CharacterID] = struct{}{}
	}
	return nil
}

func validateRaid(roster RaidRoster) error {
	if roster.RaidID == uuid.Nil {
		return fmt.Errorf("%w: nil raid ID", ErrInvalidRoster)
	}
	seen := make(map[uuid.UUID]struct{}, len(roster.Members))
	for _, memberID := range roster.Members {
		if memberID == uuid.Nil {
			return fmt.Errorf("%w: nil raid member ID", ErrInvalidRoster)
		}
		if _, duplicate := seen[memberID]; duplicate {
			return fmt.Errorf("%w: duplicate raid member %s", ErrInvalidRoster, memberID)
		}
		seen[memberID] = struct{}{}
	}
	return nil
}

func cloneGuild(roster GuildRoster) GuildRoster {
	copyOfMembers := append([]GuildMember(nil), roster.Members...)
	return GuildRoster{GuildID: roster.GuildID, Members: copyOfMembers}
}

func cloneRaid(roster RaidRoster) RaidRoster {
	copyOfMembers := append([]uuid.UUID(nil), roster.Members...)
	return RaidRoster{RaidID: roster.RaidID, Members: copyOfMembers}
}
