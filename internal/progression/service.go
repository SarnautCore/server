// Package progression applies authored experience rules to durable character
// state. It contains no default curve: production must provide the private
// native-data adapter, and construction fails when that source is incomplete.
package progression

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/charstore"
)

var (
	ErrInvalidRules  = errors.New("progression: invalid authored rules")
	ErrInvalidGrant  = errors.New("progression: invalid experience grant")
	ErrReplayChanged = errors.New("progression: replay changed its payload")
	ErrStateMismatch = errors.New("progression: persisted level and experience disagree")
)

// maxRuntimeLevel limits construction work for a malformed AuthoredRules
// implementation. It is a runtime capacity, not a gameplay curve.
const maxRuntimeLevel = 1_000_000

// AuthoredRules is the narrow adapter the private native data implements. The
// public server never embeds the retail table or its original representation.
type AuthoredRules interface {
	MaxLevel() uint32
	CumulativeExperience(level uint32) (int64, bool)
	ExperienceForMobs(mobCount, mobLevel int64) (int64, error)
}

// State is the progression portion of one character snapshot.
type State struct {
	Level      uint32
	Experience int64
}

// ExperienceGrant is one already-calculated award from a quest, kill, or
// script. ExecutionKey is mandatory because applying the same command twice is
// never a valid way to recover from a crash.
type ExperienceGrant struct {
	CharacterID  uuid.UUID
	ExecutionKey string
	Amount       int64
}

// MobExperienceGrant is the typed payload of ImpactAddExperience.
type MobExperienceGrant struct {
	CharacterID  uuid.UUID
	ExecutionKey string
	MobCount     int64
	MobLevel     int64
}

// Change is the committed domain event returned to an off-tick adapter. An
// adapter may project it only after ApplyExperience returns successfully.
type Change struct {
	CharacterID  uuid.UUID
	ExecutionKey string
	Applied      bool
	Granted      int64
	Previous     State
	Current      State
	SaveSeq      int64
}

// Service validates one authored curve at construction and applies gains in a
// transaction with their idempotency record.
type Service struct {
	repository charstore.Repository
	rules      AuthoredRules
	thresholds []int64
}

// ResolveExperience implements charstore.ExperienceResolver for rewards whose
// inventory and quest-state writes must share one outer transaction.
func (service *Service) ResolveExperience(
	level int32,
	cumulative, gain int64,
) (int32, int64, error) {
	if service == nil || level < 1 || gain < 0 {
		return 0, 0, fmt.Errorf("%w: malformed state or gain", ErrInvalidGrant)
	}
	previous := State{Level: uint32(level), Experience: cumulative}
	if err := service.validateState(previous); err != nil {
		return 0, 0, err
	}
	if gain == 0 {
		return level, cumulative, nil
	}
	current, err := service.advance(previous, gain)
	if err != nil {
		return 0, 0, err
	}
	return int32(current.Level), current.Experience, nil
}

func NewService(repository charstore.Repository, rules AuthoredRules) (*Service, error) {
	if repository == nil || rules == nil {
		return nil, fmt.Errorf("%w: repository and source are required", ErrInvalidRules)
	}
	maxLevel := rules.MaxLevel()
	if maxLevel == 0 || maxLevel > maxRuntimeLevel {
		return nil, fmt.Errorf(
			"%w: maximum level %d is outside 1..%d", ErrInvalidRules, maxLevel, maxRuntimeLevel,
		)
	}
	thresholds := make([]int64, int(maxLevel)+1)
	for level := uint32(1); level <= maxLevel; level++ {
		threshold, ok := rules.CumulativeExperience(level)
		if !ok {
			return nil, fmt.Errorf("%w: cumulative threshold for level %d is absent", ErrInvalidRules, level)
		}
		if threshold < 0 || level == 1 && threshold != 0 ||
			level > 1 && threshold <= thresholds[level-1] {
			return nil, fmt.Errorf("%w: cumulative threshold for level %d is %d", ErrInvalidRules, level, threshold)
		}
		thresholds[level] = threshold
	}
	return &Service{repository: repository, rules: rules, thresholds: thresholds}, nil
}

func (service *Service) ApplyMobExperience(
	ctx context.Context,
	grant MobExperienceGrant,
) (Change, error) {
	if service == nil || service.rules == nil {
		return Change{}, fmt.Errorf("%w: service is not configured", ErrInvalidRules)
	}
	if grant.MobCount <= 0 || grant.MobLevel <= 0 {
		return Change{}, fmt.Errorf("%w: mob count and level must be positive", ErrInvalidGrant)
	}
	amount, err := service.rules.ExperienceForMobs(grant.MobCount, grant.MobLevel)
	if err != nil {
		return Change{}, fmt.Errorf("progression: calculate authored mob experience: %w", err)
	}
	return service.ApplyExperience(ctx, ExperienceGrant{
		CharacterID:  grant.CharacterID,
		ExecutionKey: grant.ExecutionKey,
		Amount:       amount,
	})
}

