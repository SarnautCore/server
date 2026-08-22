package pack

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

// tableQuests holds one row per quest definition.
const tableQuests = "quests"

// QuestObjectiveKind is one countable goal shape (mechanics/quests.md rule
// 5.5).
//
// All three kinds reference data carries are represented, including the one M2
// does not implement. Dropping `count-special` here would hand the shard a
// quest with fewer objectives than the author wrote, which is a quest that
// completes when it should not; carrying it is what lets `internal/quests`
// refuse the definition by name at load (rule 5.5.6).
type QuestObjectiveKind uint8

const (
	QuestObjectiveUnspecified QuestObjectiveKind = iota
	// QuestObjectiveCountKill is advanced by a kill, rule 5.4.
	QuestObjectiveCountKill
	// QuestObjectiveCountItem tracks how many of an item the character holds,
	// recomputed rather than incremented (rule 5.5.3).
	QuestObjectiveCountItem
	// QuestObjectiveCountSpecial is driven by quest script impacts. M2 does not
	// implement the impact system (rule 5.5.6).
	QuestObjectiveCountSpecial
)

func (kind QuestObjectiveKind) String() string {
	switch kind {
	case QuestObjectiveCountKill:
		return "count-kill"
	case QuestObjectiveCountItem:
		return "count-item"
	case QuestObjectiveCountSpecial:
		return "count-special"
	case QuestObjectiveUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("quest-objective-kind(%d)", uint8(kind))
	}
}

// QuestObjective is one countable goal. Its position in [Quest.Objectives] is
// the counter index (rule 7.5).
type QuestObjective struct {
	Kind  QuestObjectiveKind
	Limit int32
	// TargetIDs are content ids; any one of them counts.
	TargetIDs []string
	// Internal hides the objective from the client's quest log. It is still
	// evaluated (rule 5.5.7).
	Internal bool
	// ShowCount renders `n / limit` rather than a plain incomplete marker.
	ShowCount bool
	// CounterKey is the localization key of the counter's display name.
	CounterKey string
	// RemoveOnAbandon destroys the tracked items when the quest is abandoned
	// (rule 5.5.5).
	RemoveOnAbandon bool
}

// QuestPrerequisite is one gate on offering a quest (rule 5.3.3).
type QuestPrerequisite struct {
	QuestID string
	// RequiredStatus is the authored status name, verbatim. `Finished` is the
	// only value M2 accepts, and the check is the quest module's at load rather
	// than this reader's: a pack is not malformed for carrying a status a later
	// milestone will implement.
	RequiredStatus string
}

// QuestRewardItem is one item a turn-in grants.
type QuestRewardItem struct {
	ItemID string
	Count  int32
	Hidden bool
}

// QuestRewards is the whole grant, applied together or not at all (rule 5.7).
type QuestRewards struct {
	Experience int64
	Money      int64
	Honor      int64
	// MandatoryItems are always granted.
	MandatoryItems []QuestRewardItem
	// AlternativeItems are offered as a choice. M2 grants the mandatory list
	// and nothing here; quests.md section 7.2 owns that gap.
	AlternativeItems []QuestRewardItem
}

// Quest is one immutable quest definition (mechanics/quests.md section 4).
type Quest struct {
	ID     string
	ZoneID string
	Level  uint32
	// RequiredLevel of zero means no level gate (rule 5.3.2).
	RequiredLevel uint32
	QuestType     string
	// StarterID and FinisherID are mob content ids. They are not necessarily
	// the same NPC, and the finisher is not necessarily in this zone
	// (section 7.4).
	StarterID     string
	FinisherID    string
	CanCancel     bool
	Prerequisites []QuestPrerequisite
	Objectives    []QuestObjective
	Rewards       QuestRewards

	NameKey   string
	GoalKey   string
	StartKey  string
	CheckKey  string
	FinishKey string
	// RepeatPeriod is the raw authored QuestResource.cooldown value. Its source
	// does not establish a time unit, so the pack preserves it as an integer.
	RepeatPeriod int32
}

// Quest resolves one definition by canonical id.
func (p *Pack) Quest(id string) (Quest, bool) {
	value, ok := p.quests[id]
	return value, ok
}

