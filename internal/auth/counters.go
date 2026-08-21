package auth

import "sync/atomic"

// Counters are the service's process-lifetime tallies.
//
// They exist for two reasons. One is the ordinary one: a metric to export.
// The other is a test that has to prove a *code path* ran rather than infer it
// from a stopwatch — a timing assertion about Argon2id would be flaky on any
// loaded machine and would still not say which branch was taken.
type Counters struct {
	dummyHashVerifications atomic.Uint64
	failedLogins           atomic.Uint64
	sessionsIssued         atomic.Uint64
	ticketsMinted          atomic.Uint64
	ticketsRedeemed        atomic.Uint64
}

// CountersSnapshot is a copy of the tallies at one instant.
type CountersSnapshot struct {
	// DummyHashVerifications counts logins against an unknown email that
	// performed the dummy Argon2id derivation (ADR 0030 §1).
	DummyHashVerifications uint64
	FailedLogins           uint64
	SessionsIssued         uint64
	TicketsMinted          uint64
	TicketsRedeemed        uint64
}

// Counters reports the service's tallies.
func (service *Service) Counters() CountersSnapshot {
	return CountersSnapshot{
		DummyHashVerifications: service.counters.dummyHashVerifications.Load(),
		FailedLogins:           service.counters.failedLogins.Load(),
		SessionsIssued:         service.counters.sessionsIssued.Load(),
		TicketsMinted:          service.counters.ticketsMinted.Load(),
		TicketsRedeemed:        service.counters.ticketsRedeemed.Load(),
	}
}
