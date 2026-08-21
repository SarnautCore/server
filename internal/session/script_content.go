package session

import (
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
)

// PackQuestScriptSource adapts validated compiled pack rows to the session
// driver's lookup contract. It contains no fallback data.
type PackQuestScriptSource struct {
	content  *pack.Pack
	counters map[string]CounterBinding
}

// NewPackQuestScriptSource indexes counter bindings once at shard startup.
func NewPackQuestScriptSource(content *pack.Pack) *PackQuestScriptSource {
	source := &PackQuestScriptSource{content: content, counters: make(map[string]CounterBinding)}
	if content == nil {
		return source
	}
	for _, questID := range content.QuestScriptIDs() {
		row, _ := content.QuestScript(questID)
		for _, binding := range row.Counters {
			source.counters[binding.CountID] = CounterBinding{
				QuestID: row.QuestID, ObjectiveID: binding.ObjectiveID, ObjectiveIndex: int(binding.ObjectiveIndex),
			}
		}
	}
	return source
}

func (source *PackQuestScriptSource) QuestActivation(questID string) (QuestActivation, bool) {
	if source == nil || source.content == nil {
		return QuestActivation{}, false
	}
	row, ok := source.content.QuestScript(questID)
	if !ok {
		return QuestActivation{}, false
	}
	activation := QuestActivation{
		StartImpacts:  make([]*script.Node, 0, len(row.StartImpacts)),
		TriggerAgents: make([]*script.Node, 0, len(row.TriggerAgents)),
	}
	for _, node := range row.StartImpacts {
		activation.StartImpacts = append(activation.StartImpacts, script.FromPackNode(node))
	}
	for _, node := range row.TriggerAgents {
		activation.TriggerAgents = append(activation.TriggerAgents, script.FromPackNode(node))
	}
	return activation, true
}

func (source *PackQuestScriptSource) Trigger(ref script.Ref) (*script.Node, bool) {
	if source == nil || source.content == nil {
		return nil, false
	}
	row, ok := source.content.ScriptTrigger(ref.ID)
	if !ok {
		return nil, false
	}
	return script.FromPackNode(row.Root), true
}

func (source *PackQuestScriptSource) Counter(ref script.Ref) (CounterBinding, bool) {
	if source == nil {
		return CounterBinding{}, false
	}
	binding, ok := source.counters[ref.ID]
	return binding, ok
}

func (source *PackQuestScriptSource) SpawnTableMobs(ref script.Ref) []string {
	if source == nil || source.content == nil {
		return nil
	}
	return source.content.SpawnTableMobs(ref.ID)
}
