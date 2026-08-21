// Package worldscript runs compiled tutorial world state. It consumes typed,
// engine-neutral content only. Original Allods formats never cross this package.
package worldscript

import (
	"errors"
	"fmt"
	"sort"

	"github.com/SarnautCore/server/internal/gametypes"
)

// Content is the world-script part of one private compiled pack.
type Content struct {
	Devices     []DeviceSpec
	ScriptZones []ScriptZoneSpec
	Paths       []PathSpec
	Variables   []VariableSpec
}

type DeviceKind uint8

const (
	DeviceKindUnspecified DeviceKind = iota
	DeviceKindElixirChest
)

type DeviceSpec struct {
	ID             string
	PlacementID    string
	MapID          string
	ScriptID       string
	PresentationID string
	Kind           DeviceKind
	Position       gametypes.Vec3
	Heading        float32
	LootTableID    string
	InteractRadius float32
	OneShot        bool
}

type Bounds struct {
	Min gametypes.Vec3
	Max gametypes.Vec3
}

func (bounds Bounds) Contains(point gametypes.Vec3) bool {
	return point.X >= bounds.Min.X && point.X <= bounds.Max.X &&
		point.Y >= bounds.Min.Y && point.Y <= bounds.Max.Y &&
		point.Z >= bounds.Min.Z && point.Z <= bounds.Max.Z
}

type ScriptZoneSpec struct {
	ID                string
	Bounds            Bounds
	InitiallyDisabled bool
}

type PathSpec struct {
	ID           string
	Waypoints    []gametypes.Vec3
	DefaultSpeed float32
	Loop         bool
}

type VariableSpec struct {
	ID      string
	Initial int64
}

type EventKind uint8

const (
	EventUnspecified EventKind = iota
	EventScriptZoneEntering
	EventScriptZoneEntered
	EventScriptZoneLeft
	EventDeviceInteracted
	EventPathStarted
	EventPathCompleted
	EventPathBlocked
)

// Event is the closed callback contract the session interpreter adapter sees.
type Event struct {
	Kind          EventKind
	ZoneID        string
	ActorEntityID uint64
	EntityID      uint64
	ContentID     string
	PlacementID   string
	PathID        string
	ExecutionKey  string
}

// ScriptHost runs under the world tick lock. It must not block, retain tick,
// or re-enter the zone. Returning false for EventScriptZoneEntering applies
// the compiled conditionsIn gate.
type ScriptHost interface {
	HandleWorldEvent(gametypes.Tick, Event) bool
}

type CueKind uint8

const (
	CueUnspecified CueKind = iota
	CueClientData
	CueTextMessage
	CueDeviceOpened
	CuePathStarted
	CuePathCompleted
)

// Cue carries stable product ids only. A client adapter maps it to the current
// wire schema and native presentation catalog.
type Cue struct {
	Kind          CueKind
	ActorEntityID uint64
	EntityID      uint64
	ResourceID    string
	Destinations  []gametypes.Vec3
	ExecutionKey  string
}

type PathRequest struct {
	EntityID     uint64
	PathID       string
	Speed        float32
	Loop         bool
	ExecutionKey string
}

type DeviceQuery struct {
	ContentID string
	MapID     string
	ScriptID  string
	Permanent bool
	Origin    gametypes.Vec3
	Radius    float32
}

type DeviceInteraction struct {
	ActorEntityID  uint64
	DeviceEntityID uint64
	DeviceID       string
	PlacementID    string
	LootTableID    string
}

// DeviceLootSink stands a device-sourced container up through internal/loot.
// The session composition adapter owns the concrete implementation.
type DeviceLootSink interface {
	OpenDevice(gametypes.Tick, DeviceInteraction) (containerEntityID uint64, err error)
}

type InteractRefusal uint8

const (
	InteractAllowed InteractRefusal = iota
	InteractUnknownActor
	InteractUnknownDevice
	InteractTooFar
	InteractAlreadyUsed
	InteractUnavailable
)

type InteractResult struct {
	DeviceEntityID    uint64
	ContainerEntityID uint64
	Refusal           InteractRefusal
}

var ErrInvalidContent = errors.New("worldscript: invalid compiled content")

func validateContent(content Content) error {
	ids := make(map[string]string)
	claim := func(kind, id string) error {
		if id == "" {
			return fmt.Errorf("%w: %s id is empty", ErrInvalidContent, kind)
		}
		if other, ok := ids[id]; ok {
			return fmt.Errorf("%w: id %q is shared by %s and %s", ErrInvalidContent, id, other, kind)
		}
		ids[id] = kind
		return nil
	}
	devicePlacements := make(map[string]bool)
	for _, device := range content.Devices {
		if device.ID == "" {
			return fmt.Errorf("%w: device id is empty", ErrInvalidContent)
		}
		if devicePlacements[device.PlacementID] {
			return fmt.Errorf("%w: duplicate device placement %q", ErrInvalidContent, device.PlacementID)
		}
		devicePlacements[device.PlacementID] = true
		if err := claim("device placement", device.PlacementID); err != nil {
			return err
		}
		if device.PlacementID == "" || device.PresentationID == "" ||
			device.Kind != DeviceKindElixirChest || device.LootTableID == "" ||
			device.InteractRadius <= 0 || !gametypes.Finite(device.InteractRadius) ||
			!device.Position.Finite() || !gametypes.Finite(device.Heading) {
			return fmt.Errorf("%w: device %q is malformed", ErrInvalidContent, device.ID)
		}
	}
	for _, zone := range content.ScriptZones {
		if err := claim("script zone", zone.ID); err != nil {
			return err
		}
		if !zone.Bounds.Min.Finite() || !zone.Bounds.Max.Finite() ||
			zone.Bounds.Min.X > zone.Bounds.Max.X || zone.Bounds.Min.Y > zone.Bounds.Max.Y ||
			zone.Bounds.Min.Z > zone.Bounds.Max.Z {
			return fmt.Errorf("%w: script zone %q has invalid global bounds", ErrInvalidContent, zone.ID)
		}
	}
	for _, path := range content.Paths {
		if err := claim("path", path.ID); err != nil {
			return err
		}
		if len(path.Waypoints) == 0 || path.DefaultSpeed <= 0 || !gametypes.Finite(path.DefaultSpeed) {
			return fmt.Errorf("%w: path %q is malformed", ErrInvalidContent, path.ID)
		}
		for _, point := range path.Waypoints {
			if !point.Finite() {
				return fmt.Errorf("%w: path %q has a non-finite waypoint", ErrInvalidContent, path.ID)
			}
		}
	}
	for _, variable := range content.Variables {
		if err := claim("variable", variable.ID); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
