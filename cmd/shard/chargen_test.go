package main

import (
	"path/filepath"
	"testing"

	"github.com/SarnautCore/server/internal/pack"
)

// The old fixture predates authored bag layouts. Boot must reject it instead of
// manufacturing partition boundaries from a capacity.
func TestChargenTemplatesRefuseAPackWithoutAnAuthoredBagLayout(t *testing.T) {
	t.Parallel()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	templates, err := chargenTemplates(content, content.Zone().PlayerSpawn)
	if err == nil || templates != nil {
		t.Fatalf("chargenTemplates() = %+v, %v; want refusal", templates, err)
	}
}

func TestChargenMaterializesAuthoredEquipmentBagStatsAndActions(t *testing.T) {
	t.Parallel()
	templates, option := testChargenTemplates(t)
	snapshot, ok := templates.Template(option.ID)
	if !ok {
		t.Fatalf("Template(%q) is missing", option.ID)
	}
	if snapshot.State.Position.X != 12 || snapshot.State.Position.Y != 4.5 || snapshot.State.Position.Z != 0 {
		t.Errorf("spawn = %+v, want authored position", snapshot.State.Position)
	}
	if snapshot.HUD == nil {
		t.Fatal("template has no HUD state")
	}
	if snapshot.HUD.Bag == nil || snapshot.HUD.Bag.InstanceID != 1 || snapshot.HUD.Bag.ItemID != "item.bag.newbie" {
		t.Fatalf("equipped bag = %+v, want authored bag instance 1", snapshot.HUD.Bag)
	}
	if got := snapshot.HUD.BagLayout; got.LayoutID != "bag.layout.18" || got.Capacity() != 18 ||
		len(got.Partitions) != 2 || got.Partitions[0].Capacity != 12 || got.Partitions[1].Capacity != 6 {
		t.Fatalf("bag layout = %+v, want bag.layout.18 [12,6]", got)
	}
	if len(snapshot.HUD.Equipment) != 1 || snapshot.HUD.Equipment[0].Slot != 0 ||
		snapshot.HUD.Equipment[0].InstanceID != 2 {
		t.Fatalf("equipment = %+v, want helm instance 2", snapshot.HUD.Equipment)
	}
	if len(snapshot.Inventory) != 0 {
		t.Fatalf("bag inventory = %+v, source slot bag means equipped BAG20", snapshot.Inventory)
	}
	if snapshot.HUD.Stats[0].Base == nil || *snapshot.HUD.Stats[0].Base != 12 ||
		snapshot.HUD.Stats[0].Result != nil || snapshot.HUD.Stats[0].ResultLongTerm != nil {
		t.Fatalf("strength = %+v, want authored base only", snapshot.HUD.Stats[0])
	}
	if snapshot.HUD.Stats[4].Base == nil || *snapshot.HUD.Stats[4].Base != 10 {
		t.Fatalf("stamina = %+v, want endurance alias base 10", snapshot.HUD.Stats[4])
	}
	if snapshot.HUD.Actions[0].AbilityID == nil || *snapshot.HUD.Actions[0].AbilityID != "ability.melee.harbor-cleave" ||
		snapshot.HUD.Actions[1].AbilityID != nil {
		t.Fatalf("actions = %+v, want authored ability only in slot 0", snapshot.HUD.Actions[:2])
	}
}

// Handing out the stored slice would give the second character of the day the
// first one's leftovers.
func TestTemplateHandsOutACopy(t *testing.T) {
	t.Parallel()

	templates, option := testChargenTemplates(t)
	optionID := option.ID

	first, _ := templates.Template(optionID)
	first.HUD.Equipment[0].Quantity = 999
	*first.HUD.Stats[0].Base = 999
	first.State.Position.X = -1

	second, _ := templates.Template(optionID)
	if second.HUD.Equipment[0].Quantity == 999 || *second.HUD.Stats[0].Base == 999 || second.State.Position.X == -1 {
		t.Error("Template() handed out the stored snapshot rather than a copy")
	}
}

func testChargenTemplates(t *testing.T) (chargenTemplateSet, pack.ChargenOption) {
	t.Helper()
	option := pack.ChargenOption{
		ID: "chargen.league.warrior", SpawnPosition: pack.Vec3{X: 12, Y: 4.5},
		SpawnHeading: 90, StartingLevel: 1,
		StartingStats: []pack.StatValue{{Stat: "strength", Value: 12}, {Stat: "endurance", Value: 10}},
		StartingLoadout: []pack.LoadoutItem{
			{ItemID: "item.bag.newbie", Quantity: 1, Slot: "bag"},
			{ItemID: "item.armor.helm", Quantity: 1, Slot: "helm"},
		},
		StartingAbility: []string{"ability.melee.harbor-cleave"},
		StartingQuests:  []string{"quest.paper-harbor.mossy-gate"},
	}
	items := map[string]pack.Item{
		"item.bag.newbie": {
			ID:        "item.bag.newbie",
			BagLayout: &pack.BagLayout{ID: "bag.layout.18", Capacity: 18, PartitionSizes: []uint32{12, 6}},
		},
		"item.armor.helm": {ID: "item.armor.helm"},
	}
	templates, err := chargenTemplatesFrom([]pack.ChargenOption{option}, pack.Vec3{}, func(id string) (pack.Item, bool) {
		item, ok := items[id]
		return item, ok
	}, "test")
	if err != nil {
		t.Fatalf("chargenTemplatesFrom() error = %v", err)
	}
	return templates, option
}

// A shard instance id has to distinguish two processes on one host, or one
// would silently renew the other's play lock.
func TestShardInstanceIDPrefersTheConfiguredValue(t *testing.T) {
	t.Parallel()

	if got := shardInstanceID("shard-a"); got != "shard-a" {
		t.Errorf("shardInstanceID(configured) = %q, want it unchanged", got)
	}
	derived := shardInstanceID("")
	if derived == "" || derived == "shard" {
		t.Errorf("shardInstanceID(\"\") = %q, want a host and pid", derived)
	}
	if derived != shardInstanceID("") {
		t.Error("shardInstanceID() is not stable within one process")
	}
}
