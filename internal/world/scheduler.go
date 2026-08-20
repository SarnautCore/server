package world

// wheelSlots is the number of buckets in the timer wheel, and therefore how
// far ahead work can be filed without spilling into the overflow list.
//
// 1024 slots at a 30 Hz tick is a little over 34 seconds. That covers the
// corpse timer of mechanics/combat.md rule 5.9.4 directly; a respawn, which is
// scheduled a further 10 to 14 seconds out, starts in the overflow list and is
// rebucketed once it comes into range.
const wheelSlots = 1024

// scheduled is one piece of deferred simulation work.
type scheduled struct {
	at  uint64
	run func(*Tick)
}

// timerWheel defers work to a future tick.
//
// Zone.Step had no way to say "do this later", so every deferred rule —
// respawn, corpse despawn, a cooldown expiring — had to be a countdown polled
// on every entity every tick. A wheel makes the cost proportional to the work
// that is actually due.
//
// It holds no lock: every method runs with the zone mutex held.
type timerWheel struct {
	now      uint64
	slots    [wheelSlots][]scheduled
	overflow []scheduled
}

// schedule files `run` for tick `at`. A tick that has already happened is
// treated as the next one: deferred work never silently disappears.
func (wheel *timerWheel) schedule(at uint64, run func(*Tick)) {
	if at <= wheel.now {
		at = wheel.now + 1
	}
	work := scheduled{at: at, run: run}
	if at-wheel.now < wheelSlots {
		index := at % wheelSlots
		wheel.slots[index] = append(wheel.slots[index], work)
		return
	}
	wheel.overflow = append(wheel.overflow, work)
}

// advance moves the wheel to `now` and returns the work due at it.
//
// The caller must advance one tick at a time. A slot holds only work within
// one revolution, and its index is visited exactly once per revolution, so a
// slot's contents are always due precisely at the tick that reaches it.
func (wheel *timerWheel) advance(now uint64) []scheduled {
	wheel.now = now
	if len(wheel.overflow) > 0 {
		kept := wheel.overflow[:0]
		for _, work := range wheel.overflow {
			if work.at-now < wheelSlots {
				index := work.at % wheelSlots
				wheel.slots[index] = append(wheel.slots[index], work)
				continue
			}
			kept = append(kept, work)
		}
		wheel.overflow = kept
	}

	index := now % wheelSlots
	due := wheel.slots[index]
	wheel.slots[index] = nil
	return due
}

// pending counts the work the wheel is still holding. It exists so a test can
// assert that a scheduled respawn is really queued rather than inferring it
// from the absence of a mob.
func (wheel *timerWheel) pending() int {
	total := len(wheel.overflow)
	for index := range wheel.slots {
		total += len(wheel.slots[index])
	}
	return total
}
