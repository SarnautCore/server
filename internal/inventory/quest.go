package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/SarnautCore/server/internal/store"
)

// GrantQuestReward commits one quest turn-in as a single transaction
// (mechanics/quests.md rule 5.7.4).
//
// Consumed items out, reward stacks in, experience, money and honor credited,
// and the quest row written — all inside one [store.Repository.RunInTx]. That
// is the whole guarantee of rule 5.7.6: there is no partial turn-in, and no
// state in which experience was granted and the quest is still open.
//
// The bag arithmetic runs to completion in memory before the first write, which
// is rule 5.7.3's fail-fast: on [ErrBagFull] or [ErrNotEnough] nothing has been
// mutated, so there is nothing to roll back and the instance is left byte for
// byte as it was. Doing the removals first is what makes the requirement net
// rather than gross (rule 6.2): a turn-in that consumes a character's last
// stack frees the slot its own reward needs.
//
// It is the implementation of the `Granter` interface `internal/quests`
// declares. This package does not import that one, and must not: the seam is
// structural, over the plain values in `internal/store`.
func (service *Service) GrantQuestReward(
	ctx context.Context,
	grant store.QuestGrant,
) (store.QuestGrantResult, error) {
	var result store.QuestGrantResult
	err := service.repository.RunInTx(ctx, func(ctx context.Context, tx store.Repository) error {
		state, err := tx.LoadCharacterState(ctx, grant.CharacterID)
		if err != nil {
			return fmt.Errorf("load character state: %w", err)
		}
		stored, err := tx.LoadInventory(ctx, grant.CharacterID)
		if err != nil {
			return fmt.Errorf("load inventory: %w", err)
		}

		emptied, err := Remove(FromStore(stored), grantsOf(grant.Consume))
		if err != nil {
			return err
		}
		placed, err := Insert(emptied, grantsOf(grant.Grants), service.limits, service.slots)
		if errors.Is(err, ErrBagFull) {
			// Re-raised as the sentinel the quest module can name. It cannot
			// match ErrBagFull: that would mean importing this package, which
			// is the import the seam exists to prevent.
			return fmt.Errorf("%w: %w", store.ErrGrantWouldNotFit, err)
		}
		if err != nil {
			return err
		}
		if err := tx.ReplaceInventory(ctx, grant.CharacterID, ToStore(placed)); err != nil {
			return fmt.Errorf("write inventory: %w", err)
		}

		state.Currency += grant.Money
		state.Experience += grant.Experience
		state.Honor += grant.Honor
		// The sequence advances from the row this transaction just read, not
		// from a counter the session keeps, so two writers racing both try for
		// the same number and the anti-clobber rule decides (ADR 0031 §6).
		state.SaveSeq++
		if err := tx.SaveCharacterState(ctx, state); err != nil {
			return fmt.Errorf("credit quest rewards: %w", err)
		}
		if err := tx.UpsertQuestState(ctx, grant.CharacterID, grant.Quest); err != nil {
			return fmt.Errorf("write quest state: %w", err)
		}

		result = store.QuestGrantResult{
			Inventory:  ToStore(placed),
			Currency:   state.Currency,
			Experience: state.Experience,
			Honor:      state.Honor,
			SaveSeq:    state.SaveSeq,
		}
		return nil
	})
	if err != nil {
		return store.QuestGrantResult{}, err
	}
	return result, nil
}

func grantsOf(counts []store.ItemCount) []Grant {
	if len(counts) == 0 {
		return nil
	}
	grants := make([]Grant, 0, len(counts))
	for _, count := range counts {
		grants = append(grants, Grant{ItemID: count.ItemID, Count: count.Count})
	}
	return grants
}
