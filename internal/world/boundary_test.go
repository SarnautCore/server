package world_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxFileLines is the ceiling this package's decomposition was done to.
//
// The number is arbitrary; the property is not. `world.go` was one 378-line
// file holding the tick loop, the entity struct, the registry, the input entry
// point and the snapshot builder, and every one of those wanted to grow. A
// ceiling is what keeps the next thing that grows from growing there.
const maxFileLines = 400

func TestNoFileInThisPackageIsOversized(t *testing.T) {
	t.Parallel()

	for _, name := range goFiles(t) {
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if lines := strings.Count(string(contents), "\n"); lines > maxFileLines {
			t.Errorf("%s is %d lines, over the %d-line ceiling", name, lines, maxFileLines)
		}
	}
}

// TestThisPackageDoesNotImportGeneratedProtobuf is ADR 0028's observable test.
//
// The simulation owns domain types; the wire schema stops at the session
// layer. `golangci-lint`'s depguard enforces this too, and this test exists so
// that a checkout with no linter installed still cannot regress it, and so
// that the failure names the ADR rather than a linter rule id.
func TestThisPackageDoesNotImportGeneratedProtobuf(t *testing.T) {
	t.Parallel()

	const banned = "github.com/SarnautCore/server/gen/"
	fileSet := token.NewFileSet()
	for _, name := range goFiles(t) {
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if strings.HasPrefix(path, banned) {
				t.Errorf("%s imports %s; ADR 0028 keeps generated protobuf out of internal/world",
					name, path)
			}
		}
	}
}

func goFiles(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no Go files found; the boundary checks would pass vacuously")
	}
	return names
}
