package chataudience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/social"
	"github.com/SarnautCore/server/internal/visibility"
	"github.com/SarnautCore/server/internal/world"
)

func TestWorldAdapterRoutesLivePlayersAndRejectsNPCs(t *testing.T) {
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "zone.one",
		TickInterval:     time.Second / 30,
		SnapshotInterval: 100 * time.Millisecond,
		MaxMoveSpeed:     8,
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	speakerEntity, speakerPosition := zone.JoinAt(world.Vec3{}, 0)
	recipientEntity, _ := zone.JoinAt(world.Vec3{X: 100}, 0)
	npcEntity := zone.SpawnNPC(world.NPCSpec{
		ContentID: "mob.rat", Faction: leagueFaction, MaxHealth: 1, Position: world.Vec3{X: 1},
	})
	if err := zone.Command(func(tick *world.Tick) error {
		tick.Entity(speakerEntity).Faction = leagueFaction
		tick.Entity(recipientEntity).Faction = leagueFaction
		return nil
	}); err != nil {
		t.Fatalf("set factions: %v", err)
	}

	influence := visibility.NewInfluence()
	authority, err := New(testFactions(), influence, social.NewIgnoreLists())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	speaker := WorldAvatar(uuid.New(), speakerEntity, "scanner.one", "mission.one", zone)
	recipient := WorldAvatar(uuid.New(), recipientEntity, "scanner.one", "mission.one", zone)
	mustAdmit(t, authority, speaker)
	mustAdmit(t, authority, recipient)
	if _, err := authority.Admit(WorldAvatar(uuid.New(), npcEntity, "scanner.one", "mission.one", zone)); !errors.Is(err, ErrInvalidAvatar) {
		t.Fatalf("Admit(NPC) error = %v, want ErrInvalidAvatar", err)
	}
	influence.SetCanInfluence(speakerEntity, recipientEntity, true)

	got, err := authority.Audience(context.Background(), saySpeaker(speaker, speakerPosition))
	if err != nil {
		t.Fatalf("Audience() error = %v", err)
	}
	if len(got) != 1 || got[0].CharacterID != recipient.CharacterID {
		t.Fatalf("Audience() = %+v, want live recipient at inclusive 100m edge", got)
	}

	if err := zone.Command(func(tick *world.Tick) error {
		tick.MoveTo(tick.Entity(recipientEntity), gametypes.Vec3{X: 100, Z: 1})
		return nil
	}); err != nil {
		t.Fatalf("move recipient: %v", err)
	}
	got, err = authority.Audience(context.Background(), saySpeaker(speaker, speakerPosition))
	if err != nil {
		t.Fatalf("Audience(after move) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Audience(after move) = %+v, want no recipients beyond 3D radius", got)
	}
}
