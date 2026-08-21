package quests_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/SarnautCore/server"

// forbidden names the sibling gameplay modules the quest module must not reach
// into, and the generated wire package it must not know exists.
//
// mechanics/quests.md rule 5.4.1 says this module consumes combat's MobKilled
// event and does not observe combat state. Inventory is an approved dependency
// in ADR 0033 for plain reward and bag projections; combat remains forbidden.
//
// `internal/loot` is here for a weaker reason and it is still worth stating: a
// quest counter and a corpse container have nothing to say to each other, and
// the day one of them starts is the day the M2 slice stops being three
// separable modules. `gen` is ADR 0028's rule — the wire schema stops at the
// session layer.
var forbidden = []string{
	modulePath + "/internal/combat",
	modulePath + "/internal/loot",
	modulePath + "/internal/session",
	modulePath + "/gen/sarnaut/v1",
}

// TestQuestsReachesNoSiblingGameplayModule walks the transitive import graph of
// `internal/quests` over this repository's own packages.
//
// Transitive, not direct: routing the dependency through a third package would
// defeat a direct-import check and would be the obvious way to do it by
// accident. `golangci-lint`'s depguard enforces the direct case too, and this
// test exists so that a checkout with no linter installed still cannot regress
// it, and so the failure names the rule rather than a linter id.
func TestQuestsReachesNoSiblingGameplayModule(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	graph := internalImportGraph(t, root)
	start := modulePath + "/internal/quests"
	if _, ok := graph[start]; !ok {
		t.Fatalf("%s is not in the import graph; the walk found nothing", start)
	}

	reachable := map[string]bool{start: true}
	queue := []string{start}
	from := map[string]string{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range graph[current] {
			if reachable[next] {
				continue
			}
			reachable[next] = true
			from[next] = current
			queue = append(queue, next)
		}
	}

	for _, banned := range forbidden {
		if !reachable[banned] {
			continue
		}
		t.Errorf(
			"internal/quests reaches %s (through %s); that handoff is a declared interface, not an import",
			banned, from[banned],
		)
	}
}

// internalImportGraph maps each package in this module to the packages of this
// module it imports. Test files are included: a test that reaches across the
// boundary is the same coupling with a different file suffix.
func internalImportGraph(t *testing.T, root string) map[string][]string {
	t.Helper()

	graph := make(map[string][]string)
	fileSet := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "testdata" || name == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		directory, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		self := modulePath + "/" + filepath.ToSlash(directory)
		if _, ok := graph[self]; !ok {
			graph[self] = nil
		}

		parsed, parseErr := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imported := range parsed.Imports {
			target := strings.Trim(imported.Path.Value, `"`)
			if strings.HasPrefix(target, modulePath+"/") {
				graph[self] = append(graph[self], target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return graph
}
