package script_test

import (
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

const instLeagueStartMap = "inst-league-start"

func destinationLocator(key, scriptID string) *script.Node {
	locator := basic(key+"/locator", "Struct",
		field("map", script.Value{Kind: script.ValueRef, Ref: script.Ref{
			ID: instLeagueStartMap, RowType: "map",
		}}),
		field("scriptID", text(scriptID)),
	)
	return impact(key, "DestinationLocator",
		field("locator", node(locator)),
		field("yaw", integer(0)),
	)
}

func TestImpactTurnMobMissingLocatorEmitsNoCommand(t *testing.T) {
	host := newFakeHost()
	turn := impact("quest-4-30/firewall-turn", "ImpactTurnMob",
		field("destination", node(destinationLocator("quest-4-30/firewall-turn/destination", "Firewall"))),
	)

	_, err := run(host, turn, newFrame())
	if err == nil || !strings.Contains(err.Error(), "fake host has no locator") {
		t.Fatalf("Evaluate(ImpactTurnMob) error = %v", err)
	}
	if len(host.commands) != 0 {
		t.Fatalf("commands after missing locator = %d, want none", len(host.commands))
	}
}

func TestImpactTurnMobFacesTheAuthoredFirewallLocator(t *testing.T) {
	host := newFakeHost()
	host.located[instLeagueStartMap+"|Firewall"] = script.Position{X: -4, Y: 8, Z: 1}

	turn := impact("quest-4-30/firewall-turn", "ImpactTurnMob",
		field("destination", node(destinationLocator("quest-4-30/firewall-turn/destination", "Firewall"))),
	)
	_, err := run(host, turn, newFrame())
	if err != nil {
		t.Fatalf("Evaluate(ImpactTurnMob) error = %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("commands = %d, want one turn", len(host.commands))
	}
	command := host.commands[0]
	if command.Kind != script.CommandTurnMob {
		t.Fatalf("command kind = %d, want CommandTurnMob", command.Kind)
	}
	if command.EntityID != playerID {
		t.Fatalf("turn entity = %q, want %q", command.EntityID, playerID)
	}
	if command.Destination.Map.ID != instLeagueStartMap ||
		command.Destination.Position != (script.Position{X: -4, Y: 8, Z: 1}) {
		t.Fatalf("turn destination = %#v", command.Destination)
	}
}

func TestImpactSummonCarriesDemonSpawnAndOrderedChildImpacts(t *testing.T) {
	host := newFakeHost()
	host.located[instLeagueStartMap+"|DemonSpawn4"] = script.Position{X: 10, Y: 20, Z: 3}
	first := impact("quest-4-30/summon/impacts[0]", "ImpactTurnMob",
		field("destination", node(destinationLocator("quest-4-30/summon/impacts[0]/destination", "PeopleWay3"))),
	)
	second := impact("quest-4-30/summon/impacts[1]", "ImpactsDeferred",
		field("delay", duration(20_000)),
	)
	summon := impact("quest-4-30/summon", "ImpactSummon",
		field("destination", node(destinationLocator("quest-4-30/summon/destination", "DemonSpawn4"))),
		field("impacts", nodeList(first, second)),
		field("object", script.Value{Kind: script.ValueRef, Ref: script.Ref{
			ID: "mob.inst-league1.demon-scout.demon-scout3-3killer", RowType: "mob",
		}}),
	)

	_, err := run(host, summon, newFrame())
	if err != nil {
		t.Fatalf("Evaluate(ImpactSummon) error = %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("commands = %d, want one summon", len(host.commands))
	}
	command := host.commands[0]
	if command.Kind != script.CommandSummon || command.Summon == nil {
		t.Fatalf("command = %#v, want CommandSummon payload", command)
	}
	if command.Summon.Object.ID != "mob.inst-league1.demon-scout.demon-scout3-3killer" {
		t.Fatalf("summon object = %q", command.Summon.Object.ID)
	}
	if command.Summon.Destination.Map.ID != instLeagueStartMap ||
		command.Summon.Destination.Position != (script.Position{X: 10, Y: 20, Z: 3}) {
		t.Fatalf("summon destination = %#v", command.Summon.Destination)
	}
	if len(command.Summon.Impacts) != 2 ||
		command.Summon.Impacts[0].Key != first.Key || command.Summon.Impacts[1].Key != second.Key {
		t.Fatalf("summon impacts = %#v, want source order", command.Summon.Impacts)
	}
}

func TestImpactSummonMissingMobReferenceStopsBeforeLocatorLookup(t *testing.T) {
	host := newFakeHost()
	host.located[instLeagueStartMap+"|DemonSpawn4"] = script.Position{X: 10, Y: 20, Z: 3}
	summon := impact("quest-4-30/summon", "ImpactSummon",
		field("destination", node(destinationLocator("quest-4-30/summon/destination", "DemonSpawn4"))),
		field("object", script.Value{Kind: script.ValueRef, Ref: script.Ref{
			ID: "device.not-a-mob", RowType: "device",
		}}),
	)

	_, err := run(host, summon, newFrame())
	if err == nil || !strings.Contains(err.Error(), "not a mob reference") {
		t.Fatalf("Evaluate(ImpactSummon) error = %v", err)
	}
	if len(host.trace) != 0 || len(host.commands) != 0 {
		t.Fatalf("host calls after missing mob = trace %v, commands %d", host.trace, len(host.commands))
	}
}
