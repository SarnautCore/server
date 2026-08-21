package loot

import (
	"fmt"

	"github.com/SarnautCore/server/internal/pack"
)

const (
	cursedRollsCount      = 3
	cursedDropProbability = 0.3333
)

// EvaluateWithCurses runs the ordinary table once, then performs the retail
// cursed-drop rerolls against the same advancing stream. Eligibility is a
// source-free product flag baked into the gameplay pack.
func EvaluateWithCurses(table pack.LootTable, items ItemSource, stream Stream) (Drop, error) {
	ordinary, err := Evaluate(table, stream)
	if err != nil {
		return Drop{}, err
	}
	cursed, err := rollCursedGrants(table, ordinary, items, stream)
	if err != nil {
		return Drop{}, err
	}
	if len(ordinary.Items)+len(cursed) > MaxObservableLootEntries {
		return Drop{}, ErrLootEntryLimit
	}
	ordinary.Items = append(ordinary.Items, cursed...)
	return ordinary, nil
}

func rollCursedGrants(table pack.LootTable, ordinary Drop, items ItemSource, stream Stream) ([]ItemGrant, error) {
	ordinaryProducts := make(map[string]struct{}, len(ordinary.Items))
	for _, grant := range ordinary.Items {
		ordinaryProducts[grant.ItemID] = struct{}{}
	}

	for attempt := 1; attempt <= cursedRollsCount; attempt++ {
		candidate, err := Evaluate(table, stream)
		if err != nil {
			return nil, err
		}
		eligible := make([]ItemGrant, 0, len(candidate.Items))
		seenProducts := make(map[string]struct{}, len(candidate.Items))
		duplicateOrdinary := false
		for _, grant := range candidate.Items {
			if _, seen := seenProducts[grant.ItemID]; seen {
				continue
			}
			seenProducts[grant.ItemID] = struct{}{}
			if items == nil {
				return nil, fmt.Errorf("loot: curse candidate %q has no item source", grant.ItemID)
			}
			item, ok := items.Item(grant.ItemID)
			if !ok {
				return nil, fmt.Errorf("loot: curse candidate %q is absent from the item pack", grant.ItemID)
			}
			if !item.CurseEligible {
				continue
			}
			if _, duplicate := ordinaryProducts[grant.ItemID]; duplicate {
				if attempt < cursedRollsCount {
					duplicateOrdinary = true
					break
				}
				continue
			}
			eligible = append(eligible, grant)
		}
		if duplicateOrdinary {
			continue
		}

		cursed := make([]ItemGrant, 0, len(eligible))
		for _, grant := range eligible {
			if stream.Draws() >= MaxDrawsPerRoll {
				return nil, ErrDrawBudget
			}
			if !curseProbabilityHit(stream.NextFloat64()) {
				continue
			}
			cursed = append(cursed, ItemGrant{
				ItemID:   grant.ItemID,
				Count:    1,
				IsCursed: true,
			})
		}
		return cursed, nil
	}
	return nil, nil
}

func curseProbabilityHit(value float64) bool {
	return value < cursedDropProbability
}
