package combat_test

import (
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

func TestLoadedPlayerStateIsAuthoritativeAtAdmission(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{
		PlayerLifecycle: &combat.PlayerLifecycleRules{
			RespawnDelay:         2 * tickInterval,
			ResurrectionSickness: 3 * tickInterval,
		},
	})
	fixture.leave(fixture.playerID)
	fixture.playerID = fixture.joinWith(combat.PlayerAdmission{
		Level:      2,
		Experience: 125,
		Health:     75,
		MaxHealth:  100,
		AbilityIDs: fixture.module.Rules().AbilityIDs(),
	})

	state := fixture.state(fixture.playerID)
	if state.level != 2 || state.health != 75 || !state.alive {
		t.Fatalf("admitted player = %+v, want loaded level 2 and health 75", state)
	}
}

func TestCommittedLevelIsProjectedWithoutInventingHealthGrowth(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	fixture.inspect(func(tick *world.Tick) {
		tick.Entity(fixture.playerID).Health = 75
	})
	if err := fixture.module.ApplyPlayerProgression(fixture.playerID, 2); err != nil {
		t.Fatalf("ApplyPlayerProgression() error = %v", err)
	}
	state := fixture.state(fixture.playerID)
	if state.level != 2 || state.health != 75 || state.maxHealth != combat.MaxHealth(1, 1) {
		t.Fatalf("player after committed level projection = %+v", state)
	}
	if err := fixture.module.ApplyPlayerProgression(fixture.playerID, 1); err == nil {
		t.Fatal("ApplyPlayerProgression() accepted a level rollback")
	}
}

func TestPlayerDeathRespawnsAtAdmissionAnchorAndAppliesSickness(t *testing.T) {
	t.Parallel()

	rules := combat.PlayerLifecycleRules{
		RespawnDelay:         2 * tickInterval,
		ResurrectionSickness: 3 * tickInterval,
	}
	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{
		PlayerLifecycle: &rules,
	})
	victimID := fixture.join()
	anchor := fixture.state(victimID).position

	// A second player is a public combat caster. Giving the victim the hostile
	// mob faction lets the ordinary ability path produce the killing blow.
	fixture.inspect(func(tick *world.Tick) {
		victim := tick.Entity(victimID)
		mob := tick.Entity(fixture.mobID)
		victim.Faction = mob.Faction
		victim.Health = 1
	})
	if _, err := fixture.cast(baseAbility, victimID); err != nil {
		t.Fatalf("killing cast error = %v", err)
	}
	dead := fixture.state(victimID)
	if dead.alive || dead.health != 0 {
		t.Fatalf("victim after killing blow = %+v", dead)
	}
	death := fixture.events.waitFor(t, combat.EventKindPlayerDeath)
	if death.TargetID != victimID || death.RespawnTick != fixture.tick()+2 {
		t.Fatalf("player death event = %#v", death)
	}

	fixture.step(1)
	if fixture.state(victimID).alive {
		t.Fatal("player respawned before the authored delay")
	}
	fixture.step(1)
	respawned := fixture.state(victimID)
	if !respawned.alive || respawned.health != respawned.maxHealth || respawned.position != anchor {
		t.Fatalf("respawned player = %+v, anchor %+v", respawned, anchor)
	}
	respawn := fixture.events.waitFor(t, combat.EventKindPlayerRespawn)
	if respawn.TargetID != victimID || respawn.ResurrectionSicknessUntilTick != fixture.tick()+3 {
		t.Fatalf("player respawn event = %#v", respawn)
	}

	active, until, err := fixture.module.PlayerResurrectionSickness(victimID)
	if err != nil || !active || until != respawn.ResurrectionSicknessUntilTick {
		t.Fatalf("PlayerResurrectionSickness() = %v, %d, %v", active, until, err)
	}
	fixture.step(3)
	active, _, err = fixture.module.PlayerResurrectionSickness(victimID)
	if err != nil || active {
		t.Fatalf("sickness after authored duration = %v, %v", active, err)
	}
}

func TestDeadPlayerLoadedAfterRestartStillCompletesRespawn(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{
		PlayerLifecycle: &combat.PlayerLifecycleRules{
			RespawnDelay:         tickInterval,
			ResurrectionSickness: 2 * tickInterval,
		},
	})
	fixture.leave(fixture.playerID)
	fixture.playerID = fixture.joinWith(combat.PlayerAdmission{
		Level:      1,
		Experience: 0,
		Health:     0,
		MaxHealth:  combat.MaxHealth(1, 1),
		AbilityIDs: fixture.module.Rules().AbilityIDs(),
	})
	if fixture.state(fixture.playerID).alive {
		t.Fatal("loaded dead player was silently healed during admission")
	}
	fixture.step(1)
	if state := fixture.state(fixture.playerID); !state.alive || state.health != state.maxHealth {
		t.Fatalf("loaded dead player after respawn tick = %+v", state)
	}
}

func TestLoadedResurrectionSicknessResumesForItsPersistedRemainder(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{
		PlayerLifecycle: &combat.PlayerLifecycleRules{
			RespawnDelay: tickInterval, ResurrectionSickness: 3 * tickInterval,
		},
	})
	fixture.leave(fixture.playerID)
	fixture.playerID = fixture.joinWith(combat.PlayerAdmission{
		Level: 1, Health: combat.MaxHealth(1, 1), MaxHealth: combat.MaxHealth(1, 1),
		AbilityIDs:                    fixture.module.Rules().AbilityIDs(),
		ResurrectionSicknessRemaining: 2 * tickInterval,
	})
	active, until, err := fixture.module.PlayerResurrectionSickness(fixture.playerID)
	if err != nil || !active || until != fixture.tick()+2 {
		t.Fatalf("loaded sickness = %v until %d, error %v", active, until, err)
	}
	fixture.step(2)
	active, _, err = fixture.module.PlayerResurrectionSickness(fixture.playerID)
	if err != nil || active {
		t.Fatalf("loaded sickness after remainder = %v, %v", active, err)
	}
}

func TestPlayerLifecycleRejectsMissingAuthoredRulesForADeadLoad(t *testing.T) {
	t.Parallel()

	fixture := newHarness(t, basePack, targetMob, world.Vec3{X: 6}, combat.Options{})
	entityID, _ := fixture.zone.Join()
	err := fixture.module.Admit(entityID, combat.PlayerAdmission{
		Level: 1, Health: 0, MaxHealth: combat.MaxHealth(1, 1),
		AbilityIDs: fixture.module.Rules().AbilityIDs(),
	})
	if err == nil {
		t.Fatal("Admit() accepted a dead load without authored lifecycle rules")
	}
	fixture.zone.Leave(entityID)
}
