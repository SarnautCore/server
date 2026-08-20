package session

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/store"
)

// fakeAuthority stands in for the auth service. It is deliberately literal
// about the ADR 0030 contract it is imitating: one redemption burns one ticket,
// a refusal carries a reason, and the play lock is a real piece of state rather
// than a method that always says yes.
type fakeAuthority struct {
	mu sync.Mutex
	// tickets maps a ticket string to the identity it names. A redeemed ticket
	// is deleted, so a replay finds nothing — the GETDEL the real service does.
	tickets map[string]Admission
	// refusals maps a ticket string to the reason it is refused, for the cases
	// that never reach the store: an expired ticket, a forged one.
	refusals map[string]string
	// lockedBy records the play lock holder per character. An empty holder
	// means the lock is free.
	lockedBy map[uuid.UUID]string
	// holder is this fake shard's instance id.
	holder string
	// unavailable makes every call fail as though NATS were down.
	unavailable bool

	redeemed int
	renewed  int
	released int
}

func newFakeAuthority() *fakeAuthority {
	return &fakeAuthority{
		tickets:  make(map[string]Admission),
		refusals: make(map[string]string),
		lockedBy: make(map[uuid.UUID]string),
		holder:   "shard-test",
	}
}

// mint records a ticket for an identity and returns the ticket string.
func (authority *fakeAuthority) mint(ticket string, admission Admission) string {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.tickets[ticket] = admission
	return ticket
}

// refuse records a ticket the service will decline with reason.
func (authority *fakeAuthority) refuse(ticket, reason string) string {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.refusals[ticket] = reason
	return ticket
}

func (authority *fakeAuthority) RedeemTicket(_ context.Context, ticket string) (Admission, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()

	if authority.unavailable {
		return Admission{}, &Refusal{Reason: ReasonAuthUnavailable}
	}
	if reason, refused := authority.refusals[ticket]; refused {
		return Admission{}, &Refusal{Reason: reason}
	}
	admission, ok := authority.tickets[ticket]
	if !ok {
		return Admission{}, &Refusal{Reason: ReasonUnknownTicket}
	}
	delete(authority.tickets, ticket)

	if holder, held := authority.lockedBy[admission.CharacterID]; held && holder != authority.holder {
		return Admission{}, &Refusal{Reason: ReasonPlayLockHeld}
	}
	authority.lockedBy[admission.CharacterID] = authority.holder
	authority.redeemed++
	return admission, nil
}

func (authority *fakeAuthority) RenewPlayLock(_ context.Context, characterID uuid.UUID) (bool, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.renewed++
	return authority.lockedBy[characterID] == authority.holder, nil
}

func (authority *fakeAuthority) ReleasePlayLock(_ context.Context, characterID uuid.UUID) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.released++
	if authority.lockedBy[characterID] == authority.holder {
		delete(authority.lockedBy, characterID)
	}
	return nil
}

func (authority *fakeAuthority) counts() (redeemed, renewed, released int) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.redeemed, authority.renewed, authority.released
}

// fakeCharacters is a [CharacterStore] over a map. It keeps the last snapshot
// written per character, which is what a reconnect test asserts against.
type fakeCharacters struct {
	mu sync.Mutex
	// stored is the persisted state per character.
	stored map[uuid.UUID]store.Snapshot
	// template is what a character with no stored state materializes from,
	// standing in for the pack's chargen row.
	template store.Snapshot
	// loadError, when set, fails every load.
	loadError error
	// full makes every checkpoint report a dropped save, as a full bounded
	// queue does.
	full bool

	checkpoints []store.Snapshot
	loads       int
}

func newFakeCharacters(template store.Snapshot) *fakeCharacters {
	return &fakeCharacters{stored: make(map[uuid.UUID]store.Snapshot), template: template}
}

func (characters *fakeCharacters) Load(
	_ context.Context,
	characterID uuid.UUID,
	_ string,
	zoneID string,
) (store.Snapshot, error) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	characters.loads++
	if characters.loadError != nil {
		return store.Snapshot{}, characters.loadError
	}
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

func (characters *fakeCharacters) Checkpoint(snapshot store.Snapshot) bool {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	if characters.full {
		return false
	}
	characters.checkpoints = append(characters.checkpoints, snapshot)
	if stored, ok := characters.stored[snapshot.State.CharacterID]; !ok || snapshot.State.SaveSeq > stored.State.SaveSeq {
		characters.stored[snapshot.State.CharacterID] = snapshot
	}
	return true
}

func (characters *fakeCharacters) saved(characterID uuid.UUID) (store.Snapshot, bool) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	snapshot, ok := characters.stored[characterID]
	return snapshot, ok
}

func (characters *fakeCharacters) written() []store.Snapshot {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	return append([]store.Snapshot(nil), characters.checkpoints...)
}

// waitForCheckpoint polls until at least count checkpoints have been written,
// so a test never sleeps for a fixed duration.
func (characters *fakeCharacters) waitForCheckpoint(count int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(characters.written()) >= count {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// testTemplate is the fresh-character snapshot the fakes materialize from. It
// mirrors what the chargen table gives a real first login.
func testTemplate(position store.Vec3) store.Snapshot {
	return store.Snapshot{
		State: store.CharacterState{
			Position: position,
			Level:    1,
			Health:   100,
		},
		Inventory: []store.InventoryItem{{Slot: 0, ItemID: "item.consumable.harbor-tonic", Quantity: 3}},
		Quests:    []store.QuestState{{QuestID: "quest.paper-harbor.mossy-gate", State: "offered"}},
	}
}

// testAdmission is one account with one character.
func testAdmission() Admission {
	return Admission{
		AccountID:       uuid.MustParse("019200f0-0000-7000-8000-00000000a001"),
		CharacterID:     uuid.MustParse("019200f0-0000-7000-8000-00000000c001"),
		CharacterName:   "Anne",
		ChargenOptionID: "chargen.league.warrior",
	}
}
