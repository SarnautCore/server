package quests

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/pack"
)

// Granter commits one quest grant as a single transaction.
//
// It is the seam this package declares and `internal/inventory` satisfies. It
// exists so that the quest module can be tested against a bag that is always
// full, or a transaction that fails halfway, without a database — and so that
// nothing here can reach a repository or a bag directly.
type Granter interface {
	GrantQuestReward(ctx context.Context, grant Grant) (GrantResult, error)
}

// Result is what one committing verb produced.
type Result struct {
	Update Update
	// Inventory, Currency and SaveSeq are the character's holdings as the grant
	// committed them. They are empty on a refusal, and the session adopts them
	// so its next checkpoint does not write the pre-grant bag back over this
	// one.
	Inventory []inventory.InventoryItem
	Currency  int64
	SaveSeq   int64
	// Committed is true when a transaction ran. An accept and an abandon commit
	// too: the quest row is the thing that makes them survive a crash.
	Committed bool
}

// Interact answers the generic interact verb against a quest giver
// (mechanics/quests.md rule 5.3, "on demand when a player interacts with a
// starter").
//
// It reports [RefusalNotAQuestGiver] for an entity that neither starts nor
// finishes anything, which is not an error: `interact` is also how a player
// opens a corpse, and the caller tries the other handlers.
func (module *Module) Interact(actorEntityID, targetEntityID uint64) ([]Update, Refusal) {
	var (
		updates []Update
		refusal Refusal
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		log, ok := module.logOf(actorEntityID)
		if !ok {
			refusal = RefusalNotAQuestGiver
			return nil
		}
		target := tick.Entity(targetEntityID)
		if target == nil || !target.Alive {
			// A dead entity gives no quest, and the liveness check is not
			// cosmetic: a loot corpse container carries the victim's content id
			// (mechanics/loot.md rule 5.1.1), so without it every corpse of a
			// quest mob would answer as its own quest giver and the interact
			// verb would never reach the loot handler.
			refusal = RefusalNotAQuestGiver
			return nil
		}
		offered, finished := module.catalog.StartedBy(target.ContentID), module.catalog.FinishedBy(target.ContentID)
		if len(offered) == 0 && len(finished) == 0 {
			refusal = RefusalNotAQuestGiver
			return nil
		}
		if !module.inRange(tick, actorEntityID, target) {
			refusal = RefusalOutOfRange
			return nil
		}
		for _, questID := range union(offered, finished) {
			definition, _ := module.catalog.Definition(questID)
			if held, holding := log.instances[questID]; holding {
				updates = append(updates, module.updateFor(definition, held))
				continue
			}
			if module.gate(log, definition) == StateOffered {
				updates = append(updates, Update{QuestID: questID, State: StateOffered})
				continue
			}
			// An NPC the player is standing in front of is the one place an
			// `unavailable` quest is worth reporting: it is the difference
			// between "come back at level five" and a marker that is simply
			// missing (rule 5.2 T2).
			updates = append(updates, Update{QuestID: questID, State: StateUnavailable})
		}
		return nil
	})
	return updates, refusal
}

