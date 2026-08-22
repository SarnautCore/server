package charstore_test

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/world"
	"github.com/google/uuid"
)

// failingRepository is a [charstore.Repository] that fails the test on any call.
//
// It is the runtime half of ADR 0031 §7: the 30 Hz tick holds the zone mutex for
// its whole body, so a single 5 ms query there stalls movement for every player
// in the zone. Nothing the tick does is allowed to reach this type.
type failingRepository struct {
	t     *testing.T
	calls atomic.Int64
}

func (repository *failingRepository) fail(method string) {
	repository.calls.Add(1)
	repository.t.Errorf("world tick called store.Repository.%s; the tick must perform no database I/O", method)
}

func (repository *failingRepository) CreateAccount(context.Context, charstore.Account) error {
	repository.fail("CreateAccount")
	return nil
}

func (repository *failingRepository) AccountByID(context.Context, uuid.UUID) (charstore.Account, error) {
	repository.fail("AccountByID")
	return charstore.Account{}, nil
}

func (repository *failingRepository) AccountByEmail(context.Context, string) (charstore.Account, error) {
	repository.fail("AccountByEmail")
	return charstore.Account{}, nil
}

func (repository *failingRepository) CreateCharacter(context.Context, charstore.Character) error {
	repository.fail("CreateCharacter")
	return nil
}

func (repository *failingRepository) CharacterByID(context.Context, uuid.UUID) (charstore.Character, error) {
	repository.fail("CharacterByID")
	return charstore.Character{}, nil
}

func (repository *failingRepository) CharactersByAccount(context.Context, uuid.UUID) ([]charstore.Character, error) {
	repository.fail("CharactersByAccount")
	return nil, nil
}

func (repository *failingRepository) CharacterByNormalizedName(context.Context, string) (charstore.Character, error) {
	repository.fail("CharacterByNormalizedName")
	return charstore.Character{}, nil
}

func (repository *failingRepository) DeleteCharacter(context.Context, uuid.UUID, uuid.UUID) error {
	repository.fail("DeleteCharacter")
	return nil
}

func (repository *failingRepository) ReserveName(context.Context, charstore.NameReservation) error {
	repository.fail("ReserveName")
	return nil
}

func (repository *failingRepository) ReleaseNameReservation(context.Context, string, uuid.UUID) error {
	repository.fail("ReleaseNameReservation")
	return nil
}

func (repository *failingRepository) SaveCharacterState(context.Context, charstore.CharacterState) error {
	repository.fail("SaveCharacterState")
	return nil
}

func (repository *failingRepository) SaveCharacterCheckpoint(context.Context, charstore.CharacterState) error {
	repository.fail("SaveCharacterCheckpoint")
	return nil
}

func (repository *failingRepository) LoadCharacterState(context.Context, uuid.UUID) (charstore.CharacterState, error) {
	repository.fail("LoadCharacterState")
	return charstore.CharacterState{}, nil
}

func (repository *failingRepository) RecordProgressionGrant(
	context.Context,
	charstore.ProgressionGrant,
) (charstore.ProgressionGrant, bool, error) {
	repository.fail("RecordProgressionGrant")
	return charstore.ProgressionGrant{}, false, nil
}

func (repository *failingRepository) PutItem(context.Context, uuid.UUID, charstore.InventoryItem) error {
	repository.fail("PutItem")
	return nil
}

func (repository *failingRepository) MoveItem(context.Context, uuid.UUID, int32, int32) error {
	repository.fail("MoveItem")
	return nil
}

func (repository *failingRepository) ReplaceInventory(context.Context, uuid.UUID, []charstore.InventoryItem) error {
	repository.fail("ReplaceInventory")
	return nil
}

func (repository *failingRepository) LoadInventory(context.Context, uuid.UUID) ([]charstore.InventoryItem, error) {
	repository.fail("LoadInventory")
	return nil, nil
}

func (repository *failingRepository) UpsertQuestState(context.Context, uuid.UUID, charstore.QuestState) error {
	repository.fail("UpsertQuestState")
	return nil
}

func (repository *failingRepository) LoadQuestStates(context.Context, uuid.UUID) ([]charstore.QuestState, error) {
	repository.fail("LoadQuestStates")
	return nil, nil
}

func (repository *failingRepository) RunInTx(context.Context, func(context.Context, charstore.Repository) error) error {
	repository.fail("RunInTx")
	return nil
}

var _ charstore.Repository = (*failingRepository)(nil)

func TestZoneTickPerformsNoRepositoryCalls(t *testing.T) {
	t.Parallel()

	repository := &failingRepository{t: t}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "InstLeague1",
		TickInterval:     time.Millisecond,
		SnapshotInterval: 2 * time.Millisecond,
		MaxMoveSpeed:     7,
		PlayerSpawn:      world.Vec3{X: 1, Y: 2, Z: 3},
	})
	if err != nil {
		t.Fatalf("create zone: %v", err)
	}

	zone.SpawnNPC(world.NPCSpec{
		ContentID: "mob.fixture.tick-purity",
		Level:     1,
		MaxHealth: 100,
		Position:  world.Vec3{X: 10, Y: 10},
	})
	entityID, _ := zone.Join()
	sink := &countingSink{}
	if err := zone.Subscribe(entityID, sink); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		zone.Run(ctx)
	}()

	// Drive real work through the tick: movement integration and snapshot
	// publication, which are the two bodies ADR 0031 §7 names.
	for sequence := uint64(1); sequence <= 50; sequence++ {
		intent := world.MoveIntent{
			Seq:      sequence,
			Input:    world.Vec3{X: 1},
			Duration: 33 * time.Millisecond,
		}
		if err := zone.ApplyMoveIntent(entityID, intent); err != nil {
			t.Fatalf("apply move intent %d: %v", sequence, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	<-done
	zone.Leave(entityID)

	if calls := repository.calls.Load(); calls != 0 {
		t.Fatalf("world tick made %d repository calls, want 0", calls)
	}
	if sink.count.Load() == 0 {
		t.Fatal("no snapshots were published; the tick loop did not run, so the result proves nothing")
	}
}

type countingSink struct {
	count atomic.Int64
}

func (sink *countingSink) OfferSnapshot(world.Snapshot) {
	sink.count.Add(1)
}

// The runtime test above can only observe the calls a tick actually makes today.
// This one closes the door on the calls someone could add tomorrow: if
// `internal/world` cannot reach a database driver through any transitive import,
// a query cannot be written there at all.
func TestWorldPackageCannotReachADatabaseDriver(t *testing.T) {
	t.Parallel()

	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("skipping import-graph check: no go tool on PATH (%v)", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	command := exec.CommandContext(ctx, goBinary, "list", "-deps", "github.com/SarnautCore/server/internal/world")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, output)
	}

	forbidden := []string{
		"github.com/SarnautCore/server/internal/charstore",
		"github.com/SarnautCore/server/internal/infra",
		"github.com/jackc/pgx",
		"github.com/redis/go-redis",
		"github.com/nats-io/nats.go",
		"database/sql",
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, banned := range forbidden {
			if dependency == banned || strings.HasPrefix(dependency, banned+"/") {
				t.Errorf("internal/world depends on %s; the tick must stay free of I/O", dependency)
			}
		}
	}
}
