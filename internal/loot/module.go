package loot

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/inventory"
	"github.com/SarnautCore/server/internal/world"
)

// Options tunes one loot module.
type Options struct {
	// WorldSeed is the per-shard-instance half of the roll seed (rule 5.2.4).
	// It is configuration and not a process-start value on purpose: a shard
	// restart must not change the drop a given corpse would produce, or rule
	// 5.1.1 buys nothing across restarts.
	WorldSeed string
}

// Module is the loot system for one zone.
//
// It owns the corpse containers: what is on each one, who may take it, and when
// it goes. The corpse container is a world entity of its own rather than the
// dead mob, because the mob entity belongs to its spawn slot and has to survive
// to be respawned into, while the container exists for exactly the thirty
// seconds mechanics/loot.md gives it and then genuinely disappears.
type Module struct {
	logger  *slog.Logger
	zone    *world.Zone
	rules   Rules
	awarder inventory.Awarder
	options Options

	// corpses, byVictim and owners are read and written only under the zone
	// lock, which is what lets this module hold no mutex of its own.
	corpses  map[uint64]*corpse
	byVictim map[uint64]uint64
	owners   map[uint64]uuid.UUID
}

// corpse is one lootable container.
type corpse struct {
	containerID uint64
	victimID    uint64
	// owner is the character credited with the kill (rule 5.8.1). Ownership is
	// keyed on the character and not on the session or the world entity,
	// because rule 5.8.4 keeps it across a disconnect and zone.Leave destroys
	// both of the others.
	owner       uuid.UUID
	tableID     string
	drop        Drop
	looted      bool
	inFlight    bool
	despawnTick uint64
	seed        string
}

// New builds the loot module for one zone.
func New(
	logger *slog.Logger,
	zone *world.Zone,
	rules Rules,
	awarder inventory.Awarder,
	options Options,
) *Module {
	if logger == nil {
		logger = slog.Default()
	}
	return &Module{
		logger:   logger,
		zone:     zone,
		rules:    rules,
		awarder:  awarder,
		options:  options,
		corpses:  make(map[uint64]*corpse),
		byVictim: make(map[uint64]uint64),
		owners:   make(map[uint64]uuid.UUID),
	}
}

// Rules is the rule set this module rolls against.
func (module *Module) Rules() Rules { return module.rules }

// Admit records which character a joined player entity belongs to.
//
// Loot ownership is the reason this mapping exists at all. Everything else in
// the simulation addresses a player by entity id; rule 5.8.4 says ownership
// survives a reconnect, which means it cannot be keyed on anything a reconnect
// destroys.
func (module *Module) Admit(entityID uint64, characterID uuid.UUID) {
	_ = module.zone.Command(func(*world.Tick) error {
		module.owners[entityID] = characterID
		return nil
	})
}

// Release drops the entity-to-character mapping of a departed player. The
// corpses that character owns are untouched: they stay theirs until they
// despawn.
func (module *Module) Release(entityID uint64) {
	_ = module.zone.Command(func(*world.Tick) error {
		delete(module.owners, entityID)
		return nil
	})
}

