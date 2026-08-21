package session

import (
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
)

func TestTargetAndActionCommandsUseSessionAuthority(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	harness.writeReliable(t, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_TargetSelect{
			TargetSelect: &sarnautv1.TargetSelect{
				RequestId:      41,
				TargetEntityId: harness.entityID,
			},
		},
	})
	target := readTargetReplacement(t, harness, 41)
	if !target.GetHasAuthority() || target.GetSelectedEntityId() != harness.entityID ||
		target.GetRefusal() != sarnautv1.TargetSelectRefusal_TARGET_SELECT_REFUSAL_NONE {
		t.Fatalf("target replacement = %+v, want authenticated self selection", target)
	}

	harness.writeReliable(t, &sarnautv1.ClientMessage{
		Payload: &sarnautv1.ClientMessage_ActivateAction{
			ActivateAction: &sarnautv1.ActivateAction{
				RequestId:        42,
				SlotIndex:        0,
				ExpectedRevision: 0,
			},
		},
	})
	action := readActionReplacement(t, harness, 42)
	if action.GetActivationRefusal() !=
		sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_STALE_REVISION {
		t.Fatalf("activation refusal = %s, want STALE_REVISION", action.GetActivationRefusal())
	}
	if len(action.GetSlots()) != 36 || action.GetSlots()[0].GetAbilityId() != "ability.melee.harbor-cleave" {
		t.Fatalf("action replacement did not preserve all authored bindings: %+v", action)
	}

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}

func readTargetReplacement(
	t *testing.T,
	harness *sessionHarness,
	requestID uint64,
) *sarnautv1.TargetStateReplacement {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		if replacement := harness.readReliable(t).GetTargetStateReplacement(); replacement != nil && replacement.GetRequestId() == requestID {
			return replacement
		}
	}
	t.Fatalf("no target replacement answered request %d", requestID)
	return nil
}

func readActionReplacement(
	t *testing.T,
	harness *sessionHarness,
	requestID uint64,
) *sarnautv1.ActionBarReplacement {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		if replacement := harness.readReliable(t).GetActionBarReplacement(); replacement != nil && replacement.GetRequestId() == requestID {
			return replacement
		}
	}
	t.Fatalf("no action replacement answered request %d", requestID)
	return nil
}