// Accept is transition T3.
//
// The gates, the range and the log capacity are all checked under the zone
// lock, and the instance is created there and marked in flight; the row is then
// committed with the lock released, and only a successful commit clears the
// flight marker. That is the same two-phase shape the loot take uses, and for
// the same reason: the transaction must not run under the zone lock, and a
// retransmit arriving in the window must not accept the quest twice.
//
// Rule 5.6.1 rides on it. A definition with no objectives is `completable` the
// moment it is created, inside this one transaction, so it is never observably
// `accepted` with nothing to do.
func (module *Module) Accept(
	ctx context.Context,
	actorEntityID uint64,
	questID string,
	starterEntityID uint64,
) (Result, error) {
	definition, ok := module.catalog.Definition(questID)
	if !ok {
		return refused(questID, RefusalUnknownQuest)
	}

	var (
		characterID uuid.UUID
		created     *instance
		refusal     Refusal
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		log, ok := module.logOf(actorEntityID)
		if !ok {
			refusal = RefusalUnavailable
			return nil
		}
		characterID = log.characterID
		switch {
		case module.gate(log, definition) != StateOffered:
			refusal = RefusalUnavailable
		case visibleQuestCount(log) >= QuestLogCapacity:
			refusal = RefusalLogFull
		default:
			starter := tick.Entity(starterEntityID)
			switch {
			case !isGiver(starter, definition.StarterID):
				refusal = RefusalWrongNPC
			case !module.inRange(tick, actorEntityID, starter):
				refusal = RefusalOutOfRange
			default:
				created = &instance{
					questID:        questID,
					counters:       make([]int32, len(definition.Objectives)),
					acceptedAtTick: tick.Number(),
					inFlight:       true,
				}
				// Objectives whose limit is already zero are satisfied on
				// creation (rule 5.5.8), which is what makes this vacuous for a
				// quest with no objectives at all.
				created.state = progressState(definition, created.counters)
				log.instances[questID] = created
				// A `count-item` objective may already be satisfied out of the
				// bag the character walked up with.
				module.trackItems(log, nil)
				return nil
			}
		}
		return nil
	})
	if refusal != RefusalNone {
		return refused(questID, refusal)
	}

	row, err := created.row()
	if err != nil {
		module.rollBackAccept(characterID, questID)
		return Result{Update: Update{QuestID: questID, Refusal: RefusalInternal}}, err
	}
	granted, err := module.granter.GrantQuestReward(ctx, Grant{
		CharacterID: characterID,
		Quest:       row,
	})
	if err != nil {
		module.rollBackAccept(characterID, questID)
		module.logger.Error("quest accept failed",
			"character_id", characterID.String(), "quest_id", questID, "error", err)
		return Result{Update: Update{QuestID: questID, Refusal: RefusalInternal}}, err
	}

	var update Update
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok {
			return nil
		}
		held.inFlight = false
		module.trackItems(log, granted.Inventory)
		module.recountItems(log)
		update = module.updateFor(definition, held)
		return nil
	})
	return Result{
		Update:    update,
		Inventory: granted.Inventory,
		Currency:  granted.Currency,
		SaveSeq:   granted.SaveSeq,
		Committed: true,
	}, nil
}

// rollBackAccept undoes the in-memory instance of a commit that failed. The
// character keeps nothing: no row was written, so there is nothing to reconcile
// on the next login either.
func (module *Module) rollBackAccept(characterID uuid.UUID, questID string) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		if log, ok := module.logs[characterID]; ok {
			delete(log.instances, questID)
		}
		return nil
	})
}

// TurnIn is transitions T11, T12, T13 and T17: the whole of rule 5.7.
//
// Preconditions are checked in the order the rule gives them and all before any
// mutation — state, then the NPC, then the distance. The grant that follows is
// one transaction: consumed items out, reward items in, experience, money and
// honor credited, and the instance marked `turned-in`. Rule 5.7.6 is the point
// of the shape. Any failure inside it rolls the whole thing back, the character
// keeps the quest items and gains nothing, and the instance stays
// `completable`, byte for byte as it was.
func (module *Module) TurnIn(
	ctx context.Context,
	actorEntityID uint64,
	questID string,
	finisherEntityID uint64,
) (Result, error) {
	return module.TurnInWithChoice(ctx, actorEntityID, questID, finisherEntityID, 0)
}

