package combat

import "github.com/SarnautCore/server/internal/gametypes"

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
	// ActionGroupID is server-owned extracted content identity. The current
	// legacy wire projection intentionally omits it.
	ActionGroupID string

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

// Kill is everything a downstream system needs about one mob death, handed
// over at the instant rule 5.9 creates the corpse.
//
// It carries more than the client's DeathEvent does, and that is the point:
// mechanics/loot.md rule 5.2.2 seeds a roll from the spawn slot and the death
// tick, and rule 5.8.1 copies kill credit onto the corpse. Both are facts only
// this module holds, and neither may reach the wire.
type Kill struct {
	VictimEntityID uint64
	KillerEntityID uint64
	// VictimContentID and PlacementID name the content record and the authored
	// spawn slot. The slot is the stable identity across a respawn, which is
	// what makes it a seed input rather than the entity id.
	VictimContentID string
	PlacementID     string
	// LootTableID is the tree the victim's mob record names, empty when it
	// names none.
	LootTableID string
	VictimLevel uint32
	Position    gametypes.Vec3
	Heading     float32
	DeathTick   uint64
	// DespawnTick is when the corpse goes, rule 5.9.4. loot.md section 3 sets
	// LOOT_OWNERSHIP_S equal to CORPSE_TIMER_S deliberately, so a downstream
	// container schedules itself against this same tick rather than computing a
	// second deadline that could drift from it.
	DespawnTick uint64
}

// KillSinks delivers one death to several sinks in order.
//
// A zone has more than one thing to tell about a kill — the corpse to stand up
// and the quest counters to advance — and combat holds exactly one sink because
// two would be a list with special cases for zero and one. This is that list,
// and it is here rather than in either consumer because neither of them may
// know the other exists (mechanics/combat.md rules 5.9.3 and 5.9.4).
//
// Order is registration order and it matters: loot rolls the drop at corpse
// creation (mechanics/loot.md rule 5.1.1), so a sink registered after it sees a
// world in which the corpse already exists.
type KillSinks []KillSink

// MobKilled implements [KillSink]. A nil member is skipped, so a composition
// that omits one module needs no branch at the call site.
func (sinks KillSinks) MobKilled(tick gametypes.Tick, kill Kill) {
	for _, sink := range sinks {
		if sink == nil {
			continue
		}
		sink.MobKilled(tick, kill)
	}
}

// KillSink is told about a mob death from inside the tick that caused it.
//
// It is called with the zone lock held, so an implementation must do exactly
// what a gametypes.System may do: mutate through the Tick it is handed, and
// nothing that blocks. `internal/loot` is one implementation; combat does not
// import any consumer, which keeps the dependency pointing one way.
type KillSink interface {
	MobKilled(gametypes.Tick, Kill)
}
