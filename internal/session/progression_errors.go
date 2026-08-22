package session

import "errors"

// ErrProgressionUnconfigured is returned instead of accepting an irreversible
// gain without its private native rules or durable service.
var ErrProgressionUnconfigured = errors.New("session: progression is not configured")
