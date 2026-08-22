package session

import (
	"math"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
)

// PackQuestScriptSource adapts validated compiled pack rows to the session
// driver's lookup contract. It contains no fallback data.
type PackQuestScriptSource struct {
	content  *pack.Pack
	counters map[string]CounterBinding
}

// WarriorAction adapts one validated native action row to the script-action
// host. The row already contains product ids and typed nodes.
func (source *PackQuestScriptSource) WarriorAction(actionID string) (WarriorAction, bool) {
	if source == nil || source.content == nil {
		return WarriorAction{}, false
	}
	row, ok := source.content.NativeAction(actionID)
	if !ok {
		return WarriorAction{}, false
	}
	action := WarriorAction{
		AbilityID: row.ID, ActionGroupID: row.ActionGroupID,
		PrepareDuration:        row.PrepareDuration,
		TriggersGlobalCooldown: row.TriggersGCD,
		IgnoresGlobalCooldown:  row.IgnoresGCD,
	}
	if row.Cooldown != nil {
		action.CooldownGroupID = row.Cooldown.GroupID
		switch {
		case row.Cooldown.Scaler == "":
			action.Cooldown = row.Cooldown.Duration
		case strings.EqualFold(row.Cooldown.Scaler, "weapon-speed"):
			base, ok := exactSecondsDuration(row.Cooldown.Base)
			if !ok {
				return WarriorAction{}, false
			}
			action.Cooldown = base
			action.CooldownScalesByWeaponSpeed = true
			action.CooldownSource = canonicalAttackSource(row.Resource, "Mainhand")
		default:
			return WarriorAction{}, false
		}
	}
	if row.Resource != nil {
		action.ResourceKind = row.Resource.Kind
		action.ResourceCost = script.Decimal{
			Mantissa: row.Resource.Cost.Mantissa,
			Scale:    row.Resource.Cost.Scale,
		}
		action.ScaleCostByWeaponSpeed = row.Resource.ScaleByWeaponSpeed
		action.ResourceSource = canonicalAttackSource(row.Resource, "Mainhand")
	}
	for _, node := range row.TargetImpacts {
		action.TargetImpacts = append(action.TargetImpacts, script.FromPackNode(node))
	}
	for _, node := range row.CasterConditions {
		action.CasterConditions = append(action.CasterConditions, script.FromPackNode(node))
	}
	return action, true
}

func exactSecondsDuration(value pack.Decimal) (time.Duration, bool) {
	mantissa, scale := value.Mantissa, value.Scale
	if mantissa < 0 || scale < 0 || scale > 18 {
		return 0, false
	}
	for scale > 9 && mantissa%10 == 0 {
		mantissa /= 10
		scale--
	}
	if scale > 9 {
		return 0, false
	}
	for scale < 9 {
		if mantissa > math.MaxInt64/10 {
			return 0, false
		}
		mantissa *= 10
		scale++
	}
	return time.Duration(mantissa), true
}

func canonicalAttackSource(resource *pack.NativeActionResource, fallback string) string {
	if resource == nil || resource.Source == "" {
		return fallback
	}
	switch {
	case strings.EqualFold(resource.Source, "mainhand"):
		return "Mainhand"
	case strings.EqualFold(resource.Source, "ranged"):
		return "Ranged"
	default:
		return resource.Source
	}
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

// HasDestinationIndex distinguishes an older pack from a present index that
// simply lacks one requested pair.
func (source *PackQuestScriptSource) HasDestinationIndex() bool {
	return source != nil && source.content != nil && source.content.HasMapLocatorIndex()
}

// LocateDestination resolves the exact product map slug and script id carried
// by compiled content. Source-format aliases are not accepted here.
func (source *PackQuestScriptSource) LocateDestination(mapRef script.Ref, scriptID string) (script.Position, bool) {
	if source == nil || source.content == nil || mapRef.RowType != "map" {
		return script.Position{}, false
	}
	position, ok := source.content.MapLocator(mapRef.ID, scriptID)
	return script.Position{X: position.X, Y: position.Y, Z: position.Z}, ok
}

// SummonMob resolves a mob row already validated by the compiled pack. Map
// locator loading is deliberately separate from this content lookup.
func (source *PackQuestScriptSource) SummonMob(ref script.Ref) (gametypes.Mob, bool) {
	if source == nil || source.content == nil || ref.RowType != "mob" {
		return gametypes.Mob{}, false
	}
	return source.content.Mob(ref.ID)
}