// MobKilled implements [combat.KillSink]: it rolls the drop and stands the
// corpse container up, inside the tick that caused the death.
//
// The roll happens here and not when a player opens the corpse (rule 5.1.1), so
// re-opening cannot re-roll and a disconnect mid-loot cannot change what is
// there.
func (module *Module) MobKilled(tick *world.Tick, kill combat.Kill) {
	table, ok := module.rules.Table(kill.LootTableID)
	if !ok {
		// A mob that names no table, or one this pack does not carry, drops
		// nothing and gets no container. loot.md section 7.2 notes that the
		// modifier chain which would gate a roll is deferred, so "no table" is
		// the only way content says "no loot" in M2.
		return
	}

	owner := module.owners[kill.KillerEntityID]
	seed := Seed{
		WorldSeed:         module.options.WorldSeed,
		ZoneID:            tick.ZoneID(),
		SpawnSlotID:       kill.PlacementID,
		DeathServerTick:   kill.DeathTick,
		KillerCharacterID: owner,
	}
	// Rule 5.2.5: logged before the first draw, not after. A panic mid-roll
	// still leaves behind the one value needed to reproduce it.
	digest := seed.Hex()
	module.logger.Info("loot roll seeded",
		"zone_id", tick.ZoneID(),
		"victim_entity_id", kill.VictimEntityID,
		"loot_table_id", table.ID,
		"loot_seed", digest,
	)

	stream := NewStream(seed)
	drop, err := Evaluate(table, stream)
	if err != nil {
		module.logger.Error("loot roll failed",
			"loot_table_id", table.ID, "loot_seed", digest, "error", err)
		return
	}
	// Rule 5.2.6: the drop plus its seed plus its draw count is enough to
	// replay the roll offline with no server state.
	module.logger.Info("loot rolled",
		"loot_table_id", table.ID,
		"loot_seed", digest,
		"draws", stream.Draws(),
		"money", drop.Money,
		"item_grants", len(drop.Items),
	)
	if drop.Empty() {
		// Nothing to hold, so nothing to stand up. A container with no drop is
		// a prop, and a prop that has to be despawned is a prop that can leak.
		return
	}

	container := tick.SpawnNPC(world.NPCSpec{
		ContentID:   kill.VictimContentID,
		Position:    kill.Position,
		Heading:     kill.Heading,
		Faction:     "",
		Level:       kill.VictimLevel,
		PlacementID: "",
	})
	// A container is not a combatant. Zero max health is what makes it
	// untargetable by mechanics/combat.md rule 5.2.3 without combat needing to
	// know that loot exists, and Alive false is what keeps it out of a client's
	// list of things to fight.
	container.Alive = false
	container.MaxHealth = 0
	container.Health = 0

	module.corpses[container.ID] = &corpse{
		containerID: container.ID,
		victimID:    kill.VictimEntityID,
		owner:       owner,
		tableID:     table.ID,
		drop:        drop,
		despawnTick: kill.DespawnTick,
		seed:        digest,
	}
	module.byVictim[kill.VictimEntityID] = container.ID

	containerID := container.ID
	delay := uint64(0)
	if kill.DespawnTick > tick.Number() {
		delay = kill.DespawnTick - tick.Number()
	}
	tick.After(delay, func(later *world.Tick) {
		module.despawn(later, containerID)
	})
}

// despawn is rule 5.1.2: the container and its drop go together.
//
// Both go, and that is the assertion worth making about this function. A
// container removed from the registry while its record stayed would leak one
// map entry per kill for the life of the shard; a record removed while the
// entity stayed would leave a corpse on every client that nothing can open.
func (module *Module) despawn(tick *world.Tick, containerID uint64) {
	held, ok := module.corpses[containerID]
	if !ok {
		return
	}
	delete(module.corpses, containerID)
	delete(module.byVictim, held.victimID)
	tick.Despawn(containerID)
}

// Offer is what the client sees when it interacts with a corpse: the whole
// drop, before taking it.
type Offer struct {
	CorpseEntityID uint64
	LootTableID    string
	Money          int64
	Items          []ItemGrant
}

// Result is what one take produced.
type Result struct {
	CorpseEntityID uint64
	Refusal        Refusal
	Money          int64
	Items          []ItemGrant
	// Slots and Currency are the character's inventory as it was committed.
	// They are empty on a refusal.
	Slots    []inventory.Stack
	Currency int64
	SaveSeq  int64
}

// Look answers ClientMessage.interact against a corpse: what is on it, if the
// asking character is allowed to know.
func (module *Module) Look(actorEntityID, corpseEntityID uint64) (Offer, Refusal) {
	var (
		offer   Offer
		refusal Refusal
	)
	_ = module.zone.Command(func(*world.Tick) error {
		held, actor := module.corpses[corpseEntityID], module.owners[actorEntityID]
		switch {
		case held == nil:
			refusal = RefusalNoCorpse
		case held.owner != actor:
			refusal = RefusalNotYourLoot
		case held.looted:
			refusal = RefusalAlreadyLooted
		default:
			offer = Offer{
				CorpseEntityID: corpseEntityID,
				LootTableID:    held.tableID,
				Money:          held.drop.Money,
				Items:          held.drop.Clone().Items,
			}
		}
		return nil
	})
	return offer, refusal
}

