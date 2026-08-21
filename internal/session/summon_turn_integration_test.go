package session_test

import (
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/combat"
	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/script"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/world"
)

const (
	quest430ID      = "quest.inst-league1.quest-4-30"
	demonScoutID    = "mob.inst-league1.demon-scout.demon-scout3-3killer"
	leagueMapSlug   = "inst-league-start"
	demoMobForRules = "mob.paper-harbor.tide-crab"
)

type quest430LocatorSource struct {
	mob            gametypes.Mob
	mapID          string
	missingMob     bool
	missingLocator string
}

func (source quest430LocatorSource) QuestActivation(questID string) (session.QuestActivation, bool) {
	if questID != quest430ID {
		return session.QuestActivation{}, false
	}
	mapID := source.mapID
	if mapID == "" {
		mapID = leagueMapSlug
	}
	turnTowardPath := scriptNode(script.FamilyImpact, "quest-4-30/summon/impacts[0]", "ImpactTurnMob",
		script.Field{Name: "destination", Value: script.Value{Kind: script.ValueNode,
			Node: quest430Destination("quest-4-30/summon/impacts[0]/destination", mapID, "PeopleWay3")}},
	)
	turnTowardFirewall := scriptNode(script.FamilyImpact, "quest-4-30/summon/impacts[1]", "ImpactTurnMob",
		script.Field{Name: "destination", Value: script.Value{Kind: script.ValueNode,
			Node: quest430Destination("quest-4-30/summon/impacts[1]/destination", mapID, "Firewall")}},
	)
	summon := scriptNode(script.FamilyImpact, "quest-4-30/summon", "ImpactSummon",
		script.Field{Name: "destination", Value: script.Value{Kind: script.ValueNode,
			Node: quest430Destination("quest-4-30/summon/destination", mapID, "DemonSpawn4")}},
		script.Field{Name: "impacts", Value: scriptNodes(turnTowardPath, turnTowardFirewall)},
		script.Field{Name: "object", Value: script.Value{Kind: script.ValueRef,
			Ref: script.Ref{ID: demonScoutID, RowType: "mob"}}},
	)
	return session.QuestActivation{StartImpacts: []*script.Node{summon}}, true
}

func (quest430LocatorSource) Trigger(script.Ref) (*script.Node, bool) { return nil, false }

func (quest430LocatorSource) Counter(script.Ref) (session.CounterBinding, bool) {
	return session.CounterBinding{}, false
}

func (quest430LocatorSource) SpawnTableMobs(script.Ref) []string { return nil }

func (source quest430LocatorSource) SummonMob(ref script.Ref) (gametypes.Mob, bool) {
	return source.mob, !source.missingMob && ref.ID == source.mob.ID && ref.RowType == "mob"
}

func (source quest430LocatorSource) LocateDestination(
	mapRef script.Ref, scriptID string,
) (script.Position, bool) {
	if mapRef.ID != leagueMapSlug || mapRef.RowType != "map" {
		return script.Position{}, false
	}
	if scriptID == source.missingLocator {
		return script.Position{}, false
	}
	switch scriptID {
	case "DemonSpawn4":
		return script.Position{X: 10, Y: 20, Z: 3}, true
	case "PeopleWay3":
		return script.Position{X: 15, Y: 20, Z: 3}, true
	case "Firewall":
		return script.Position{X: 10, Y: 25, Z: 3}, true
	default:
		return script.Position{}, false
	}
}

func quest430Destination(key, mapID, scriptID string) *script.Node {
	locator := scriptNode(script.FamilyBasic, key+"/locator", "Struct",
		script.Field{Name: "map", Value: script.Value{Kind: script.ValueRef,
			Ref: script.Ref{ID: mapID, RowType: "map"}}},
		script.Field{Name: "scriptID", Value: script.Value{Kind: script.ValueText, Text: scriptID}},
	)
	return scriptNode(script.FamilyImpact, key, "DestinationLocator",
		script.Field{Name: "locator", Value: script.Value{Kind: script.ValueNode, Node: locator}},
		script.Field{Name: "yaw", Value: script.Value{Kind: script.ValueInteger}},
	)
}

