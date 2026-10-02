package fswatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The constructor inventory beside this test reads syntax trees, and its own
// comment says what that cannot see: a method call on a variable receiver.
// w.Add(path) on an *fsnotify.Watcher joins no class there — yet a raw Add
// is exactly the class #1554 routed through fswatch.Add on six sites, because
// inotify_add_watch is the call that reports a spent max_user_watches budget
// as a bare "no space left on device". Nothing pinned the seventh site: a
// selector heuristic false-positives on wg.Add / drain.Add / atomics and
// still misses s.w.Add. This test closes that side of the guard with type
// resolution: every selector the type checker resolves to fsnotify's Add OR
// AddWith method (the same inotify_add_watch, options aside) — an ident
// receiver, a field, an embedded promotion, however spelled — is an offender
// unless the allowlist below names its file.
//
// The type information is the toolchain's own: `go list -export` hands over
// the export data of every dependency (test variants included, so _test.go
// files are checked with the package), and go/types re-checks each package
// of this module against it. No new dependency: go/importer's gc reader is
// the standard library reading what the compiler just wrote. What this
// inherits from the toolchain is its blind spots, named rather than hidden:
//
//   - files the CURRENT platform's build tags exclude: a raw Add in a
//     _windows.go file is checked by NEITHER guard — the constructor
//     inventory's syntax walk reads every file but only knows
//     NewWatcher/NewBufferedWatcher, and this test only checks the
//     packages that build;
//   - nested modules ./... does not reach;
//   - indirection the type checker answers through a non-fsnotify type:
//     a watcher stored in an interface whose method set names Add, or
//     behind a generic bound, resolves on the interface, not on fsnotify;
//   - the allowlist is per FILE: a raw Add moved into watcher.go would
//     pass — the staleness check below keeps the list from growing
//     silently, not from being abused on purpose.
//
// Like the constructor inventory, this test is the INVENTORY of the class:
// its product is the offender list, reported first, and every check below
// it is an Errorf — a Fatalf would delete the answer.
func TestEveryRawWatcherAddGoesThroughFswatch(t *testing.T) {
	// An allowlist entry is a decision on the record: the file AND why it
	// calls the raw method. One that no longer fires is reported stale —
	// an unreported allowance is how a class grows unnoticed.
	allowed := map[string]string{
		"internal/fswatch/watcher.go":  "the wrapper itself: fswatch.Add IS w.Add plus the resource evidence",
		"internal/fswatch/add_test.go": "asserts an ordinary refusal passes through byte-identical, which takes the raw call to compare against",
	}
	root := filepath.Join("..", "..")
	// Generous rather than clever: on a cold cache this is one full build of
	// the module's export data, which `go test ./...` is paying anyway; the
	// suite's own -timeout is 1200s.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-test", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := out
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Fatalf("go list -export did not answer, so no package was inventoried for raw watcher Add calls: %v\n%s", err, tail)
	}

	type listedPackage struct {
		ImportPath string
		Dir        string
		Export     string
		GoFiles    []string
		CgoFiles   []string
		ImportMap  map[string]string
		Incomplete bool
	}
	var pkgs []listedPackage
	exports := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("go list answered %d bytes that did not decode, so the inventory stopped mid-tree: %v", len(out), err)
		}
		pkgs = append(pkgs, p)
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	vendorRoot := filepath.Join(absRoot, "vendor")
	impFset := token.NewFileSet()
	lookup := func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export) // #nosec G304 -- the build cache path go list just named
	}
	importerFor := func(importMap map[string]string) types.Importer {
		if len(importMap) == 0 {
			return importer.ForCompiler(impFset, "gc", lookup)
		}
		// A package whose imports were remapped — vendored paths, test
		// variants — resolves through its own map first.
		return importer.ForCompiler(impFset, "gc", func(path string) (io.ReadCloser, error) {
			if mapped, ok := importMap[path]; ok {
				path = mapped
			}
			return lookup(path)
		})
	}

	checked := 0
	seen := map[string]bool{}
	var offenders []string
	for _, p := range pkgs {
		// What is not this module's buildable tree is not checked: vendor/
		// is other people's code (fsnotify's own Add is there, and is not
		// ours to route), pkg.test entries are generated test mains, and an
		// Incomplete package does not compile — which the build says louder
		// than this test could.
		if p.Dir == "" || !strings.HasPrefix(p.Dir, absRoot+string(filepath.Separator)) || p.Incomplete {
			continue
		}
		if strings.HasPrefix(p.Dir, vendorRoot+string(filepath.Separator)) || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		files := append(append([]string{}, p.GoFiles...), p.CgoFiles...)
		if len(files) == 0 {
			continue
		}
		fset := token.NewFileSet()
		asts := make([]*ast.File, 0, len(files))
		parsed := true
		for _, name := range files {
			f, perr := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if perr != nil {
				t.Errorf("%s could not be parsed, so package %s was NOT inventoried for raw watcher Add calls: %v", name, p.ImportPath, perr)
				parsed = false
				break
			}
			asts = append(asts, f)
		}
		if !parsed {
			continue
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: importerFor(p.ImportMap)}
		if _, cerr := conf.Check(p.ImportPath, fset, asts, info); cerr != nil {
			t.Errorf("package %s did not type-check against the toolchain's own export data, so it was NOT inventoried for raw watcher Add calls: %v", p.ImportPath, cerr)
			continue
		}
		checked++
		for sel, selection := range info.Selections {
			fn, isFunc := selection.Obj().(*types.Func)
			if !isFunc || (fn.Name() != "Add" && fn.Name() != "AddWith") || fn.Pkg() == nil || fn.Pkg().Path() != fsnotifyPackagePath {
				continue
			}
			pos := fset.Position(sel.Pos())
			rel, rerr := filepath.Rel(absRoot, pos.Filename)
			if rerr != nil {
				rel = pos.Filename
			}
			where := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pos.Line)
			site := where + " " + fn.Name()
			if !seen[site] {
				seen[site] = true
				if _, ok := allowed[filepath.ToSlash(rel)]; !ok {
					offenders = append(offenders, site)
				}
			}
		}
	}
	sort.Strings(offenders)
	for _, site := range offenders {
		where, method, _ := strings.Cut(site, " ")
		t.Errorf("(*fsnotify.Watcher).%s referenced directly at %s — called, or taken as a value: call fswatch.Add instead, or a refused watch reaches the log as a bare %q with no way to tell a spent max_user_watches budget from a full disk (#1554). A file that genuinely cannot reach this package is listed in `allowed` above with that reason", method, where, "no space left on device")
	}
	var stale []string
	for file := range allowed {
		fired := false
		for site := range seen {
			if where, _, _ := strings.Cut(site, ":"); where == file {
				fired = true
				break
			}
		}
		if !fired {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	for _, file := range stale {
		t.Errorf("%s is listed as an allowed raw watcher Add caller but no such call was found: check whether the file still exists and still calls it. Only if it no longer does does the entry come out", file)
	}
	// A walk that lost the tree finds no offender and reads exactly like a
	// clean run. The constructor inventory anchors its file walk; this one
	// anchors the package count the same way — 447 module packages (test
	// variants included) were checked when this test was written, so a floor
	// far under that only fires when the listing collapsed.
	if checked < 300 {
		t.Errorf("only %d module packages were type-checked, fewer than the 300 floor: the listing lost the tree, and what was not checked reports no offender", checked)
	}
}