// QuestIDs lists every quest the pack carries, in canonical-id order.
func (p *Pack) QuestIDs() []string {
	ids := make([]string, 0, len(p.quests))
	for id := range p.quests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// readQuests decodes every quest row eagerly.
//
// Quests are read at load for the same reason loot trees are: there are three
// figures of them at most, and a definition that cannot be played is a content
// mistake that should stop a boot rather than surface when a player clicks an
// NPC. What this reader does not do is decide which definitions are playable —
// that is `internal/quests`, which owns rule 5.5.6 and can name the quest.
func readQuests(tables map[string]*table) (map[string]Quest, error) {
	loaded, ok := tables[tableQuests]
	if !ok {
		// A pack with no quest table is legal: a zone can have nothing to do.
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_QUEST; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf(
			"%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableQuests, contentv1.RowType(loaded.rowTypeID), want,
		)
	}

	quests := make(map[string]Quest, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.Quest
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode quest row: %w", ErrMalformedTable, err)
		}
		if row.GetId() == "" {
			return nil, fmt.Errorf("%w: table %q holds a row with no id", ErrMalformedTable, tableQuests)
		}
		quest := Quest{
			ID:            row.GetId(),
			ZoneID:        row.GetZoneId(),
			Level:         row.GetLevel(),
			RequiredLevel: row.GetRequiredLevel(),
			QuestType:     row.GetQuestType(),
			StarterID:     row.GetStarterId(),
			FinisherID:    row.GetFinisherId(),
			CanCancel:     row.GetCanCancel(),
			NameKey:       row.GetNameKey(),
			GoalKey:       row.GetGoalKey(),
			StartKey:      row.GetStartKey(),
			CheckKey:      row.GetCheckKey(),
			FinishKey:     row.GetFinishKey(),
			RepeatPeriod:  row.GetRepeatPeriod(),
		}
		for _, prerequisite := range row.GetPrerequisites() {
			quest.Prerequisites = append(quest.Prerequisites, QuestPrerequisite{
				QuestID:        prerequisite.GetQuestId(),
				RequiredStatus: prerequisite.GetRequiredStatus(),
			})
		}
		for index, objective := range row.GetObjectives() {
			if objective.GetLimit() < 0 {
				return nil, fmt.Errorf(
					"%w: quest %q objective %d declares a negative limit %d",
					ErrMalformedTable, quest.ID, index, objective.GetLimit(),
				)
			}
			quest.Objectives = append(quest.Objectives, QuestObjective{
				Kind:            questObjectiveKind(objective.GetKind()),
				Limit:           objective.GetLimit(),
				TargetIDs:       append([]string(nil), objective.GetTargetIds()...),
				Internal:        objective.GetInternal(),
				ShowCount:       objective.GetShowCount(),
				CounterKey:      objective.GetCounterKey(),
				RemoveOnAbandon: objective.GetRemoveOnAbandon(),
			})
		}
		quest.Rewards = QuestRewards{
			Experience:       row.GetRewards().GetExperience(),
			Money:            row.GetRewards().GetMoney(),
			Honor:            row.GetRewards().GetHonor(),
			MandatoryItems:   questRewardItems(row.GetRewards().GetMandatoryItems()),
			AlternativeItems: questRewardItems(row.GetRewards().GetAlternativeItems()),
		}
		quests[quest.ID] = quest
	}
	return quests, nil
}

func questRewardItems(rows []*contentv1.QuestRewardItem) []QuestRewardItem {
	if len(rows) == 0 {
		return nil
	}
	items := make([]QuestRewardItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, QuestRewardItem{
			ItemID: row.GetItemId(),
			Count:  row.GetCount(),
			Hidden: row.GetHidden(),
		})
	}
	return items
}

// questObjectiveKind maps the wire enum onto the domain one. An unknown value
// becomes [QuestObjectiveUnspecified], which the quest module refuses by name
// alongside `count-special`: a kind this build has never heard of is exactly as
// unplayable as one it has heard of and does not implement.
func questObjectiveKind(kind contentv1.QuestObjectiveKind) QuestObjectiveKind {
	switch kind {
	case contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_COUNT_KILL:
		return QuestObjectiveCountKill
	case contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_COUNT_ITEM:
		return QuestObjectiveCountItem
	case contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_COUNT_SPECIAL:
		return QuestObjectiveCountSpecial
	case contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_UNSPECIFIED:
		return QuestObjectiveUnspecified
	default:
		return QuestObjectiveUnspecified
	}
}
