package worldscript

import (
	"fmt"
	"math"
	"sort"
)

// State is the complete restart boundary for authored world-script state.
// It contains no source paths or engine objects.
type State struct {
	Variables     map[string]int64
	DisabledZones map[string]bool
	OpenedDevices map[string]bool
	Executed      map[string]bool
}

func (state State) clone() State {
	return State{
		Variables: cloneMap(state.Variables), DisabledZones: cloneMap(state.DisabledZones),
		OpenedDevices: cloneMap(state.OpenedDevices), Executed: cloneMap(state.Executed),
	}
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	copy := make(map[K]V, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

// Snapshot copies the restart state under the zone lock.
func (module *Module) Snapshot() State {
	var result State
	_ = module.zone.GameCommand(func(_ Tick) error {
		result = module.state.clone()
		return nil
	})
	return result
}

// Variable returns one map variable.
func (module *Module) Variable(id string) (int64, bool) {
	var value int64
	var ok bool
	_ = module.zone.GameCommand(func(_ Tick) error {
		value, ok = module.state.Variables[id]
		return nil
	})
	return value, ok
}

// AddVariable applies one idempotent ImpactScriptZoneVariableSummand command.
func (module *Module) AddVariable(id string, delta int64, executionKey string) error {
	return module.zone.GameCommand(func(_ Tick) error {
		if _, ok := module.state.Variables[id]; !ok {
			return fmt.Errorf("worldscript: unknown variable %q", id)
		}
		if module.alreadyExecuted(executionKey) {
			return nil
		}
		current := module.state.Variables[id]
		if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < math.MinInt64-delta) {
			return fmt.Errorf("worldscript: variable %q would overflow", id)
		}
		module.state.Variables[id] = current + delta
		module.markExecuted(executionKey)
		return nil
	})
}

// SetZoneDisabled applies one idempotent ImpactScriptZoneSetDisabled command.
func (module *Module) SetZoneDisabled(id string, disabled bool, executionKey string) error {
	return module.zone.GameCommand(func(_ Tick) error {
		if _, ok := module.zones[id]; !ok {
			return fmt.Errorf("worldscript: unknown script zone %q", id)
		}
		if module.alreadyExecuted(executionKey) {
			return nil
		}
		module.state.DisabledZones[id] = disabled
		if disabled {
			delete(module.members, id)
		}
		module.markExecuted(executionKey)
		return nil
	})
}

// EmitCue is the typed host for ImpactClientData and text messages.
func (module *Module) EmitCue(cue Cue) error {
	if cue.Kind != CueClientData && cue.Kind != CueTextMessage {
		return fmt.Errorf("worldscript: unsupported direct cue kind %d", cue.Kind)
	}
	if cue.ResourceID == "" {
		return fmt.Errorf("worldscript: cue resource id is required")
	}
	return module.zone.GameCommand(func(_ Tick) error {
		if module.alreadyExecuted(cue.ExecutionKey) {
			return nil
		}
		module.cues = append(module.cues, cue)
		module.markExecuted(cue.ExecutionKey)
		return nil
	})
}

// DrainCues transfers queued product cues in simulation order.
func (module *Module) DrainCues() []Cue {
	var result []Cue
	_ = module.zone.GameCommand(func(_ Tick) error {
		result = append(result, module.cues...)
		module.cues = module.cues[:0]
		return nil
	})
	return result
}

func (module *Module) alreadyExecuted(key string) bool {
	return key != "" && module.state.Executed[key]
}

func (module *Module) markExecuted(key string) {
	if key != "" {
		module.state.Executed[key] = true
	}
}

// StateDigestInput returns stable key/value lines for a persistence adapter.
func (module *Module) StateDigestInput() []string {
	state := module.Snapshot()
	var lines []string
	appendValues := func(prefix string, values map[string]int64) {
		for _, key := range sortedKeys(values) {
			lines = append(lines, fmt.Sprintf("%s|%s|%d", prefix, key, values[key]))
		}
	}
	appendBools := func(prefix string, values map[string]bool) {
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("%s|%s|%t", prefix, key, values[key]))
		}
	}
	appendValues("variable", state.Variables)
	appendBools("zone", state.DisabledZones)
	appendBools("device", state.OpenedDevices)
	appendBools("executed", state.Executed)
	return lines
}
