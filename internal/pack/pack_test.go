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
	"time"
)

// fixturePackID pins the digest of the vendored golden fixture. A silent change
// to the pack format, to the compiler, or to the demo dataset fails this test
// rather than surfacing as a mismatched handshake at connect time.
const fixturePackID = "9e1db72b1ef03cc1f61d16d03381d2744b51a009623bead25701490a230c3df5"

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
	byPlacement := make(map[string]NPCSpawn, len(spawns))
	for index, spawn := range spawns {
		byPlacement[spawn.PlacementID] = spawn
		// The `time-never` placement is authored and inert; it must not appear.
		if strings.HasSuffix(spawn.PlacementID, ".3") {
			t.Errorf("NPCSpawns() included the inert placement %q", spawn.PlacementID)
		}
		if index > 0 && spawns[index-1].PlacementID > spawn.PlacementID {
			t.Error("NPCSpawns() is not sorted by placement id")
		}
		if _, ok := loaded.Mob(spawn.MobID); !ok {
			t.Errorf("spawn %q resolves to mob %q, which the pack does not describe",
				spawn.PlacementID, spawn.MobID)
		}
	}

	// One placement names a spawn table and the rest name mobs directly. Both
	// paths have to land on a mob the pack can describe.
	throughTable, ok := byPlacement["spawn.paper-harbor.placement.tide-steps.1"]
	if !ok {
		t.Fatal("the placement that resolves through a spawn table is missing")
	}
	if throughTable.MobID != "mob.paper-harbor.copper-sparrow" {
		t.Errorf("table placement resolved to %q", throughTable.MobID)
	}
	if throughTable.Position == (Vec3{}) {
		t.Error("the table placement has no position")
	}
	if throughTable.Heading == 0 {
		t.Error("the table placement lost its authored heading")
	}

	// The respawn window is a property of the spawn slot, not of the mob.
	target, ok := byPlacement["placement.paper-harbor.tide-crab-1"]
	if !ok {
		t.Fatal("the M2 combat target placement is missing")
	}
	if target.RespawnMin != 10*time.Second || target.RespawnMax != 14*time.Second {
		t.Errorf("respawn window = [%v, %v], want [10s, 14s]", target.RespawnMin, target.RespawnMax)
	}
}

// TestCombatTablesCarryTheRulesTheSpecReads is the pack-side half of the
// promise that combat rules are content: if these fields do not survive the
// compile, nothing downstream can read them and every one of them becomes a Go
// constant by default.
func TestCombatTablesCarryTheRulesTheSpecReads(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ability, ok := loaded.Ability("ability.melee.harbor-cleave")
	if !ok {
		t.Fatal("the fixture ability is missing from the pack")
	}
	if ability.RangeM != 10 {
		t.Errorf("range_m = %v, want 10", ability.RangeM)
	}
	if !ability.TriggersGCD {
		t.Error("triggers_gcd = false, want true")
	}
	if len(ability.Effects) != 1 || ability.Effects[0].Kind != "damage" {
		t.Fatalf("effects = %+v, want one damage effect", ability.Effects)
	}
	if ability.Effects[0].Amount != 18 || ability.Effects[0].AttackPowerCoeff != 0.5 {
		t.Errorf("effect = %+v, want amount 18 and coefficient 0.5", ability.Effects[0])
	}

	wild, ok := loaded.Faction("faction.wild")
	if !ok {
		t.Fatal("the hostile faction is missing from the pack")
	}
	if !wild.Attackable {
		t.Error("faction.wild is not attackable; nothing in M2 could be killed")
	}
	if got := wild.StanceTowards("faction.league"); got != StanceHostile {
		t.Errorf("faction.wild towards faction.league = %q, want %q", got, StanceHostile)
	}
	league, ok := loaded.Faction("faction.league")
	if !ok {
		t.Fatal("the player faction is missing from the pack")
	}
	if league.Attackable || league.StanceTowards("faction.league") != StanceFriendly {
		t.Errorf("faction.league = %+v, want unattackable and friendly to itself", league)
	}

	// The worked example in mechanics/combat.md section 6.1 assumes a level 2
	// mob with no hp_mod. The fixture has to actually say that.
	mob, ok := loaded.Mob("mob.paper-harbor.tide-crab")
	if !ok {
		t.Fatal("the M2 target mob is missing from the pack")
	}
	if mob.LevelMin != 2 || mob.LevelMax != 2 {
		t.Errorf("level range = [%d, %d], want [2, 2]", mob.LevelMin, mob.LevelMax)
	}
	if mob.HPMod != 1 {
		t.Errorf("hp_mod = %v, want 1", mob.HPMod)
	}
	if mob.FactionID != "faction.wild" {
		t.Errorf("faction = %q, want faction.wild", mob.FactionID)
	}
	if mob.AggroRadiusM != 12 || mob.LeashRadiusM != 40 {
		t.Errorf("aggro/leash = %v/%v, want 12/40", mob.AggroRadiusM, mob.LeashRadiusM)
	}
	if mob.WalkSpeed != 2 {
		t.Errorf("walk_speed = %v, want 2", mob.WalkSpeed)
	}

	// A second mob with different numbers, so that a reader which pinned one
	// mob's values fails here.
	sparrow, ok := loaded.Mob("mob.paper-harbor.copper-sparrow")
	if !ok {
		t.Fatal("the second fixture mob is missing from the pack")
	}
	if sparrow.AggroRadiusM == mob.AggroRadiusM || sparrow.HPMod == mob.HPMod {
		t.Errorf("the two fixture mobs do not differ: %+v and %+v", sparrow, mob)
	}
}

