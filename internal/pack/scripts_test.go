package pack

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const (
	compiledQuestID = "quest.inst-league1.compiled-count"
	compiledCountID = "questcount.inst-league1.compiled-count.0"
)

func TestPackReadsAndValidatesCompiledQuestScripts(t *testing.T) {
	t.Parallel()
	directory := compiledScriptPack(t, false)

	content, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := content.ValidateQuestScriptCoverage(); err != nil {
		t.Fatalf("ValidateQuestScriptCoverage() error = %v", err)
	}
	quest, ok := content.Quest(compiledQuestID)
	if !ok || quest.RepeatPeriod != -17 {
		t.Fatalf("Quest(%q) repeat period = %d, %v; want -17, true",
			compiledQuestID, quest.RepeatPeriod, ok)
	}
	row, ok := content.QuestScript(compiledQuestID)
	if !ok {
		t.Fatalf("QuestScript(%q) = false", compiledQuestID)
	}
	if row.ID != "script.inst-league1.compiled-count" || len(row.Counters) != 1 || len(row.StartImpacts) != 2 {
		t.Fatalf("quest script = %#v", row)
	}
	if got := row.StartImpacts[0]; got.Tier != ScriptTierImplemented || got.Opcode != "ImpactSequence" {
		t.Errorf("implemented node = %#v", got)
	}
	if got := row.StartImpacts[1]; got.Tier != ScriptTierInert || got.Opcode != "FutureCosmeticImpact" {
		t.Errorf("inert unknown node = %#v", got)
	}
	trigger, ok := content.ScriptTrigger("trigger.inst-league1.compiled-count")
	if !ok || trigger.Root.Family != "trigger" {
		t.Fatalf("ScriptTrigger() = %#v, %v", trigger, ok)
	}
}

