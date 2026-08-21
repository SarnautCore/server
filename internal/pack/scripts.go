package pack

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const (
	tableQuestScripts   = "quest-scripts"
	tableScriptTriggers = "script-triggers"
)

// ScriptTier is the coverage decision compiled into one script node.
type ScriptTier uint8

const (
	ScriptTierRefused ScriptTier = iota
	ScriptTierInert
	ScriptTierImplemented
)

// ScriptNode is the pack-native form of content.v1.ScriptNode. Keeping this
// type in pack lets the reader validate compiled rows without making pack
// depend on the evaluator.
type ScriptNode struct {
	Key    string
	Family string
	Opcode string
	Tier   ScriptTier
	Fields []ScriptField
}

type ScriptField struct {
	Name  string
	Value ScriptValue
}

type ScriptValueKind uint8

const (
	ScriptValueUnspecified ScriptValueKind = iota
	ScriptValueInteger
	ScriptValueDecimal
	ScriptValueBool
	ScriptValueText
	ScriptValueRef
	ScriptValueDurationMS
	ScriptValueNode
	ScriptValueList
)

type ScriptValue struct {
	Kind       ScriptValueKind
	Integer    int64
	Mantissa   int64
	Scale      int32
	Bool       bool
	Text       string
	Ref        ScriptRef
	DurationMS uint64
	Node       *ScriptNode
	List       []ScriptValue
}

type ScriptRef struct {
	ID      string
	RowType string
}

type QuestCounterBinding struct {
	CountID        string
	ObjectiveIndex uint32
	ObjectiveID    string
}

// QuestScript is one quest's activation trees and counter bindings.
type QuestScript struct {
	ID            string
	QuestID       string
	Counters      []QuestCounterBinding
	StartImpacts  []ScriptNode
	TriggerAgents []ScriptNode
}

type ScriptTrigger struct {
	ID   string
	Root ScriptNode
}

// QuestScript returns the compiled script row owned by questID.
func (p *Pack) QuestScript(questID string) (QuestScript, bool) {
	row, ok := p.questScripts[questID]
	return row, ok
}

// ScriptTrigger resolves one shared TriggerResource row by canonical id.
func (p *Pack) ScriptTrigger(id string) (ScriptTrigger, bool) {
	row, ok := p.scriptTriggers[id]
	return row, ok
}

