package fswatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const fsnotifyPackagePath = "github.com/fsnotify/fsnotify"

// fsnotify's two watcher constructors. Both return the bare EMFILE /
// ENFILE / ENOSPC the kernel gave, with nothing that says which ceiling
// was hit; NewWatcher is the one this package wraps, NewBufferedWatcher
// the alternative a future site would otherwise reach for.
var watcherConstructors = map[string]bool{"NewWatcher": true, "NewBufferedWatcher": true}

// A walk that lost the tree finds no offender and reads exactly like a clean
// run, so what was walked is asserted, two ways. walkFloor catches a walk that
// collapsed; anchorDirs catches a skip rule that ate ONE parent, which a total
// cannot — pkg/ carries 3315 of the 3866 Go files this repo has today, so
// losing every other top-level directory still clears any tolerable floor.
// third_party/ is anchored like the rest: go.mod replaces codex-agent-sdk-go
// into this build, so its code is in the class — 67 of its 145 files are the
// vendored mirror; the tests and examples beside them are walked under the
// same over-inclusion rule as the e2e fixtures below.
//
// Nine top-level directories hold Go files; seven are anchored. The measure is
// what a silent loss would hide: examples/ (1 file) and ci/arc-runner/smoke/
// (2 files, its own module) hide nothing, and are named here rather than left
// to chance. contrib/ is anchored because `go list ./contrib/...` resolves it
// into the root module's build, its 15 files and all.
//
// What anchors do NOT catch: a skip that eats a directory BELOW the top level
// — skipping "dispatcher" hides three of the five real call sites while every
// anchor stays non-zero. The inventory is the net for that; the anchors and
// the floor only certify that the inventory was given the tree to read.
const walkFloor = 2000

var anchorDirs = []string{"pkg", "cmd", "internal", "bots", "e2e", "third_party", "contrib"}

