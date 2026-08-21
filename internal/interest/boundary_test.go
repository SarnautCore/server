package interest_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterestImportsOnlyGameTypesInsideTheServer(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	const (
		internalPrefix = "github.com/SarnautCore/server/internal/"
		allowed        = internalPrefix + "gametypes"
	)
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if strings.HasPrefix(path, internalPrefix) && path != allowed {
				t.Errorf("%s imports %s; ADR 0033 permits interest to import only gametypes", file, path)
			}
		}
	}
}
