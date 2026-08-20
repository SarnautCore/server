package main

import (
	"path/filepath"
	"testing"

	"github.com/SarnautCore/server/internal/pack"
)

// The point of ADR 0032 is that none of this is in Go source. The test reads
// the vendored fixture pack and asserts that what a fresh character
// materializes with is what the pack row says — spawn, level, loadout and
// starting quests.
func TestChargenTemplatesComeFromThePack(t *testing.T) {
	t.Parallel()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	options := content.ChargenOptions()
	if len(options) == 0 {
		t.Fatal("the fixture pack carries no chargen options")
	}
	option := options[0]

	templates, err := chargenTemplates(content, content.Zone().PlayerSpawn)
	if err != nil {
		t.Fatalf("chargenTemplates() error = %v", err)
	}
	snapshot, ok := templates.Template(option.ID)
	if !ok {
		t.Fatalf("Template(%q) is missing", option.ID)
	}

	if snapshot.State.Position.X != option.SpawnPosition.X ||
		snapshot.State.Position.Y != option.SpawnPosition.Y ||
		snapshot.State.Position.Z != option.SpawnPosition.Z {
		t.Errorf("spawn = %+v, want the pack's %+v", snapshot.State.Position, option.SpawnPosition)
	}
	// The zone's configured player spawn is a fallback for debug and unowned
	// entities; a character's spawn is the option's.
	if snapshot.State.Position.X == content.Zone().PlayerSpawn.X &&
		snapshot.State.Position.Y == content.Zone().PlayerSpawn.Y {
		t.Error("the template used the zone's PlayerSpawn instead of the option's spawn")
	}
	if snapshot.State.Heading != option.SpawnHeading {
		t.Errorf("heading = %v, want %v", snapshot.State.Heading, option.SpawnHeading)
	}
	if snapshot.State.Level != int32(option.StartingLevel) {
		t.Errorf("level = %d, want %d", snapshot.State.Level, option.StartingLevel)
	}
	if len(snapshot.Inventory) != len(option.StartingLoadout) {
		t.Fatalf("inventory has %d items, want the pack's %d", len(snapshot.Inventory), len(option.StartingLoadout))
	}
	for index, item := range option.StartingLoadout {
		if snapshot.Inventory[index].ItemID != item.ItemID {
			t.Errorf("slot %d holds %q, want %q", index, snapshot.Inventory[index].ItemID, item.ItemID)
		}
		if snapshot.Inventory[index].Quantity != int32(item.Quantity) {
			t.Errorf("slot %d holds %d, want %d", index, snapshot.Inventory[index].Quantity, item.Quantity)
		}
	}
	if len(snapshot.Quests) != len(option.StartingQuests) {
		t.Errorf("quests = %+v, want the pack's %v", snapshot.Quests, option.StartingQuests)
	}
	if snapshot.State.Health <= 0 {
		t.Error("a fresh character materialized with no health")
	}
}

// Handing out the stored slice would give the second character of the day the
// first one's leftovers.
func TestTemplateHandsOutACopy(t *testing.T) {
	t.Parallel()

	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	templates, err := chargenTemplates(content, content.Zone().PlayerSpawn)
	if err != nil {
		t.Fatalf("chargenTemplates() error = %v", err)
	}
	optionID := content.ChargenOptions()[0].ID

	first, _ := templates.Template(optionID)
	if len(first.Inventory) == 0 {
		t.Fatal("the fixture option grants no starting items, so this test proves nothing")
	}
	first.Inventory[0].Quantity = 999
	first.State.Position.X = -1

	second, _ := templates.Template(optionID)
	if second.Inventory[0].Quantity == 999 || second.State.Position.X == -1 {
		t.Error("Template() handed out the stored snapshot rather than a copy")
	}
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