func (service *Service) ApplyExperience(
	ctx context.Context,
	grant ExperienceGrant,
) (Change, error) {
	const attempts = 8
	for attempt := 0; attempt < attempts; attempt++ {
		change, err := service.applyExperienceOnce(ctx, grant)
		if !errors.Is(err, charstore.ErrStaleSave) {
			return change, err
		}
		if err := ctx.Err(); err != nil {
			return Change{}, err
		}
	}
	return Change{}, fmt.Errorf("progression: experience transaction lost %d save-sequence races: %w",
		attempts, charstore.ErrStaleSave)
}

func (service *Service) applyExperienceOnce(
	ctx context.Context,
	grant ExperienceGrant,
) (Change, error) {
	if service == nil || service.repository == nil {
		return Change{}, fmt.Errorf("%w: service is not configured", ErrInvalidRules)
	}
	if grant.CharacterID == uuid.Nil || grant.ExecutionKey == "" || grant.Amount <= 0 {
		return Change{}, fmt.Errorf("%w: character, execution key, and positive amount are required", ErrInvalidGrant)
	}

	var change Change
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx charstore.Repository) error {
		storedGrant, inserted, err := tx.RecordProgressionGrant(ctx, charstore.ProgressionGrant{
			CharacterID:  grant.CharacterID,
			ExecutionKey: grant.ExecutionKey,
			Amount:       grant.Amount,
		})
		if err != nil {
			return err
		}
		if !inserted && storedGrant.Amount != grant.Amount {
			return fmt.Errorf("%w: key %q stored %d, replay supplied %d",
				ErrReplayChanged, grant.ExecutionKey, storedGrant.Amount, grant.Amount)
		}

		persisted, err := tx.LoadCharacterState(ctx, grant.CharacterID)
		if err != nil {
			return err
		}
		previous := State{Level: uint32(persisted.Level), Experience: persisted.Experience}
		if err := service.validateState(previous); err != nil {
			return err
		}
		change = Change{
			CharacterID:  grant.CharacterID,
			ExecutionKey: grant.ExecutionKey,
			Applied:      inserted,
			Granted:      grant.Amount,
			Previous:     previous,
			Current:      previous,
			SaveSeq:      persisted.SaveSeq,
		}
		if !inserted {
			return nil
		}

		current, err := service.advance(previous, grant.Amount)
		if err != nil {
			return err
		}
		persisted.Level = int32(current.Level)
		persisted.Experience = current.Experience
		if persisted.SaveSeq == math.MaxInt64 {
			return fmt.Errorf("%w: save sequence overflow", ErrInvalidGrant)
		}
		persisted.SaveSeq++
		if err := tx.SaveCharacterState(ctx, persisted); err != nil {
			return err
		}
		change.Current = current
		change.SaveSeq = persisted.SaveSeq
		return nil
	})
	if err != nil {
		return Change{}, err
	}
	return change, nil
}

func (service *Service) validateState(state State) error {
	if state.Level == 0 || state.Level >= uint32(len(service.thresholds)) || state.Experience < 0 {
		return fmt.Errorf("%w: level %d, experience %d", ErrStateMismatch, state.Level, state.Experience)
	}
	if state.Experience < service.thresholds[state.Level] {
		return fmt.Errorf("%w: level %d begins at %d, state has %d",
			ErrStateMismatch, state.Level, service.thresholds[state.Level], state.Experience)
	}
	if state.Level+1 < uint32(len(service.thresholds)) &&
		state.Experience >= service.thresholds[state.Level+1] {
		return fmt.Errorf("%w: experience %d already reaches level %d",
			ErrStateMismatch, state.Experience, state.Level+1)
	}
	return nil
}

func (service *Service) advance(previous State, amount int64) (State, error) {
	if amount > math.MaxInt64-previous.Experience {
		return State{}, fmt.Errorf("%w: experience overflow", ErrInvalidGrant)
	}
	current := State{Level: previous.Level, Experience: previous.Experience + amount}
	maxLevel := uint32(len(service.thresholds) - 1)
	for current.Level < maxLevel && current.Experience >= service.thresholds[current.Level+1] {
		current.Level++
	}
	if current.Level == maxLevel && current.Experience > service.thresholds[maxLevel] {
		current.Experience = service.thresholds[maxLevel]
	}
	return current, nil
}
