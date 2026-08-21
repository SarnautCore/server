package world

import (
	"math"

	"github.com/SarnautCore/server/internal/gametypes"
)

// Vec3 remains available from world while callers migrate to gametypes.
type Vec3 = gametypes.Vec3

// Distance is the Euclidean distance between two points, using all three axes
// (mechanics/combat.md rule 5.3.1). The intermediate sum is float64 so that a
// distance comparison against a range constant does not turn on float32
// rounding of the squares.
func Distance(from, to Vec3) float32 {
	return gametypes.Distance(from, to)
}

// normalized clamps a control-input vector to unit length. A magnitude at or
// below one is returned unchanged, so a half-deflected stick still means half
// speed.
func normalized(x, y float32) (float32, float32) {
	magnitude := float32(math.Hypot(float64(x), float64(y)))
	if magnitude <= 1 {
		return x, y
	}
	return x / magnitude, y / magnitude
}

func finite(value float32) bool {
	return gametypes.Finite(value)
}
