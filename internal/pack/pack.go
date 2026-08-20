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
	for _, entry := range document.Tables {
		path := filepath.Join(directory, filepath.FromSlash(entry.File))
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

// namedTable pairs a table's manifest name with its bytes for digesting.
type namedTable struct {
	Name  string
	Bytes []byte
}
