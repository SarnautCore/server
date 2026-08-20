// Package pack reads the compiled runtime content packs `sarnaut-pack` writes.
//
// A pack is a directory of `.sptbl` tables plus a `manifest.json` whose
// `pack_id` is a BLAKE3-256 digest over the table bytes. The shard loads packs
// and never parses authored YAML (ADR 0006, ADR 0029).
package pack

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

// Named failures a caller can distinguish. Every one of them aborts startup:
// there is no partial load and no repair path.
var (
	// ErrUnsupportedSchemaVersion means the pack was written for a different
	// format version of the pack itself.
	ErrUnsupportedSchemaVersion = errors.New("unsupported pack schema version")
	// ErrDigestMismatch means the bytes on disk do not hash to what the
	// manifest records, for one table or for the pack as a whole.
	ErrDigestMismatch = errors.New("pack digest mismatch")
	// ErrMalformedTable means a table broke a structural rule of the format.
	ErrMalformedTable = errors.New("malformed pack table")
	// ErrMissingTable means the pack does not carry a table the shard needs.
	ErrMissingTable = errors.New("pack is missing a required table")
	// ErrExtraNotAllowed means the pack carries the untyped `extra:`
	// passthrough and the shard was not configured to accept it.
	ErrExtraNotAllowed = errors.New("pack carries the untyped extra passthrough")
)

// Table names this reader knows.
const (
	tableZone        = "zone"
	tablePlacements  = "placements"
	tableSpawnTables = "spawn-tables"
	tableChargen     = "chargen"
)

// spawnTimeNever marks an authored object as inert: it exists in the pack, and
// the shard does not spawn it.
const spawnTimeNever = "time-never"

// mobIDPrefix distinguishes a placement that names a mob directly from one that
// points at a spawn table.
const mobIDPrefix = "mob."

// Vec3 is a world-space position. Z is the vertical axis.
type Vec3 struct {
	X float32
	Y float32
	Z float32
}

// NPCSpawn is one mob the shard registers at boot, resolved from a placement
// either directly or through a spawn table.
type NPCSpawn struct {
	PlacementID string
	MobID       string
	Position    Vec3
	Heading     float32
	// RespawnMin and RespawnMax bound the delay before this slot fills again
	// (mechanics/combat.md rule 5.9.6). Both zero means the placement authored
	// no window and the shard uses its own default.
	RespawnMin time.Duration
	RespawnMax time.Duration
}

// StatValue is one starting stat of a chargen option.
type StatValue struct {
	Stat  string
	Value float32
}

// LoadoutItem is one item a fresh character of a chargen option is created
// with. Slot is an equipment slot, or `bag`.
type LoadoutItem struct {
	ItemID   string
	Quantity uint32
	Slot     string
}

// ChargenOption is one selectable character-creation option (ADR 0032).
//
// Every field here is a fact the auth service would otherwise have to hold as a
// Go constant: the race and class a player may pick, where the character
// spawns, what it starts with. Adding the second playable option is a data
// change.
type ChargenOption struct {
	ID              string
	Race            string
	Class           string
	Sex             string
	Faction         string
	Enabled         bool
	NameKey         string
	DescriptionKey  string
	VisualRef       string
	SpawnZoneID     string
	SpawnPosition   Vec3
	SpawnHeading    float32
	StartingLevel   uint32
	StartingStats   []StatValue
	StartingLoadout []LoadoutItem
	StartingAbility []string
	StartingQuests  []string
}

// Zone is what a pack says about the zone it describes.
type Zone struct {
	ID                 string
	Ruleset            string
	Slug               string
	PlayerSpawn        Vec3
	PlayerSpawnHeading float32
}

// Options tunes what a reader will accept.
type Options struct {
	// AllowExtra permits a pack built with `--keep-extra`. Its rows carry
	// verbatim MY.GAMES attribute names, so the default is false (ADR 0011).
	AllowExtra bool
}

// Pack is a loaded, fully validated content pack.
type Pack struct {
	id         string
	directory  string
	keepExtra  bool
	zone       Zone
	npcs       []NPCSpawn
	abilities  map[string]Ability
	abilityIDs []string
	factions   map[string]Faction
	mobs       map[string]Mob
	chargen    []ChargenOption
	lootTables map[string]LootTable
	// items is the table handle, not its contents. See Pack.Item: the item
	// tree is the one table this reader never materializes.
	items *table
}

