package chataudience

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/visibility"
)

const (
	leagueFaction = "faction.league"
	empireFaction = "faction.empire"
	alliedFaction = "faction.allied"
)

type factionTable map[string]gametypes.Faction

func (table factionTable) Faction(id string) (gametypes.Faction, bool) {
	faction, ok := table[id]
	return faction, ok
}

func testFactions() factionTable {
	return factionTable{
		leagueFaction: {
			ID:            leagueFaction,
			NameKey:       "loc.faction.league",
			DefaultStance: gametypes.StanceHostile,
			Relations: []gametypes.FactionRelation{
				{FactionID: leagueFaction, Stance: gametypes.StanceFriendly},
				{FactionID: alliedFaction, Stance: gametypes.StanceFriendly},
			},
		},
		alliedFaction: {
			ID:            alliedFaction,
			NameKey:       "loc.faction.allied",
			DefaultStance: gametypes.StanceHostile,
			Relations: []gametypes.FactionRelation{
				{FactionID: alliedFaction, Stance: gametypes.StanceFriendly},
			},
		},
		empireFaction: {
			ID:            empireFaction,
			NameKey:       "loc.faction.empire",
			DefaultStance: gametypes.StanceHostile,
			Relations: []gametypes.FactionRelation{
				{FactionID: empireFaction, Stance: gametypes.StanceFriendly},
			},
		},
	}
}

type testAvatar struct {
	avatar   Avatar
	position gametypes.Vec3
	faction  string
}

func newTestAvatar(entityID uint64, position gametypes.Vec3, faction string) *testAvatar {
	current := &testAvatar{position: position, faction: faction}
	current.avatar = Avatar{
		CharacterID: uuid.MustParse("00000000-0000-0000-0000-" + formatEntityID(entityID)),
		EntityID:    entityID,
		ZoneID:      "zone.one",
		ScannerID:   "scanner.one",
		MissionID:   "mission.one",
		Observe: func() (Observation, bool) {
			return Observation{Position: current.position, FactionID: current.faction}, true
		},
	}
	return current
}

func formatEntityID(entityID uint64) string {
	const digits = "000000000000"
	text := fmtUint(entityID)
	return digits[:len(digits)-len(text)] + text
}

func fmtUint(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}

