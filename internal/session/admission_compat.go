package session

import (
	"errors"

	"github.com/SarnautCore/server/internal/gateway"
)

// These aliases keep the explicit direct-path compatibility toggle source
// compatible while the M3 driver proves both routes. Production composition
// puts the authority on cmd/gateway. Delete this file with ServeDirect after
// that proof is recorded.
type Admission = gateway.Admission
type Authority = gateway.Authority
type Refusal = gateway.Refusal

const (
	ReasonNoTicket        = gateway.ReasonNoTicket
	ReasonMalformed       = gateway.ReasonMalformed
	ReasonUnknownTicket   = gateway.ReasonUnknownTicket
	ReasonCharacterGone   = gateway.ReasonCharacterGone
	ReasonNotOwned        = gateway.ReasonNotOwned
	ReasonPlayLockHeld    = gateway.ReasonPlayLockHeld
	ReasonAuthUnavailable = gateway.ReasonAuthUnavailable
	ReasonAuthInternal    = gateway.ReasonAuthInternal
)

func refusalReason(err error) string {
	var refusal *gateway.Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}