func TestPackMapsUnspecifiedCoverageToRefused(t *testing.T) {
	t.Parallel()
	content, err := Load(compiledScriptPack(t, true), Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	row, _ := content.QuestScript(compiledQuestID)
	if got := row.StartImpacts[1].Tier; got != ScriptTierRefused {
		t.Fatalf("unspecified tier = %v, want refused", got)
	}
}

func TestPackRejectsUnsortedScriptFields(t *testing.T) {
	t.Parallel()
	directory := copyFixture(t)
	row := &contentv1.QuestScript{
		Id: "script.bad", QuestId: "quest.bad",
		StartImpacts: []*contentv1.ScriptNode{{
			NodeKey: "script.bad/startImpacts[0]", Family: "impact", Opcode: "ImpactBad",
			Tier: contentv1.CoverageTier_COVERAGE_TIER_INERT,
			Fields: []*contentv1.ScriptField{
				{Name: "z", Value: textValue("last")},
				{Name: "a", Value: textValue("first")},
			},
		}},
	}
	replaceCompiledTable(t, directory, tableQuestScripts, contentv1.RowType_ROW_TYPE_QUEST_SCRIPT,
		[]compiledRow{{id: row.GetId(), message: row}})
	reseal(t, directory)

	_, err := Load(directory, Options{})
	if !errors.Is(err, ErrMalformedTable) || !strings.Contains(err.Error(), "not strictly bytewise sorted") {
		t.Fatalf("Load() error = %v, want malformed unsorted fields", err)
	}
}

func compiledScriptPack(t *testing.T, unspecified bool) string {
	t.Helper()
	directory := copyFixture(t)
	quest := &contentv1.Quest{
		Id: compiledQuestID, ZoneId: "zone.inst-league1", Level: 1,
		RepeatPeriod: -17,
		Objectives: []*contentv1.QuestObjective{{
			Kind:  contentv1.QuestObjectiveKind_QUEST_OBJECTIVE_KIND_COUNT_SPECIAL,
			Limit: 3, ShowCount: true,
		}},
		Rewards: &contentv1.QuestRewards{},
	}
	replaceCompiledTable(t, directory, tableQuests, contentv1.RowType_ROW_TYPE_QUEST,
		[]compiledRow{{id: quest.GetId(), message: quest}})

	unknownTier := contentv1.CoverageTier_COVERAGE_TIER_INERT
	if unspecified {
		unknownTier = contentv1.CoverageTier_COVERAGE_TIER_UNSPECIFIED
	}
	row := &contentv1.QuestScript{
		Id: "script.inst-league1.compiled-count", QuestId: compiledQuestID,
		Counters: []*contentv1.QuestCounterBinding{{
			CountId: compiledCountID, ObjectiveIndex: 0,
			ObjectiveId: compiledQuestID + ".objective." + strings.Repeat("a", 64),
		}},
		StartImpacts: []*contentv1.ScriptNode{
			{
				NodeKey: "script.inst-league1.compiled-count/startImpacts[0]", Family: "impact",
				Opcode: "ImpactSequence", Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
				Fields: []*contentv1.ScriptField{{Name: "impacts", Value: listValue(nodeValue(&contentv1.ScriptNode{
					NodeKey: "script.inst-league1.compiled-count/startImpacts[0]/impacts[0]",
					Family:  "impact", Opcode: "ImpactIncreaseQuestCount",
					Tier:   contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
					Fields: []*contentv1.ScriptField{{Name: "count", Value: integerValue(1)}},
				}))}},
			},
			{
				NodeKey: "script.inst-league1.compiled-count/startImpacts[1]", Family: "impact",
				Opcode: "FutureCosmeticImpact", Tier: unknownTier,
			},
		},
	}
	replaceCompiledTable(t, directory, tableQuestScripts, contentv1.RowType_ROW_TYPE_QUEST_SCRIPT,
		[]compiledRow{{id: row.GetId(), message: row}})

	trigger := &contentv1.ScriptTrigger{Id: "trigger.inst-league1.compiled-count", Root: &contentv1.ScriptNode{
		NodeKey: "trigger.inst-league1.compiled-count/root", Family: "trigger",
		Opcode: "TriggerResource", Tier: contentv1.CoverageTier_COVERAGE_TIER_IMPLEMENTED,
	}}
	replaceCompiledTable(t, directory, tableScriptTriggers, contentv1.RowType_ROW_TYPE_SCRIPT_TRIGGER,
		[]compiledRow{{id: trigger.GetId(), message: trigger}})
	reseal(t, directory)
	return directory
}

type compiledRow struct {
	id      string
	message proto.Message
}

func replaceCompiledTable(t *testing.T, directory, name string, rowType contentv1.RowType, rows []compiledRow) {
	t.Helper()
	payload := encodeCompiledTable(t, rowType, rows)
	file := filepath.ToSlash(filepath.Join(tablesDirectory, name+".sptbl"))
	writeFile(t, filepath.Join(directory, filepath.FromSlash(file)), payload)
	document := loadManifest(t, directory)
	entry := manifestTableEntry{Name: name, File: file, RowType: rowType.String(), Rows: uint32(len(rows))}
	replaced := false
	for index := range document.Tables {
		if document.Tables[index].Name == name {
			document.Tables[index] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		document.Tables = append(document.Tables, entry)
	}
	saveManifest(t, directory, document)
}

func encodeCompiledTable(t *testing.T, rowType contentv1.RowType, rows []compiledRow) []byte {
	t.Helper()
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(row.message)
		if err != nil {
			t.Fatalf("marshal row %q: %v", row.id, err)
		}
		encoded[index] = payload
	}
	keyOffset := uint32(tableHeaderBytes)
	rowOffset := keyOffset + uint32(tableKeyEntrySize*len(rows))
	dataOffset := rowOffset + uint32(4*(len(rows)+1))
	dataBytes := 0
	for _, row := range encoded {
		dataBytes += len(row)
	}
	payload := make([]byte, int(dataOffset)+dataBytes)
	copy(payload[:4], tableMagic)
	binary.LittleEndian.PutUint16(payload[4:], tableFormatV1)
	binary.LittleEndian.PutUint16(payload[6:], tableFlagKeyIndex)
	binary.LittleEndian.PutUint32(payload[8:], uint32(rowType))
	binary.LittleEndian.PutUint32(payload[12:], uint32(len(rows)))
	binary.LittleEndian.PutUint32(payload[16:], keyOffset)
	binary.LittleEndian.PutUint32(payload[20:], rowOffset)
	binary.LittleEndian.PutUint32(payload[24:], dataOffset)
	binary.LittleEndian.PutUint32(payload[28:], uint32(dataBytes))

	type keyEntry struct {
		hash    uint64
		ordinal uint32
	}
	keys := make([]keyEntry, len(rows))
	for index, row := range rows {
		keys[index] = keyEntry{hash: keyHash(row.id), ordinal: uint32(index)}
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].hash < keys[right].hash || keys[left].hash == keys[right].hash && keys[left].ordinal < keys[right].ordinal
	})
	for index, key := range keys {
		base := int(keyOffset) + index*tableKeyEntrySize
		binary.LittleEndian.PutUint64(payload[base:], key.hash)
		binary.LittleEndian.PutUint32(payload[base+8:], key.ordinal)
	}
	offset := 0
	for index, row := range encoded {
		binary.LittleEndian.PutUint32(payload[int(rowOffset)+index*4:], uint32(offset))
		copy(payload[int(dataOffset)+offset:], row)
		offset += len(row)
	}
	binary.LittleEndian.PutUint32(payload[int(rowOffset)+len(rows)*4:], uint32(offset))
	return payload
}

func integerValue(value int64) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Integer{Integer: value}}
}

func textValue(value string) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Text{Text: value}}
}

func nodeValue(value *contentv1.ScriptNode) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_Node{Node: value}}
}

func listValue(values ...*contentv1.ScriptValue) *contentv1.ScriptValue {
	return &contentv1.ScriptValue{Value: &contentv1.ScriptValue_List{List: &contentv1.ScriptValueList{Values: values}}}
}
