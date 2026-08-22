package pack

import (
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

type experienceImpactKey struct {
	mobCount int64
	mobLevel int64
}

// PlayerProgression is the complete authored avatar curve and lifecycle row.
// Its methods are the narrow runtime contract consumed by progression.Service.
type PlayerProgression struct {
	id                           string
	maxLevel                     uint32
	thresholds                   []int64
	experienceImpacts            map[experienceImpactKey]int64
	respawnDelay                 time.Duration
	resurrectionSicknessDuration time.Duration
}

func (rules PlayerProgression) ID() string { return rules.id }

func (rules PlayerProgression) MaxLevel() uint32 { return rules.maxLevel }

func (rules PlayerProgression) CumulativeExperience(level uint32) (int64, bool) {
	if level == 0 || level >= uint32(len(rules.thresholds)) {
		return 0, false
	}
	return rules.thresholds[level], true
}

func (rules PlayerProgression) ExperienceForMobs(mobCount, mobLevel int64) (int64, error) {
	value, ok := rules.experienceImpacts[experienceImpactKey{mobCount: mobCount, mobLevel: mobLevel}]
	if !ok {
		return 0, fmt.Errorf(
			"player progression %q has no authored experience for mob_count=%d mob_level=%d",
			rules.id, mobCount, mobLevel,
		)
	}
	return value, nil
}

func (rules PlayerProgression) RespawnDelay() time.Duration { return rules.respawnDelay }

func (rules PlayerProgression) ResurrectionSicknessDuration() time.Duration {
	return rules.resurrectionSicknessDuration
}

func readPlayerProgression(tables map[string]*table) (PlayerProgression, bool, error) {
	loaded, ok := tables[tablePlayerProgression]
	if !ok {
		return PlayerProgression{}, false, nil
	}
	if loaded.rowCount != 1 {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: table %q holds %d rows, want exactly one",
			ErrMalformedTable, tablePlayerProgression, loaded.rowCount,
		)
	}

	var row contentv1.PlayerProgression
	if err := proto.Unmarshal(loaded.row(0), &row); err != nil {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: decode player progression row: %w", ErrMalformedTable, err,
		)
	}
	if row.GetId() == "" || row.GetMaxLevel() == 0 {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: player progression needs an id and positive max_level", ErrMalformedTable,
		)
	}
	if len(row.GetThresholds()) != int(row.GetMaxLevel()) {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: player progression %q has %d thresholds, want max_level %d",
			ErrMalformedTable, row.GetId(), len(row.GetThresholds()), row.GetMaxLevel(),
		)
	}

	rules := PlayerProgression{
		id:                           row.GetId(),
		maxLevel:                     row.GetMaxLevel(),
		thresholds:                   make([]int64, row.GetMaxLevel()+1),
		experienceImpacts:            make(map[experienceImpactKey]int64, len(row.GetExperienceImpacts())),
		respawnDelay:                 time.Duration(row.GetRespawnDelayMs()) * time.Millisecond,
		resurrectionSicknessDuration: time.Duration(row.GetResurrectionSicknessDurationMs()) * time.Millisecond,
	}
	if rules.respawnDelay <= 0 || rules.resurrectionSicknessDuration <= 0 {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: player progression %q has a zero lifecycle duration",
			ErrMalformedTable, row.GetId(),
		)
	}

	var previous int64
	for index, threshold := range row.GetThresholds() {
		level := uint32(index + 1)
		if threshold.GetLevel() != level || threshold.GetCumulativeExperience() > math.MaxInt64 {
			return PlayerProgression{}, false, fmt.Errorf(
				"%w: player progression %q threshold %d is malformed",
				ErrMalformedTable, row.GetId(), index,
			)
		}
		value := int64(threshold.GetCumulativeExperience())
		if level == 1 && value != 0 || level > 1 && value <= previous {
			return PlayerProgression{}, false, fmt.Errorf(
				"%w: player progression %q threshold for level %d is %d after %d",
				ErrMalformedTable, row.GetId(), level, value, previous,
			)
		}
		rules.thresholds[level] = value
		previous = value
	}

	for _, impact := range row.GetExperienceImpacts() {
		if impact.GetId() == "" || impact.GetMobCount() == 0 || impact.GetMobLevel() == 0 ||
			impact.GetResolvedExperience() == 0 || impact.GetResolvedExperience() > math.MaxInt64 {
			return PlayerProgression{}, false, fmt.Errorf(
				"%w: player progression %q carries a malformed experience impact",
				ErrMalformedTable, row.GetId(),
			)
		}
		key := experienceImpactKey{
			mobCount: int64(impact.GetMobCount()),
			mobLevel: int64(impact.GetMobLevel()),
		}
		if _, duplicate := rules.experienceImpacts[key]; duplicate {
			return PlayerProgression{}, false, fmt.Errorf(
				"%w: player progression %q repeats mob_count=%d mob_level=%d",
				ErrMalformedTable, row.GetId(), key.mobCount, key.mobLevel,
			)
		}
		rules.experienceImpacts[key] = int64(impact.GetResolvedExperience())
	}
	if len(rules.experienceImpacts) == 0 {
		return PlayerProgression{}, false, fmt.Errorf(
			"%w: player progression %q has no experience impacts",
			ErrMalformedTable, row.GetId(),
		)
	}
	return rules, true, nil
}
