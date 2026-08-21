package combat_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/world"
)

func TestAdmitWithActionsUsesOnlyAuthoredBindings(t *testing.T) {
	fixture := newHarness(t, extendedPack, targetMob, zeroVector, combat.Options{})
	bindings := []combat.ActionBinding{
		{SlotIndex: 35, AbilityID: extendedAbility},
		{SlotIndex: 0, AbilityID: baseAbility},
	}
	if err := fixture.module.AdmitWithActions(fixture.playerID, bindings); err != nil {
		t.Fatalf("AdmitWithActions() error = %v", err)
	}

	bar, err := fixture.module.ActionBar(fixture.playerID)
	if err != nil {
		t.Fatalf("ActionBar() error = %v", err)
	}
	if len(bar.Slots) != combat.ActionBarSlotCount {
		t.Fatalf("action bar has %d slots, want %d", len(bar.Slots), combat.ActionBarSlotCount)
	}
	for index, slot := range bar.Slots {
		if slot.SlotIndex != uint32(index) {
			t.Errorf("slot %d reports index %d", index, slot.SlotIndex)
		}
	}
	if bar.Slots[0].AbilityID != baseAbility || bar.Slots[35].AbilityID != extendedAbility {
		t.Fatalf("authored bindings were not preserved: slot 0 = %q, slot 35 = %q",
			bar.Slots[0].AbilityID, bar.Slots[35].AbilityID)
	}
	if bar.Slots[1].AbilityID != "" || bar.Slots[1].Available ||
		bar.Slots[1].UnavailableReason != combat.ActionUnavailableEmptySlot {
		t.Errorf("empty slot 1 = %+v", bar.Slots[1])
	}
	event, err := fixture.module.UseAbility(fixture.playerID, combat.AbilityRequest{TargetID: fixture.mobID})
	if err != nil || event.AbilityID != baseAbility {
		t.Fatalf("default ability after reversed binding input = %q, %v, want %q, nil",
			event.AbilityID, err, baseAbility)
	}
}

func TestAuthoredAdmissionDoesNotGrantOtherPackAbilities(t *testing.T) {
	fixture := authoredActionHarness(t)
	_, err := fixture.module.UseAbility(fixture.playerID, combat.AbilityRequest{
		TargetID:  fixture.mobID,
		AbilityID: extendedAbility,
	})
	if !errors.Is(err, combat.ErrUnknownAbility) {
		t.Fatalf("unbound pack ability use returned %v, want ErrUnknownAbility", err)
	}
}

func TestAdmitWithActionsRejectsInvalidBindingsBeforeMutation(t *testing.T) {
	fixture := newHarness(t, extendedPack, targetMob, zeroVector, combat.Options{})
	before, err := fixture.module.ActionBar(fixture.playerID)
	if err != nil {
		t.Fatalf("ActionBar() before invalid admission = %v", err)
	}

	tests := []struct {
		name     string
		bindings []combat.ActionBinding
		want     error
	}{
		{
			name:     "slot out of range",
			bindings: []combat.ActionBinding{{SlotIndex: 36, AbilityID: baseAbility}},
			want:     combat.ErrInvalidActionSlot,
		},
		{
			name: "slot assigned twice",
			bindings: []combat.ActionBinding{
				{SlotIndex: 2, AbilityID: baseAbility},
				{SlotIndex: 2, AbilityID: extendedAbility},
			},
			want: combat.ErrDuplicateActionSlot,
		},
		{
			name:     "ability absent from rules",
			bindings: []combat.ActionBinding{{SlotIndex: 0, AbilityID: "ability.forged"}},
			want:     combat.ErrUnknownAbility,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := fixture.module.AdmitWithActions(fixture.playerID, test.bindings); !errors.Is(err, test.want) {
				t.Fatalf("AdmitWithActions() error = %v, want %v", err, test.want)
			}
			after, err := fixture.module.ActionBar(fixture.playerID)
			if err != nil {
				t.Fatalf("ActionBar() after invalid admission = %v", err)
			}
			if after != before {
				t.Fatalf("invalid admission mutated action bar\nbefore: %+v\nafter:  %+v", before, after)
			}
		})
	}
}

