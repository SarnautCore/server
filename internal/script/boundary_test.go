package script_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/SarnautCore/server"

// allowed is the whole of what internal/script may reach inside this module.
//
// ADR 0036: "internal/script imports only internal/gametypes and internal/pack,
// plus the Go standard library." This is stated as an allow-list rather than a
// deny-list because the interesting failure is not "script imported combat" but
// "script imported something nobody thought to ban". At the time of writing the
// evaluator imports neither entry: internal/gametypes does not exist yet, and
// the script rows it will read from internal/pack land in M3-09.
//
// The LifeGuard pre-commitment of ADR 0035/0036 is what this test is really
// protecting. If honouring LifeGuard ever seems to require internal/combat here,
// the seam is wrong: the damage clamp moves behind the existing combat hook,
// with a host adapter above both packages. A depguard exemption is never the
// remedy, and neither is an entry in this slice.
var allowed = map[string]bool{
	modulePath + "/internal/gametypes": true,
	modulePath + "/internal/pack":      true,
}

// TestScriptReachesOnlyItsTwoLeafDependencies walks the transitive import graph
// of internal/script over this repository's own packages.
//
// Transitive, not direct, for the same reason internal/quests checks it that
// way: routing a dependency through a third package defeats a direct-import
// check and is the obvious way to do it by accident.
//
// This test does the work alone today. ADR 0033 §2's allow-list has no
// internal/script row — server/.golangci.yml carries three depguard rules and
// none of them names this package — because M3-05 owns that amendment under an
// exclusive server lock. Until it lands, the linter cannot catch a regression
// here and this test is the only thing that can.
func TestScriptReachesOnlyItsTwoLeafDependencies(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	graph := internalImportGraph(t, root)
	start := modulePath + "/internal/script"
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

	for target := range reachable {
		if target == start || allowed[target] {
			continue
		}
		t.Errorf(
			"internal/script reaches %s (through %s); ADR 0036 allows only internal/gametypes and internal/pack",
			target, from[target],
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
