package combat

// EventKind distinguishes the two things combat tells the rest of the server
// about.
type EventKind uint8

const (
	EventKindUnspecified EventKind = iota
	// EventKindAbility is one resolved or refused ability use.
	EventKindAbility
	// EventKindDeath is the server-internal MobKilled of rule 5.9.3. The
	// client projection drops VictimContentID; see the mapping in
	// `internal/session`.
	EventKindDeath
)

// Event is one thing that happened in combat.
//
// It is a domain value. The two wire messages it maps onto — CombatEvent and
// DeathEvent — are the session layer's business, and rule 5.9.3 is explicit
// that the internal event and its client projection are deliberately not the
// same shape.
type Event struct {
	Kind       EventKind
	ServerTick uint64
	ZoneID     string

	CasterID  uint64
	TargetID  uint64
	AbilityID string

	Damage          int32
	TargetHealth    int32
	TargetMaxHealth int32
	KillingBlow     bool
	Rejection       Rejection

	// VictimContentID is content identity, and rule 5.9.3 keeps it off the
	// wire: the client has no business inferring kill credit from it.
	VictimContentID   string
	VictimLevel       uint32
	CorpseDespawnTick uint64

	// PrivateTo is the entity the event is for, or zero to broadcast. A
	// refusal is nobody's business but the caster's.
	PrivateTo uint64
}

// EventSink receives combat events for one session.
type EventSink interface {
	OfferCombatEvent(Event)
}
