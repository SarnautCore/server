package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/inventory"
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
	stored map[uuid.UUID]charstore.Snapshot
	// template is what a character with no stored state materializes from,
	// standing in for the pack's chargen row.
	template charstore.Snapshot
	// loadError, when set, fails every load.
	loadError error
	// full makes every checkpoint report a dropped save, as a full bounded
	// queue does.
	full bool

	checkpoints []charstore.Snapshot
	loads       int
}

func newFakeCharacters(template charstore.Snapshot) *fakeCharacters {
	return &fakeCharacters{stored: make(map[uuid.UUID]charstore.Snapshot), template: template}
}

func (characters *fakeCharacters) Load(
	_ context.Context,
	characterID uuid.UUID,
	_ string,
	zoneID string,
) (charstore.Snapshot, error) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	characters.loads++
	if characters.loadError != nil {
		return charstore.Snapshot{}, characters.loadError
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

func (characters *fakeCharacters) Checkpoint(snapshot charstore.Snapshot) bool {
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

// UpdateInventory gives session command tests the same atomic seam used by
// charstore.InventoryService in the shard composition. Keeping it on the
// character fake means zone admission and inventory moves observe one stored
// snapshot instead of two unrelated in-memory repositories.
func (characters *fakeCharacters) UpdateInventory(
	ctx context.Context,
	characterID uuid.UUID,
	update func(inventory.MoveState) (inventory.MoveState, error),
) (inventory.MoveState, error) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return inventory.MoveState{}, err
	}
	if update == nil {
		return inventory.MoveState{}, fmt.Errorf("session fake: inventory update callback is required")
	}
	snapshot, ok := characters.stored[characterID]
	if !ok {
		return inventory.MoveState{}, charstore.ErrNotFound
	}
	if snapshot.HUD == nil {
		return inventory.MoveState{}, fmt.Errorf("session fake: character has no HUD state")
	}
	partitions := make([]int32, len(snapshot.HUD.BagLayout.Partitions))
	for index, partition := range snapshot.HUD.BagLayout.Partitions {
		partitions[index] = partition.Capacity
	}
	layout := inventory.BagLayout{
		ID:         inventory.BagLayoutID(snapshot.HUD.BagLayout.LayoutID),
		Partitions: partitions,
	}
	current := inventory.MoveState{
		Items:   append([]inventory.InventoryItem(nil), snapshot.Inventory...),
		SaveSeq: snapshot.State.SaveSeq,
		Layout:  layout,
	}
	replacement, err := update(current)
	if err != nil {
		return inventory.MoveState{}, err
	}
	if replacement.SaveSeq != current.SaveSeq+1 {
		return inventory.MoveState{}, fmt.Errorf(
			"session fake: inventory save sequence %d did not advance %d by one",
			replacement.SaveSeq,
			current.SaveSeq,
		)
	}
	if replacement.Layout.ID != layout.ID || len(replacement.Layout.Partitions) != len(layout.Partitions) {
		return inventory.MoveState{}, fmt.Errorf("session fake: inventory move changed bag layout")
	}
	for index := range layout.Partitions {
		if replacement.Layout.Partitions[index] != layout.Partitions[index] {
			return inventory.MoveState{}, fmt.Errorf("session fake: inventory move changed bag layout")
		}
	}
	snapshot.Inventory = append([]charstore.InventoryItem(nil), replacement.Items...)
	snapshot.State.SaveSeq = replacement.SaveSeq
	characters.stored[characterID] = snapshot
	return inventory.MoveState{
		Items:   append([]inventory.InventoryItem(nil), replacement.Items...),
		SaveSeq: replacement.SaveSeq,
		Layout: inventory.BagLayout{
			ID:         layout.ID,
			Partitions: append([]int32(nil), layout.Partitions...),
		},
	}, nil
}

func (characters *fakeCharacters) saved(characterID uuid.UUID) (charstore.Snapshot, bool) {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	snapshot, ok := characters.stored[characterID]
	return snapshot, ok
}

func (characters *fakeCharacters) written() []charstore.Snapshot {
	characters.mu.Lock()
	defer characters.mu.Unlock()
	return append([]charstore.Snapshot(nil), characters.checkpoints...)
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
func testTemplate(position charstore.Vec3) charstore.Snapshot {
	actions := charstore.EmptyOrderedActionSlots()
	abilityID := "ability.melee.harbor-cleave"
	actions[0].AbilityID = &abilityID
	return charstore.Snapshot{
		State: charstore.CharacterState{
			Position: position,
			Level:    1,
			Health:   100,
		},
		Inventory: []charstore.InventoryItem{{Slot: 0, InstanceID: 1, ItemID: "item.consumable.harbor-tonic", Quantity: 3}},
		Quests:    []charstore.QuestState{{QuestID: "quest.paper-harbor.mossy-gate", State: "offered"}},
		HUD: &charstore.CharacterHUDState{
			Bag: &charstore.ItemInstance{InstanceID: 2, ItemID: "item.bag.test-16", Quantity: 1},
			BagLayout: charstore.ProductBagLayout{
				LayoutID:   "bag.layout.16",
				Partitions: []charstore.BagPartition{{Ordinal: 0, Capacity: 16}},
			},
			Stats:   charstore.EmptyOrderedStats(),
			Actions: actions,
		},
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
