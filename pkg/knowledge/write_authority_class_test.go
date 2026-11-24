package knowledge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// mutatingStoreMethods are the MemoryStore methods that change bytes. Every
// adapter must put the shared authority gate first in each of them; a reader
// is deliberately absent (the global space is readable by every org, which is
// what it is FOR).
var mutatingStoreMethods = []string{"WriteDocument", "DeleteDocument"}

// implAssertion matches the compile-time assertion every adapter uses to
// declare itself a MemoryStore: `var _ knowledge.MemoryStore = (*X)(nil)`.
// Discovering adapters through it is what makes this guard survive a THIRD
// one: a new adapter that forgets the gate is found by the same walk, not by
// a list this file would have to remember to grow.
var implAssertion = regexp.MustCompile(`(?:knowledge\.)?MemoryStore\s*=\s*\(\*(\w+)\)\(nil\)`)

// TestEveryMemoryStoreAdapterGatesItsMutations is the class guard for #2400.
//
// The defect was ONE surface holding the gate: `requireMemoryWriteAuth` on
// REST, while the engine's install site wrote the instance-wide space with no
// check at all. A per-surface guard drifts at the next surface — this repo has
// paid four review rounds for exactly that shape — so the authority is asked
// once, by knowledge.AuthorizeSpaceWrite, at the chokepoint every writer
// passes: the store. This test refuses an adapter that does not ask.
//
// Known blind spot, stated rather than hidden: it reads the SOURCE, so an
// adapter that calls the gate through an indirection this walk cannot see
// would pass. The behavioural witnesses (pkg/memory's FS arms, the Mongo
// aggregate arms, the engine install arms) are what prove the refusal
// actually happens; this one prevents a silent omission.
func TestEveryMemoryStoreAdapterGatesItsMutations(t *testing.T) {
	root := repoRoot(t)
	impls := map[string][]string{} // file -> receiver type names
	var files []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata", ".works", ".iterion", ".claude", "graphify-out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range implAssertion.FindAllStringSubmatch(string(data), -1) {
			impls[path] = append(impls[path], m[1])
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(impls) < 2 {
		t.Fatalf("premise lost: found %d MemoryStore adapter(s) (%v), expected at least the FS and cloud ones — the discovery regexp no longer matches the tree's idiom, so this guard proves nothing", len(impls), impls)
	}

	sort.Strings(files)
	for _, file := range files {
		for _, recv := range impls[file] {
			checkAdapterGates(t, filepath.Dir(file), recv)
		}
	}
}

// checkAdapterGates parses every non-test file of the adapter's directory (a
// method may live beside its declaration) and requires each mutating method
// of the receiver to call the shared gate. Files are parsed one by one rather
// than through the deprecated parser.ParseDir: build tags do not matter here
// (a store adapter is not tag-gated) and a per-file walk needs no extra
// dependency.
func checkAdapterGates(t *testing.T, dir, recv string) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || receiverType(fn.Recv) != recv {
				continue
			}
			if !slicesContains(mutatingStoreMethods, fn.Name.Name) {
				continue
			}
			found[fn.Name.Name] = true
			if !callsGate(fn.Body) {
				t.Errorf("%s.%s (%s) mutates a memory space without asking AuthorizeSpaceWrite — the instance-wide `global` space would be writable from any run again",
					recv, fn.Name.Name, fset.Position(fn.Pos()))
			}
		}
	}
	for _, m := range mutatingStoreMethods {
		if !found[m] {
			t.Errorf("adapter %s in %s declares itself a MemoryStore but no %s method was parsed — the guard cannot judge what it cannot see", recv, dir, m)
		}
	}
}

// callsGate reports whether the body calls AuthorizeSpaceWrite (qualified or
// not — the FS and cloud adapters import knowledge, a future in-package one
// would not).
func callsGate(body *ast.BlockStmt) bool {
	seen := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == "AuthorizeSpaceWrite" {
				seen = true
			}
		case *ast.Ident:
			if fn.Name == "AuthorizeSpaceWrite" {
				seen = true
			}
		}
		return !seen
	})
	return seen
}

func receiverType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	switch e := recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return e.Name
	}
	return ""
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// repoRoot walks up from this package to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("no go.mod above %s", dir)
	return ""
}