func TestQuest430SummonsDemonAtDemonSpawn4ThenFacesItTowardFirewall(t *testing.T) {
	zone, driver, playerID := newQuest430Integration(t, func(*quest430LocatorSource) {})

	driver.QuestActivated(playerID, quest430ID)

	var summoned *gametypes.EntityData
	var summonedPosition gametypes.Vec3
	err := zone.GameCommand(func(tick gametypes.Tick) error {
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.ContentID == demonScoutID {
				copy := *entity
				summoned = &copy
				summonedPosition = tick.Position(entity)
				return false
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("zone.GameCommand() error = %v", err)
	}
	if summoned == nil {
		t.Fatal("Quest 4-30 spawned no DemonScout3_3Killer")
	}
	if !summoned.Alive || summoned.Kind != gametypes.EntityKindNPC {
		t.Fatalf("summoned entity = %#v", summoned)
	}
	if summonedPosition != (gametypes.Vec3{X: 10, Y: 20, Z: 3}) {
		t.Fatalf("summoned position = %#v", summonedPosition)
	}
	if math.Abs(float64(summoned.Heading-1.5707964)) > 0.000001 {
		t.Fatalf("summoned heading = %.7f, want pi/2 toward Firewall", summoned.Heading)
	}
}

func TestQuest430RollsSummonBackWhenFirewallLocatorIsMissing(t *testing.T) {
	zone, driver, playerID := newQuest430Integration(t, func(source *quest430LocatorSource) {
		source.missingLocator = "Firewall"
	})

	driver.QuestActivated(playerID, quest430ID)

	if got := countEntitiesWithContentID(t, zone, demonScoutID); got != 0 {
		t.Fatalf("live demons after child failure = %d, want rollback", got)
	}
}

func TestQuest430RefusesExternalMapIDsBeforeSummoning(t *testing.T) {
	zone, driver, playerID := newQuest430Integration(t, func(source *quest430LocatorSource) {
		source.mapID = "ext.maps.inst-league-start.map-resource"
	})

	driver.QuestActivated(playerID, quest430ID)

	if got := countEntitiesWithContentID(t, zone, demonScoutID); got != 0 {
		t.Fatalf("live demons from ext map id = %d, want none", got)
	}
}

func TestQuest430RefusesMissingSummonMobBeforeWorldMutation(t *testing.T) {
	zone, driver, playerID := newQuest430Integration(t, func(source *quest430LocatorSource) {
		source.missingMob = true
	})

	driver.QuestActivated(playerID, quest430ID)

	if got := countEntitiesWithContentID(t, zone, demonScoutID); got != 0 {
		t.Fatalf("live demons without a mob row = %d, want none", got)
	}
}

func TestRealCompiledPackQuest430CarriesSummonAndTurnMobContracts(t *testing.T) {
	packPath := os.Getenv("SARNAUT_M3_INTEGRATION_PACK")
	if packPath == "" {
		t.Skip("set SARNAUT_M3_INTEGRATION_PACK to the private compiled M3 pack")
	}
	content, err := pack.Load(packPath, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load(real M3 pack) error = %v", err)
	}
	if content.ID() == "" {
		t.Fatal("real M3 pack has no content identity")
	}
	source := session.NewPackQuestScriptSource(content)
	if !source.HasDestinationIndex() {
		t.Fatal("real M3 pack has no strict destination index")
	}
	activation, ok := source.QuestActivation(quest430ID)
	if !ok {
		t.Fatalf("real M3 pack has no %s activation", quest430ID)
	}
	summons := scriptNodesByOpcode(activation.StartImpacts, "ImpactSummon")
	turns := scriptNodesByOpcode(activation.StartImpacts, "ImpactTurnMob")
	if len(summons) != 1 || len(turns) != 1 {
		t.Fatalf("Quest 4-30 world-state nodes: summon=%d turn=%d, want 1/1", len(summons), len(turns))
	}

	host := newPackTraceHost(source)
	evaluator := script.New(host, script.Options{Enabled: true})
	frame := script.Frame{
		EvaluationID: "real-pack-world-state", PackID: content.ID(),
		SourceID: quest430ID, ZoneID: leagueMapSlug,
		CasterID: "character.player", TargetID: "mob.target", Addressee: "mob.target",
	}
	if err := evaluator.Evaluate(t.Context(), summons[0], frame); err != nil {
		t.Fatalf("evaluate real ImpactSummon: %v", err)
	}
	if err := evaluator.Evaluate(t.Context(), turns[0], frame); err != nil {
		t.Fatalf("evaluate real ImpactTurnMob: %v", err)
	}
	if len(host.commands) != 2 {
		t.Fatalf("real world-state commands = %d, want two", len(host.commands))
	}
	summon := host.commands[0]
	if summon.Kind != script.CommandSummon || summon.Summon == nil ||
		summon.Summon.Object.ID != demonScoutID ||
		summon.Summon.Destination.Map != (script.Ref{ID: leagueMapSlug, RowType: "map"}) {
		t.Fatalf("real summon command = %#v", summon)
	}
	if got := summon.Summon.Impacts; len(got) != 2 ||
		got[0].Opcode != "GoThroughPath" || got[1].Opcode != "ImpactsDeferred" {
		t.Fatalf("real summon child order = %#v", got)
	}
	turn := host.commands[1]
	if turn.Kind != script.CommandTurnMob || turn.EntityID != frame.Addressee ||
		turn.Destination.Map != (script.Ref{ID: leagueMapSlug, RowType: "map"}) {
		t.Fatalf("real turn command = %#v", turn)
	}
	if got, want := host.trace, []string{"locate:DemonSpawn4", "locate:Firewall"}; !equalStrings(got, want) {
		t.Fatalf("real locator trace = %v, want %v", got, want)
	}
}

func scriptNodesByOpcode(roots []*script.Node, opcode string) []*script.Node {
	var found []*script.Node
	var visit func(*script.Node)
	visit = func(node *script.Node) {
		if node == nil {
			return
		}
		if node.Opcode == opcode {
			found = append(found, node)
		}
		for _, field := range node.Fields {
			switch field.Value.Kind {
			case script.ValueNode:
				visit(field.Value.Node)
			case script.ValueList:
				for _, entry := range field.Value.List {
					if entry.Kind == script.ValueNode {
						visit(entry.Node)
					}
				}
			}
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return found
}

func newQuest430Integration(
	t *testing.T,
	configure func(*quest430LocatorSource),
) (*world.Zone, *session.ScriptDriver, uint64) {
	t.Helper()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	mob, ok := content.Mob(demoMobForRules)
	if !ok {
		t.Fatalf("fixture pack has no %s", demoMobForRules)
	}
	mob.ID = demonScoutID
	source := quest430LocatorSource{mob: mob}
	configure(&source)

	zone, err := world.NewZone(world.ZoneConfig{
		ID: "inst-league-start", TickInterval: time.Second / 30,
		SnapshotInterval: time.Second / 15, MaxMoveSpeed: 6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	rules, err := combat.RulesFromPack(content)
	if err != nil {
		t.Fatalf("combat.RulesFromPack() error = %v", err)
	}
	combatModule := combat.New(slog.New(slog.DiscardHandler), zone, rules, combat.Options{Seed: 7})
	driver := session.NewScriptDriver(
		slog.New(slog.DiscardHandler), zone, nil, source,
		script.Options{Enabled: true},
	)
	driver.BindCombat(combatModule)
	playerID, _ := zone.Join()
	return zone, driver, playerID
}

func countEntitiesWithContentID(t *testing.T, zone *world.Zone, contentID string) int {
	t.Helper()
	count := 0
	err := zone.GameCommand(func(tick gametypes.Tick) error {
		tick.Each(func(entity *gametypes.EntityData) bool {
			if entity.ContentID == contentID {
				count++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("zone.GameCommand() error = %v", err)
	}
	return count
}
