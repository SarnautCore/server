package pack

import (
	"errors"
	"math"
	"strings"
	"testing"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const locatorMapID = "inst-league-start"

func TestOldPackWithoutMapLocatorsStillLoads(t *testing.T) {
	t.Parallel()
	content, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if content.HasMapLocatorIndex() {
		t.Fatal("old fixture unexpectedly has a map-locator index")
	}
	if _, ok := content.MapLocator(locatorMapID, "DemonSpawn4"); ok {
		t.Fatal("old fixture resolved an absent locator")
	}
}

func TestPackIndexesExactQuest430MapLocators(t *testing.T) {
	t.Parallel()
	directory := copyFixture(t)
	rows := []compiledRow{
		locatorCompiledRow(locatorMapID, "DemonSpawn4", 318.3686, 5871.289, 32.2894),
		locatorCompiledRow(locatorMapID, "Firewall", 306.745, 5830.664, 32.2787),
	}
	replaceCompiledTable(t, directory, tableMapLocators, contentv1.RowType_ROW_TYPE_MAP_LOCATOR, rows)
	reseal(t, directory)

	content, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !content.HasMapLocatorIndex() {
		t.Fatal("pack has no map-locator index")
	}
	assertPackLocator(t, content, "DemonSpawn4", Vec3{X: 318.3686, Y: 5871.289, Z: 32.2894})
	assertPackLocator(t, content, "Firewall", Vec3{X: 306.745, Y: 5830.664, Z: 32.2787})
	if _, ok := content.MapLocator(locatorMapID, "Missing"); ok {
		t.Fatal("missing pair resolved")
	}
	if _, ok := content.MapLocator("ext.maps.inst-league-start.map-resource", "Firewall"); ok {
		t.Fatal("source-format alias resolved")
	}
}

func TestPackRejectsDuplicateMapLocatorPair(t *testing.T) {
	t.Parallel()
	directory := copyFixture(t)
	rows := []compiledRow{
		locatorCompiledRow(locatorMapID, "Firewall", 1, 2, 3),
		locatorCompiledRow(locatorMapID, "Firewall", 4, 5, 6),
	}
	replaceCompiledTable(t, directory, tableMapLocators, contentv1.RowType_ROW_TYPE_MAP_LOCATOR, rows)
	reseal(t, directory)

	_, err := Load(directory, Options{})
	if !errors.Is(err, ErrMalformedTable) || !strings.Contains(err.Error(), "repeats locator key") {
		t.Fatalf("Load() error = %v, want duplicate locator failure", err)
	}
}

func TestPackRejectsMalformedMapLocatorRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		row  compiledRow
		want string
	}{
		{name: "empty map", row: locatorCompiledRow("", "Firewall", 1, 2, 3), want: "empty map_id"},
		{name: "map slash", row: locatorCompiledRow("inst/league", "Firewall", 1, 2, 3), want: "containing a slash"},
		{name: "script backslash", row: locatorCompiledRow(locatorMapID, `Fire\\wall`, 1, 2, 3), want: "backslash"},
		{name: "script control", row: locatorCompiledRow(locatorMapID, "Fire\nwall", 1, 2, 3), want: "control character"},
		{name: "missing position", row: compiledRow{
			id: locatorMapID + "/Firewall", message: &contentv1.MapLocator{MapId: locatorMapID, ScriptId: "Firewall"},
		}, want: "missing or non-finite position"},
		{name: "non-finite", row: locatorCompiledRow(locatorMapID, "Firewall", float32(math.NaN()), 2, 3), want: "non-finite position"},
		{name: "wrong key", row: compiledRow{
			id:      "inst-league-start/Other",
			message: &contentv1.MapLocator{MapId: locatorMapID, ScriptId: "Firewall", Position: &contentv1.Vec3{X: 1, Y: 2, Z: 3}},
		}, want: "does not match its key index entry"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyFixture(t)
			replaceCompiledTable(t, directory, tableMapLocators, contentv1.RowType_ROW_TYPE_MAP_LOCATOR,
				[]compiledRow{testCase.row})
			reseal(t, directory)
			_, err := Load(directory, Options{})
			if !errors.Is(err, ErrMalformedTable) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Load() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func locatorCompiledRow(mapID, scriptID string, x, y, z float32) compiledRow {
	return compiledRow{
		id: mapLocatorRowKey(mapID, scriptID),
		message: &contentv1.MapLocator{
			MapId: mapID, ScriptId: scriptID, Position: &contentv1.Vec3{X: x, Y: y, Z: z},
		},
	}
}

func assertPackLocator(t *testing.T, content *Pack, scriptID string, want Vec3) {
	t.Helper()
	got, ok := content.MapLocator(locatorMapID, scriptID)
	if !ok || got != want {
		t.Fatalf("MapLocator(%q, %q) = %#v, %t, want %#v", locatorMapID, scriptID, got, ok, want)
	}
}
