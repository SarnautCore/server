package session

import (
	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/world"
)

// KillSink is the fan-out one zone's composition installs on its combat module:
// the corpse first, then the quest counters.
//
// It lives here because this is the one package that already knows every module
// a zone is made of. `internal/combat` may not import either consumer, and
// neither consumer may import the other, so the wiring has to happen somewhere
// above all three — and doing it in `cmd/shard` would leave the adapter
// untested and duplicated in every test and driver that stands a zone up.
//
// A binding with neither module returns a sink that does nothing rather than
// nil, so a caller never has to decide whether to install one.
func (binding ZoneBinding) KillSink() combat.KillSink {
	sinks := make(combat.KillSinks, 0, 3)
	if binding.Loot != nil {
		sinks = append(sinks, binding.Loot)
	}
	if binding.Quests != nil {
		sinks = append(sinks, questKills{module: binding.Quests})
	}
	// The script driver goes last: shape B's health triggers credit the killer
	// through the quest module, and running after questKills means the plain
	// count-kill path has already seen the death when a scripted counter moves
	// in the same tick.
	if binding.Scripts != nil {
		sinks = append(sinks, binding.Scripts)
	}
	return sinks
}

// questKills is the translation mechanics/combat.md rule 5.9.3 describes: the
// internal MobKilled event, narrowed to the three fields rule 5.4 reads.
//
// It exists so that `internal/quests` declares its own kill value and imports
// nothing of combat's. The loot table, the placement id and the corpse despawn
// tick that travel with a combat.Kill are things a quest counter has no
// business seeing, and an adapter is where they stop.
type questKills struct {
	module *quests.Module
}

func (sink questKills) MobKilled(tick *world.Tick, kill combat.Kill) {
	sink.module.CreditKill(tick, quests.Kill{
		KillerEntityID:  kill.KillerEntityID,
		VictimContentID: kill.VictimContentID,
		ServerTick:      kill.DeathTick,
	})
}