// TurnInWithChoice applies ReturnQuest's zero-based alternative reward index.
// Mandatory rewards remain part of every successful grant. A quest with no
// alternatives ignores rewardIndex, preserving the original turn-in contract.
func (module *Module) TurnInWithChoice(
	ctx context.Context,
	actorEntityID uint64,
	questID string,
	finisherEntityID uint64,
	rewardIndex int32,
) (Result, error) {
	definition, ok := module.catalog.Definition(questID)
	if !ok {
		return refused(questID, RefusalUnknownQuest)
	}

	var (
		characterID uuid.UUID
		grant       Grant
		refusal     Refusal
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		log, ok := module.logOf(actorEntityID)
		if !ok {
			refusal = RefusalUnavailable
			return nil
		}
		characterID = log.characterID
		held, holding := log.instances[questID]
		switch {
		case !holding || held.state == StateAbandoned:
			refusal = RefusalUnavailable
		case held.state == StateTurnedIn:
			// T17. Terminal, and a retransmit lands here rather than granting
			// a second reward.
			refusal = RefusalAlreadyComplete
		case held.inFlight:
			// A grant for this quest is already committing. Refusing is what
			// keeps a double-click from being a second chance to insert the
			// same reward.
			refusal = RefusalAlreadyComplete
		case held.state != StateCompletable:
			refusal = RefusalNotComplete
		default:
			finisher := tick.Entity(finisherEntityID)
			switch {
			case !isGiver(finisher, definition.FinisherID):
				refusal = RefusalWrongNPC
			case !module.inRange(tick, actorEntityID, finisher):
				refusal = RefusalOutOfRange
			default:
				rewards, valid := rewardItems(definition, rewardIndex)
				if !valid {
					refusal = RefusalInvalidRewardChoice
					return nil
				}
				committed := *held
				committed.state = StateTurnedIn
				row, err := committed.row()
				if err != nil {
					refusal = RefusalInternal
					return nil
				}
				held.inFlight = true
				grant = Grant{
					CharacterID: characterID,
					Consume:     consumedItems(definition, log),
					Grants:      rewards,
					Experience:  definition.Rewards.Experience,
					Money:       definition.Rewards.Money,
					Honor:       definition.Rewards.Honor,
					Quest:       row,
				}
			}
		}
		return nil
	})
	if refusal != RefusalNone {
		return refused(questID, refusal)
	}

	granted, err := module.granter.GrantQuestReward(ctx, grant)
	if err != nil {
		module.clearFlight(characterID, questID)
		if errors.Is(err, ErrGrantWouldNotFit) {
			// T12. Nothing was applied, the instance is unchanged, and the
			// player can free a slot and try again.
			return refused(questID, RefusalBagFull)
		}
		module.logger.Error("quest turn-in failed",
			"character_id", characterID.String(), "quest_id", questID, "error", err)
		return Result{Update: Update{QuestID: questID, Refusal: RefusalInternal}}, err
	}

	var update Update
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok {
			return nil
		}
		held.state = StateTurnedIn
		held.inFlight = false
		// Rule 5.5.3: the consumed items are gone, so every other instance
		// tracking them is recounted against the bag the grant committed.
		module.trackItems(log, granted.Inventory)
		for _, changed := range module.recountItems(log) {
			module.publish(characterID, changed)
		}
		// Rule 5.7.5, outside the transaction: every quest gated on this one is
		// re-evaluated, and the ones that just became available are announced.
		for _, dependent := range module.catalog.Dependents(questID) {
			definition, found := module.catalog.Definition(dependent)
			if !found {
				continue
			}
			if module.gate(log, definition) == StateOffered {
				module.publish(characterID, Update{QuestID: dependent, State: StateOffered})
			}
		}
		update = module.updateFor(definition, held)
		return nil
	})
	update.Experience = definition.Rewards.Experience
	update.Money = definition.Rewards.Money
	update.Honor = definition.Rewards.Honor
	update.Items = grant.Grants
	return Result{
		Update:    update,
		Inventory: granted.Inventory,
		Currency:  granted.Currency,
		SaveSeq:   granted.SaveSeq,
		Committed: true,
	}, nil
}

// Abandon is transitions T14 and T15.
//
// The instance becomes `abandoned` rather than disappearing. The row is what
// tells the next login that this character may take the quest again from
// scratch (T16), and a deleted row is indistinguishable from one that was never
// written.
func (module *Module) Abandon(ctx context.Context, actorEntityID uint64, questID string) (Result, error) {
	definition, ok := module.catalog.Definition(questID)
	if !ok {
		return refused(questID, RefusalUnknownQuest)
	}

	var (
		characterID uuid.UUID
		grant       Grant
		refusal     Refusal
	)
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logOf(actorEntityID)
		if !ok {
			refusal = RefusalUnavailable
			return nil
		}
		characterID = log.characterID
		held, holding := log.instances[questID]
		switch {
		case !holding || !held.state.Active() && held.state != StateCompletable:
			refusal = RefusalUnavailable
		case !definition.CanCancel:
			// T15. Retail's starting quests set this false and M2's curated
			// quest follows.
			refusal = RefusalCannotCancel
		case held.inFlight:
			refusal = RefusalAlreadyComplete
		default:
			committed := *held
			committed.state = StateAbandoned
			row, err := committed.row()
			if err != nil {
				refusal = RefusalInternal
				return nil
			}
			held.inFlight = true
			grant = Grant{
				CharacterID: characterID,
				// Rule 5.5.5: an item objective the definition marks as removed
				// on abandon takes its items with it.
				Consume: abandonedItems(definition, log),
				Quest:   row,
			}
		}
		return nil
	})
	if refusal != RefusalNone {
		return refused(questID, refusal)
	}

	granted, err := module.granter.GrantQuestReward(ctx, grant)
	if err != nil {
		module.clearFlight(characterID, questID)
		module.logger.Error("quest abandon failed",
			"character_id", characterID.String(), "quest_id", questID, "error", err)
		return Result{Update: Update{QuestID: questID, Refusal: RefusalInternal}}, err
	}

	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok {
			return nil
		}
		held.state = StateAbandoned
		held.inFlight = false
		held.counters = make([]int32, len(definition.Objectives))
		module.trackItems(log, granted.Inventory)
		module.recountItems(log)
		return nil
	})
	return Result{
		Update:    Update{QuestID: questID, State: StateAbandoned},
		Inventory: granted.Inventory,
		Currency:  granted.Currency,
		SaveSeq:   granted.SaveSeq,
		Committed: true,
	}, nil
}

