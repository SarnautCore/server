package gametypes

// EntityKind is what a world entity is in simulation terms.
type EntityKind uint8

const (
	EntityKindUnspecified EntityKind = iota
	EntityKindPlayer
	EntityKindNPC
)

// AnimationState is the coarse pose the client should play.
type AnimationState uint8

const (
	AnimationStateUnspecified AnimationState = iota
	AnimationStateIdle
	AnimationStateMoving
)
