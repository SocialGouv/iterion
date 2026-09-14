package unit

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryParseCallerChoosesFileOrUnit: parser.Parse reads ONE file. A
// surface that compiles a bot must read its unit — the file and the
// fragments its imports reach — or it compiles a program with pieces
// missing: C030 at best, C001/C003 for whatever the main happened not to
// reference. Every production call site of parser.Parse outside the DSL
// packages is therefore listed here with the reason it reads a document
// rather than a unit; a new one fails this test until it chooses, and an
// entry whose file no longer parses a document is removed.
func TestEveryParseCallerChoosesFileOrUnit(t *testing.T) {
	documentSurfaces := map[string]string{
		"pkg/botimport/validate.go":        "an imported draft is one generated file",
		"pkg/botscaffold/shapes.go":        "a scaffold shape is one generated file",
		"pkg/runview/rewind_auto.go":       "parses the recorded main of a run launched before units were recorded; a unit run is read through unit.LoadMap and LoadDir, and a unit run recorded main-only is refused",
		"pkg/server/bot_sources_routes.go": "the cloud editor validates a document of the source's files map (lot 3, cloud editor)",
		"pkg/server/cost_preview.go":       "previews an uploaded document, the flattened unit (lot 3, remote launch)",
		"pkg/server/server_dsl.go":         "the studio's parse endpoint and the embedded example hand the EDITOR a document; the unit is compiled at launch and validate",
		"pkg/server/server_files.go":       "the save guard normalises the document being saved",
	}
	root := filepath.Join("..", "..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}
	dslDir := filepath.Join(root, "pkg", "dsl")

	seen := map[string]bool{}
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			// Hidden directories hold worktrees and scratch clones; the DSL
			// packages parse by trade.
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) || path == dslDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path) // #nosec G304 -- walking this repository's own tree
		if rerr != nil {
			return rerr
		}
		calls := false
		for _, line := range bytes.Split(src, []byte("\n")) {
			trimmed := bytes.TrimSpace(line)
			// A call quoted inside a comment is prose, not a call site.
			if bytes.HasPrefix(trimmed, []byte("//")) {
				continue
			}
			if bytes.Contains(line, []byte("parser.Parse(")) {
				calls = true
				break
			}
		}
		if !calls {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		if _, ok := documentSurfaces[rel]; !ok {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(offenders)
	for _, rel := range offenders {
		t.Errorf("%s calls parser.Parse: a compile reads the unit (unit.LoadDir / LoadMap / LoadDirWithMain); a surface that hands a DOCUMENT over is listed in this test with its reason", rel)
	}
	var stale []string
	for rel := range documentSurfaces {
		if !seen[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	for _, rel := range stale {
		t.Errorf("%s is listed as a document surface but no longer calls parser.Parse: remove the entry", rel)
	}
}
