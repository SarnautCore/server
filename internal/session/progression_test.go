package session

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/progression"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/world"
)

type workerRules struct{}

func (workerRules) MaxLevel() uint32 { return 2 }
func (workerRules) CumulativeExperience(level uint32) (int64, bool) {
	switch level {
	case 1:
		return 0, true
	case 2:
		return 100, true
	default:
		return 0, false
	}
}
func (workerRules) ExperienceForMobs(count, level int64) (int64, error) {
	if count == 4 && level == 2 {
		return 125, nil
	}
	return 0, progression.ErrInvalidGrant
}

func TestCommittedProgressionIsAdoptedBeforeTheNextCheckpoint(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("b6a64fbf-cf70-47fd-bdfa-72ba9145475b")
	character := newCharacterSession(Admission{CharacterID: characterID}, "InstLeague1", charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID,
			ZoneID:      "InstLeague1",
			Level:       1,
			Experience:  90,
			Health:      100,
			SaveSeq:     7,
		},
	})
	character.adoptProgression(progression.Change{
		CharacterID: characterID,
		Applied:     true,
		Granted:     25,
		Previous:    progression.State{Level: 1, Experience: 90},
		Current:     progression.State{Level: 2, Experience: 115},
		SaveSeq:     8,
	})

	snapshot := character.snapshotFrom(world.CharacterSnapshot{
		Level: 2, Health: 100, Alive: true,
	})
	if snapshot.State.Level != 2 || snapshot.State.Experience != 115 || snapshot.State.SaveSeq != 9 {
		t.Fatalf("checkpoint after committed progression = %#v", snapshot.State)
	}
}

type recordingExperienceSink struct {
	accepted bool
	commands []ExperienceCommand
}

func (sink *recordingExperienceSink) OfferExperience(command ExperienceCommand) bool {
	sink.commands = append(sink.commands, command)
	return sink.accepted
}

func TestScriptExperienceCommandCrossesOnlyTheNonBlockingOffTickSeam(t *testing.T) {
	t.Parallel()

	sink := &recordingExperienceSink{accepted: true}
	driver := &ScriptDriver{}
	driver.BindProgression(sink)
	err := (scriptHost{driver: driver}).Apply(context.Background(), script.Command{
		Kind:         script.CommandAddExperience,
		EntityID:     "42",
		Count:        4,
		MobLevel:     2,
		ExecutionKey: "eval-1|quest-1-40/experience",
	})
	if err != nil {
		t.Fatalf("Apply(CommandAddExperience) error = %v", err)
	}
	if len(sink.commands) != 1 || sink.commands[0] != (ExperienceCommand{
		EntityID: 42, MobCount: 4, MobLevel: 2,
		ExecutionKey: "eval-1|quest-1-40/experience",
	}) {
		t.Fatalf("off-tick commands = %#v", sink.commands)
	}
}

func TestScriptExperienceCommandFailsClosedWithoutAnOffTickSink(t *testing.T) {
	t.Parallel()

	driver := &ScriptDriver{}
	err := (scriptHost{driver: driver}).Apply(context.Background(), script.Command{
		Kind: script.CommandAddExperience, EntityID: "42",
		Count: 4, MobLevel: 2, ExecutionKey: "eval-1|experience",
	})
	if err == nil {
		t.Fatal("Apply(CommandAddExperience) accepted an unconfigured progression sink")
	}
}