// Load reads, validates and resolves the pack directory at `directory`.
//
// Validation is all-or-nothing: magic and version, offsets and ordering, each
// table's digest, and the recomputed `pack_id`. A pack that fails any of them
// is not usable, so Load returns an error rather than a degraded Pack.
func Load(directory string, options Options) (*Pack, error) {
	document, err := readManifest(directory)
	if err != nil {
		return nil, err
	}
	if document.KeepExtra && !options.AllowExtra {
		return nil, fmt.Errorf(
			"%w: pack %q (%s) was built with --keep-extra; set content.allow_extra to load it",
			ErrExtraNotAllowed, directory, document.PackID,
		)
	}

	tables := make(map[string]*table, len(document.Tables))
	digestInput := make([]namedTable, 0, len(document.Tables))
	listed := make(map[string]struct{}, len(document.Tables))
	for _, entry := range document.Tables {
		path, err := tablePath(directory, entry.File)
		if err != nil {
			return nil, err
		}
		listed[entry.File] = struct{}{}
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read pack table %q: %w", path, err)
		}
		if uint64(len(payload)) != entry.Bytes {
			return nil, fmt.Errorf(
				"%w: table %q is %d bytes, but the manifest records %d",
				ErrDigestMismatch, entry.Name, len(payload), entry.Bytes,
			)
		}
		if digest := tableDigest(payload); digest != entry.Blake3 {
			return nil, fmt.Errorf(
				"%w: table %q hashes to %s, but the manifest records %s",
				ErrDigestMismatch, entry.Name, digest, entry.Blake3,
			)
		}

		loaded, err := openTable(entry.Name, payload)
		if err != nil {
			return nil, err
		}
		if name := contentv1.RowType(loaded.rowTypeID).String(); name != entry.RowType {
			return nil, fmt.Errorf(
				"%w: table %q holds %s rows, but the manifest records %s",
				ErrMalformedTable, entry.Name, name, entry.RowType,
			)
		}
		if loaded.rowCount != entry.Rows {
			return nil, fmt.Errorf(
				"%w: table %q holds %d rows, but the manifest records %d",
				ErrMalformedTable, entry.Name, loaded.rowCount, entry.Rows,
			)
		}
		tables[entry.Name] = loaded
		digestInput = append(digestInput, namedTable{Name: entry.Name, Bytes: payload})
	}

	if err := rejectUnlistedTables(directory, listed); err != nil {
		return nil, err
	}

	sort.Slice(digestInput, func(left, right int) bool {
		return digestInput[left].Name < digestInput[right].Name
	})
	if recomputed := computePackID(digestInput); recomputed != document.PackID {
		return nil, fmt.Errorf(
			"%w: pack %q table bytes hash to %s, but the manifest records %s",
			ErrDigestMismatch, directory, recomputed, document.PackID,
		)
	}

	zone, err := readZone(tables)
	if err != nil {
		return nil, err
	}
	npcs, err := resolveNPCs(tables)
	if err != nil {
		return nil, err
	}
	abilities, abilityIDs, err := readAbilities(tables)
	if err != nil {
		return nil, err
	}
	factions, err := readFactions(tables)
	if err != nil {
		return nil, err
	}
	mobs, err := readMobs(tables)
	if err != nil {
		return nil, err
	}
	chargen, err := readChargen(tables)
	if err != nil {
		return nil, err
	}
	lootTables, err := readLootTables(tables)
	if err != nil {
		return nil, err
	}
	items, err := readItems(tables)
	if err != nil {
		return nil, err
	}
	return &Pack{
		id:         document.PackID,
		directory:  directory,
		keepExtra:  document.KeepExtra,
		zone:       zone,
		npcs:       npcs,
		abilities:  abilities,
		abilityIDs: abilityIDs,
		factions:   factions,
		mobs:       mobs,
		chargen:    chargen,
		lootTables: lootTables,
		items:      items,
	}, nil
}

// ID is the pack's content digest: 64 lowercase hex characters.
func (p *Pack) ID() string { return p.id }

// Directory is where the pack was loaded from.
func (p *Pack) Directory() string { return p.directory }

// KeepExtra reports whether the pack carries the untyped `extra:` passthrough.
func (p *Pack) KeepExtra() bool { return p.keepExtra }

