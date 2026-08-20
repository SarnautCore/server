package content_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SarnautCore/server/internal/content"
)

func TestLoadZoneNPCsResolvesTablesAndDirectMobs(t *testing.T) {
	root := t.TempDir()
	tables := filepath.Join(root, "classic", "zones", "fixture-zone", "spawns", "tables")
	placements := filepath.Join(root, "classic", "zones", "fixture-zone", "spawns", "placements")
	if err := os.MkdirAll(tables, 0o755); err != nil {
		t.Fatalf("create tables directory: %v", err)
	}
	if err := os.MkdirAll(placements, 0o755); err != nil {
		t.Fatalf("create placements directory: %v", err)
	}

	writeFixture(t, filepath.Join(tables, "forest-creatures.yaml"), `
id: spawn.fixture-zone.table.forest-creatures
entries:
- object:
    id: mob.fixture-zone.wolf
  spawn_time: time-once
- object:
    id: item.fixture-zone.berry-bush
`)
	writeFixture(t, filepath.Join(placements, "forest.yaml"), `
placements:
- id: spawn.fixture-zone.placement.wolf
  object:
    id: spawn.fixture-zone.table.forest-creatures
  position: {x: 1.5, y: 2.5, z: 3.5}
  orientation: {yaw: 0.75}
- id: spawn.fixture-zone.placement.guide
  object:
    id: mob.fixture-zone.guide
  position: {x: 4, y: 5, z: 6}
- id: spawn.fixture-zone.placement.sleeping
  object:
    id: mob.fixture-zone.sleeping
  position: {x: 7, y: 8, z: 9}
  spawn_time: time-never
`)

	got, err := content.LoadZoneNPCs(root, "classic", "fixture-zone")
	if err != nil {
		t.Fatalf("LoadZoneNPCs() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadZoneNPCs() count = %d, want 2", len(got))
	}
	if got[0].MobID != "mob.fixture-zone.guide" {
		t.Errorf("first MobID = %q, want guide", got[0].MobID)
	}
	if got[1].MobID != "mob.fixture-zone.wolf" {
		t.Errorf("second MobID = %q, want wolf", got[1].MobID)
	}
	if got[1].Position.X != 1.5 || got[1].Position.Y != 2.5 || got[1].Position.Z != 3.5 {
		t.Errorf("wolf position = %+v, want 1.5, 2.5, 3.5", got[1].Position)
	}
	if got[1].Heading != 0.75 {
		t.Errorf("wolf heading = %v, want 0.75", got[1].Heading)
	}
}

func TestLoadZoneNPCsRejectsEscapingZonePath(t *testing.T) {
	if _, err := content.LoadZoneNPCs(t.TempDir(), "classic", "../outside"); err == nil {
		t.Fatal("LoadZoneNPCs() error = nil, want path validation error")
	}
}

func writeFixture(t *testing.T, path, payload string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write fixture %q: %v", path, err)
	}
}