func TestProgressionWorkerProjectsOnlyAfterTheExperienceTransactionCommits(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("6b9dbce0-06d1-4379-bd1e-33e5b910956a")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "InstLeague1",
		Level: 1, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	service, err := progression.NewService(repository, workerRules{})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	worker := NewProgressionWorker(service, slog.New(slog.DiscardHandler), 4)
	projected := make(chan progression.Change, 1)
	if err := worker.Admit(42, characterID, func(change progression.Change) error {
		persisted, err := repository.LoadCharacterState(ctx, characterID)
		if err != nil {
			return err
		}
		if persisted.Level != int32(change.Current.Level) ||
			persisted.Experience != change.Current.Experience ||
			persisted.SaveSeq != change.SaveSeq {
			t.Errorf("projection saw state %#v before change %#v was durable", persisted, change)
		}
		projected <- change
		return nil
	}); err != nil {
		t.Fatalf("worker.Admit() error = %v", err)
	}
	go func() { _ = worker.Run(ctx) }()
	if !worker.OfferExperience(ExperienceCommand{
		EntityID: 42, ExecutionKey: "eval-worker|experience", MobCount: 4, MobLevel: 2,
	}) {
		t.Fatal("OfferExperience() rejected a ready worker")
	}

	select {
	case change := <-projected:
		if !change.Applied || change.Current.Level != 2 || change.Current.Experience != 100 {
			t.Fatalf("projected change = %#v", change)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("committed progression was not projected")
	}
	if worker.Persisted() != 1 || worker.Failed() != 0 {
		t.Fatalf("worker counts: persisted=%d failed=%d", worker.Persisted(), worker.Failed())
	}
}

func TestProgressionWorkerRefusesAFullTickIngressWithoutBlocking(t *testing.T) {
	t.Parallel()

	worker := NewProgressionWorker(&fakeExperienceService{}, slog.New(slog.DiscardHandler), 1)
	if err := worker.Admit(1, uuid.New(), func(progression.Change) error { return nil }); err != nil {
		t.Fatalf("Admit() error = %v", err)
	}
	first := worker.OfferExperience(ExperienceCommand{EntityID: 1, ExecutionKey: "first", MobCount: 1, MobLevel: 1})
	second := worker.OfferExperience(ExperienceCommand{EntityID: 1, ExecutionKey: "second", MobCount: 1, MobLevel: 1})
	if !first || second || worker.Dropped() != 1 {
		t.Fatalf("offers = %v, %v; dropped=%d", first, second, worker.Dropped())
	}
}

func TestAcceptedExperiencePersistsAfterTheOriginatingSessionReleases(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &recordingExperienceService{grants: make(chan progression.MobExperienceGrant, 1)}
	worker := NewProgressionWorker(service, slog.New(slog.DiscardHandler), 1)
	characterID := uuid.MustParse("7b5b954b-b090-43f1-907b-1b955122ccda")
	projected := make(chan struct{}, 1)
	if err := worker.Admit(42, characterID, func(progression.Change) error {
		projected <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Admit() error = %v", err)
	}
	if !worker.OfferExperience(ExperienceCommand{
		EntityID: 42, ExecutionKey: "accepted-before-release", MobCount: 4, MobLevel: 2,
	}) {
		t.Fatal("OfferExperience() rejected an admitted character")
	}
	worker.Release(42)
	go func() { _ = worker.Run(ctx) }()

	select {
	case grant := <-service.grants:
		if grant.CharacterID != characterID || grant.ExecutionKey != "accepted-before-release" {
			t.Fatalf("persisted grant = %#v", grant)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accepted experience was lost after release")
	}
	select {
	case <-projected:
		t.Fatal("released session received a live projection")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestProgressionWorkerRejectsAnEntityWithoutACharacterBinding(t *testing.T) {
	t.Parallel()

	worker := NewProgressionWorker(&fakeExperienceService{}, slog.New(slog.DiscardHandler), 1)
	if worker.OfferExperience(ExperienceCommand{
		EntityID: 99, ExecutionKey: "unbound", MobCount: 1, MobLevel: 1,
	}) {
		t.Fatal("OfferExperience() accepted an unbound entity")
	}
	if worker.Dropped() != 1 || worker.Depth() != 0 {
		t.Fatalf("worker counts: dropped=%d depth=%d", worker.Dropped(), worker.Depth())
	}
}

type fakeExperienceService struct{}

func (*fakeExperienceService) ApplyMobExperience(
	context.Context,
	progression.MobExperienceGrant,
) (progression.Change, error) {
	return progression.Change{}, nil
}

type recordingExperienceService struct {
	grants chan progression.MobExperienceGrant
}

func (service *recordingExperienceService) ApplyMobExperience(
	_ context.Context,
	grant progression.MobExperienceGrant,
) (progression.Change, error) {
	service.grants <- grant
	return progression.Change{CharacterID: grant.CharacterID}, nil
}

func TestOlderProgressionProjectionCannotRollSessionStateBack(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("907b0092-8010-4115-91fe-12b4978ad68c")
	character := newCharacterSession(Admission{CharacterID: characterID}, "InstLeague1", charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID, ZoneID: "InstLeague1",
			Level: 2, Experience: 115, Health: 100, SaveSeq: 8,
		},
	})
	character.adoptProgression(progression.Change{
		CharacterID: characterID,
		Applied:     true,
		Current:     progression.State{Level: 1, Experience: 90},
		SaveSeq:     7,
	})

	snapshot := character.snapshotFrom(world.CharacterSnapshot{Level: 2, Health: 100, Alive: true})
	if snapshot.State.Level != 2 || snapshot.State.Experience != 115 || snapshot.State.SaveSeq != 9 {
		t.Fatalf("checkpoint after stale progression projection = %#v", snapshot.State)
	}
}

func TestCommittedProgressionSurvivesNewerQueuedCheckpointSequences(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("f77e829f-fe87-4534-92bd-4e29765039e1")
	character := newCharacterSession(Admission{CharacterID: characterID}, "InstLeague1", charstore.Snapshot{
		State: charstore.CharacterState{
			CharacterID: characterID, ZoneID: "InstLeague1",
			Level: 1, Experience: 90, Health: 100, SaveSeq: 7,
		},
	})
	view := world.CharacterSnapshot{Level: 1, Health: 100, Alive: true}
	if first := character.snapshotFrom(view); first.State.SaveSeq != 8 {
		t.Fatalf("first queued checkpoint sequence = %d", first.State.SaveSeq)
	}
	if second := character.snapshotFrom(view); second.State.SaveSeq != 9 {
		t.Fatalf("second queued checkpoint sequence = %d", second.State.SaveSeq)
	}

	// The durable transaction loaded sequence 7 and committed sequence 8
	// before either queued checkpoint reached storage. Its state is newer even
	// though its numeric sequence is below the highest locally issued number.
	character.adoptProgression(progression.Change{
		CharacterID: characterID,
		Applied:     true,
		Current:     progression.State{Level: 2, Experience: 115},
		SaveSeq:     8,
	})
	after := character.snapshotFrom(world.CharacterSnapshot{Level: 2, Health: 100, Alive: true})
	if after.State.Level != 2 || after.State.Experience != 115 || after.State.SaveSeq != 10 {
		t.Fatalf("checkpoint after durable progression = %#v", after.State)
	}
}