// Take is rule 5.6: the whole drop on one corpse, into one character.
//
// It runs in three phases, and the shape is forced by two constraints that
// pull against each other. The corpse lives under the zone lock, and the award
// is a database transaction that must never run under it. So: reserve under the
// lock, commit outside it, then clear under the lock again.
//
// The reservation is what makes the two-phase shape safe. Between phases the
// corpse still holds the drop and is marked in flight, so a retransmit is
// refused rather than committing the same drop twice, and a crash in phase two
// leaves the drop exactly where it was. The item is in the corpse or in the
// bag, never in both and never in neither.
func (module *Module) Take(ctx context.Context, actorEntityID, corpseEntityID uint64) (Result, error) {
	var (
		reserved Drop
		owner    uuid.UUID
		refusal  Refusal
	)
	_ = module.zone.Command(func(*world.Tick) error {
		held, actor := module.corpses[corpseEntityID], module.owners[actorEntityID]
		switch {
		case held == nil:
			refusal = RefusalNoCorpse
		case held.owner != actor:
			refusal = RefusalNotYourLoot
		case held.looted:
			refusal = RefusalAlreadyLooted
		case held.inFlight:
			refusal = RefusalInProgress
		default:
			held.inFlight = true
			reserved, owner = held.drop.Clone(), held.owner
		}
		return nil
	})
	if refusal != RefusalNone {
		return Result{CorpseEntityID: corpseEntityID, Refusal: refusal}, refusal.err()
	}

	awarded, err := module.awarder.Award(ctx, owner, inventory.Award{
		Money:  reserved.Money,
		Grants: grantsFor(reserved),
	})
	if err != nil {
		module.release(corpseEntityID)
		if errors.Is(err, inventory.ErrBagFull) {
			// Rule 5.6.3. The corpse is intact, the money is uncredited, and
			// the client is told why. Nothing was destroyed.
			return Result{CorpseEntityID: corpseEntityID, Refusal: RefusalBagFull}, ErrBagFull
		}
		module.logger.Error("loot award failed",
			"corpse_entity_id", corpseEntityID,
			"character_id", owner.String(),
			"error", err,
		)
		return Result{CorpseEntityID: corpseEntityID, Refusal: RefusalInternal}, err
	}

	// Committed. Only now is the corpse emptied (rule 5.6.4); it stays standing
	// until its despawn tick, but it is empty.
	module.zoneClear(corpseEntityID)
	return Result{
		CorpseEntityID: corpseEntityID,
		Refusal:        RefusalNone,
		Money:          reserved.Money,
		Items:          reserved.Items,
		Slots:          awarded.Slots,
		Currency:       awarded.Currency,
		SaveSeq:        awarded.SaveSeq,
	}, nil
}

func (module *Module) release(corpseEntityID uint64) {
	_ = module.zone.Command(func(*world.Tick) error {
		if held, ok := module.corpses[corpseEntityID]; ok {
			held.inFlight = false
		}
		return nil
	})
}

func (module *Module) zoneClear(corpseEntityID uint64) {
	_ = module.zone.Command(func(*world.Tick) error {
		held, ok := module.corpses[corpseEntityID]
		if !ok {
			// Despawned while the award was in flight. The character keeps what
			// was committed; there is nothing left to empty.
			return nil
		}
		held.looted = true
		held.inFlight = false
		held.drop = Drop{}
		return nil
	})
}

// CorpseCount is how many containers the module is holding. It is a diagnostic
// and a test hook, not a simulation input: it is what an entity-count assertion
// is compared against when a corpse despawns unlooted.
func (module *Module) CorpseCount() int {
	var count int
	_ = module.zone.Command(func(*world.Tick) error {
		count = len(module.corpses)
		return nil
	})
	return count
}

// CorpseFor resolves the container standing over one dead mob, so a caller that
// only saw the DeathEvent can address the loot.
func (module *Module) CorpseFor(victimEntityID uint64) (uint64, bool) {
	var (
		containerID uint64
		ok          bool
	)
	_ = module.zone.Command(func(*world.Tick) error {
		containerID, ok = module.byVictim[victimEntityID]
		return nil
	})
	return containerID, ok
}

func grantsFor(drop Drop) []inventory.Grant {
	grants := make([]inventory.Grant, 0, len(drop.Items))
	for _, item := range drop.Items {
		grants = append(grants, inventory.Grant{ItemID: item.ItemID, Count: item.Count})
	}
	return grants
}
