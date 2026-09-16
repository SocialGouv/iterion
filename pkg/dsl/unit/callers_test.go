package unit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const parserPackagePath = "github.com/SocialGouv/iterion/pkg/dsl/parser"

// TestEveryParseCallerChoosesFileOrUnit: parser.Parse reads ONE file. A
// surface that compiles a bot must read its unit — the file and the
// fragments its imports reach — or it compiles a program with pieces
// missing: C030 at best, C001/C003 for whatever the main happened not to
// reference. Every production call site of parser.Parse is therefore
// listed here with the reason it reads a document rather than a unit; a
// new one fails this test until it chooses, and an entry whose file no
// longer parses a document is removed. The call is found through the Go
// syntax tree — the package by its import path, whatever local name or
// dot import it is given, the function wherever it is referenced — never
// by the spelling of a line.
func TestEveryParseCallerChoosesFileOrUnit(t *testing.T) {
	documentSurfaces := map[string]string{
		"pkg/botimport/validate.go":  "an imported draft is one generated file",
		"pkg/cli/fmt.go":             "the formatter rewrites one file on its own text — a unit is formatted file by file, each proven against itself, as the studio's per-file save is",
		"pkg/dsl/migrate/migrate.go": "the migrator rewrites one file in place; a unit migrates file by file",
		"pkg/dsl/unit/unit.go":       "the unit loader itself parses each file of the unit",
		"pkg/dsl/unparse/verify.go":  "Verify re-parses the one file it wrote",
		"pkg/runview/rewind_auto.go": "parses the recorded main of a run launched before units were recorded; a unit run is read through unit.LoadMap and LoadDir, and a unit run recorded main-only is refused",
		"pkg/server/cost_preview.go": "previews an uploaded document, the flattened unit (lot 3, remote launch)",
		"pkg/server/server_dsl.go":   "the studio's parse endpoint hands the EDITOR a document from text, and the example loader parses the one program it serves — a bot in several files it reads as its unit (on disk) or serves flat (embedded)",
		"pkg/server/server_files.go": "the save guard normalises the document being saved",
	}
	root := filepath.Join("..", "..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}
	parserDir := filepath.Join(root, "pkg", "dsl", "parser")

	seen := map[string]bool{}
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			// Hidden directories hold worktrees and scratch clones; the
			// parser package is the function's own.
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) || path == parserDir {
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
		calls, perr := referencesParse(path, src)
		if perr != nil {
			return perr
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

// referencesParse reports whether a Go source references the DSL parser's
// Parse — called, or taken as a value — under whatever name the file
// imports the package by.
func referencesParse(filename string, src []byte) (bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return false, err
	}
	local := ""
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != parserPackagePath {
			continue
		}
		local = "parser"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" {
		return false, nil
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == local && x.Sel.Name == "Parse" {
				found = true
			}
		case *ast.Ident:
			// A dot import puts Parse in the file's own scope.
			if local == "." && x.Name == "Parse" && x.Obj == nil {
				found = true
			}
		}
		return !found
	})
	return found, nil
}

// The detector finds the call however it is spelled — and only the DSL
// parser's, not a same-named function of another package.
func TestParseCallSitesAreFoundWhateverTheSpelling(t *testing.T) {
	for name, src := range map[string]string{
		"plain":   "package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { parser.Parse(\"a\", \"b\") }\n",
		"alias":   "package x\nimport p \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { p.Parse(\"a\", \"b\") }\n",
		"value":   "package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nvar parse = parser.Parse\nfunc f() { parse(\"a\", \"b\") }\n",
		"dot":     "package x\nimport . \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { Parse(\"a\", \"b\") }\n",
		"comment": "package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n// parser.Parse( is prose here\nfunc f() { p := parser.Parse; _ = p }\n",
	} {
		if got, err := referencesParse(name+".go", []byte(src)); err != nil || !got {
			t.Errorf("%s: found=%v err=%v", name, got, err)
		}
	}
	for name, src := range map[string]string{
		"other package":  "package x\nimport \"go/parser\"\nfunc f() { parser.ParseFile(nil, \"\", nil, 0) }\n",
		"other function": "package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { _ = parser.ReadPreamble(\"\") }\n",
		"prose only":     "package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n// parser.Parse( is prose\nvar _ = parser.MaxProfile\n",
		"blank import":   "package x\nimport _ \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc Parse() {}\nfunc f() { Parse() }\n",
	} {
		if got, err := referencesParse(name+".go", []byte(src)); err != nil || got {
			t.Errorf("%s: found=%v err=%v, want not found", name, got, err)
		}
	}
}
