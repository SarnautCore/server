package worldscript

import (
	"fmt"
	"sort"

	"github.com/SarnautCore/server/internal/gametypes"
)

// Tick aliases the gameplay tick so state helpers stay readable without
// widening the public package contract.
type Tick = gametypes.Tick

type Options struct {
	InitialState State
	// TeleportPaths is cut-ladder rung 11. It keeps the same completion event
	// while replacing traversal with an endpoint snap.
	TeleportPaths bool
	Host          ScriptHost
	Loot          DeviceLootSink
}

type Module struct {
	zone gametypes.Zone
	host ScriptHost
	loot DeviceLootSink

	devices  map[uint64]DeviceSpec
	byDevice map[string][]uint64
	zones    map[string]ScriptZoneSpec
	paths    map[string]PathSpec
	members  map[string]map[uint64]bool
	motions  map[uint64]*motion
	state    State
	cues     []Cue

	teleportPaths bool
}

type motion struct {
	request  PathRequest
	waypoint int
	paused   bool
}

// New validates compiled content and registers one deterministic system.
func New(zone gametypes.Zone, content Content, options Options) (*Module, error) {
	if zone == nil {
		return nil, fmt.Errorf("worldscript: zone is required")
	}
	if err := validateContent(content); err != nil {
		return nil, err
	}
	module := &Module{
		zone: zone, host: options.Host, loot: options.Loot,
		devices: make(map[uint64]DeviceSpec), byDevice: make(map[string][]uint64),
		zones: make(map[string]ScriptZoneSpec), paths: make(map[string]PathSpec),
		members: make(map[string]map[uint64]bool), motions: make(map[uint64]*motion),
		state: options.InitialState.clone(), teleportPaths: options.TeleportPaths,
	}
	if module.state.Variables == nil {
		module.state.Variables = make(map[string]int64)
	}
	if module.state.DisabledZones == nil {
		module.state.DisabledZones = make(map[string]bool)
	}
	if module.state.OpenedDevices == nil {
		module.state.OpenedDevices = make(map[string]bool)
	}
	if module.state.Executed == nil {
		module.state.Executed = make(map[string]bool)
	}
	for _, variable := range content.Variables {
		if _, restored := module.state.Variables[variable.ID]; !restored {
			module.state.Variables[variable.ID] = variable.Initial
		}
	}
	for _, zoneSpec := range content.ScriptZones {
		module.zones[zoneSpec.ID] = zoneSpec
		if _, restored := module.state.DisabledZones[zoneSpec.ID]; !restored {
			module.state.DisabledZones[zoneSpec.ID] = zoneSpec.InitiallyDisabled
		}
	}
	for _, path := range content.Paths {
		path.Waypoints = append([]gametypes.Vec3(nil), path.Waypoints...)
		module.paths[path.ID] = path
	}
	if err := module.populateDevices(content.Devices); err != nil {
		return nil, err
	}
	zone.GameAddSystem(module)
	return module, nil
}

// Step runs zone crossings before motion, both in stable content/entity order.
func (module *Module) Step(tick gametypes.Tick) {
	module.stepScriptZones(tick)
	module.stepPaths(tick)
}

func (module *Module) emit(tick gametypes.Tick, event Event) bool {
	if module.host == nil {
		return true
	}
	return module.host.HandleWorldEvent(tick, event)
}

func (module *Module) sortedMotionIDs() []uint64 {
	ids := make([]uint64, 0, len(module.motions))
	for id := range module.motions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}
