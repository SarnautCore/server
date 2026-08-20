package quests

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SarnautCore/server/internal/pack"
)

// The invented constants of mechanics/quests.md section 3.
const (
	// QuestLogCapacity is how many instances one character may hold at once.
	QuestLogCapacity = 25
	// TurnInRangeM is how close a character must stand to a starter or a
	// finisher. It is deliberately shorter than combat's ABILITY_RANGE_M so
	// that a player has to walk to the NPC rather than shout at it.
	TurnInRangeM = 5.0
)

// finishedStatus is the only prerequisite status M2 implements (rule 5.3.3).
// It is the authored spelling, compared case-insensitively.
const finishedStatus = "Finished"

// ItemSource answers what one item is. `*pack.Pack` implements it; a test
// implements it in three lines.
//
// It is an interface rather than a *pack.Pack so that a catalog can be built
// from three hand-written items, and so that nothing here can reach for a
// second field of an item record by accident.
type ItemSource interface {
	Item(id string) (pack.Item, bool)
}

// Catalog is every quest definition the shard plays, indexed the three ways
// the rules ask about them: by id, by the NPC that starts them, and by the
// quest they wait on.
//
// It is a value with no behaviour beyond lookup, so a test can build one from a
// hand-made definition and assert a transition without standing up a zone.
// Adding a second quest is a row in the pack and nothing here.
type Catalog struct {
	quests     map[string]pack.Quest
	order      []string
	byStarter  map[string][]string
	byFinisher map[string][]string
	dependents map[string][]string
	unresolved []string
}

// CatalogFromPack reads the quest table of a loaded pack and validates every
// definition against the rules M2 implements.
//
// It fails the boot rather than skipping a definition. mechanics/quests.md rule
// 5.5.6 is explicit about why: a quest whose objective kind this build cannot
// advance is a quest a player can accept and never finish, and finding that out
// from a support ticket costs far more than a refused start-up.
func CatalogFromPack(content *pack.Pack) (Catalog, error) {
	definitions := make([]pack.Quest, 0, len(content.QuestIDs()))
	for _, id := range content.QuestIDs() {
		definition, ok := content.Quest(id)
		if !ok {
			continue
		}
		definitions = append(definitions, definition)
	}
	catalog, err := NewCatalog(definitions, content)
	if err != nil {
		return Catalog{}, fmt.Errorf("content pack %s: %w", content.ID(), err)
	}
	return catalog, nil
}

// NewCatalog builds a catalog directly, for a test that has definitions but no
// pack. `items` may be nil, which skips the reward-item check.
func NewCatalog(definitions []pack.Quest, items ItemSource) (Catalog, error) {
	catalog := Catalog{
		quests:     make(map[string]pack.Quest, len(definitions)),
		byStarter:  make(map[string][]string),
		byFinisher: make(map[string][]string),
		dependents: make(map[string][]string),
	}
	for _, definition := range definitions {
		if definition.ID == "" {
			return Catalog{}, fmt.Errorf("a quest definition carries no id")
		}
		if _, duplicate := catalog.quests[definition.ID]; duplicate {
			return Catalog{}, fmt.Errorf("quest %q is defined twice", definition.ID)
		}
		if err := checkObjectives(definition); err != nil {
			return Catalog{}, err
		}
		if err := checkPrerequisites(definition); err != nil {
			return Catalog{}, err
		}
		if err := checkRewards(definition, items); err != nil {
			return Catalog{}, err
		}
		catalog.quests[definition.ID] = definition
		catalog.order = append(catalog.order, definition.ID)
	}
	sort.Strings(catalog.order)

	for _, id := range catalog.order {
		definition := catalog.quests[id]
		if definition.StarterID != "" {
			catalog.byStarter[definition.StarterID] = append(catalog.byStarter[definition.StarterID], id)
		}
		if definition.FinisherID != "" {
			catalog.byFinisher[definition.FinisherID] = append(catalog.byFinisher[definition.FinisherID], id)
		}
		for _, prerequisite := range definition.Prerequisites {
			catalog.dependents[prerequisite.QuestID] = append(catalog.dependents[prerequisite.QuestID], id)
			if _, ok := catalog.quests[prerequisite.QuestID]; !ok {
				catalog.unresolved = append(catalog.unresolved, id+" -> "+prerequisite.QuestID)
			}
		}
	}
	return catalog, nil
}

