package combat

import "github.com/SarnautCore/server/internal/world"

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
	Position    world.Vec3
	Heading     float32
	DeathTick   uint64
	// DespawnTick is when the corpse goes, rule 5.9.4. loot.md section 3 sets
	// LOOT_OWNERSHIP_S equal to CORPSE_TIMER_S deliberately, so a downstream
	// container schedules itself against this same tick rather than computing a
	// second deadline that could drift from it.
	DespawnTick uint64
}

// KillSink is told about a mob death from inside the tick that caused it.
//
// It is called with the zone lock held, so an implementation must do exactly
// what a world.System may do: mutate through the *Tick it is handed, and
// nothing that blocks. `internal/loot` is the implementation; combat does not
// import it, which is what keeps the dependency pointing one way.
type KillSink interface {
	MobKilled(*world.Tick, Kill)
}
