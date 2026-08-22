package progression_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/progression"
)

type authoredRules struct {
	max        uint32
	thresholds map[uint32]int64
	impactXP   int64
}

type staleOnceRepository struct {
	charstore.Repository
	failed atomic.Bool
}

func (repository *staleOnceRepository) RunInTx(
	ctx context.Context,
	fn func(context.Context, charstore.Repository) error,
) error {
	return repository.Repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		return fn(ctx, &staleOnceTransaction{Repository: tx, owner: repository})
	})
}

type staleOnceTransaction struct {
	charstore.Repository
	owner *staleOnceRepository
}

func (tx *staleOnceTransaction) SaveCharacterState(
	ctx context.Context,
	state charstore.CharacterState,
) error {
	if tx.owner.failed.CompareAndSwap(false, true) {
		return charstore.ErrStaleSave
	}
	return tx.Repository.SaveCharacterState(ctx, state)
}

func testRules() authoredRules {
	return authoredRules{
		max: 3,
		thresholds: map[uint32]int64{
			1: 0,
			2: 100,
			3: 300,
		},
	}
}

func (rules authoredRules) MaxLevel() uint32 { return rules.max }

func (rules authoredRules) CumulativeExperience(level uint32) (int64, bool) {
	value, ok := rules.thresholds[level]
	return value, ok
}

func (rules authoredRules) ExperienceForMobs(mobCount, mobLevel int64) (int64, error) {
	return rules.impactXP, nil
}

func TestExperienceCommitsBeforeItIsReportedAndReplayIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("f5819560-8d1c-41d2-8a18-a8454b489e8b")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID,
		ZoneID:      "ZoneLeague1",
		Level:       1,
		Experience:  90,
		Health:      100,
		SaveSeq:     1,
	}); err != nil {
		t.Fatalf("seed character state: %v", err)
	}

	service, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	change, err := service.ApplyExperience(ctx, progression.ExperienceGrant{
		CharacterID:  characterID,
		ExecutionKey: "quest-1-40:manticore|impact-add-experience",
		Amount:       25,
	})
	if err != nil {
		t.Fatalf("ApplyExperience() error = %v", err)
	}
	if !change.Applied || change.Previous.Level != 1 || change.Previous.Experience != 90 ||
		change.Current.Level != 2 || change.Current.Experience != 115 || change.SaveSeq != 2 {
		t.Fatalf("ApplyExperience() change = %#v", change)
	}

	persisted, err := repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if persisted.Level != 2 || persisted.Experience != 115 || persisted.SaveSeq != 2 {
		t.Fatalf("persisted state = %#v", persisted)
	}

	replayed, err := service.ApplyExperience(ctx, progression.ExperienceGrant{
		CharacterID:  characterID,
		ExecutionKey: "quest-1-40:manticore|impact-add-experience",
		Amount:       25,
	})
	if err != nil {
		t.Fatalf("replayed ApplyExperience() error = %v", err)
	}
	if replayed.Applied {
		t.Fatalf("replayed ApplyExperience() change = %#v, want an idempotent no-op", replayed)
	}
	persisted, err = repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() after replay error = %v", err)
	}
	if persisted.Level != 2 || persisted.Experience != 115 || persisted.SaveSeq != 2 {
		t.Fatalf("state after replay = %#v", persisted)
	}
}

func TestFailedExperienceTransactionRollsBackItsExecutionKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("46b536f7-1d87-4012-a799-134482867349")
	// Level two at 50 cumulative experience is corrupt. The service records the
	// execution key first, then discovers this mismatch inside the same unit of
	// work. A correct rollback removes both changes.
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 2, Experience: 50, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}
	service, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	grant := progression.ExperienceGrant{
		CharacterID: characterID, ExecutionKey: "eval-rollback|impact", Amount: 75,
	}
	if _, err := service.ApplyExperience(ctx, grant); !errors.Is(err, progression.ErrStateMismatch) {
		t.Fatalf("ApplyExperience() error = %v, want ErrStateMismatch", err)
	}

	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 50, Health: 100, SaveSeq: 2,
	}); err != nil {
		t.Fatalf("repair state: %v", err)
	}
	change, err := service.ApplyExperience(ctx, grant)
	if err != nil {
		t.Fatalf("retry ApplyExperience() error = %v", err)
	}
	if !change.Applied || change.Current.Level != 2 || change.Current.Experience != 125 {
		t.Fatalf("retry change = %#v, want the rolled-back key to apply", change)
	}
}

func TestExperienceReplayRemainsIdempotentAfterServiceRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("67eb91d4-31ca-4072-96f5-f63166671478")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 0, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	first, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("first NewService() error = %v", err)
	}
	grant := progression.ExperienceGrant{
		CharacterID: characterID, ExecutionKey: "eval-restart|impact", Amount: 100,
	}
	if change, err := first.ApplyExperience(ctx, grant); err != nil || !change.Applied {
		t.Fatalf("first ApplyExperience() = %#v, %v", change, err)
	}

	second, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("second NewService() error = %v", err)
	}
	change, err := second.ApplyExperience(ctx, grant)
	if err != nil {
		t.Fatalf("restarted ApplyExperience() error = %v", err)
	}
	if change.Applied || change.Current.Level != 2 || change.Current.Experience != 100 || change.SaveSeq != 2 {
		t.Fatalf("restarted change = %#v", change)
	}
}

func TestConcurrentExperienceReplayAppliesOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("70847f6c-fb67-4627-8d91-2af392113e49")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 0, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	service, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	grant := progression.ExperienceGrant{
		CharacterID: characterID, ExecutionKey: "eval-race|impact", Amount: 25,
	}

	const workers = 32
	var wait sync.WaitGroup
	wait.Add(workers)
	changes := make(chan progression.Change, workers)
	errorsSeen := make(chan error, workers)
	for range workers {
		go func() {
			defer wait.Done()
			change, err := service.ApplyExperience(ctx, grant)
			if err != nil {
				errorsSeen <- err
				return
			}
			changes <- change
		}()
	}
	wait.Wait()
	close(changes)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("ApplyExperience() error = %v", err)
	}
	applied := 0
	for change := range changes {
		if change.Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied changes = %d, want 1", applied)
	}
	persisted, err := repository.LoadCharacterState(ctx, characterID)
	if err != nil {
		t.Fatalf("LoadCharacterState() error = %v", err)
	}
	if persisted.Experience != 25 || persisted.SaveSeq != 2 {
		t.Fatalf("persisted state = %#v", persisted)
	}
}

func TestExperienceReplayRejectsChangedPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("f2ca145e-b42d-451a-a550-c330717b56ac")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 0, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	service, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	first := progression.ExperienceGrant{
		CharacterID: characterID, ExecutionKey: "eval-changed|impact", Amount: 25,
	}
	if _, err := service.ApplyExperience(ctx, first); err != nil {
		t.Fatalf("first ApplyExperience() error = %v", err)
	}
	first.Amount = 26
	if _, err := service.ApplyExperience(ctx, first); !errors.Is(err, progression.ErrReplayChanged) {
		t.Fatalf("changed replay error = %v, want ErrReplayChanged", err)
	}
}

func TestConcurrentSaveConflictRetriesTheWholeExperienceTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	base := charstore.NewMemory()
	characterID := uuid.MustParse("31e45dc0-5109-4472-a641-57baf753d331")
	if err := base.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 0, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	repository := &staleOnceRepository{Repository: base}
	service, err := progression.NewService(repository, testRules())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	change, err := service.ApplyExperience(ctx, progression.ExperienceGrant{
		CharacterID: characterID, ExecutionKey: "eval-stale|impact", Amount: 25,
	})
	if err != nil {
		t.Fatalf("ApplyExperience() after a save race error = %v", err)
	}
	if !change.Applied || change.Current.Experience != 25 || change.SaveSeq != 2 {
		t.Fatalf("ApplyExperience() change = %#v", change)
	}
}

func TestImpactAddExperienceUsesAuthoredMobInputs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository := charstore.NewMemory()
	characterID := uuid.MustParse("f8c77623-454e-4f92-b84e-d97b52eaa461")
	if err := repository.SaveCharacterState(ctx, charstore.CharacterState{
		CharacterID: characterID, ZoneID: "ZoneLeague1",
		Level: 1, Experience: 0, Health: 100, SaveSeq: 1,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	rules := testRules()
	rules.impactXP = 75
	service, err := progression.NewService(repository, rules)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	change, err := service.ApplyMobExperience(ctx, progression.MobExperienceGrant{
		CharacterID:  characterID,
		ExecutionKey: "eval-manticore|impact",
		MobCount:     4,
		MobLevel:     2,
	})
	if err != nil {
		t.Fatalf("ApplyMobExperience() error = %v", err)
	}
	if !change.Applied || change.Granted != 75 || change.Current.Experience != 75 {
		t.Fatalf("ApplyMobExperience() change = %#v", change)
	}
}

func TestServiceRejectsMalformedAuthoredCurvesWithoutPanicking(t *testing.T) {
	t.Parallel()

	tests := map[string]authoredRules{
		"zero max level": {max: 0},
		"absent threshold": {
			max: 2, thresholds: map[uint32]int64{1: 0},
		},
		"nonzero level one": {
			max: 2, thresholds: map[uint32]int64{1: 1, 2: 100},
		},
		"non-increasing": {
			max: 3, thresholds: map[uint32]int64{1: 0, 2: 100, 3: 100},
		},
		"impossible allocation": {max: ^uint32(0)},
	}
	for name, rules := range tests {
		name, rules := name, rules
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := progression.NewService(charstore.NewMemory(), rules); !errors.Is(err, progression.ErrInvalidRules) {
				t.Fatalf("NewService() error = %v, want ErrInvalidRules", err)
			}
		})
	}
}

func TestNilServiceRejectsMobExperience(t *testing.T) {
	t.Parallel()

	var service *progression.Service
	_, err := service.ApplyMobExperience(context.Background(), progression.MobExperienceGrant{
		CharacterID: uuid.New(), ExecutionKey: "nil-service", MobCount: 1, MobLevel: 1,
	})
	if !errors.Is(err, progression.ErrInvalidRules) {
		t.Fatalf("ApplyMobExperience() error = %v, want ErrInvalidRules", err)
	}
}