func TestChargenOptionsCarryTheWholeStartingCharacter(t *testing.T) {
	t.Parallel()

	loaded, err := Load(fixtureDirectory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	options := loaded.ChargenOptions()
	if len(options) != 1 {
		t.Fatalf("ChargenOptions() returned %d options, want 1", len(options))
	}
	option := options[0]
	if option.ID != "chargen.league.warrior" || !option.Enabled {
		t.Fatalf("ChargenOptions()[0] = %+v", option)
	}
	if option.Race != "race.human" || option.Class != "class.warrior" || option.Faction != "faction.league" {
		t.Errorf("option taxonomy = %q/%q/%q", option.Race, option.Class, option.Faction)
	}
	// The spawn a created character gets comes from the option, not from the
	// zone's PlayerSpawn, which is why the two differ in the fixture (ADR 0032).
	if option.SpawnZoneID != loaded.Zone().ID {
		t.Errorf("SpawnZoneID = %q, want %q", option.SpawnZoneID, loaded.Zone().ID)
	}
	if option.SpawnPosition == (Vec3{}) || option.SpawnPosition == loaded.Zone().PlayerSpawn {
		t.Errorf("SpawnPosition = %+v; the option must carry its own spawn", option.SpawnPosition)
	}
	if option.StartingLevel != 1 {
		t.Errorf("StartingLevel = %d, want 1", option.StartingLevel)
	}
	if len(option.StartingStats) == 0 || len(option.StartingLoadout) == 0 {
		t.Fatalf("option carries no stats or no loadout: %+v", option)
	}
	if option.StartingLoadout[0].Quantity == 0 || option.StartingLoadout[0].Slot == "" {
		t.Errorf("loadout entry = %+v", option.StartingLoadout[0])
	}
	if len(option.StartingQuests) == 0 || len(option.StartingAbility) == 0 {
		t.Errorf("option grants no starting quest or ability: %+v", option)
	}
}

func TestChargenOptionsAreAbsentFromAPackWithoutTheTable(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	document := loadManifest(t, directory)
	kept := document.Tables[:0]
	for _, entry := range document.Tables {
		if entry.Name != tableChargen {
			kept = append(kept, entry)
		}
	}
	document.Tables = kept
	saveManifest(t, directory, document)
	if err := os.Remove(filepath.Join(directory, "tables", tableChargen+".sptbl")); err != nil {
		t.Fatalf("remove chargen table: %v", err)
	}
	reseal(t, directory)

	// A pack with no chargen table still loads: only the auth service needs
	// options, and a shard-only pack is a legitimate artifact.
	loaded, err := Load(directory, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := loaded.ChargenOptions(); len(got) != 0 {
		t.Errorf("ChargenOptions() = %v, want none", got)
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

// openTable's doc comment promises that every failure is an ErrMalformedTable,
// and its own bounds checks are what make the rest of the reader safe to index
// blind. In uint32 the region lengths wrap: with row_count 0x40000000 the key
// index is 12*row_count = 0 bytes long and the row index is 4*(row_count+1) = 4,
// both of which fit inside a 48-byte file, and the row-index walk then reads off
// the end of the payload and panics. The Rust twin uses checked arithmetic, so
// this is also the difference between `sarnaut-pack verify` and the shard
// accepting the same set of files.
func TestOpenTableRefusesARowCountThatOverflowsTheRegionArithmetic(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 48)
	copy(payload, tableMagic)
	binary.LittleEndian.PutUint16(payload[4:], tableFormatV1)
	binary.LittleEndian.PutUint16(payload[6:], tableFlagKeyIndex)
	binary.LittleEndian.PutUint32(payload[12:], 0x40000000) // row_count
	binary.LittleEndian.PutUint32(payload[16:], 40)         // key_index_offset
	binary.LittleEndian.PutUint32(payload[20:], 40)         // row_index_offset
	binary.LittleEndian.PutUint32(payload[24:], 44)         // row_data_offset
	binary.LittleEndian.PutUint32(payload[28:], 4)          // row_data_bytes

	_, err := openTable("crafted", payload)
	if !errors.Is(err, ErrMalformedTable) {
		t.Fatalf("openTable() error = %v, want ErrMalformedTable rather than a panic", err)
	}
}

// A manifest is data, and a reader that follows it out of its own directory is
// an arbitrary-file oracle: the size and BLAKE3 of the named file come back in
// the digest-mismatch error.
func TestLoadRefusesAManifestFileEntryThatEscapesThePackDirectory(t *testing.T) {
	t.Parallel()

	for _, escape := range []string{
		"../../../../etc/shadow",
		"tables/../../outside.sptbl",
		"/etc/shadow",
	} {
		t.Run(escape, func(t *testing.T) {
			t.Parallel()

			directory := copyFixture(t)
			document := loadManifest(t, directory)
			document.Tables[0].File = escape
			saveManifest(t, directory, document)

			_, err := Load(directory, Options{})
			if !errors.Is(err, ErrMalformedTable) {
				t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
			}
			if strings.Contains(err.Error(), "bytes") {
				t.Errorf("Load() error %q reports the escaped file's size", err)
			}
		})
	}
}

// Two directories that differ only by an unlisted table would otherwise share a
// pack_id, so a stale file left by a partial rebuild is silently loaded past.
// `sarnaut-pack verify` refuses exactly this, and the two readers have to agree.
func TestLoadRejectsATableTheManifestDoesNotList(t *testing.T) {
	t.Parallel()

	directory := copyFixture(t)
	writeFile(t, filepath.Join(directory, "tables", "stowaway.sptbl"), []byte("SPK1"))

	_, err := Load(directory, Options{})
	if !errors.Is(err, ErrMalformedTable) {
		t.Fatalf("Load() error = %v, want ErrMalformedTable", err)
	}
	if !strings.Contains(err.Error(), "stowaway.sptbl") {
		t.Errorf("Load() error %q does not name the unlisted file", err)
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
