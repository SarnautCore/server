package world

import "math"

// Vec3 is a world-space vector. Z is the vertical axis.
type Vec3 struct {
	X float32
	Y float32
	Z float32
}

// Add returns the componentwise sum.
func (value Vec3) Add(other Vec3) Vec3 {
	return Vec3{X: value.X + other.X, Y: value.Y + other.Y, Z: value.Z + other.Z}
}

// Sub returns the componentwise difference.
func (value Vec3) Sub(other Vec3) Vec3 {
	return Vec3{X: value.X - other.X, Y: value.Y - other.Y, Z: value.Z - other.Z}
}

// Scale returns the vector multiplied by a scalar.
func (value Vec3) Scale(factor float32) Vec3 {
	return Vec3{X: value.X * factor, Y: value.Y * factor, Z: value.Z * factor}
}

// Length is the Euclidean magnitude, over all three axes.
func (value Vec3) Length() float32 {
	return float32(math.Sqrt(float64(value.X)*float64(value.X) +
		float64(value.Y)*float64(value.Y) +
		float64(value.Z)*float64(value.Z)))
}

// Distance is the Euclidean distance between two points, using all three axes
// (mechanics/combat.md rule 5.3.1). The intermediate sum is float64 so that a
// distance comparison against a range constant does not turn on float32
// rounding of the squares.
func Distance(from, to Vec3) float32 {
	return from.Sub(to).Length()
}

// Finite reports whether every component is a real number. Anything a client
// sends is checked with it before it reaches simulation state.
func (value Vec3) Finite() bool {
	return finite(value.X) && finite(value.Y) && finite(value.Z)
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
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
