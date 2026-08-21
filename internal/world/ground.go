package world

import (
	"errors"
	"fmt"
	"math"
)

// Ground is immutable terrain authority loaded from a compiled content pack.
// Runtime code never knows the source terrain format.
type Ground interface {
	Sample(x, y float32) (GroundSample, bool)
}

// GroundSample is the authored height and movement policy at one point.
type GroundSample struct {
	Height   float32
	Walkable bool
}

// GroundPlacement is one compiled tutorial placement checked at boot.
type GroundPlacement struct {
	ID       string
	Position Vec3
}

type PlacementMismatch struct {
	ID             string
	AuthoredHeight float32
	SampledHeight  float32
	OutsideTerrain bool
}

type PlacementSweep struct {
	Checked    int
	Mismatches []PlacementMismatch
}

// SweepGroundPlacements checks every placement. It never stops at the first
// mismatch, so a coordinate-frame error produces the broad failure pattern an
// operator needs instead of looking like one bad spawn.
func SweepGroundPlacements(ground Ground, placements []GroundPlacement, tolerance float32) (PlacementSweep, error) {
	if ground == nil {
		return PlacementSweep{}, fmt.Errorf("ground placement sweep requires terrain")
	}
	if tolerance < 0 || !finite(tolerance) {
		return PlacementSweep{}, fmt.Errorf("ground placement tolerance must be finite and non-negative")
	}
	report := PlacementSweep{Checked: len(placements)}
	for _, placement := range placements {
		if placement.ID == "" || !placement.Position.Finite() {
			return PlacementSweep{}, fmt.Errorf("ground placement is malformed")
		}
		sample, ok := ground.Sample(placement.Position.X, placement.Position.Y)
		if !ok {
			report.Mismatches = append(report.Mismatches, PlacementMismatch{
				ID: placement.ID, AuthoredHeight: placement.Position.Z, OutsideTerrain: true,
			})
			continue
		}
		if abs(sample.Height-placement.Position.Z) > tolerance {
			report.Mismatches = append(report.Mismatches, PlacementMismatch{
				ID: placement.ID, AuthoredHeight: placement.Position.Z, SampledHeight: sample.Height,
			})
		}
	}
	return report, nil
}

// Heightfield is a regular grid in the zone's global coordinate frame.
// Heights are row-major, from the minimum Y row to the maximum Y row.
type Heightfield struct {
	originX  float32
	originY  float32
	cellSize float32
	width    int
	height   int
	heights  []float32
	walkable []bool
	maxGrade float32
}

// HeightfieldSpec is the engine-neutral compiled terrain row.
type HeightfieldSpec struct {
	OriginX  float32
	OriginY  float32
	CellSize float32
	Width    int
	Height   int
	Heights  []float32
	Walkable []bool
	// MaxGrade is the maximum vertical change divided by horizontal distance.
	// Zero disables the slope rule while keeping the authored walkability mask.
	MaxGrade float32
}

// NewHeightfield validates and copies one compiled height grid.
func NewHeightfield(spec HeightfieldSpec) (*Heightfield, error) {
	if !finite(spec.OriginX) || !finite(spec.OriginY) ||
		!finite(spec.CellSize) || spec.CellSize <= 0 {
		return nil, fmt.Errorf("heightfield origin and cell size must be finite, with positive cell size")
	}
	if spec.Width < 2 || spec.Height < 2 {
		return nil, fmt.Errorf("heightfield dimensions must both be at least two")
	}
	count := spec.Width * spec.Height
	if len(spec.Heights) != count {
		return nil, fmt.Errorf("heightfield has %d heights, want %d", len(spec.Heights), count)
	}
	if len(spec.Walkable) != 0 && len(spec.Walkable) != count {
		return nil, fmt.Errorf("heightfield has %d walkability values, want zero or %d", len(spec.Walkable), count)
	}
	if !finite(spec.MaxGrade) || spec.MaxGrade < 0 {
		return nil, fmt.Errorf("heightfield maximum grade must be finite and non-negative")
	}
	for index, value := range spec.Heights {
		if !finite(value) {
			return nil, fmt.Errorf("heightfield height %d is not finite", index)
		}
	}
	heights := append([]float32(nil), spec.Heights...)
	walkable := make([]bool, count)
	if len(spec.Walkable) == 0 {
		for index := range walkable {
			walkable[index] = true
		}
	} else {
		copy(walkable, spec.Walkable)
	}
	return &Heightfield{
		originX: spec.OriginX, originY: spec.OriginY, cellSize: spec.CellSize,
		width: spec.Width, height: spec.Height, heights: heights,
		walkable: walkable, maxGrade: spec.MaxGrade,
	}, nil
}

