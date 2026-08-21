package session_test

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/session"
)

// The integration tests in this package drive the shard through its exported
// surface, so they cannot reach the richer fakes `package session` keeps for the
// unit tests. These two stubs are what an end-to-end test actually needs: a
// ticket that redeems once and a character that loads. Anything that asserts on
// refusal reasons, play-lock arbitration or checkpoint sequences belongs in the
// internal tests, which is where those fakes live.

// stubTicket is the ticket every test in this package presents.
const stubTicket = "sarnaut_tk_integration"

// stubAdmission is the one account and character these tests play as.
func stubAdmission() session.Admission {
	return session.Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000b001"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000d001"),
		CharacterName:   "Integration",
		ChargenOptionID: "chargen.league.warrior",
	}
}

// integrationTemplate is the fresh-character snapshot the stub store
// materializes from, standing in for the pack's chargen row.
func integrationTemplate(position charstore.Vec3) charstore.Snapshot {
	return charstore.Snapshot{
		State: charstore.CharacterState{
			Position: position,
			Level:    1,
			Health:   100,
		},
	}
}

// stubAuthority redeems stubTicket once and holds the play lock for whoever
// redeemed it. A replay finds nothing, because the real service does a GETDEL.
type stubAuthority struct {
	mu       sync.Mutex
	redeemed bool
}

func (authority *stubAuthority) RedeemTicket(_ context.Context, ticket string) (session.Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if ticket != stubTicket || authority.redeemed {
		return session.Admission{}, &session.Refusal{Reason: session.ReasonUnknownTicket}
	}
	authority.redeemed = true
	return stubAdmission(), nil
}

func (authority *stubAuthority) RenewPlayLock(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func (authority *stubAuthority) ReleasePlayLock(context.Context, uuid.UUID) error { return nil }

// stubCharacters materializes a character from a template on first load and
// keeps whatever the session last checkpointed.
type stubCharacters struct {
	mu       sync.Mutex
	template charstore.Snapshot
	stored   map[uuid.UUID]charstore.Snapshot
}

func newStubCharacters(template charstore.Snapshot) *stubCharacters {
	return &stubCharacters{template: template, stored: make(map[uuid.UUID]charstore.Snapshot)}
}

func (characters *stubCharacters) Load(
	_ context.Context,
	characterID uuid.UUID,
	_ string,
	zoneID string,
) (charstore.Snapshot, error) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	if snapshot, ok := characters.stored[characterID]; ok {
		return snapshot, nil
	}
	fresh := characters.template
	fresh.State.CharacterID = characterID
	fresh.State.ZoneID = zoneID
	fresh.State.SaveSeq = 1
	characters.stored[characterID] = fresh
	return fresh, nil
}

func (characters *stubCharacters) Checkpoint(snapshot charstore.Snapshot) bool {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	stored, ok := characters.stored[snapshot.State.CharacterID]
	if !ok || snapshot.State.SaveSeq > stored.State.SaveSeq {
		characters.stored[snapshot.State.CharacterID] = snapshot
	}
	return true
}
