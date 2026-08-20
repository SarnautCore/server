package combat_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/SarnautCore/server"

// forbidden names the sibling gameplay modules combat must not reach into.
//
// The M2 slice has combat hand off to loot and to quests, and the temptation
// on both sides is to reach across for the state the other one holds: combat
// asking the quest log whether this kill counts, loot asking combat for the
// threat table. Those are event handoffs (mechanics/combat.md rules 5.9.3 and
// 5.9.4), and an import is how they stop being.
//
// The packages do not exist yet, which is the point: the check is here before
// the first one lands rather than after the first shortcut.
var forbidden = []string{
	modulePath + "/internal/quests",
	modulePath + "/internal/inventory",
	modulePath + "/internal/loot",
}

// TestCombatReachesNoSiblingGameplayModule walks the transitive import graph of
// `internal/combat` over this repository's own packages.
//
// Transitive, not direct: routing the dependency through a third package would
// defeat a direct-import check, and would be the obvious way to do it by
// accident.
func TestCombatReachesNoSiblingGameplayModule(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	graph := internalImportGraph(t, root)
	start := modulePath + "/internal/combat"
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
		t.Errorf("internal/combat reaches %s (through %s); that handoff is an event, not an import",
			banned, from[banned])
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
