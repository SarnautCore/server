package pack

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"
)

const (
	manifestFileName = "manifest.json"
	// schemaVersion is the only pack format version this reader accepts.
	schemaVersion = 1
)

// manifest mirrors `manifest.json` as ADR 0029 specifies it.
type manifest struct {
	SchemaVersion int                  `json:"schema_version"`
	Ruleset       string               `json:"ruleset"`
	Zone          string               `json:"zone"`
	PackID        string               `json:"pack_id"`
	Builder       manifestBuilder      `json:"builder"`
	Source        manifestSource       `json:"source"`
	KeepExtra     bool                 `json:"keep_extra"`
	Tables        []manifestTableEntry `json:"tables"`
}

type manifestBuilder struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type manifestSource struct {
	Repo     string   `json:"repo"`
	Commit   string   `json:"commit"`
	Overlays []string `json:"overlays"`
}

type manifestTableEntry struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	RowType string `json:"row_type"`
	Rows    uint32 `json:"rows"`
	Bytes   uint64 `json:"bytes"`
	Blake3  string `json:"blake3"`
}

func readManifest(directory string) (manifest, error) {
	path := filepath.Join(directory, manifestFileName)
	payload, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read pack manifest %q: %w", path, err)
	}
	var document manifest
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifest{}, fmt.Errorf("parse pack manifest %q: %w", path, err)
	}
	if document.SchemaVersion != schemaVersion {
		return manifest{}, fmt.Errorf(
			"%w: pack %q declares schema_version %d, and this build reads version %d",
			ErrUnsupportedSchemaVersion, directory, document.SchemaVersion, schemaVersion,
		)
	}
	return document, nil
}

// tableDigest is the BLAKE3-256 of one table's bytes, lowercase hex.
func tableDigest(payload []byte) string {
	sum := blake3.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// computePackID hashes the table bytes in table-name order, length-prefixing
// both name and bytes so no pair of tables can be reassociated into the same
// stream. The manifest is deliberately not an input: `pack_id` answers "is this
// the same content", not "was this built by the same run".
//
// `named` must already be sorted by name, bytewise ascending.
func computePackID(named []namedTable) string {
	hasher := blake3.New()
	scratch := make([]byte, 8)
	for _, entry := range named {
		binary.LittleEndian.PutUint32(scratch[:4], uint32(len(entry.Name)))
		_, _ = hasher.Write(scratch[:4])
		_, _ = hasher.Write([]byte(entry.Name))
		binary.LittleEndian.PutUint64(scratch, uint64(len(entry.Bytes)))
		_, _ = hasher.Write(scratch)
		_, _ = hasher.Write(entry.Bytes)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// keyHash is the first 8 bytes of BLAKE3-256 over a canonical id, read
// little-endian: the key a `.sptbl` key index is sorted by.
func keyHash(canonicalID string) uint64 {
	sum := blake3.Sum256([]byte(canonicalID))
	return binary.LittleEndian.Uint64(sum[:8])
}
