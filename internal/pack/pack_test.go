package pack

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixturePackID pins the digest of the vendored golden fixture. A silent change
// to the pack format, to the compiler, or to the demo dataset fails this test
// rather than surfacing as a mismatched handshake at connect time.
const fixturePackID = "b2fa8016f2d8be24a116db81d6c093f254cc3b36b4e0e337872866de28a293dc"

// fixtureDirectory is the vendored pack every server test shares. It is
// compiled from `data-schemas/demo`, which is invented content, so no
// MY.GAMES-derived data reaches this repository (ADR 0011, ADR 0029).
var fixtureDirectory = filepath.Join("..", "..", "testdata", "packs", "demo")

func TestLoadReadsTheVendoredFixture(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.ID() != fixturePackID {
		t.Errorf("ID() = %q, want %q; the fixture pack changed without the pin", loaded.ID(), fixturePackID)
	}
	if loaded.KeepExtra() {
		t.Error("KeepExtra() = true; the vendored fixture must not carry the untyped passthrough")
	}

	zone := loaded.Zone()
	if zone.ID != "zone.paper-harbor" || zone.Slug != "paper-harbor" || zone.Ruleset != "classic" {
		t.Errorf("Zone() = %+v", zone)
	}
	if zone.PlayerSpawn == (Vec3{}) {
		t.Error("Zone().PlayerSpawn is the origin; the shard would drop every player at 0,0,0")
	}
}

func TestNPCSpawnsResolveThroughTablesAndSkipInertPlacements(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	spawns := loaded.NPCSpawns()
	if len(spawns) != 2 {
		t.Fatalf("NPCSpawns() returned %d spawns, want 2", len(spawns))
	}
	// One placement names a spawn table and the other names a mob directly;
	// both must land on the same mob id.
	for _, spawn := range spawns {
		if spawn.MobID != "mob.paper-harbor.copper-sparrow" {
			t.Errorf("spawn %q resolved to %q", spawn.PlacementID, spawn.MobID)
		}
		// The third placement is authored `time-never` and must not appear.
		if strings.HasSuffix(spawn.PlacementID, ".3") {
			t.Errorf("NPCSpawns() included the inert placement %q", spawn.PlacementID)
		}
	}
	if spawns[0].PlacementID > spawns[1].PlacementID {
		t.Error("NPCSpawns() is not sorted by placement id")
	}
	if spawns[0].Heading == 0 && spawns[1].Heading == 0 {
		t.Error("no spawn carried an authored heading")
	}
	if spawns[0].Position == (Vec3{}) {
		t.Error("the first spawn has no position")
	}
}

func TestNPCSpawnsHandsOutACopy(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loaded.NPCSpawns()[0].MobID = "mob.scribbled-over"
	if loaded.NPCSpawns()[0].MobID == "mob.scribbled-over" {
		t.Error("NPCSpawns() handed out the pack's own slice")
	}
}

// Every case below corrupts a copy of the fixture in a temp directory, so the
// vendored bytes are never touched.

func TestLoadRejectsACorruptedTable(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	corruptTable(t, directory, func(payload []byte) { payload[len(payload)-1] ^= 0x01 })

	_, err := Load(directory, Options{})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
	}
	if !strings.Contains(err.Error(), "placements") {
		t.Errorf("Load() error %q does not name the table", err)
	}
}

func TestLoadRejectsATruncatedTable(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	table := filepath.Join(directory, "tables", "placements.sptbl")
	payload := readFile(t, table)
	writeFile(t, table, payload[:len(payload)-1])

	if _, err := Load(directory, Options{}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
	}
}

func TestLoadRejectsARewrittenPackID(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	document.PackID = strings.Repeat("0", 64)
	saveManifest(t, directory, document)

	if _, err := Load(directory, Options{}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
	}
}

func TestLoadRejectsAnUnsupportedSchemaVersion(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	document.SchemaVersion = schemaVersion + 1
	saveManifest(t, directory, document)

	if _, err := Load(directory, Options{}); !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("Load() error = %v, want ErrUnsupportedSchemaVersion", err)
	}
}

func TestLoadRejectsAKeepExtraPackUnlessAllowed(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	// `keep_extra` is manifest metadata and is not an input to pack_id, so
	// flipping it leaves every digest intact.
	document := loadManifest(t, directory)
	document.KeepExtra = true
	saveManifest(t, directory, document)

	if _, err := Load(directory, Options{}); !errors.Is(err, ErrExtraNotAllowed) {
		t.Fatalf("Load() error = %v, want ErrExtraNotAllowed", err)
	}
	loaded, err := Load(directory, Options{AllowExtra: true})
	if err != nil {
		t.Fatalf("Load(AllowExtra) error = %v", err)
	}
	if !loaded.KeepExtra() {
		t.Error("KeepExtra() = false after loading a keep_extra pack")
	}
}