func TestActivateSlotResolvesServerBindingAndSelectedTarget(t *testing.T) {
	fixture := authoredActionHarness(t)
	if selected, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID); err != nil || selected != fixture.mobID {
		t.Fatalf("SelectTarget() = %d, %v", selected, err)
	}
	ready, err := fixture.module.ActionBar(fixture.playerID)
	if err != nil || !ready.Slots[0].Available ||
		ready.Slots[0].UnavailableReason != combat.ActionAvailable {
		t.Fatalf("selected, ready action slot = %+v, %v", ready.Slots[0], err)
	}
	before := fixture.state(fixture.mobID).health
	event, err := fixture.module.ActivateSlot(fixture.playerID, 0, 7)
	if err != nil {
		t.Fatalf("ActivateSlot() error = %v", err)
	}
	if event.CasterID != fixture.playerID || event.TargetID != fixture.mobID || event.AbilityID != baseAbility {
		t.Fatalf("ActivateSlot() event = %+v", event)
	}
	if after := fixture.state(fixture.mobID).health; after >= before {
		t.Fatalf("target health after activation = %d, want below %d", after, before)
	}

	bar, err := fixture.module.ActionBar(fixture.playerID)
	if err != nil {
		t.Fatalf("ActionBar() after activation = %v", err)
	}
	slot := bar.Slots[0]
	if slot.Available || slot.UnavailableReason != combat.ActionUnavailableOnCooldown {
		t.Errorf("activated slot availability = %+v", slot)
	}
	if slot.CooldownRemainingMS <= 0 || slot.CooldownDurationMS <= 0 ||
		slot.CooldownRemainingMS > slot.CooldownDurationMS {
		t.Errorf("activated slot cooldown = %d/%d ms",
			slot.CooldownRemainingMS, slot.CooldownDurationMS)
	}
	fixture.zone.Step()
	next, err := fixture.module.ActionBar(fixture.playerID)
	if err != nil {
		t.Fatalf("ActionBar() one tick later = %v", err)
	}
	if next.Slots[0].CooldownRemainingMS < 0 ||
		next.Slots[0].CooldownRemainingMS >= slot.CooldownRemainingMS {
		t.Errorf("cooldown did not decrease and clamp: before %d, after %d",
			slot.CooldownRemainingMS, next.Slots[0].CooldownRemainingMS)
	}
	if elapsed := slot.CooldownRemainingMS - next.Slots[0].CooldownRemainingMS; elapsed != tickInterval.Milliseconds() {
		t.Errorf("one tick reduced cooldown by %d ms, want tick interval %d ms",
			elapsed, tickInterval.Milliseconds())
	}
}

func TestInvalidOrEmptySlotDoesNotMutateOrConsumeSequence(t *testing.T) {
	for _, test := range []struct {
		name string
		slot uint32
		want error
	}{
		{"empty", 1, combat.ErrEmptyActionSlot},
		{"out of range", 36, combat.ErrInvalidActionSlot},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := authoredActionHarness(t)
			if _, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
				t.Fatalf("SelectTarget() error = %v", err)
			}
			beforeHealth := fixture.state(fixture.mobID).health
			beforeBar, _ := fixture.module.ActionBar(fixture.playerID)
			if _, err := fixture.module.ActivateSlot(fixture.playerID, test.slot, 41); !errors.Is(err, test.want) {
				t.Fatalf("ActivateSlot(%d) error = %v, want %v", test.slot, err, test.want)
			}
			afterBar, _ := fixture.module.ActionBar(fixture.playerID)
			if afterBar != beforeBar || fixture.state(fixture.mobID).health != beforeHealth {
				t.Fatal("refused slot activation mutated health or cooldown state")
			}
			if _, err := fixture.module.ActivateSlot(fixture.playerID, 0, 41); err != nil {
				t.Fatalf("same sequence after refusal was not accepted: %v", err)
			}
		})
	}
}

func TestActivationWithoutSelectionDoesNotConsumeSequence(t *testing.T) {
	fixture := authoredActionHarness(t)
	if _, err := fixture.module.ActivateSlot(fixture.playerID, 0, 13); !errors.Is(err, combat.ErrNoTarget) {
		t.Fatalf("activation without target error = %v, want ErrNoTarget", err)
	}
	if _, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID); err != nil {
		t.Fatalf("SelectTarget() error = %v", err)
	}
	if _, err := fixture.module.ActivateSlot(fixture.playerID, 0, 13); err != nil {
		t.Fatalf("same sequence after no-target refusal was not accepted: %v", err)
	}
}

func TestActionAuthoritySerializesConcurrentReadsAndTargetChanges(t *testing.T) {
	fixture := authoredActionHarness(t)
	const workers = 24
	const iterations = 80
	start := make(chan struct{})
	errCh := make(chan error, workers*iterations*2)
	var group sync.WaitGroup
	for worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for iteration := range iterations {
				if (worker+iteration)%2 == 0 {
					_, err := fixture.module.SelectTarget(fixture.playerID, fixture.mobID)
					errCh <- err
				} else {
					_, err := fixture.module.SelectTarget(fixture.playerID, 0)
					errCh <- err
				}
				_, err := fixture.module.ActionBar(fixture.playerID)
				errCh <- err
			}
		}()
	}
	close(start)
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent action authority call returned %v", err)
		}
	}
	selected, err := fixture.module.SelectedTarget(fixture.playerID)
	if err != nil {
		t.Fatalf("SelectedTarget() after concurrent calls = %v", err)
	}
	if selected != 0 && selected != fixture.mobID {
		t.Fatalf("selected target = %d, want zero or %d", selected, fixture.mobID)
	}
}

func authoredActionHarness(t *testing.T) *harness {
	t.Helper()
	fixture := newHarness(t, extendedPack, targetMob, zeroVector, combat.Options{})
	if err := fixture.module.AdmitWithActions(fixture.playerID, []combat.ActionBinding{
		{SlotIndex: 0, AbilityID: baseAbility},
	}); err != nil {
		t.Fatalf("AdmitWithActions() error = %v", err)
	}
	return fixture
}

var zeroVector = world.Vec3{}
