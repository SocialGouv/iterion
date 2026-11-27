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
	"regexp"
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
//     directory — as is the literal tail of a Join on `filepath.Dir(file)`
//     where file comes from runtime.Caller in a test file (the test file's
//     own directory). Only a resolution naming an existing path counts: a
//     traversal fixture (`"../../etc/passwd"`) names nothing in the tree.
//   - root: a call to a root finder — a parameterless (or testing.TB-only)
//     function whose body looks for go.mod, asks git for --show-toplevel,
//     asks go for the module (`go env GOMOD`, `go list -m -f {{.Dir}}`),
//     reads runtime.Caller in a test file, or climbs relatively out of its
//     own directory — and any root signal in a test file itself, in a
//     finder called or not, whatever its parameters (a Test function that
//     anchors itself is never called by name). A root-anchored test can
//     read anything; the package needs a declaration, whose globs the scan
//     cannot check in full.
//   - anchored: a literal (or a Join tail of literals) that names an
//     existing path when joined to one of the package's anchors — the repo
//     root when it reaches one, and every directory a relative read names.
//
// A relative path holding a glob segment (`../../bots/*/x.yaml`, the pattern
// a test hands filepath.Glob) is kept as the pattern: it counts when it
// matches something in the tree, and it reads every tracked file it matches —
// and everything under a matching directory, the shape of a Join whose glob
// prefix is followed by a computed name. An anchored literal with a glob is
// not taken: outside a climb, a starred string is far more often a pattern
// under test (a permission rule, a URL shape) than a read, and a root-anchored
// package's entry already answers for what it reads.

type evidence struct {
	Pkg  string // package directory, repo-relative
	File string // repo-relative Go file
	Line int
	Kind string // relative | root | anchored
	Path string // repo-relative path read; "." for the root itself
	Dir  bool
	Glob bool // Path is a glob pattern; Dir: a match is a directory
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
			if !f.test {
				continue
			}
			if mayDefineFinder(f.src) {
				if err := f.parse(); err != nil {
					return nil, err
				}
				ev = append(ev, rootSignalReads(f)...)
			}
			if !mayRead(f.src, finderNames) {
				continue
			}
			if err := f.parse(); err != nil {
				return nil, err
			}
			ev = append(ev, relativeReads(root, f)...)
			ev = append(ev, finderCalls(module, f, finders)...)
		}
		for _, e := range ev {
			if e.Dir && !e.Glob {
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
		bytes.Contains(src, []byte("GOMOD")) || bytes.Contains(src, []byte("{{.Dir}}")) ||
		bytes.Contains(src, []byte(".Caller(")) || mayClimb(src)
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

// globMeta are the characters that make a path segment a glob pattern.
const globMeta = "*?["

// globBase is the part of a slash path before its first glob segment: the
// directory a pattern reads from ("." for a pattern at the root); the path
// itself when it holds no glob.
func globBase(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.ContainsAny(s, globMeta) {
			if i == 0 {
				return "."
			}
			return strings.Join(segs[:i], "/")
		}
	}
	return p
}

// existingOrMatched is existing for a plain path. For a glob pattern it
// reports whether the pattern matches anything in the tree, and isDir when
// one of the matches is a directory.
func existingOrMatched(root, p string) (exists, isDir, glob bool) {
	if !strings.ContainsAny(p, globMeta) {
		exists, isDir = existing(root, p)
		return exists, isDir, false
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil || len(matches) == 0 {
		return false, false, true
	}
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			return true, true, true
		}
	}
	return true, false, true
}