// Zone describes the zone this pack covers.
func (p *Pack) Zone() Zone { return p.zone }

// NPCSpawns lists every mob the shard should register, sorted by placement id
// and then by mob id. Inert placements and inert spawn-table entries are
// already excluded.
func (p *Pack) NPCSpawns() []NPCSpawn {
	result := make([]NPCSpawn, len(p.npcs))
	copy(result, p.npcs)
	return result
}

// ChargenOptions lists every character-creation option the pack carries, in
// canonical id order, enabled or not. A pack that carries no chargen table
// returns none: the auth service is what refuses to start without options, so
// that a shard-only pack still loads (ADR 0032).
func (p *Pack) ChargenOptions() []ChargenOption {
	result := make([]ChargenOption, len(p.chargen))
	copy(result, p.chargen)
	return result
}

func readChargen(tables map[string]*table) ([]ChargenOption, error) {
	loaded, ok := tables[tableChargen]
	if !ok {
		return nil, nil
	}
	options := make([]ChargenOption, 0, loaded.rowCount)
	for _, encoded := range loaded.rows() {
		var row contentv1.ChargenOption
		if err := proto.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("%w: decode chargen row: %w", ErrMalformedTable, err)
		}
		option := ChargenOption{
			ID:              row.GetId(),
			Race:            row.GetRace(),
			Class:           row.GetClass(),
			Sex:             row.GetSex(),
			Faction:         row.GetFaction(),
			Enabled:         row.GetEnabled(),
			NameKey:         row.GetNameKey(),
			DescriptionKey:  row.GetDescriptionKey(),
			VisualRef:       row.GetVisualRef(),
			SpawnZoneID:     row.GetSpawnZoneId(),
			SpawnPosition:   vec3(row.GetSpawnPosition()),
			SpawnHeading:    row.GetSpawnHeading(),
			StartingLevel:   row.GetStartingLevel(),
			StartingAbility: row.GetStartingAbilities(),
			StartingQuests:  row.GetStartingQuests(),
		}
		for _, stat := range row.GetStartingStats() {
			option.StartingStats = append(option.StartingStats, StatValue{
				Stat:  stat.GetStat(),
				Value: stat.GetValue(),
			})
		}
		for _, item := range row.GetStartingLoadout() {
			option.StartingLoadout = append(option.StartingLoadout, LoadoutItem{
				ItemID:   item.GetItemId(),
				Quantity: item.GetQuantity(),
				Slot:     item.GetSlot(),
			})
		}
		options = append(options, option)
	}
	sort.Slice(options, func(left, right int) bool { return options[left].ID < options[right].ID })
	return options, nil
}

func readZone(tables map[string]*table) (Zone, error) {
	loaded, ok := tables[tableZone]
	if !ok {
		return Zone{}, fmt.Errorf("%w: %q", ErrMissingTable, tableZone)
	}
	if loaded.rowCount != 1 {
		return Zone{}, fmt.Errorf(
			"%w: table %q holds %d rows, want exactly one zone",
			ErrMalformedTable, tableZone, loaded.rowCount,
		)
	}
	var row contentv1.Zone
	if err := proto.Unmarshal(loaded.row(0), &row); err != nil {
		return Zone{}, fmt.Errorf("%w: decode zone row: %w", ErrMalformedTable, err)
	}
	return Zone{
		ID:                 row.GetId(),
		Ruleset:            row.GetRuleset(),
		Slug:               row.GetSlug(),
		PlayerSpawn:        vec3(row.GetPlayerSpawn()),
		PlayerSpawnHeading: row.GetPlayerSpawnHeading(),
	}, nil
}

