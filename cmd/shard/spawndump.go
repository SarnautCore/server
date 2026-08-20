package main

import (
	"encoding/json"

	"github.com/SarnautCore/server/internal/pack"
)

// spawnDump is the resolved NPC spawn set of one pack, in a form that can be
// compared byte for byte across builds. It is the check that a change to the
// compiler or the reader did not silently move the world.
type spawnDump struct {
	Ruleset        string         `json:"ruleset"`
	ZoneSlug       string         `json:"zone_slug"`
	PlacementCount int            `json:"placement_count"`
	NPCCount       int            `json:"npc_count"`
	Spawns         []spawnDumpRow `json:"spawns"`
}

type spawnDumpRow struct {
	PlacementID string       `json:"placement_id"`
	MobID       string       `json:"mob_id"`
	Position    spawnDumpVec `json:"position"`
	Heading     float32      `json:"heading"`
}

type spawnDumpVec struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}

// renderSpawnDump encodes what `loaded` would spawn. The row order is the order
// the shard registers entities in, so the dump and the running zone agree.
func renderSpawnDump(loaded *pack.Pack) ([]byte, error) {
	spawns := loaded.NPCSpawns()
	placements := make(map[string]struct{}, len(spawns))
	rows := make([]spawnDumpRow, 0, len(spawns))
	for _, spawn := range spawns {
		placements[spawn.PlacementID] = struct{}{}
		rows = append(rows, spawnDumpRow{
			PlacementID: spawn.PlacementID,
			MobID:       spawn.MobID,
			Position: spawnDumpVec{
				X: spawn.Position.X,
				Y: spawn.Position.Y,
				Z: spawn.Position.Z,
			},
			Heading: spawn.Heading,
		})
	}

	payload, err := json.MarshalIndent(spawnDump{
		Ruleset:        loaded.Zone().Ruleset,
		ZoneSlug:       loaded.Zone().Slug,
		PlacementCount: len(placements),
		NPCCount:       len(rows),
		Spawns:         rows,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}