func TestAudienceEnforcesEveryRecoveredSayRule(t *testing.T) {
	influence := visibility.NewInfluence()
	ignores := social.NewIgnoreLists()
	authority, err := New(testFactions(), influence, ignores)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	speaker := newTestAvatar(1, gametypes.Vec3{}, leagueFaction)
	mustAdmit(t, authority, speaker.avatar)

	friendAtEdge := newTestAvatar(2, gametypes.Vec3{X: 60, Y: 80}, leagueFaction)
	enemyInside := newTestAvatar(3, gametypes.Vec3{X: 99, Z: 1}, empireFaction)
	asymmetricFriend := newTestAvatar(10, gametypes.Vec3{X: 2}, alliedFaction)
	overEdge := newTestAvatar(4, gametypes.Vec3{X: 60, Y: 80, Z: 0.01}, leagueFaction)
	otherScanner := newTestAvatar(5, gametypes.Vec3{X: 1}, leagueFaction)
	otherScanner.avatar.ScannerID = "scanner.two"
	otherMission := newTestAvatar(6, gametypes.Vec3{X: 1}, leagueFaction)
	otherMission.avatar.MissionID = "mission.two"
	otherZone := newTestAvatar(7, gametypes.Vec3{X: 1}, leagueFaction)
	otherZone.avatar.ZoneID = "zone.two"
	ignored := newTestAvatar(8, gametypes.Vec3{X: 1}, leagueFaction)
	noInfluence := newTestAvatar(9, gametypes.Vec3{X: 1}, leagueFaction)
	for _, current := range []*testAvatar{
		friendAtEdge, enemyInside, asymmetricFriend, overEdge, otherScanner, otherMission, otherZone, ignored, noInfluence,
	} {
		mustAdmit(t, authority, current.avatar)
		influence.SetCanInfluence(speaker.avatar.EntityID, current.avatar.EntityID, true)
	}
	influence.SetCanInfluence(speaker.avatar.EntityID, noInfluence.avatar.EntityID, false)
	ignores.Set(ignored.avatar.CharacterID, speaker.avatar.CharacterID, true)

	got, err := authority.Audience(context.Background(), saySpeaker(speaker.avatar, speaker.position))
	if err != nil {
		t.Fatalf("Audience() error = %v", err)
	}
	want := []chat.SayRecipient{
		{CharacterID: friendAtEdge.avatar.CharacterID},
		{
			CharacterID:                     enemyInside.avatar.CharacterID,
			UnreadableFactionLocalizationID: "loc.faction.league",
		},
		{CharacterID: asymmetricFriend.avatar.CharacterID},
	}
	if len(got) != len(want) {
		t.Fatalf("Audience() = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("Audience()[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestAudienceAuthenticatesSpeakerAndFailsClosed(t *testing.T) {
	authority, err := New(testFactions(), visibility.NewInfluence(), social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	speaker := newTestAvatar(1, gametypes.Vec3{}, leagueFaction)
	session := mustAdmit(t, authority, speaker.avatar)

	tests := []struct {
		name    string
		speaker chat.SaySpeaker
	}{
		{name: "unknown character", speaker: saySpeaker(newTestAvatar(20, gametypes.Vec3{}, leagueFaction).avatar, gametypes.Vec3{})},
		{name: "forged entity", speaker: chat.SaySpeaker{CharacterID: speaker.avatar.CharacterID, EntityID: 99, ZoneID: speaker.avatar.ZoneID}},
		{name: "forged zone", speaker: chat.SaySpeaker{CharacterID: speaker.avatar.CharacterID, EntityID: speaker.avatar.EntityID, ZoneID: "zone.other"}},
		{name: "nonfinite position", speaker: chat.SaySpeaker{CharacterID: speaker.avatar.CharacterID, EntityID: speaker.avatar.EntityID, ZoneID: speaker.avatar.ZoneID, Position: chat.Position{X: float32(math.NaN())}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := authority.Audience(context.Background(), test.speaker); !errors.Is(err, ErrInvalidAvatar) {
				t.Fatalf("Audience() error = %v, want ErrInvalidAvatar", err)
			}
		})
	}
	session.Close()
	if _, err := authority.Audience(context.Background(), saySpeaker(speaker.avatar, speaker.position)); !errors.Is(err, ErrInvalidAvatar) {
		t.Fatalf("Audience(after close) error = %v, want ErrInvalidAvatar", err)
	}
}

func TestAudienceRejectsMissingFactionData(t *testing.T) {
	influence := visibility.NewInfluence()
	authority, err := New(testFactions(), influence, social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	speaker := newTestAvatar(1, gametypes.Vec3{}, leagueFaction)
	unknown := newTestAvatar(2, gametypes.Vec3{X: 1}, "faction.missing")
	mustAdmit(t, authority, speaker.avatar)
	mustAdmit(t, authority, unknown.avatar)
	influence.SetCanInfluence(1, 2, true)
	if _, err := authority.Audience(context.Background(), saySpeaker(speaker.avatar, speaker.position)); !errors.Is(err, ErrUnknownFaction) {
		t.Fatalf("Audience() error = %v, want ErrUnknownFaction", err)
	}
}

func TestAudienceScopeUpdateIsAtomicWithConcurrentQueries(t *testing.T) {
	influence := visibility.NewInfluence()
	authority, err := New(testFactions(), influence, social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	speaker := newTestAvatar(1, gametypes.Vec3{}, leagueFaction)
	recipient := newTestAvatar(2, gametypes.Vec3{X: 1}, leagueFaction)
	mustAdmit(t, authority, speaker.avatar)
	session := mustAdmit(t, authority, recipient.avatar)
	influence.SetCanInfluence(1, 2, true)

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(offset int) {
			defer group.Done()
			for index := 0; index < 1000; index++ {
				mission := "mission.one"
				if (index+offset)%2 == 0 {
					mission = "mission.two"
				}
				_ = session.UpdateScope("scanner.one", mission)
				_, _ = authority.Audience(context.Background(), saySpeaker(speaker.avatar, speaker.position))
			}
		}(worker)
	}
	group.Wait()
}

func TestNewAndAdmitRejectIncompleteAuthority(t *testing.T) {
	if _, err := New(nil, visibility.NewInfluence(), social.NewIgnoreLists()); !errors.Is(err, ErrMissingSource) {
		t.Fatalf("New(nil factions) error = %v, want ErrMissingSource", err)
	}
	authority, err := New(testFactions(), visibility.NewInfluence(), social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := authority.Admit(Avatar{}); !errors.Is(err, ErrInvalidAvatar) {
		t.Fatalf("Admit(empty) error = %v, want ErrInvalidAvatar", err)
	}
}

func mustAdmit(t *testing.T, authority *Authority, avatar Avatar) *Session {
	t.Helper()
	session, err := authority.Admit(avatar)
	if err != nil {
		t.Fatalf("Admit(%s) error = %v", avatar.CharacterID, err)
	}
	return session
}

func saySpeaker(avatar Avatar, position gametypes.Vec3) chat.SaySpeaker {
	return chat.SaySpeaker{
		CharacterID: avatar.CharacterID,
		EntityID:    avatar.EntityID,
		ZoneID:      avatar.ZoneID,
		Position:    chat.Position{X: position.X, Y: position.Y, Z: position.Z},
	}
}