// TestEveryFsnotifyWatcherConstructorGoesThroughFswatch: an inotify
// refusal on a loaded host is "too many open files" and nothing else —
// the same six words whether the process ran out of descriptors or the
// real UID ran out of inotify instances, a budget shared with every
// other container under that UID. fswatch.NewWatcher is the one place
// that attaches the observations telling those apart, so it is the one
// place allowed to call fsnotify's constructors; every other site calls
// fswatch. This test is the INVENTORY of that class, not its guarantee —
// the guarantee is proven by exercising a constructor under a really
// exhausted budget (resources_linux_test.go, and the index watcher's own
// pkg/dispatcher/native/watcher_linux_test.go). Its job is that a site
// added later cannot join the class unnoticed: the previous count was
// taken by hand, read four sites of five, and the missed one was already
// in the tree when that count was taken.
//
// The call is found through the Go syntax tree — the package by its
// import path, whatever local name it is given, the function called or
// taken as a value — never by the spelling of a line. What it does not
// resolve is types: a variable named fsnotify carrying a NewWatcher field
// reads as the package. That direction is a false positive, which fails
// loudly and is answered by renaming the variable; the direction that
// matters — a construction the walk does not see — is what the fixtures
// below are for.
func TestEveryFsnotifyWatcherConstructorGoesThroughFswatch(t *testing.T) {
	allowed := map[string]string{
		"internal/fswatch/watcher.go": "the wrapper itself: it calls fsnotify and turns a refused CONSTRUCTION into the evidence every other site then gets for free (a refused w.Add is still raw everywhere — #1554)",
	}
	root := filepath.Join("..", "..")
	skipNames := map[string]bool{"vendor": true, "node_modules": true}
	studioDir := filepath.Join(root, "studio")

	visited := 0
	perDir := map[string]int{}
	seen := map[string]bool{}
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			// Hidden directories hold worktrees and scratch clones; vendor/
			// and node_modules/ are other people's trees. testdata/ is NOT
			// skipped: a package under one still compiles when something
			// imports it. studio/ is the TypeScript front end, skipped by
			// PATH rather than by name so that a pkg/studio/ added later is
			// still walked, and docs/studio/ with it.
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) || path == studioDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		visited++
		perDir[strings.SplitN(rel, "/", 2)[0]]++
		src, readErr := os.ReadFile(path) // #nosec G304 -- walking this repository's own tree
		if readErr != nil {
			// One unreadable file costs one file of coverage, never the
			// whole inventory: aborting here would hide every offender
			// behind an unrelated fixture, and the cheapest way out of
			// that is a skip rule, which is the defect this test exists
			// to prevent.
			t.Errorf("%s could not be read, so it was NOT inventoried for fsnotify constructors: %v", rel, readErr)
			return nil
		}
		// A test that builds its own raw watcher is not exempt: the two
		// merge-queue ejections that opened #1198 were test failures, and
		// what made them diagnosable was the evidence in a test's log.
		// Nor is a nested module: go.mod replaces third_party/codex-agent-sdk-go
		// into this build and pkg/backend/delegate imports it, so its watchers
		// reach an iterion log like any other. The e2e fixture modules do not
		// ship, and are walked anyway — naming one file too many costs an
		// `allowed` entry; naming one too few is how the fifth site shipped.
		refs, perr := referencesFsnotifyConstructor(path, src)
		if perr != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried for fsnotify constructors: %v", rel, perr)
			return nil
		}
		if !refs {
			return nil
		}
		seen[rel] = true
		if _, ok := allowed[rel]; !ok {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The offender list is this test's product: it is reported FIRST, and every
	// check below it is an Errorf, never a Fatalf. A check that aborted would
	// delete the inventory's answer — the round that found this had a planted
	// offender go unnamed behind a zero anchor.
	sort.Strings(offenders)
	for _, rel := range offenders {
		t.Errorf("%s constructs an fsnotify watcher directly: call fswatch.NewWatcher instead, or its refusal reaches the log as a bare %q with no way to tell a descriptor ceiling from a shared per-UID inotify ceiling. A file that genuinely cannot reach this package — a nested module of its own — is listed in `allowed` above with that reason", rel, "too many open files")
	}
	var stale []string
	for rel := range allowed {
		if !seen[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	// Both messages below hand over the DISCRIMINATOR, not a remedy: each has
	// two causes, and the remedy for one of them silences this test over the
	// other.
	for _, rel := range stale {
		t.Errorf("%s is listed as an allowed fsnotify caller but the walk did not see it construct one: check whether the file still exists and still calls fsnotify. If it does, a skip rule ate it — fix the skip. Only if it no longer constructs a watcher does the entry come out", rel)
	}
	if visited < walkFloor {
		t.Errorf("the inventory visited %d Go files, fewer than the %d floor: the walk lost the tree, and what it did not read reports no offender", visited, walkFloor)
	}
	for _, dir := range anchorDirs {
		if perDir[dir] == 0 {
			t.Errorf("the inventory visited no Go file under %s/ (total %d): check whether %s/ still exists on disk. If it does, a skip rule ate it — fix the skip. Only if it is gone for good does it come out of anchorDirs", dir, visited, dir)
		}
	}
}

// referencesFsnotifyConstructor reports whether a Go source references one
// of fsnotify's watcher constructors — called, or taken as a value — under
// whatever name the file imports the package by.
func referencesFsnotifyConstructor(filename string, src []byte) (bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return false, err
	}
	// EVERY local name the file gives the package, not the last one: a file
	// may import one path twice and construct through either name.
	locals := map[string]bool{}
	for _, imp := range f.Imports {
		// Unquote, never Trim: an import path is a string literal, and both
		// a raw-string path and an escaped one compile. The parser already
		// validated the literal, so an error here is not reachable — it is
		// returned rather than ignored.
		path, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			return false, uerr
		}
		if path != fsnotifyPackagePath {
			continue
		}
		name := "fsnotify"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" {
			continue // imported for its side effects; nothing is named
		}
		locals[name] = true
	}
	if len(locals) == 0 {
		return false, nil
	}
	found := false
	var inspect func(ast.Node) bool
	inspect = func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && locals[id.Name] && watcherConstructors[x.Sel.Name] {
				found = true
				return false
			}
			// Walk only what is selected FROM. A selector's Sel is not a name
			// of the file's scope, so no dot import can have put it there —
			// visiting it would read every other.NewWatcher() in a file that
			// dot-imports fsnotify as fsnotify's own.
			ast.Inspect(x.X, inspect)
			return false
		case *ast.Ident:
			// A dot import puts the constructors in the file's own scope.
			if locals["."] && watcherConstructors[x.Name] && x.Obj == nil {
				found = true
			}
		}
		return !found
	}
	ast.Inspect(f, inspect)
	return found, nil
}