func (module *Module) clearFlight(characterID uuid.UUID, questID string) {
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		if log, ok := module.logs[characterID]; ok {
			if held, holding := log.instances[questID]; holding {
				held.inFlight = false
			}
		}
		return nil
	})
}

// logOf resolves the acting entity to its quest log. It is called with the zone
// lock held.
func (module *Module) logOf(entityID uint64) (*questLog, bool) {
	characterID, ok := module.actors[entityID]
	if !ok {
		return nil, false
	}
	log, ok := module.logs[characterID]
	return log, ok
}

// isGiver reports whether one entity is the live NPC a definition names.
//
// The liveness half is what mechanics/quests.md section 7.4 leaves open, and it
// is answered here in the only direction that is safe: a dead NPC hands nothing
// over and takes nothing back. It is also load-bearing rather than cosmetic — a
// loot corpse container carries its victim's content id (mechanics/loot.md rule
// 5.1.1), so without it a player could turn a quest in at the corpse of its own
// finisher.
func isGiver(entity *gametypes.EntityData, contentID string) bool {
	return entity != nil && entity.Alive && entity.ContentID == contentID
}

// inRange is TURN_IN_RANGE_M, measured over all three axes exactly as
// mechanics/combat.md rule 5.3.1 measures ability range. There is no tolerance
// term: unlike a cast, a turn-in is not racing the target's movement.
func (module *Module) inRange(tick gametypes.Tick, actorEntityID uint64, npc *gametypes.EntityData) bool {
	actor := tick.Entity(actorEntityID)
	if actor == nil {
		return false
	}
	return gametypes.Distance(tick.Position(actor), tick.Position(npc)) <= TurnInRangeM
}

// consumedItems is what rule 5.7.4 takes out of the bag: the tracked items of
// every `count-item` objective, up to each objective's limit.
//
// The tally is the authority rather than the counter, so an objective the bag
// can no longer cover asks for what the objective demands and the removal fails
// the whole transaction. Consuming "as many as there are" would be a partial
// turn-in.
func consumedItems(definition pack.Quest, log *questLog) []ItemCount {
	var consumed []ItemCount
	for _, objective := range definition.Objectives {
		if objective.Kind != pack.QuestObjectiveCountItem {
			continue
		}
		remaining := objective.Limit
		for _, target := range objective.TargetIDs {
			if remaining <= 0 {
				break
			}
			take := log.held[target]
			if take > remaining {
				take = remaining
			}
			if take <= 0 {
				continue
			}
			consumed = append(consumed, ItemCount{ItemID: target, Count: take})
			remaining -= take
		}
		if remaining > 0 && len(objective.TargetIDs) > 0 {
			// Ask for the shortfall anyway, against the first target. The
			// removal refuses it and the transaction rolls back, which is the
			// answer: the objective was not actually satisfied.
			consumed = append(consumed, ItemCount{ItemID: objective.TargetIDs[0], Count: remaining})
		}
	}
	return consumed
}

// abandonedItems is rule 5.5.5, which is the same arithmetic gated on the
// definition's own flag.
func abandonedItems(definition pack.Quest, log *questLog) []ItemCount {
	var consumed []ItemCount
	for _, objective := range definition.Objectives {
		if objective.Kind != pack.QuestObjectiveCountItem || !objective.RemoveOnAbandon {
			continue
		}
		for _, target := range objective.TargetIDs {
			if held := log.held[target]; held > 0 {
				consumed = append(consumed, ItemCount{ItemID: target, Count: held})
			}
		}
	}
	return consumed
}

func rewardItems(definition pack.Quest, rewardIndex int32) ([]ItemCount, bool) {
	var items []ItemCount
	for _, reward := range definition.Rewards.MandatoryItems {
		items = appendReward(items, reward)
	}
	if len(definition.Rewards.AlternativeItems) == 0 {
		return items, true
	}
	if rewardIndex < 0 || int64(rewardIndex) >= int64(len(definition.Rewards.AlternativeItems)) {
		return nil, false
	}
	items = appendReward(items, definition.Rewards.AlternativeItems[rewardIndex])
	return items, true
}

func appendReward(items []ItemCount, reward pack.QuestRewardItem) []ItemCount {
	if reward.Count <= 0 {
		return items
	}
	return append(items, ItemCount{ItemID: reward.ItemID, Count: reward.Count})
}

func refused(questID string, refusal Refusal) (Result, error) {
	return Result{Update: Update{QuestID: questID, Refusal: refusal}}, refusal.err()
}

func union(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	result := make([]string, 0, len(left)+len(right))
	for _, values := range [][]string{left, right} {
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