// resolveNPCs walks the placements, following spawn-table references, and keeps
// the mobs that are not switched off. It reproduces the resolution the shard
// used to do over the authored YAML directly.
func resolveNPCs(tables map[string]*table) ([]NPCSpawn, error) {
	placements, ok := tables[tablePlacements]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMissingTable, tablePlacements)
	}
	spawnTables, ok := tables[tableSpawnTables]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMissingTable, tableSpawnTables)
	}

	var result []NPCSpawn
	for _, encoded := range placements.rows() {
		var placement contentv1.Placement
		if err := proto.Unmarshal(encoded, &placement); err != nil {
			return nil, fmt.Errorf("%w: decode placement row: %w", ErrMalformedTable, err)
		}
		if inert(placement.GetSpawnTime()) {
			continue
		}
		mobs, err := resolveMobs(placement.GetObjectId(), spawnTables)
		if err != nil {
			return nil, err
		}
		for _, mobID := range mobs {
			result = append(result, NPCSpawn{
				PlacementID: placement.GetId(),
				MobID:       mobID,
				Position:    vec3(placement.GetPosition()),
				Heading:     placement.GetHeading(),
				RespawnMin:  time.Duration(placement.GetRespawnDelayMinMs()) * time.Millisecond,
				RespawnMax:  time.Duration(placement.GetRespawnDelayMaxMs()) * time.Millisecond,
			})
		}
	}

	sort.Slice(result, func(left, right int) bool {
		if result[left].PlacementID == result[right].PlacementID {
			return result[left].MobID < result[right].MobID
		}
		return result[left].PlacementID < result[right].PlacementID
	})
	return result, nil
}

func resolveMobs(objectID string, spawnTables *table) ([]string, error) {
	if strings.HasPrefix(objectID, mobIDPrefix) {
		return []string{objectID}, nil
	}
	for _, encoded := range spawnTables.candidates(objectID) {
		var spawnTable contentv1.SpawnTable
		if err := proto.Unmarshal(encoded, &spawnTable); err != nil {
			return nil, fmt.Errorf("%w: decode spawn table row: %w", ErrMalformedTable, err)
		}
		if spawnTable.GetId() != objectID {
			// A key-hash collision. Legal, so try the next candidate.
			continue
		}
		var mobs []string
		for _, entry := range spawnTable.GetEntries() {
			if strings.HasPrefix(entry.GetObjectId(), mobIDPrefix) && !inert(entry.GetSpawnTime()) {
				mobs = append(mobs, entry.GetObjectId())
			}
		}
		return mobs, nil
	}
	// A placement can point at a table this pack does not carry only if the
	// compiler's cross-reference check was bypassed; it spawns nothing.
	return nil, nil
}

func inert(spawnTime string) bool {
	return strings.EqualFold(spawnTime, spawnTimeNever)
}

func vec3(value *contentv1.Vec3) Vec3 {
	return Vec3{X: value.GetX(), Y: value.GetY(), Z: value.GetZ()}
}

// tablesDirectory is the one place inside a pack where table files live. It is
// where an unlisted file is looked for, and the only prefix a manifest entry may
// name.
const tablesDirectory = "tables"

// tablePath resolves one manifest `file` entry against the pack directory and
// refuses anything that escapes it.
//
// Without this, a manifest naming `../../../../etc/shadow` makes the shard read
// that file. The digest check would then reject the pack, but not before the
// error messages had reported the file's exact length and BLAKE3 — a size and
// hash oracle for any path, offered to whoever can write a manifest.json.
func tablePath(directory string, file string) (string, error) {
	clean := path.Clean(file)
	if file == "" ||
		clean != file ||
		path.IsAbs(file) ||
		filepath.IsAbs(file) ||
		strings.Contains(file, `\`) ||
		clean == ".." ||
		strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf(
			"%w: manifest entry names file %q, which is not a plain path inside the pack",
			ErrMalformedTable, file,
		)
	}
	return filepath.Join(directory, filepath.FromSlash(clean)), nil
}

// rejectUnlistedTables refuses a pack directory holding a table the manifest
// does not name.
//
// Two directories that differ only by an unlisted file would otherwise share a
// pack_id, and a stale table left by a partial rebuild would be invisible. The
// Rust verifier bails on exactly this case, and `sarnaut-pack verify` is only
// worth running if it accepts the same set of packs the shard does.
func rejectUnlistedTables(directory string, listed map[string]struct{}) error {
	entries, err := os.ReadDir(filepath.Join(directory, tablesDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list pack tables: %w", err)
	}
	for _, entry := range entries {
		relative := tablesDirectory + "/" + entry.Name()
		if _, ok := listed[relative]; !ok {
			return fmt.Errorf(
				"%w: pack %q holds %s, which the manifest does not list",
				ErrMalformedTable, directory, relative,
			)
		}
	}
	return nil
}

// namedTable pairs a table's manifest name with its bytes for digesting.
type namedTable struct {
	Name  string
	Bytes []byte
}