// The detector finds the reference however it is spelled — and only
// fsnotify's constructors, not a same-named function of another package.
func TestFsnotifyConstructorSitesAreFoundWhateverTheSpelling(t *testing.T) {
	for name, src := range map[string]string{
		"plain":         "package x\nimport \"github.com/fsnotify/fsnotify\"\nfunc f() { fsnotify.NewWatcher() }\n",
		"alias":         "package x\nimport fs \"github.com/fsnotify/fsnotify\"\nfunc f() { fs.NewWatcher() }\n",
		"value":         "package x\nimport \"github.com/fsnotify/fsnotify\"\nvar ctor = fsnotify.NewWatcher\nfunc f() { ctor() }\n",
		"dot":           "package x\nimport . \"github.com/fsnotify/fsnotify\"\nfunc f() { NewWatcher() }\n",
		"buffered":      "package x\nimport \"github.com/fsnotify/fsnotify\"\nfunc f() { fsnotify.NewBufferedWatcher(64) }\n",
		"comment":       "package x\nimport \"github.com/fsnotify/fsnotify\"\n// fsnotify.NewWatcher( is prose here\nfunc f() { c := fsnotify.NewWatcher; _ = c }\n",
		"double import": "package x\nimport (\n\t\"github.com/fsnotify/fsnotify\"\n\tfs2 \"github.com/fsnotify/fsnotify\"\n)\nvar _ = fs2.Chmod\nfunc f() { fsnotify.NewWatcher() }\n",
		"blank beside":  "package x\nimport (\n\t\"github.com/fsnotify/fsnotify\"\n\t_ \"github.com/fsnotify/fsnotify\"\n)\nfunc f() { fsnotify.NewWatcher() }\n",
		"raw string":    "package x\nimport `github.com/fsnotify/fsnotify`\nfunc f() { fsnotify.NewWatcher() }\n",
		"escaped":       "package x\nimport \"github.com/fsnotify/\\x66snotify\"\nfunc f() { fsnotify.NewWatcher() }\n",
	} {
		if got, err := referencesFsnotifyConstructor(name+".go", []byte(src)); err != nil || !got {
			t.Errorf("%s: found=%v err=%v", name, got, err)
		}
	}
	for name, src := range map[string]string{
		"other package":     "package x\nimport \"myfs/fsnotify\"\nfunc f() { fsnotify.NewWatcher() }\n",
		"other function":    "package x\nimport \"github.com/fsnotify/fsnotify\"\nfunc f() { var w *fsnotify.Watcher; _ = w.Close() }\n",
		"prose only":        "package x\nimport \"github.com/fsnotify/fsnotify\"\n// fsnotify.NewWatcher( is prose\nvar _ = fsnotify.ErrEventOverflow\n",
		"blank import":      "package x\nimport _ \"github.com/fsnotify/fsnotify\"\nfunc NewWatcher() {}\nfunc f() { NewWatcher() }\n",
		"local wrapper":     "package x\nfunc NewWatcher() {}\nfunc f() { NewWatcher() }\n",
		"dot, other's ctor": "package x\nimport (\n\t. \"github.com/fsnotify/fsnotify\"\n\t\"example.com/other\"\n)\nvar _ Op\nfunc f() { other.NewWatcher() }\n",
	} {
		if got, err := referencesFsnotifyConstructor(name+".go", []byte(src)); err != nil || got {
			t.Errorf("%s: found=%v err=%v, want not found", name, got, err)
		}
	}
}
