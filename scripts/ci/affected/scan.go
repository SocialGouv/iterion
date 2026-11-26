package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The static read scan: every place a package's Go files name a repository
// path outside the package directory. It is heuristic by construction — it
// reads string literals, not program behaviour — and leans toward reporting:
// a reported read the declarations must cover costs a broader selection, a
// missed one costs a false green.
//
// Three shapes are recognised:
//
//   - relative: a literal path climbing out of the directory, `"../../docs"`
//     or `filepath.Join("..", "..", "docs")`, resolved against the package
//     directory. Only a resolution naming an existing path counts: a
//     traversal fixture (`"../../etc/passwd"`) names nothing in the tree.
//   - root: a call to a root finder — a parameterless (or testing.TB-only)
//     function whose body looks for go.mod, asks git for --show-toplevel,
//     reads runtime.Caller in a test file, or climbs relatively out of its
//     own directory. A root-anchored test can read anything; the package
//     needs a declaration, whose globs the scan cannot check in full.
//   - anchored: a literal (or a Join tail of literals) that names an
//     existing path when joined to one of the package's anchors — the repo
//     root when it reaches one, and every directory a relative read names.

type evidence struct {
	Pkg  string // package directory, repo-relative
	File string // repo-relative Go file
	Line int
	Kind string // relative | root | anchored
	Path string // repo-relative path read; "." for the root itself
	Dir  bool
}

func (e evidence) String() string {
	what := e.Path
	if e.Dir {
		what += "/"
	}
	return fmt.Sprintf("%s:%d: %s %s", e.File, e.Line, e.Kind, what)
}

// scanSkipDirs are directory names the scan never enters: they hold no
// package of the module.
func scanSkipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

type scanFile struct {
	rel  string // repo-relative
	dir  string // package dir
	test bool
	src  []byte
	ast  *ast.File
	fset *token.FileSet
}

// scanTree runs the read scan over every package directory under root.
func scanTree(root, module string) ([]evidence, error) {
	dirs, err := packageDirs(root)
	if err != nil {
		return nil, err
	}
	// Pass 1: root finders, from files that can define one.
	finders := map[string]map[string]bool{} // import path -> func names
	files := map[string][]*scanFile{}
	for _, dir := range dirs {
		fs, err := readGoFiles(root, dir)
		if err != nil {
			return nil, err
		}
		files[dir] = fs
		for _, f := range fs {
			if !mayDefineFinder(f.src) {
				continue
			}
			if err := f.parse(); err != nil {
				return nil, err
			}
			for _, name := range rootFinders(root, f) {
				ip := importPathOf(module, dir)
				if finders[ip] == nil {
					finders[ip] = map[string]bool{}
				}
				finders[ip][name] = true
			}
		}
	}
	finderNames := map[string]bool{}
	for _, names := range finders {
		for n := range names {
			finderNames[n] = true
		}
	}

	// Pass 2: reads, per package.
	var out []evidence
	for _, dir := range dirs {
		var ev []evidence
		anchors := map[string]bool{}
		for _, f := range files[dir] {
			// Reads count in test files only: a relative path in production
			// code resolves against whichever test's directory runs it, which
			// the scan cannot know. Root finders count wherever they are.
			if !f.test || !mayRead(f.src, finderNames) {
				continue
			}
			if err := f.parse(); err != nil {
				return nil, err
			}
			ev = append(ev, relativeReads(root, f)...)
			ev = append(ev, finderCalls(module, f, finders)...)
		}
		for _, e := range ev {
			if e.Dir {
				anchors[e.Path] = true
			}
		}
		if len(anchors) > 0 {
			for _, f := range files[dir] {
				if !f.test {
					continue
				}
				if err := f.parse(); err != nil {
					return nil, err
				}
				ev = append(ev, anchoredReads(root, f, anchors)...)
			}
		}
		out = append(out, dedupe(ev)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pkg != out[j].Pkg {
			return out[i].Pkg < out[j].Pkg
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// dedupe keeps the first site per (kind, path).
func dedupe(ev []evidence) []evidence {
	seen := map[string]bool{}
	var out []evidence
	for _, e := range ev {
		k := e.Kind + "\x00" + e.Path
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

func importPathOf(module, dir string) string {
	if dir == "." {
		return module
	}
	return module + "/" + dir
}

// packageDirs lists the repo-relative directories holding .go files the
// module can build: no vendor, testdata, dot or underscore directory, and
// no nested module or checkout.
func packageDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "." {
			if scanSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			// A nested module or a nested checkout (a parked worktree, a
			// clone) is not this module's source.
			for _, marker := range []string{"go.mod", ".git"} {
				if _, err := os.Lstat(filepath.Join(p, marker)); err == nil {
					return filepath.SkipDir
				}
			}
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				dirs = append(dirs, rel)
				break
			}
		}
		return nil
	})
	return dirs, err
}

func readGoFiles(root, dir string) ([]*scanFile, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, err
	}
	var out []*scanFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		rel := path.Join(dir, e.Name())
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		out = append(out, &scanFile{rel: rel, dir: dir, test: strings.HasSuffix(e.Name(), "_test.go"), src: src})
	}
	return out, nil
}

