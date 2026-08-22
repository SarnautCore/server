package combat_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/world"
)

// The fixture dataset. `demo` is the base pack; `demo-extended` is the same
// dataset with one overlay applied, adding a second ability and a second mob
// and nothing else. Every assertion below is driven from whichever of the two
// it names, and no test in this package contains a damage number, a range or a
// cooldown that is not derived from the pack it loaded.
const (
	basePack     = "demo"
	extendedPack = "demo-extended"

	// The M2 target of mechanics/combat.md section 6.1.
	targetMob   = "mob.paper-harbor.tide-crab"
	baseAbility = "ability.melee.harbor-cleave"

	// The overlay's additions.
	extendedMob     = "mob.paper-harbor.reef-lurker"
	extendedAbility = "ability.magic.brine-bolt"
)

// tickInterval is the shard's configured rate. The worked example's thirty-tick
// global cooldown is a statement about this number, so the tests run at it.
const tickInterval = time.Second / 30

// harness is one zone, one combat module and one admitted player, driven a
// tick at a time with no wall clock anywhere.
type harness struct {
	t        *testing.T
	zone     *world.Zone
	module   *combat.Module
	content  *pack.Pack
	playerID uint64
	mobID    uint64
	events   *recordingSink
}

// newHarness stands up a zone from a fixture pack and places the player at
// `offset` metres from the named mob's anchor.
func newHarness(t *testing.T, packName, mobID string, offset world.Vec3, options combat.Options) *harness {
	t.Helper()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", packName), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load(%q) error = %v", packName, err)
	}
	anchor, ok := anchorOf(content, mobID)
	if !ok {
		t.Fatalf("pack %q has no placement spawning %q", packName, mobID)
	}

	zone, err := world.NewZone(world.ZoneConfig{
		ID:               "CombatFixture",
		TickInterval:     tickInterval,
		SnapshotInterval: 2 * tickInterval,
		MaxMoveSpeed:     6,
		PlayerSpawn:      anchor.Add(offset),
	})
	if err != nil {
		t.Fatalf("NewZone() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("RulesFromPack() error = %v", err)
	}
	module := combat.New(slog.New(slog.DiscardHandler), zone, rules, options)
	if err := module.Populate(content.NPCSpawns()); err != nil {
		t.Fatalf("Populate() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go module.Run(ctx)

	fixture := &harness{
		t:       t,
		zone:    zone,
		module:  module,
		content: content,
		events:  newRecordingSink(),
	}
	fixture.playerID = fixture.join()
	fixture.mobID = fixture.findMob(mobID)
	return fixture
}

func (fixture *harness) join() uint64 {
	return fixture.joinWith(combat.PlayerAdmission{
		Level:      1,
		Health:     combat.MaxHealth(1, 1),
		MaxHealth:  combat.MaxHealth(1, 1),
		AbilityIDs: fixture.module.Rules().AbilityIDs(),
	})
}

func (fixture *harness) joinWith(admission combat.PlayerAdmission) uint64 {
	fixture.t.Helper()
	entityID, _ := fixture.zone.Join()
	if err := fixture.module.Admit(entityID, admission); err != nil {
		fixture.t.Fatalf("Admit() error = %v", err)
	}
	fixture.module.Subscribe(entityID, fixture.events)
	if err := fixture.zone.Subscribe(entityID, discardSnapshots{}); err != nil {
		fixture.t.Fatalf("Subscribe() error = %v", err)
	}
	return entityID
}

// leave is a disconnect: the entity goes, which is what rule 5.8.1 reacts to.
func (fixture *harness) leave(entityID uint64) {
	fixture.module.Release(entityID)
	fixture.zone.Leave(entityID)
}

func (fixture *harness) findMob(contentID string) uint64 {
	fixture.t.Helper()
	var found uint64
	fixture.inspect(func(tick *world.Tick) {
		tick.Each(func(entity *world.Entity) bool {
			if entity.ContentID == contentID {
				found = entity.ID
				return false
			}
			return true
		})
	})
	if found == 0 {
		fixture.t.Fatalf("no spawned entity carries content id %q", contentID)
	}
	return found
}

// inspect reads world state under the zone lock, the same way a system does.
func (fixture *harness) inspect(read func(*world.Tick)) {
	_ = fixture.zone.Command(func(tick *world.Tick) error {
		read(tick)
		return nil
	})
}

type entityState struct {
	position   world.Vec3
	health     int32
	maxHealth  int32
	level      uint32
	alive      bool
	replicated bool
	present    bool
}

func (fixture *harness) state(entityID uint64) entityState {
	var snapshot entityState
	fixture.inspect(func(tick *world.Tick) {
		entity := tick.Entity(entityID)
		if entity == nil {
			return
		}
		snapshot = entityState{
			position:   entity.Position(),
			health:     entity.Health,
			maxHealth:  entity.MaxHealth,
			level:      entity.Level,
			alive:      entity.Alive,
			replicated: entity.Replicated,
			present:    true,
		}
	})
	return snapshot
}

func (fixture *harness) tick() uint64 {
	var number uint64
	fixture.inspect(func(tick *world.Tick) { number = tick.Number() })
	return number
}

func (fixture *harness) step(count int) {
	for index := 0; index < count; index++ {
		fixture.zone.Step()
	}
}

// walk pushes the player one tick's worth of movement in `direction` at
// `speed` metres per second, for `count` ticks.
func (fixture *harness) walk(seq *uint64, direction world.Vec3, speed float32, count int) {
	fixture.t.Helper()
	scale := speed / 6 // ZoneConfig.MaxMoveSpeed
	for index := 0; index < count; index++ {
		*seq++
		if err := fixture.zone.ApplyMoveIntent(fixture.playerID, world.MoveIntent{
			Seq:      *seq,
			Input:    direction.Scale(scale),
			Duration: tickInterval,
		}); err != nil {
			fixture.t.Fatalf("ApplyMoveIntent() error = %v", err)
		}
		fixture.zone.Step()
	}
}

func (fixture *harness) cast(abilityID string, targetID uint64) (combat.Event, error) {
	return fixture.module.UseAbility(fixture.playerID, combat.AbilityRequest{
		TargetID:  targetID,
		AbilityID: abilityID,
	})
}

// castUntilReady advances ticks until the ability resolves, and reports how
// many ticks that took. It is how a test measures a cooldown without knowing
// its length: the pack does.
func (fixture *harness) castUntilReady(abilityID string, targetID uint64, limit int) (combat.Event, uint64) {
	fixture.t.Helper()
	start := fixture.tick()
	for index := 0; index <= limit; index++ {
		event, err := fixture.cast(abilityID, targetID)
		if err == nil {
			return event, fixture.tick() - start
		}
		if !errors.Is(err, combat.ErrOnCooldown) {
			fixture.t.Fatalf("cast rejected with %v, want a cooldown or a hit", err)
		}
		fixture.zone.Step()
	}
	fixture.t.Fatalf("the ability never came off cooldown within %d ticks", limit)
	return combat.Event{}, 0
}

func anchorOf(content *pack.Pack, mobID string) (world.Vec3, bool) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID != mobID {
			continue
		}
		return world.Vec3{X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z}, true
	}
	return world.Vec3{}, false
}

