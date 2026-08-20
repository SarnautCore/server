package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/SarnautCore/server/internal/pack"
)

// privateTestdataVariable points at the `testdata/` directory of the private
// `data` repository. That repository holds fixtures derived from MY.GAMES-owned
// source material and can never be vendored here, so this test is opt-in: it
// skips with a logged reason when the variable is unset, and public CI never
// needs it (ADR 0004, ADR 0011).
const privateTestdataVariable = "SARNAUT_PRIVATE_TESTDATA"

// contentPackVariable is the same variable the shard reads at startup.
const contentPackVariable = "SARNAUT_CONTENT_PACK"

const goldenSpawnDumpFile = "golden-spawn-instleague1.json"

func TestSpawnDumpMatchesThePrivateGoldenDump(t *testing.T) {
	testdata := os.Getenv(privateTestdataVariable)
	if testdata == "" {
		t.Skipf(
			"skipping the golden spawn comparison: %s is unset, so the private data "+
				"repository's testdata directory cannot be located",
			privateTestdataVariable,
		)
	}
	packPath := os.Getenv(contentPackVariable)
	if packPath == "" {
		t.Skipf(
			"skipping the golden spawn comparison: %s is set but %s is not, so there "+
				"is no real pack to compare against",
			privateTestdataVariable, contentPackVariable,
		)
	}

	golden := filepath.Join(testdata, goldenSpawnDumpFile)
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden spawn dump %s: %v", golden, err)
	}

	loaded, err := pack.Load(packPath, pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load(%s) error = %v", packPath, err)
	}
	got, err := renderSpawnDump(loaded)
	if err != nil {
		t.Fatalf("renderSpawnDump() error = %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf(
			"the pack resolves a different spawn set than the pre-pack shard did\n"+
				"golden: %s (%d bytes, from %s)\npack:   %s (%d bytes)",
			golden, len(want), privateTestdataVariable, packPath, len(got),
		)
		reportFirstDifference(t, got, want)
	}
}

// A whole-file diff of a few hundred spawns is unreadable, so point at the
// first line that differs.
func reportFirstDifference(t *testing.T, got, want []byte) {
	t.Helper()
	gotLines := bytes.Split(got, []byte("\n"))
	wantLines := bytes.Split(want, []byte("\n"))
	for index := 0; index < len(gotLines) && index < len(wantLines); index++ {
		if !bytes.Equal(gotLines[index], wantLines[index]) {
			t.Errorf("first difference at line %d:\n  got  %s\n  want %s",
				index+1, gotLines[index], wantLines[index])
			return
		}
	}
	t.Errorf("dumps agree for %d lines but differ in length: got %d lines, want %d",
		min(len(gotLines), len(wantLines)), len(gotLines), len(wantLines))
}

func TestSpawnDumpOfTheFixturePackIsStable(t *testing.T) {
	t.Parallel()

	loaded, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	first, err := renderSpawnDump(loaded)
	if err != nil {
		t.Fatalf("renderSpawnDump() error = %v", err)
	}
	second, err := renderSpawnDump(loaded)
	if err != nil {
		t.Fatalf("renderSpawnDump() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("renderSpawnDump() is not deterministic")
	}
	if !bytes.HasSuffix(first, []byte("}\n")) {
		t.Error("the spawn dump has no trailing newline")
	}
	if bytes.Contains(first, []byte("\r")) {
		t.Error("the spawn dump uses CRLF line endings")
	}
}