func TestLoadRejectsMalformedTables(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		corrupt func(payload []byte)
		want    string
	}{
		{"wrong magic", func(payload []byte) { payload[0] = 'X' }, "magic"},
		{"unsupported format version", func(payload []byte) { binary.LittleEndian.PutUint16(payload[4:], 2) }, "format_version"},
		{"reserved flag bit", func(payload []byte) { binary.LittleEndian.PutUint16(payload[6:], 0b11) }, "reserved flag bits"},
		{"no key index", func(payload []byte) { binary.LittleEndian.PutUint16(payload[6:], 0) }, "no key index"},
		{"reserved header bytes set", func(payload []byte) { payload[39] = 1 }, "reserved header bytes"},
		{"row data runs past the file", func(payload []byte) { binary.LittleEndian.PutUint32(payload[28:], 1<<20) }, "past the"},
		{"offset inside the header", func(payload []byte) { binary.LittleEndian.PutUint32(payload[16:], 8) }, "overlaps the header"},
		{"row index not increasing", func(payload []byte) {
			rowIndex := binary.LittleEndian.Uint32(payload[20:])
			binary.LittleEndian.PutUint32(payload[rowIndex+4:], 0)
		}, "strictly increasing"},
		{"key index unsorted", func(payload []byte) {
			keyIndex := binary.LittleEndian.Uint32(payload[16:])
			binary.LittleEndian.PutUint64(payload[keyIndex:], ^uint64(0))
		}, "not sorted"},
		{"row type disagrees with the manifest", func(payload []byte) {
			binary.LittleEndian.PutUint32(payload[8:], 1)
		}, "the manifest records"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			directory := copyFixture(t)
			corruptTable(t, directory, testCase.corrupt)
			reseal(t, directory)

			_, err := Load(directory, Options{})
			if !errors.Is(err, ErrMalformedTable) {
				t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("Load() error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestLoadRejectsARowCountThatDisagreesWithTheManifest(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	for index := range document.Tables {
		if document.Tables[index].Name == tablePlacements {
			document.Tables[index].Rows = 99
		}
	}
	saveManifest(t, directory, document)

	_, err := Load(directory, Options{})
	if !errors.Is(err, ErrMalformedTable) {
		t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
	}
}

func TestLoadRejectsAPackMissingATableTheShardNeeds(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	kept := document.Tables[:0]
	for _, entry := range document.Tables {
		if entry.Name != tableZone {
			kept = append(kept, entry)
		}
	}
	document.Tables = kept
	saveManifest(t, directory, document)
	if err := os.Remove(filepath.Join(directory, "tables", "zone.sptbl")); err != nil {
		t.Fatalf("remove zone table: %v", err)
	}
	reseal(t, directory)

	if _, err := Load(directory, Options{}); !errors.Is(err, ErrMissingTable) {
		t.Fatalf("Load() error = %v, want ErrMissingTable", err)
	}
}

func TestLoadReportsAMissingManifest(t *testing.T) {
	t.Parallel()

	if _, err := Load(t.TempDir(), Options{}); err == nil {
		t.Fatal("Load() error = nil, want a missing manifest error")
	}
}

func copyFixture(t *testing.T) string {
	t.Helper()
	destination := t.TempDir()
	if err := os.MkdirAll(filepath.Join(destination, "tables"), 0o755); err != nil {
		t.Fatalf("create table directory: %v", err)
	}
	writeFile(t, filepath.Join(destination, manifestFileName),
		readFile(t, filepath.Join(fixtureDirectory, manifestFileName)))

	entries, err := os.ReadDir(filepath.Join(fixtureDirectory, "tables"))
	if err != nil {
		t.Fatalf("list fixture tables: %v", err)
	}
	for _, entry := range entries {
		writeFile(t, filepath.Join(destination, "tables", entry.Name()),
			readFile(t, filepath.Join(fixtureDirectory, "tables", entry.Name())))
	}
	return destination
}

func corruptTable(t *testing.T, directory string, corrupt func(payload []byte)) {
	t.Helper()
	path := filepath.Join(directory, "tables", tablePlacements+".sptbl")
	payload := readFile(t, path)
	corrupt(payload)
	writeFile(t, path, payload)
}

// reseal recomputes every digest the manifest records from the bytes now on
// disk, so a test that means to exercise a structural rule is not stopped by
// the digest check first.
func reseal(t *testing.T, directory string) {
	t.Helper()
	document := loadManifest(t, directory)
	named := make([]namedTable, 0, len(document.Tables))
	for index, entry := range document.Tables {
		payload := readFile(t, filepath.Join(directory, filepath.FromSlash(entry.File)))
		document.Tables[index].Bytes = uint64(len(payload))
		document.Tables[index].Blake3 = tableDigest(payload)
		named = append(named, namedTable{Name: entry.Name, Bytes: payload})
	}
	sort.Slice(named, func(left, right int) bool { return named[left].Name < named[right].Name })
	document.PackID = computePackID(named)
	saveManifest(t, directory, document)
}

func loadManifest(t *testing.T, directory string) manifest {
	t.Helper()
	var document manifest
	if err := json.Unmarshal(readFile(t, filepath.Join(directory, manifestFileName)), &document); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return document
}

func saveManifest(t *testing.T, directory string, document manifest) {
	t.Helper()
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	writeFile(t, filepath.Join(directory, manifestFileName), append(payload, '\n'))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return payload
}

func writeFile(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