func (f *scanFile) parse() error {
	if f.ast != nil {
		return nil
	}
	f.fset = token.NewFileSet()
	a, err := parser.ParseFile(f.fset, f.rel, f.src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	f.ast = a
	return nil
}

func (f *scanFile) line(n ast.Node) int { return f.fset.Position(n.Pos()).Line }

func mayDefineFinder(src []byte) bool {
	return bytes.Contains(src, []byte(`go.mod"`)) || bytes.Contains(src, []byte("show-toplevel")) ||
		bytes.Contains(src, []byte("runtime.Caller")) || mayClimb(src)
}

func mayClimb(src []byte) bool {
	return bytes.Contains(src, []byte(`".."`)) || bytes.Contains(src, []byte(`"../`))
}

func mayRead(src []byte, finderNames map[string]bool) bool {
	if mayClimb(src) {
		return true
	}
	for n := range finderNames {
		if bytes.Contains(src, []byte(n+"(")) {
			return true
		}
	}
	return false
}

// stringLit returns the value of a string literal expression.
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// isJoin reports a call to filepath.Join or path.Join.
func isJoin(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && (id.Name == "filepath" || id.Name == "path")
}

// literalPrefix joins the leading string-literal arguments of a Join call.
func literalPrefix(args []ast.Expr) (string, int) {
	var parts []string
	for _, a := range args {
		s, ok := stringLit(a)
		if !ok {
			break
		}
		parts = append(parts, s)
	}
	return path.Join(parts...), len(parts)
}

// climbs reports whether a slash path starts by leaving its directory.
func climbs(p string) bool { return p == ".." || strings.HasPrefix(p, "../") }

// resolveFrom resolves a relative path against a package directory and
// reports the repo-relative result, or false when it leaves the repository.
func resolveFrom(dir, rel string) (string, bool) {
	p := path.Clean(path.Join(dir, filepath.ToSlash(rel)))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	return dir == "." || p == dir || strings.HasPrefix(p, dir+"/")
}

// existing reports whether the repo-relative path exists, and is a directory.
func existing(root, p string) (exists, isDir bool) {
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil {
		return false, false
	}
	return true, st.IsDir()
}

// relativeReads finds the literal paths of f that climb out of its
// directory and name an existing path of the repository.
func relativeReads(root string, f *scanFile) []evidence {
	var out []evidence
	consumed := map[ast.Expr]bool{}
	add := func(n ast.Node, rel string) {
		p, ok := resolveFrom(f.dir, rel)
		if !ok || within(p, f.dir) {
			return
		}
		exists, isDir := existing(root, p)
		if !exists {
			return
		}
		kind := "relative"
		if p == "." {
			kind = "root"
		}
		out = append(out, evidence{Pkg: f.dir, File: f.rel, Line: f.line(n), Kind: kind, Path: p, Dir: isDir})
	}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if !isJoin(n) {
				return true
			}
			prefix, k := literalPrefix(n.Args)
			for _, a := range n.Args[:k] {
				consumed[a] = true
			}
			if k > 0 && climbs(prefix) {
				add(n, prefix)
			} else if k == 0 {
				// filepath.Join(x, "..", ...): the climb is relative to
				// x, not to the package; never a package read by itself.
				for _, a := range n.Args {
					if s, ok := stringLit(a); ok && (s == ".." || climbs(s)) {
						consumed[a] = true
					}
				}
			}
		case *ast.BasicLit:
			if consumed[n] {
				return true
			}
			// A bare ".." outside a Join is a traversal test, not a path.
			if s, ok := stringLit(n); ok && strings.HasPrefix(s, "../") && !strings.ContainsAny(s, " \t\n") {
				add(n, s)
			}
		}
		return true
	})
	return out
}