// checkObjectives is rule 5.5.6 and the two shapes that cannot be evaluated.
func checkObjectives(definition pack.Quest) error {
	for index, objective := range definition.Objectives {
		switch objective.Kind {
		case pack.QuestObjectiveCountKill, pack.QuestObjectiveCountItem:
		case pack.QuestObjectiveCountSpecial, pack.QuestObjectiveUnspecified:
			return fmt.Errorf(
				"quest %q objective %d is of kind %s, which mechanics/quests.md rule 5.5.6 does not implement",
				definition.ID, index, objective.Kind,
			)
		default:
			return fmt.Errorf(
				"quest %q objective %d is of kind %s, which this build does not implement",
				definition.ID, index, objective.Kind,
			)
		}
		if objective.Limit > 0 && len(objective.TargetIDs) == 0 {
			return fmt.Errorf(
				"quest %q objective %d asks for %d of nothing: it names no target",
				definition.ID, index, objective.Limit,
			)
		}
	}
	return nil
}

// checkPrerequisites is rule 5.3.3's "at content-load rather than at evaluation
// time".
//
// An absent status is not "another value": the schema makes the field optional
// and `Finished` is the only status M2 knows, so an omitted one can only mean
// that. A status that is present and is something else is refused, because
// treating an unimplemented gate as satisfied is how a quest chain silently
// unlocks out of order.
func checkPrerequisites(definition pack.Quest) error {
	for index, prerequisite := range definition.Prerequisites {
		if prerequisite.QuestID == "" {
			return fmt.Errorf("quest %q prerequisite %d names no quest", definition.ID, index)
		}
		if prerequisite.RequiredStatus == "" {
			continue
		}
		if !strings.EqualFold(prerequisite.RequiredStatus, finishedStatus) {
			return fmt.Errorf(
				"quest %q prerequisite %d requires status %q; M2 implements only %q",
				definition.ID, index, prerequisite.RequiredStatus, finishedStatus,
			)
		}
	}
	return nil
}

// checkRewards refuses a grant the bag could never absorb.
//
// The pack compiler checks the same reference, and this is not redundant: the
// shard is what has to refuse a pack built by an older writer, and an item with
// no stack limit has no defined slot cost, so the failure would otherwise
// surface at the moment a player turns a quest in.
func checkRewards(definition pack.Quest, items ItemSource) error {
	if items == nil {
		return nil
	}
	for _, reward := range definition.Rewards.MandatoryItems {
		if reward.ItemID == "" {
			return fmt.Errorf("quest %q grants a reward item with no id", definition.ID)
		}
		if _, ok := items.Item(reward.ItemID); !ok {
			return fmt.Errorf(
				"quest %q grants %q, which the pack does not carry as an item",
				definition.ID, reward.ItemID,
			)
		}
	}
	return nil
}

// Definition returns one quest by canonical id.
func (catalog Catalog) Definition(id string) (pack.Quest, bool) {
	definition, ok := catalog.quests[id]
	return definition, ok
}

// IDs lists every quest the catalog carries, in canonical-id order.
func (catalog Catalog) IDs() []string {
	ids := make([]string, len(catalog.order))
	copy(ids, catalog.order)
	return ids
}

// Count is how many definitions the catalog holds.
func (catalog Catalog) Count() int { return len(catalog.quests) }

// StartedBy lists the quests one mob offers, in canonical-id order.
func (catalog Catalog) StartedBy(mobContentID string) []string {
	return append([]string(nil), catalog.byStarter[mobContentID]...)
}

// FinishedBy lists the quests one mob accepts the turn-in for.
func (catalog Catalog) FinishedBy(mobContentID string) []string {
	return append([]string(nil), catalog.byFinisher[mobContentID]...)
}

// Dependents lists the quests that name `questID` as a prerequisite. Rule 5.7.5
// re-evaluates exactly these after a turn-in commits.
func (catalog Catalog) Dependents(questID string) []string {
	return append([]string(nil), catalog.dependents[questID]...)
}

// UnresolvedPrerequisites lists `dependent -> prerequisite` edges whose
// prerequisite this pack does not carry.
//
// It is reported rather than refused. A pack covers one zone (ADR 0029) and
// reference data has quests gated on a quest in the zone next door, so a
// dangling edge is ordinary; what it is not is invisible, because every quest
// behind one is permanently unavailable and that is worth a line in the boot
// log.
func (catalog Catalog) UnresolvedPrerequisites() []string {
	return append([]string(nil), catalog.unresolved...)
}
