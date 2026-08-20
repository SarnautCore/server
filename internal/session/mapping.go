package session

import (
	"fmt"
	"math"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

// This file is the entire translation layer between the wire schema and the
// simulation's domain types, and it holds nothing else (ADR 0028). Adding a
// wire field the simulation does not model is a change here and nowhere else;
// a second front-end speaking a different protocol is a second file next to
// this one, with `internal/world` untouched.

func moveIntentFromProto(intent *sarnautv1.ClientMoveIntent) (world.MoveIntent, error) {
	if intent == nil || intent.GetInput() == nil {
		return world.MoveIntent{}, fmt.Errorf("move intent has no input")
	}
	if !isFinite(intent.GetDtSeconds()) {
		return world.MoveIntent{}, fmt.Errorf("move intent carries a non-finite delta")
	}
	return world.MoveIntent{
		Seq:      intent.GetSeq(),
		Input:    vec3FromProto(intent.GetInput()),
		Heading:  intent.GetHeading(),
		Duration: time.Duration(float64(intent.GetDtSeconds()) * float64(time.Second)),
	}, nil
}

func abilityRequestFromProto(use *sarnautv1.AbilityUse, clientSeq uint64) combat.AbilityRequest {
	// caster_id is on the wire and deliberately not read: the actor is the
	// session (protocol/session.md rule 5.2.6).
	return combat.AbilityRequest{
		Seq:       clientSeq,
		TargetID:  use.GetTargetId(),
		AbilityID: use.GetAbilityId(),
	}
}

func snapshotToProto(snapshot world.Snapshot) (*sarnautv1.SnapshotBatch, error) {
	entities := make([]*sarnautv1.EntitySnapshot, 0, len(snapshot.Entities))
	for _, view := range snapshot.Entities {
		entity, err := entitySnapshotToProto(view)
		if err != nil {
			return nil, err
		}
		entities = append(entities, entity)
	}
	// ChunkCount 1 says "this is the whole tick". splitSnapshot overwrites it
	// when a datagram cannot hold the batch; on the reliable fallback it stands,
	// so a receiver applies the same completeness rule on both carriers instead
	// of inferring one from the carrier (protocol/session.md rule 5.5.7).
	return &sarnautv1.SnapshotBatch{
		ServerTick: snapshot.ServerTick,
		Entities:   entities,
		ChunkCount: 1,
	}, nil
}

func entitySnapshotToProto(view world.EntitySnapshot) (*sarnautv1.EntitySnapshot, error) {
	kind, err := entityKindToProto(view.Kind)
	if err != nil {
		return nil, fmt.Errorf("entity %d: %w", view.EntityID, err)
	}
	animation, err := animationStateToProto(view.Animation)
	if err != nil {
		return nil, fmt.Errorf("entity %d: %w", view.EntityID, err)
	}
	return &sarnautv1.EntitySnapshot{
		EntityId:       view.EntityID,
		Kind:           kind,
		Position:       vec3ToProto(view.Position),
		Heading:        view.Heading,
		Velocity:       vec3ToProto(view.Velocity),
		AnimationState: animation,
		ContentId:      view.ContentID,
		NameKey:        view.NameKey,
		Level:          view.Level,
		Faction:        view.Faction,
		Health:         view.Health,
		MaxHealth:      view.MaxHealth,
		Alive:          view.Alive,
	}, nil
}

// combatEventToProto projects one domain event onto the wire.
//
// A death is the interesting case: mechanics/combat.md rule 5.9.3 keeps the
// internal MobKilled and its client projection deliberately separate, because
// the internal one carries the victim's content id and the client has no
// business inferring kill credit from it. That separation is enforced right
// here, by not copying the field.
func combatEventToProto(event combat.Event) (*sarnautv1.ServerMessage, error) {
	switch event.Kind {
	case combat.EventKindAbility:
		rejection, err := rejectionToProto(event.Rejection)
		if err != nil {
			return nil, err
		}
		return &sarnautv1.ServerMessage{
			ServerTick: event.ServerTick,
			Payload: &sarnautv1.ServerMessage_CombatEvent{
				CombatEvent: &sarnautv1.CombatEvent{
					CasterId:        event.CasterID,
					TargetId:        event.TargetID,
					AbilityId:       event.AbilityID,
					Damage:          event.Damage,
					TargetHealth:    event.TargetHealth,
					TargetMaxHealth: event.TargetMaxHealth,
					KillingBlow:     event.KillingBlow,
					Rejection:       rejection,
				},
			},
		}, nil
	case combat.EventKindDeath:
		return &sarnautv1.ServerMessage{
			ServerTick: event.ServerTick,
			Payload: &sarnautv1.ServerMessage_DeathEvent{
				DeathEvent: &sarnautv1.DeathEvent{
					VictimEntityId:    event.TargetID,
					KillerEntityId:    event.CasterID,
					VictimLevel:       event.VictimLevel,
					CorpseDespawnTick: event.CorpseDespawnTick,
				},
			},
		}, nil
	case combat.EventKindUnspecified:
		return nil, fmt.Errorf("combat event has no kind")
	default:
		return nil, fmt.Errorf("combat event kind %d has no wire projection", event.Kind)
	}
}

func entityKindToProto(kind world.EntityKind) (sarnautv1.EntityKind, error) {
	switch kind {
	case world.EntityKindPlayer:
		return sarnautv1.EntityKind_ENTITY_KIND_PLAYER, nil
	case world.EntityKindNPC:
		return sarnautv1.EntityKind_ENTITY_KIND_NPC, nil
	case world.EntityKindUnspecified:
		return sarnautv1.EntityKind_ENTITY_KIND_UNSPECIFIED, nil
	default:
		return 0, fmt.Errorf("entity kind %d has no wire value", kind)
	}
}

func animationStateToProto(state world.AnimationState) (sarnautv1.AnimationState, error) {
	switch state {
	case world.AnimationStateIdle:
		return sarnautv1.AnimationState_ANIMATION_STATE_IDLE, nil
	case world.AnimationStateMoving:
		return sarnautv1.AnimationState_ANIMATION_STATE_MOVING, nil
	case world.AnimationStateUnspecified:
		return sarnautv1.AnimationState_ANIMATION_STATE_UNSPECIFIED, nil
	default:
		return 0, fmt.Errorf("animation state %d has no wire value", state)
	}
}

func rejectionToProto(rejection combat.Rejection) (sarnautv1.AbilityRejection, error) {
	switch rejection {
	case combat.RejectionNone:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_NONE, nil
	case combat.RejectionNoTarget:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_NO_TARGET, nil
	case combat.RejectionInvalidTarget:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_INVALID_TARGET, nil
	case combat.RejectionTargetDead:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_TARGET_DEAD, nil
	case combat.RejectionOutOfRange:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_OUT_OF_RANGE, nil
	case combat.RejectionOnCooldown:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_ON_COOLDOWN, nil
	case combat.RejectionUnknownAbility:
		return sarnautv1.AbilityRejection_ABILITY_REJECTION_UNKNOWN_ABILITY, nil
	default:
		return 0, fmt.Errorf("combat rejection %d has no wire value", uint8(rejection))
	}
}

func vec3FromProto(value *sarnautv1.Vec3) world.Vec3 {
	return world.Vec3{X: value.GetX(), Y: value.GetY(), Z: value.GetZ()}
}

func vec3ToProto(value world.Vec3) *sarnautv1.Vec3 {
	return &sarnautv1.Vec3{X: value.X, Y: value.Y, Z: value.Z}
}

func isFinite(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