// rootFinders returns the names of the functions of f that find the
// repository root on their own: parameterless or testing.TB-only, and
// looking for go.mod, asking git for --show-toplevel, climbing out of the
// directory with relative literals, or (test files) reading runtime.Caller.
func rootFinders(root string, f *scanFile) []string {
	var names []string
	for _, d := range f.ast.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Recv != nil || !onlyTestingParams(fd.Type.Params) {
			continue
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			switch n := n.(type) {
			case *ast.BasicLit:
				if s, ok := stringLit(n); ok && (s == "go.mod" || strings.HasSuffix(s, "/go.mod") || s == "--show-toplevel") {
					found = true
				}
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && id.Name == "runtime" && n.Sel.Name == "Caller" && f.test {
					found = true
				}
			}
			return true
		})
		if !found {
			sub := &scanFile{rel: f.rel, dir: f.dir, test: f.test, fset: f.fset, ast: &ast.File{Name: f.ast.Name, Decls: []ast.Decl{fd}}}
			for _, e := range relativeReads(root, sub) {
				if e.Dir {
					found = true
					break
				}
			}
		}
		if found {
			names = append(names, fd.Name.Name)
		}
	}
	return names
}

func onlyTestingParams(fl *ast.FieldList) bool {
	if fl == nil {
		return true
	}
	for _, field := range fl.List {
		t := field.Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		sel, ok := t.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "testing" {
			return false
		}
	}
	return true
}

// finderCalls finds the calls of f to a root finder: a same-package one by
// name, another package's through its import.
func finderCalls(module string, f *scanFile, finders map[string]map[string]bool) []evidence {
	imports := map[string]string{}
	for _, im := range f.ast.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := path.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = p
	}
	own := importPathOf(module, f.dir)
	var out []evidence
	ast.Inspect(f.ast, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var ip, name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			ip, name = own, fn.Name
		case *ast.SelectorExpr:
			id, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ip, ok = imports[id.Name]; !ok {
				return true
			}
			name = fn.Sel.Name
		default:
			return true
		}
		if finders[ip][name] {
			out = append(out, evidence{Pkg: f.dir, File: f.rel, Line: f.line(call), Kind: "root", Path: ".", Dir: true})
		}
		return true
	})
	return out
}

// anchoredReads finds the literals of f naming an existing path outside its
// directory when joined to an anchor. A bare word is only taken from a Join
// tail; elsewhere a literal must hold an inner slash (`docs/x.md`, not a
// `bots/` prefix test).
func anchoredReads(root string, f *scanFile, anchors map[string]bool) []evidence {
	var out []evidence
	try := func(n ast.Node, rel string) {
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || rel == "." || climbs(rel) || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, " \t\n:*?[{$%") {
			return
		}
		for a := range anchors {
			p := path.Clean(path.Join(a, rel))
			if within(p, f.dir) || p == "." {
				continue
			}
			if exists, isDir := existing(root, p); exists {
				out = append(out, evidence{Pkg: f.dir, File: f.rel, Line: f.line(n), Kind: "anchored", Path: p, Dir: isDir})
			}
		}
	}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.CallExpr:
			if isJoin(n) && len(n.Args) > 1 {
				if _, ok := stringLit(n.Args[0]); !ok {
					tail, k := literalPrefix(n.Args[1:])
					if k > 0 {
						try(n, tail)
					}
				}
			}
		case *ast.BasicLit:
			if s, ok := stringLit(n); ok && !strings.HasSuffix(s, "/") && strings.Contains(strings.TrimPrefix(s, "./"), "/") {
				try(n, s)
			}
		}
		return true
	})
	return out
}

// runScan prints the scan, undeclared reads first; exit 1 when any is.
func runScan(root string, in *inputs, w io.Writer) error {
	module, err := readModulePath(root)
	if err != nil {
		return err
	}
	ev, err := scanTree(root, module)
	if err != nil {
		return err
	}
	tracked, err := trackedFiles(context.Background(), root)
	if err != nil {
		return err
	}
	bad := undeclared(in, tracked, ev)
	for _, u := range bad {
		fmt.Fprintf(w, "UNDECLARED %s\n", u)
	}
	for _, e := range ev {
		fmt.Fprintf(w, "%s\n", e)
	}
	if len(bad) > 0 {
		return exitCode(1)
	}
	return nil
}
