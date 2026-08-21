package combat_test

import (
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

func TestSelectTargetReturnsTypedRefusalAndAuthoritativeSelection(t *testing.T) {
	fixture := authoredActionHarness(t)
	selected, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID)
	if err != nil || selected != fixture.mobID {
		t.Fatalf("SelectTarget(valid) = %d, %v", selected, err)
	}

	for _, test := range []struct {
		name   string
		target uint64
		want   error
	}{
		{"unknown", fixture.mobID + 99999, combat.ErrNoTarget},
	} {
		t.Run(test.name, func(t *testing.T) {
			authoritative, err := fixture.module.SelectTarget(fixture.playerID, test.target)
			if !errors.Is(err, test.want) {
				t.Fatalf("SelectTarget() error = %v, want %v", err, test.want)
			}
			if authoritative != fixture.mobID {
				t.Fatalf("authoritative target = %d, want existing %d",
					authoritative, fixture.mobID)
			}
		})
	}

	self, err := fixture.module.SelectTarget(fixture.playerID, fixture.playerID)
	if err != nil || self != fixture.playerID {
		t.Fatalf("SelectTarget(self) = %d, %v, want %d, nil", self, err, fixture.playerID)
	}

	cleared, err := fixture.module.SelectTarget(fixture.playerID, 0)
	if err != nil || cleared != 0 {
		t.Fatalf("SelectTarget(clear) = %d, %v", cleared, err)
	}
}

func TestSelectTargetAllowsFriendlyLiveCombatants(t *testing.T) {
	fixture := authoredActionHarness(t)
	friendlyID, _ := fixture.zone.Join()
	if err := fixture.module.AdmitWithActions(friendlyID, nil); err != nil {
		t.Fatalf("admit friendly combatant: %v", err)
	}
	if err := fixture.zone.Subscribe(friendlyID, discardSnapshots{}); err != nil {
		t.Fatalf("subscribe friendly combatant: %v", err)
	}
	t.Cleanup(func() {
		fixture.module.Release(friendlyID)
		fixture.zone.Leave(friendlyID)
	})
	selected, err := fixture.module.SelectTarget(fixture.playerID, friendlyID)
	if err != nil || selected != friendlyID {
		t.Fatalf("SelectTarget(friendly) = %d, %v, want %d, nil", selected, err, friendlyID)
	}
}

func TestSelectTargetRequiresReplicatedLiveEntity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*harness)
		want   error
	}{
		{
			name: "not replicated",
			mutate: func(fixture *harness) {
				fixture.inspect(func(tick *world.Tick) { tick.Entity(fixture.mobID).Replicated = false })
			},
			want: combat.ErrInvalidTarget,
		},
		{
			name: "dead",
			mutate: func(fixture *harness) {
				fixture.inspect(func(tick *world.Tick) { tick.Entity(fixture.mobID).Alive = false })
			},
			want: combat.ErrTargetDead,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := authoredActionHarness(t)
			test.mutate(fixture)
			selected, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID)
			if selected != 0 || !errors.Is(err, test.want) {
				t.Fatalf("SelectTarget() = %d, %v, want 0, %v", selected, err, test.want)
			}
		})
	}
}

func TestSelectTargetDoesNotRequireCombatStats(t *testing.T) {
	fixture := authoredActionHarness(t)
	fixture.inspect(func(tick *world.Tick) { tick.Entity(fixture.mobID).MaxHealth = 0 })
	selected, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID)
	if err != nil || selected != fixture.mobID {
		t.Fatalf("SelectTarget(entity without combat stats) = %d, %v, want %d, nil",
			selected, err, fixture.mobID)
	}
}

func TestSelectedTargetClearsWhenTargetDies(t *testing.T) {
	fixture := authoredActionHarness(t)
	if _, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	fixture.inspect(func(tick *world.Tick) { tick.Entity(fixture.mobID).Health = 1 })
	if _, err := fixture.module.ActivateSlot(fixture.playerID, 0, 1); err != nil {
		t.Fatalf("lethal ActivateSlot() error = %v", err)
	}
	selected, err := fixture.module.SelectedTarget(fixture.playerID)
	if err != nil || selected != 0 {
		t.Fatalf("SelectedTarget() after death = %d, %v, want 0, nil", selected, err)
	}
}

func TestSelectedTargetClearsWhenTargetDespawns(t *testing.T) {
	fixture := authoredActionHarness(t)
	if _, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	fixture.inspect(func(tick *world.Tick) { tick.Despawn(fixture.mobID) })
	selected, err := fixture.module.SelectedTarget(fixture.playerID)
	if err != nil || selected != 0 {
		t.Fatalf("SelectedTarget() after despawn = %d, %v, want 0, nil", selected, err)
	}
}
