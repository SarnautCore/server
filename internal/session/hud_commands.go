package session

import (
	"errors"
	"fmt"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/combat"
)

func (reader *commandReader) targetSelect(request *sarnautv1.TargetSelect) error {
	selected := reader.selectedTarget
	rejection := combat.RejectionInvalidTarget
	hasAuthority := reader.combat != nil
	if reader.combat != nil {
		var err error
		selected, err = reader.combat.SelectTarget(reader.entityID, request.GetTargetEntityId())
		switch {
		case err == nil:
			rejection = combat.RejectionNone
		case errors.As(err, &rejection):
		default:
			reader.span.RecordError(err)
			rejection = combat.RejectionInvalidTarget
		}
	}
	if selected != reader.selectedTarget {
		reader.targetRevision++
		reader.selectedTarget = selected
	}
	message, err := targetStateReplacementToProto(
		reader.targetRevision,
		request.GetRequestId(),
		hasAuthority,
		selected,
		rejection,
	)
	if err != nil {
		return err
	}
	return reader.writer.write(message)
}

func (reader *commandReader) activateAction(request *sarnautv1.ActivateAction) error {
	if reader.combat == nil {
		return reader.writeActionBar(
			request.GetRequestId(),
			combat.ActionRejectionInvalidSlot,
			sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_INTERNAL,
		)
	}
	if request.GetExpectedRevision() != reader.actionRevision {
		return reader.writeActionBar(
			request.GetRequestId(),
			combat.ActionRejectionNone,
			sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_STALE_REVISION,
		)
	}
	_, activationErr := reader.combat.ActivateSlot(
		reader.entityID,
		request.GetSlotIndex(),
		request.GetClientTick(),
	)
	rejection := combat.ActionRejectionNone
	if activationErr != nil {
		if !errors.As(activationErr, &rejection) {
			var ordinary combat.Rejection
			if !errors.As(activationErr, &ordinary) && !errors.Is(activationErr, combat.ErrDuplicateCommand) {
				reader.span.RecordError(activationErr)
				return reader.writeActionBar(
					request.GetRequestId(),
					combat.ActionRejectionNone,
					sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_INTERNAL,
				)
			}
		}
	}
	return reader.writeActionBar(
		request.GetRequestId(),
		rejection,
		sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_UNSPECIFIED,
	)
}

func (reader *commandReader) writeActionBar(
	requestID uint64,
	rejection combat.ActionRejection,
	override sarnautv1.ActionActivationRefusal,
) error {
	bar := emptyActionBar()
	if reader.combat != nil {
		var err error
		bar, err = reader.combat.ActionBar(reader.entityID)
		if err != nil {
			return fmt.Errorf("read action bar: %w", err)
		}
	}
	message, err := actionBarReplacementToProto(
		reader.actionRevision, requestID, bar, rejection,
	)
	if err != nil {
		return err
	}
	if override != sarnautv1.ActionActivationRefusal_ACTION_ACTIVATION_REFUSAL_UNSPECIFIED {
		message.GetActionBarReplacement().ActivationRefusal = override
	}
	return reader.writer.write(message)
}