func spawnOf(content *pack.Pack, mobID string) (pack.NPCSpawn, bool) {
	for _, spawn := range content.NPCSpawns() {
		if spawn.MobID == mobID {
			return spawn, true
		}
	}
	return pack.NPCSpawn{}, false
}

// recordingSink collects the combat events one session would receive.
//
// It is unbounded on purpose. A real session's queue drops under flood, which
// is the right behaviour there and a source of false failures here: a test that
// spams a cooldown produces hundreds of refusals, and a bounded sink would
// quietly lose the one event the test is waiting for.
type recordingSink struct {
	mu       sync.Mutex
	received []combat.Event
	consumed int
}

func newRecordingSink() *recordingSink { return &recordingSink{} }

func (sink *recordingSink) OfferCombatEvent(event combat.Event) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.received = append(sink.received, event)
}

// waitFor returns the next unconsumed event of `kind`, or fails. The
// simulation is deterministic; only the hand-off to the fan-out goroutine is
// not, so this is the one place a test waits on anything.
func (sink *recordingSink) waitFor(t *testing.T, kind combat.EventKind) combat.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		for index := sink.consumed; index < len(sink.received); index++ {
			event := sink.received[index]
			if event.Kind != kind {
				continue
			}
			sink.consumed = index + 1
			sink.mu.Unlock()
			return event
		}
		sink.consumed = len(sink.received)
		sink.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no event of kind %d arrived", kind)
		}
		time.Sleep(time.Millisecond)
	}
}

// count consumes what has arrived so far and counts events of one kind. It is
// called after waitFor has already synchronised on the interesting event.
func (sink *recordingSink) count(kind combat.EventKind) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	total := 0
	for _, event := range sink.received[sink.consumed:] {
		if event.Kind == kind {
			total++
		}
	}
	sink.consumed = len(sink.received)
	return total
}

type discardSnapshots struct{}

func (discardSnapshots) OfferSnapshot(world.Snapshot) {}