// relativeReads finds the literal paths of f that climb out of its
// directory and name an existing path of the repository.
func relativeReads(root string, f *scanFile) []evidence {
	var out []evidence
	consumed := map[ast.Expr]bool{}
	callerDirs := callerDirIdents(f)
	add := func(n ast.Node, rel string) {
		p, ok := resolveFrom(f.dir, rel)
		if !ok || within(globBase(p), f.dir) {
			return
		}
		exists, isDir, glob := existingOrMatched(root, p)
		if !exists {
			return
		}
		kind := "relative"
		if p == "." {
			kind = "root"
		}
		out = append(out, evidence{Pkg: f.dir, File: f.rel, Line: f.line(n), Kind: kind, Path: p, Dir: isDir, Glob: glob})
	}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if !isJoin(n) {
				return true
			}
			if depth := callerDirDepth(f, callerDirs, n.Args[0]); depth > 0 && len(n.Args) > 1 {
				// filepath.Join(filepath.Dir(file), "../../x"), file from
				// runtime.Caller: the test file's directory is the package's.
				tail, k := literalPrefix(n.Args[1:])
				for _, a := range n.Args[1 : 1+k] {
					consumed[a] = true
				}
				if k > 0 {
					add(n, path.Join(strings.Repeat("../", depth-1), tail))
				}
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
		found := len(rootSignals(f, fd.Body, false)) > 0
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

// gomodWord matches GOMOD as a word (`go env GOMOD`), not GOMODCACHE.
var gomodWord = regexp.MustCompile(`(^|[^A-Za-z0-9_])GOMOD([^A-Za-z0-9_]|$)`)

// rootSignals returns the sites under n that find the repository root
// without naming a path: a go.mod literal, git's --show-toplevel, go's own
// answer (`go env GOMOD`; `go list -m` with a {{.Dir}} template), and, in a
// test file, runtime.Caller (the test file's compile-time path).
//
// probesOnly keeps a go.mod literal only where it is probed — inside a
// stat, read, open or exists call (`os.Stat(filepath.Join(dir, "go.mod"))`)
// — and leaves out the go.mod a test writes, maps or `git add`s into a
// fixture repository. It serves the uncalled-definition read, where every
// Test function counts; a finder must be called to count, so rootFinders
// keeps every go.mod literal.
func rootSignals(f *scanFile, n ast.Node, probesOnly bool) []ast.Node {
	var sites []ast.Node
	var listM, dirTmpl ast.Node
	rt := importName(f, "runtime")
	var stack []ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.BasicLit:
			s, ok := stringLit(n)
			if !ok {
				return true
			}
			gomod := s == "go.mod" || strings.HasSuffix(s, "/go.mod")
			if gomod && (!probesOnly || probesPath(stack)) || strings.Contains(s, "--show-toplevel") || gomodWord.MatchString(s) {
				sites = append(sites, n)
			}
			if s == "-m" || strings.Contains(s, "list -m") {
				listM = n
			}
			if strings.Contains(s, "{{.Dir}}") {
				dirTmpl = n
			}
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && rt != "" && id.Name == rt && n.Sel.Name == "Caller" && f.test {
				sites = append(sites, n)
			}
		}
		return true
	})
	if listM != nil && dirTmpl != nil {
		sites = append(sites, dirTmpl)
	}
	return sites
}

// probesPath reports whether the innermost node of stack, a literal, is an
// argument (at any depth) of a call that probes a path: Stat, Lstat,
// ReadFile, Open, OpenFile, or a name holding "exist" (fileExists).
func probesPath(stack []ast.Node) bool {
	for _, n := range stack[:len(stack)-1] {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		switch name {
		case "Stat", "Lstat", "ReadFile", "Open", "OpenFile":
			return true
		}
		if strings.Contains(strings.ToLower(name), "exist") {
			return true
		}
	}
	return false
}

// rootSignalReads is the root read of a test file that finds the root
// itself: one per top-level declaration holding a root signal, called or
// not.
func rootSignalReads(f *scanFile) []evidence {
	var out []evidence
	for _, d := range f.ast.Decls {
		if sites := rootSignals(f, d, true); len(sites) > 0 {
			out = append(out, evidence{Pkg: f.dir, File: f.rel, Line: f.line(sites[0]), Kind: "root", Path: ".", Dir: true})
		}
	}
	return out
}

// importName is the name f refers to the package at importPath by, or ""
// when f does not import it.
func importName(f *scanFile, importPath string) string {
	for _, im := range f.ast.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == importPath {
			if im.Name != nil {
				return im.Name.Name
			}
			return path.Base(p)
		}
	}
	return ""
}

// callerDirIdents maps the identifiers of a test file bound to a directory
// derived from runtime.Caller to how many directories up the file they are:
// `_, file, _, _ := runtime.Caller(0)` binds file at 0, `dir :=
// filepath.Dir(file)` binds dir at 1.
func callerDirIdents(f *scanFile) map[string]int {
	out := map[string]int{}
	rt := importName(f, "runtime")
	if !f.test || rt == "" {
		return out
	}
	bind := func(lhs []ast.Expr, rhs []ast.Expr) {
		if len(rhs) != 1 {
			return
		}
		call, ok := rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Caller" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == rt && len(lhs) == 4 {
				if name, ok := lhs[1].(*ast.Ident); ok && name.Name != "_" {
					out[name.Name] = 0
				}
				return
			}
		}
		if len(lhs) == 1 {
			if d := callerDirDepth(f, out, rhs[0]); d > 0 {
				if name, ok := lhs[0].(*ast.Ident); ok {
					out[name.Name] = d
				}
			}
		}
	}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			bind(n.Lhs, n.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(n.Names))
			for i, id := range n.Names {
				lhs[i] = id
			}
			bind(lhs, n.Values)
		}
		return true
	})
	return out
}

// callerDirDepth reports how many directories up from a runtime.Caller file
// e is (1 for its directory, the package's), or 0 when e is no such
// expression: filepath.Dir or path.Dir over a caller identifier, nested.
func callerDirDepth(f *scanFile, idents map[string]int, e ast.Expr) int {
	switch e := e.(type) {
	case *ast.Ident:
		if d, ok := idents[e.Name]; ok && d > 0 {
			return d
		}
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Dir" || len(e.Args) != 1 {
			return 0
		}
		if id, ok := sel.X.(*ast.Ident); !ok || (id.Name != "filepath" && id.Name != "path") {
			return 0
		}
		if id, ok := e.Args[0].(*ast.Ident); ok {
			if d, ok := idents[id.Name]; ok {
				return d + 1
			}
			return 0
		}
		if d := callerDirDepth(f, idents, e.Args[0]); d > 0 {
			return d + 1
		}
	}
	return 0
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