// Sample bilinearly interpolates height. A cell is walkable only when all four
// vertices are authored walkable and its steepest edge is within MaxGrade.
func (field *Heightfield) Sample(x, y float32) (GroundSample, bool) {
	if field == nil || !finite(x) || !finite(y) {
		return GroundSample{}, false
	}
	gx := (x - field.originX) / field.cellSize
	gy := (y - field.originY) / field.cellSize
	if gx < 0 || gy < 0 || gx > float32(field.width-1) || gy > float32(field.height-1) {
		return GroundSample{}, false
	}
	left := min(int(math.Floor(float64(gx))), field.width-2)
	bottom := min(int(math.Floor(float64(gy))), field.height-2)
	tx := gx - float32(left)
	ty := gy - float32(bottom)
	indices := [4]int{
		bottom*field.width + left,
		bottom*field.width + left + 1,
		(bottom+1)*field.width + left,
		(bottom+1)*field.width + left + 1,
	}
	h00, h10 := field.heights[indices[0]], field.heights[indices[1]]
	h01, h11 := field.heights[indices[2]], field.heights[indices[3]]
	bottomHeight := h00 + (h10-h00)*tx
	topHeight := h01 + (h11-h01)*tx
	walkable := field.walkable[indices[0]] && field.walkable[indices[1]] &&
		field.walkable[indices[2]] && field.walkable[indices[3]]
	if walkable && field.maxGrade > 0 {
		limit := field.maxGrade * field.cellSize
		walkable = abs(h10-h00) <= limit && abs(h11-h01) <= limit &&
			abs(h01-h00) <= limit && abs(h11-h10) <= limit
	}
	return GroundSample{Height: bottomHeight + (topHeight-bottomHeight)*ty, Walkable: walkable}, true
}

func abs(value float32) float32 {
	if value < 0 {
		return -value
	}
	return value
}

// MoveRefusalReason is stable domain state that session adapters can map to a
// wire rejection after the protocol schema freezes.
type MoveRefusalReason uint8

const (
	MoveRefusalUnspecified MoveRefusalReason = iota
	MoveRefusalOutsideTerrain
	MoveRefusalNonWalkable
)

// MoveRefusalError reports why the terrain authority rejected an intent.
type MoveRefusalError struct {
	Reason MoveRefusalReason
}

func (refusal *MoveRefusalError) Error() string {
	switch refusal.Reason {
	case MoveRefusalOutsideTerrain:
		return "move destination is outside authored terrain"
	case MoveRefusalNonWalkable:
		return "move destination is not walkable"
	default:
		return "move destination was refused"
	}
}

// IsMoveRefusal reports whether err carries a typed terrain rejection.
func IsMoveRefusal(err error) bool {
	var refusal *MoveRefusalError
	return errors.As(err, &refusal)
}

func (zone *Zone) groundPoint(position Vec3, requireWalkable bool) (Vec3, error) {
	if zone.config.Ground == nil {
		return position, nil
	}
	sample, ok := zone.config.Ground.Sample(position.X, position.Y)
	if !ok {
		return Vec3{}, &MoveRefusalError{Reason: MoveRefusalOutsideTerrain}
	}
	if requireWalkable && !sample.Walkable {
		return Vec3{}, &MoveRefusalError{Reason: MoveRefusalNonWalkable}
	}
	position.Z = sample.Height
	return position, nil
}

func (zone *Zone) groundMove(from, to Vec3, requireWalkable bool) (Vec3, error) {
	grounded, err := zone.groundPoint(to, requireWalkable)
	if err != nil {
		return Vec3{}, err
	}
	if zone.config.Ground == nil || !requireWalkable {
		return grounded, nil
	}
	// Sample at half-cell intervals so a long client intent cannot jump across
	// a narrow forbidden cell while its endpoint happens to be walkable.
	distance := float32(math.Hypot(float64(to.X-from.X), float64(to.Y-from.Y)))
	step := zone.config.GroundSampleStep
	if step <= 0 {
		step = 0.5
	}
	segments := max(1, int(math.Ceil(float64(distance/step))))
	for index := 1; index < segments; index++ {
		factor := float32(index) / float32(segments)
		point := Vec3{X: from.X + (to.X-from.X)*factor, Y: from.Y + (to.Y-from.Y)*factor}
		if _, err := zone.groundPoint(point, true); err != nil {
			return Vec3{}, err
		}
	}
	return grounded, nil
}
