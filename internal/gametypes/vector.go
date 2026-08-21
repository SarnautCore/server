// Package gametypes holds the plain values shared by gameplay modules.
//
// It is deliberately a standard-library-only leaf. Behaviour and ownership
// stay in the modules that use these values (ADR 0033 section 2).
package gametypes

import "math"

// EntityID identifies one entity inside a running shard.
//
// It remains an alias during the package extraction so existing persistence
// and wire adapters do not need conversion code.
type EntityID = uint64

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

// Length is the Euclidean magnitude over all three axes.
func (value Vec3) Length() float32 {
	return float32(math.Sqrt(float64(value.X)*float64(value.X) +
		float64(value.Y)*float64(value.Y) +
		float64(value.Z)*float64(value.Z)))
}

// Distance is the Euclidean distance between two points, using all three axes.
func Distance(from, to Vec3) float32 {
	return from.Sub(to).Length()
}

// Finite reports whether every component is a real number.
func (value Vec3) Finite() bool {
	return Finite(value.X) && Finite(value.Y) && Finite(value.Z)
}

// Finite reports whether one float32 is neither infinite nor NaN.
func Finite(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