// QuestScriptIDs lists quest ids carrying activation scripts in bytewise order.
func (p *Pack) QuestScriptIDs() []string {
	ids := make([]string, 0, len(p.questScripts))
	for id := range p.questScripts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ValidateQuestScriptCoverage checks the completeness required before the
// feature flag may admit count-special objectives. Packs may carry partial
// script rows while the flag is off, but the enabled shard may not offer a
// counter it cannot advance.
func (p *Pack) ValidateQuestScriptCoverage() error {
	globalCounts := make(map[string]string)
	for _, questID := range p.QuestScriptIDs() {
		row := p.questScripts[questID]
		quest, ok := p.quests[row.QuestID]
		if !ok {
			return fmt.Errorf("quest script %q names missing quest %q", row.ID, row.QuestID)
		}
		bound := make(map[uint32]bool)
		for _, binding := range row.Counters {
			if !validObjectiveID(row.QuestID, binding.ObjectiveID) {
				return fmt.Errorf("quest script %q binds %q to invalid stable objective id %q",
					row.ID, binding.CountID, binding.ObjectiveID)
			}
			if int(binding.ObjectiveIndex) >= len(quest.Objectives) {
				return fmt.Errorf("quest script %q binds %q to objective %d, but quest %q has %d objectives",
					row.ID, binding.CountID, binding.ObjectiveIndex, quest.ID, len(quest.Objectives))
			}
			if kind := quest.Objectives[binding.ObjectiveIndex].Kind; kind != QuestObjectiveCountSpecial {
				return fmt.Errorf("quest script %q binds %q to objective %d of kind %s, want count-special",
					row.ID, binding.CountID, binding.ObjectiveIndex, kind)
			}
			if owner, exists := globalCounts[binding.CountID]; exists {
				return fmt.Errorf("count id %q is bound by both quest scripts %q and %q", binding.CountID, owner, row.ID)
			}
			globalCounts[binding.CountID] = row.ID
			bound[binding.ObjectiveIndex] = true
		}
		for index, objective := range quest.Objectives {
			if objective.Kind == QuestObjectiveCountSpecial && !bound[uint32(index)] {
				return fmt.Errorf("quest script %q has no counter binding for count-special objective %d", row.ID, index)
			}
		}
	}
	for _, questID := range p.QuestIDs() {
		quest := p.quests[questID]
		for _, objective := range quest.Objectives {
			if objective.Kind != QuestObjectiveCountSpecial {
				continue
			}
			if _, ok := p.questScripts[questID]; !ok {
				return fmt.Errorf("count-special quest %q has no compiled quest script row", questID)
			}
			break
		}
	}
	return nil
}

func readQuestScripts(tables map[string]*table) (map[string]QuestScript, error) {
	loaded, ok := tables[tableQuestScripts]
	if !ok {
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_QUEST_SCRIPT; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf("%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableQuestScripts, contentv1.RowType(loaded.rowTypeID), want)
	}

	rows := make(map[string]QuestScript, loaded.rowCount)
	rowIDs := make(map[string]bool, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var wire contentv1.QuestScript
		if err := proto.Unmarshal(encoded, &wire); err != nil {
			return nil, fmt.Errorf("%w: decode quest script row: %w", ErrMalformedTable, err)
		}
		if wire.GetId() == "" || wire.GetQuestId() == "" {
			return nil, fmt.Errorf("%w: table %q holds a row with id %q and quest_id %q",
				ErrMalformedTable, tableQuestScripts, wire.GetId(), wire.GetQuestId())
		}
		if rowIDs[wire.GetId()] {
			return nil, fmt.Errorf("%w: table %q repeats row id %q", ErrMalformedTable, tableQuestScripts, wire.GetId())
		}
		if _, exists := rows[wire.GetQuestId()]; exists {
			return nil, fmt.Errorf("%w: table %q repeats quest_id %q", ErrMalformedTable, tableQuestScripts, wire.GetQuestId())
		}

		row := QuestScript{ID: wire.GetId(), QuestID: wire.GetQuestId()}
		counts := make(map[string]bool, len(wire.GetCounters()))
		for _, binding := range wire.GetCounters() {
			if binding.GetCountId() == "" || counts[binding.GetCountId()] {
				return nil, fmt.Errorf("%w: quest script %q has an empty or repeated count_id %q",
					ErrMalformedTable, wire.GetId(), binding.GetCountId())
			}
			if !validObjectiveID(wire.GetQuestId(), binding.GetObjectiveId()) {
				return nil, fmt.Errorf("%w: quest script %q binds %q to invalid stable objective id %q",
					ErrMalformedTable, wire.GetId(), binding.GetCountId(), binding.GetObjectiveId())
			}
			counts[binding.GetCountId()] = true
			row.Counters = append(row.Counters, QuestCounterBinding{
				CountID: binding.GetCountId(), ObjectiveIndex: binding.GetObjectiveIndex(), ObjectiveID: binding.GetObjectiveId(),
			})
		}
		for index, node := range wire.GetStartImpacts() {
			converted, err := readScriptNode(wire.GetId(), fmt.Sprintf("start_impacts[%d]", index), node)
			if err != nil {
				return nil, err
			}
			row.StartImpacts = append(row.StartImpacts, converted)
		}
		for index, node := range wire.GetTriggerAgents() {
			converted, err := readScriptNode(wire.GetId(), fmt.Sprintf("trigger_agents[%d]", index), node)
			if err != nil {
				return nil, err
			}
			row.TriggerAgents = append(row.TriggerAgents, converted)
		}
		rowIDs[row.ID] = true
		rows[row.QuestID] = row
	}
	return rows, nil
}

func validObjectiveID(questID, objectiveID string) bool {
	prefix := questID + ".objective."
	if !strings.HasPrefix(objectiveID, prefix) || len(objectiveID) != len(prefix)+64 {
		return false
	}
	for _, character := range objectiveID[len(prefix):] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func readSpawnTableMobs(tables map[string]*table) (map[string][]string, error) {
	loaded, ok := tables[tableSpawnTables]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMissingTable, tableSpawnTables)
	}
	if want := contentv1.RowType_ROW_TYPE_SPAWN_TABLE; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf("%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableSpawnTables, contentv1.RowType(loaded.rowTypeID), want)
	}
	result := make(map[string][]string, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.SpawnTable
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode spawn table row: %w", ErrMalformedTable, err)
		}
		if row.GetId() == "" {
			return nil, fmt.Errorf("%w: table %q holds a row with no id", ErrMalformedTable, tableSpawnTables)
		}
		if _, exists := result[row.GetId()]; exists {
			return nil, fmt.Errorf("%w: table %q repeats id %q", ErrMalformedTable, tableSpawnTables, row.GetId())
		}
		for _, entry := range row.GetEntries() {
			if strings.HasPrefix(entry.GetObjectId(), mobIDPrefix) && !inert(entry.GetSpawnTime()) {
				result[row.GetId()] = append(result[row.GetId()], entry.GetObjectId())
			}
		}
	}
	return result, nil
}

