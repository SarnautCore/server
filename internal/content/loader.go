// Package content loads shard world data from the private runtime data repository.
package content

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Vec3 is a position from a placement file.
type Vec3 struct {
	X float32 `yaml:"x"`
	Y float32 `yaml:"y"`
	Z float32 `yaml:"z"`
}

// NPCSpawn is one mob resolved from a placement or spawn table.
type NPCSpawn struct {
	PlacementID string
	MobID       string
	Position    Vec3
	Heading     float32
}

type objectRef struct {
	ID string `yaml:"id"`
}

type tableFile struct {
	ID      string       `yaml:"id"`
	Entries []tableEntry `yaml:"entries"`
}

type tableEntry struct {
	Object    objectRef `yaml:"object"`
	SpawnTime string    `yaml:"spawn_time"`
}

type placementFile struct {
	Placements []placement `yaml:"placements"`
}

type placement struct {
	ID          string    `yaml:"id"`
	Object      objectRef `yaml:"object"`
	Position    Vec3      `yaml:"position"`
	Orientation struct {
		Yaw float32 `yaml:"yaw"`
	} `yaml:"orientation"`
	SpawnTime string `yaml:"spawn_time"`
}

// LoadZoneNPCs resolves active mob placements under a ruleset and zone slug.
func LoadZoneNPCs(root, ruleset, zone string) ([]NPCSpawn, error) {
	zoneDir, err := childPath(root, ruleset, "zones", zone)
	if err != nil {
		return nil, err
	}
	spawnDir := filepath.Join(zoneDir, "spawns")

	tables, err := loadTables(filepath.Join(spawnDir, "tables"))
	if err != nil {
		return nil, err
	}
	placements, err := readYAMLFiles[placementFile](filepath.Join(spawnDir, "placements"))
	if err != nil {
		return nil, err
	}

	var result []NPCSpawn
	for _, document := range placements {
		for _, placed := range document.Placements {
			if inactive(placed.SpawnTime) {
				continue
			}
			mobIDs := resolveMobs(placed.Object.ID, tables)
			for _, mobID := range mobIDs {
				result = append(result, NPCSpawn{
					PlacementID: placed.ID,
					MobID:       mobID,
					Position:    placed.Position,
					Heading:     placed.Orientation.Yaw,
				})
			}
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

func childPath(root string, elements ...string) (string, error) {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve content root %q: %w", root, err)
	}
	joined := filepath.Join(append([]string{cleanRoot}, elements...)...)
	relative, err := filepath.Rel(cleanRoot, joined)
	if err != nil {
		return "", fmt.Errorf("resolve content path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content path escapes root %q", root)
	}
	return joined, nil
}

func loadTables(directory string) (map[string][]string, error) {
	documents, err := readYAMLFiles[tableFile](directory)
	if err != nil {
		return nil, err
	}
	tables := make(map[string][]string, len(documents))
	for _, table := range documents {
		if table.ID == "" {
			return nil, fmt.Errorf("spawn table in %q has no id", directory)
		}
		for _, entry := range table.Entries {
			if isMob(entry.Object.ID) && !inactive(entry.SpawnTime) {
				tables[table.ID] = append(tables[table.ID], entry.Object.ID)
			}
		}
	}
	return tables, nil
}

func readYAMLFiles[T any](directory string) ([]T, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read content directory %q: %w", directory, err)
	}
	var result []T
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".yaml") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read content file %q: %w", path, err)
		}
		var document T
		if err := yaml.Unmarshal(payload, &document); err != nil {
			return nil, fmt.Errorf("parse content file %q: %w", path, err)
		}
		result = append(result, document)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("content directory %q contains no YAML files", directory)
	}
	return result, nil
}

func resolveMobs(objectID string, tables map[string][]string) []string {
	if isMob(objectID) {
		return []string{objectID}
	}
	return tables[objectID]
}

func isMob(id string) bool {
	return strings.HasPrefix(id, "mob.")
}

func inactive(spawnTime string) bool {
	return strings.EqualFold(spawnTime, "time-never")
}