func readScriptTriggers(tables map[string]*table) (map[string]ScriptTrigger, error) {
	loaded, ok := tables[tableScriptTriggers]
	if !ok {
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_SCRIPT_TRIGGER; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf("%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableScriptTriggers, contentv1.RowType(loaded.rowTypeID), want)
	}

	rows := make(map[string]ScriptTrigger, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var wire contentv1.ScriptTrigger
		if err := proto.Unmarshal(encoded, &wire); err != nil {
			return nil, fmt.Errorf("%w: decode script trigger row: %w", ErrMalformedTable, err)
		}
		if wire.GetId() == "" || wire.GetRoot() == nil {
			return nil, fmt.Errorf("%w: table %q holds trigger id %q with no root",
				ErrMalformedTable, tableScriptTriggers, wire.GetId())
		}
		if _, exists := rows[wire.GetId()]; exists {
			return nil, fmt.Errorf("%w: table %q repeats id %q", ErrMalformedTable, tableScriptTriggers, wire.GetId())
		}
		root, err := readScriptNode(wire.GetId(), "root", wire.GetRoot())
		if err != nil {
			return nil, err
		}
		rows[wire.GetId()] = ScriptTrigger{ID: wire.GetId(), Root: root}
	}
	return rows, nil
}

func readScriptNode(rowID, pointer string, wire *contentv1.ScriptNode) (ScriptNode, error) {
	fail := func(format string, arguments ...any) (ScriptNode, error) {
		return ScriptNode{}, fmt.Errorf("%w: script row %q node %s %s",
			ErrMalformedTable, rowID, pointer, fmt.Sprintf(format, arguments...))
	}
	if wire == nil {
		return fail("is nil")
	}
	if wire.GetNodeKey() == "" || wire.GetFamily() == "" || wire.GetOpcode() == "" {
		return fail("has key %q, family %q and opcode %q", wire.GetNodeKey(), wire.GetFamily(), wire.GetOpcode())
	}
	node := ScriptNode{
		Key: wire.GetNodeKey(), Family: wire.GetFamily(), Opcode: wire.GetOpcode(), Tier: scriptTier(wire.GetTier()),
	}
	previous := ""
	for index, field := range wire.GetFields() {
		if field.GetName() == "" {
			return fail("field %d has no name", index)
		}
		if index > 0 && field.GetName() <= previous {
			return fail("fields are not strictly bytewise sorted at %q after %q", field.GetName(), previous)
		}
		value, err := readScriptValue(rowID, pointer+"/"+field.GetName(), field.GetValue())
		if err != nil {
			return ScriptNode{}, err
		}
		node.Fields = append(node.Fields, ScriptField{Name: field.GetName(), Value: value})
		previous = field.GetName()
	}
	return node, nil
}

func readScriptValue(rowID, pointer string, wire *contentv1.ScriptValue) (ScriptValue, error) {
	fail := func(format string, arguments ...any) (ScriptValue, error) {
		return ScriptValue{}, fmt.Errorf("%w: script row %q value %s %s",
			ErrMalformedTable, rowID, pointer, fmt.Sprintf(format, arguments...))
	}
	if wire == nil || wire.GetValue() == nil {
		return fail("is unset")
	}
	switch value := wire.GetValue().(type) {
	case *contentv1.ScriptValue_Integer:
		return ScriptValue{Kind: ScriptValueInteger, Integer: value.Integer}, nil
	case *contentv1.ScriptValue_Decimal:
		if value.Decimal == nil {
			return fail("has a nil decimal")
		}
		return ScriptValue{Kind: ScriptValueDecimal, Mantissa: value.Decimal.GetMantissa(), Scale: value.Decimal.GetScale()}, nil
	case *contentv1.ScriptValue_Boolean:
		return ScriptValue{Kind: ScriptValueBool, Bool: value.Boolean}, nil
	case *contentv1.ScriptValue_Text:
		return ScriptValue{Kind: ScriptValueText, Text: value.Text}, nil
	case *contentv1.ScriptValue_Reference:
		if value.Reference == nil || value.Reference.GetId() == "" {
			return fail("has a reference with no id")
		}
		return ScriptValue{Kind: ScriptValueRef, Ref: ScriptRef{ID: value.Reference.GetId(), RowType: value.Reference.GetRowType()}}, nil
	case *contentv1.ScriptValue_DurationMs:
		return ScriptValue{Kind: ScriptValueDurationMS, DurationMS: value.DurationMs}, nil
	case *contentv1.ScriptValue_Node:
		node, err := readScriptNode(rowID, pointer, value.Node)
		if err != nil {
			return ScriptValue{}, err
		}
		return ScriptValue{Kind: ScriptValueNode, Node: &node}, nil
	case *contentv1.ScriptValue_List:
		if value.List == nil {
			return fail("has a nil list")
		}
		result := ScriptValue{Kind: ScriptValueList}
		for index, entry := range value.List.GetValues() {
			converted, err := readScriptValue(rowID, fmt.Sprintf("%s[%d]", pointer, index), entry)
			if err != nil {
				return ScriptValue{}, err
			}
			result.List = append(result.List, converted)
		}
		return result, nil
	default:
		return fail("has unknown oneof member %T", value)
	}
}

func scriptTier(tier contentv1.CoverageTier) ScriptTier {
	switch tier {
	case contentv1.CoverageTier_COVERAGE_TIER_INERT:
		return ScriptTierInert
	case contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED:
		return ScriptTierImplemented
	default:
		return ScriptTierRefused
	}
}
