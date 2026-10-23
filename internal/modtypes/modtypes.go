// Package modtypes type-checks this module's packages as the compiler sees
// them, for the guard tests that hold a rule over the whole tree: which code
// references a door, which function holds a value of a type, which exported
// API reaches a type. A guard that reads source text alone answers on
// spellings — an alias, an import name, a method value, a wrapper — where
// this answers on the objects the type checker resolved.
//
// The package list is the build system's own (`go list -e -export -deps
// ./...`): every package the module builds, wherever its directory is, and
// none it does not. A directory whose name begins with "." or "_", or named
// testdata, holds no package of ./... — a package there is read only when a
// package of the module imports it, which puts it in the dependencies go
// list answers (pkg/dsl/unit TestNoGuardIsReadAroundByTheBuild holds every
// Go file of the repository to a package read here).
// Dependencies are read from the export data the toolchain just wrote
// (go/importer's gc reader): no new dependency, and the same blind spots as
// the toolchain's — a file the current platform's build tags exclude is no
// file of its package here (IgnoredGoFiles names it).
//
// Only test code imports this package.
package modtypes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"sync"
	"time"
)

// Package is one package of the module, parsed with its comments and
// type-checked.
type Package struct {
	ImportPath string
	Name       string
	// Dir is the package's directory; Rel its slash path from the module
	// root ("." for the root).
	Dir, Rel string
	// Files are the package's Go files the build compiles on this platform
	// (go list's GoFiles and CgoFiles), FileRels their slash paths from the
	// module root, in the same order.
	Files    []*ast.File
	FileRels []string
	// IgnoredGoFiles are the package directory's Go files the build does
	// not compile on this platform (another GOOS, a build tag), by slash
	// path from the module root: no rule here reads them.
	IgnoredGoFiles []string
	Types          *types.Package
	Info           *types.Info
	// Imports and Deps are go list's: the direct imports and the whole
	// dependency closure, by import path.
	Imports, Deps []string
}

// Module is the module's packages.
type Module struct {
	// Root is the module root's absolute path; Path the module path.
	Root, Path string
	// Fset positions every file of Packages.
	Fset *token.FileSet
	// Packages are the module's own packages (vendored ones excluded), by
	// import path.
	Packages []*Package
	byPath   map[string]*Package
	// exports maps every package go list answered to its export data.
	exports map[string]string
}

// Package is the module package of import path p, nil when there is none.
func (m *Module) Package(p string) *Package { return m.byPath[p] }

// Rel is the slash path of pos's file from the module root.
func (m *Module) Rel(pos token.Pos) string {
	name := m.Fset.Position(pos).Filename
	if rel, err := filepath.Rel(m.Root, name); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(name)
}

var (
	loadMu sync.Mutex
	loaded = map[string]loadResult{}
)

type loadResult struct {
	m   *Module
	err error
}

// Load lists and type-checks the module rooted at root, once per process
// for a root: the guards of one test binary share it.
func Load(root string) (*Module, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	if r, ok := loaded[abs]; ok {
		return r.m, r.err
	}
	m, err := load(abs)
	loaded[abs] = loadResult{m, err}
	return m, err
}

type listedPackage struct {
	ImportPath     string
	Name           string
	Dir            string
	Export         string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	ImportMap      map[string]string
	Imports        []string
	Deps           []string
	Incomplete     bool
	Error          *struct{ Err string }
	Module         *struct {
		Path string
		Main bool
	}
}

func load(root string) (*Module, error) {
	// Generous rather than clever: on a cold cache this is one build of the
	// module's export data, which the test run pays for anyway.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("modtypes: go list -export did not answer, so no package was read: %v\n%s", err, tail(stderr.Bytes()))
	}
	m := &Module{Root: root, Fset: token.NewFileSet(), byPath: map[string]*Package{}, exports: map[string]string{}}
	var listed []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("modtypes: go list answered %d bytes that do not decode: %v", len(out), err)
		}
		if p.Export != "" {
			m.exports[p.ImportPath] = p.Export
		}
		listed = append(listed, p)
	}
	vendor := filepath.Join(root, "vendor") + string(filepath.Separator)
	importers := map[string]types.Importer{}
	for _, p := range listed {
		if p.Module == nil || !p.Module.Main || strings.HasPrefix(p.Dir+string(filepath.Separator), vendor) {
			continue
		}
		if m.Path == "" {
			m.Path = p.Module.Path
		}
		if p.Incomplete || p.Error != nil {
			msg := "incomplete"
			if p.Error != nil {
				msg = p.Error.Err
			}
			return nil, fmt.Errorf("modtypes: package %s does not build (%s), so no rule can read it", p.ImportPath, msg)
		}
		pkg, err := m.check(p, importers)
		if err != nil {
			return nil, err
		}
		m.Packages = append(m.Packages, pkg)
		m.byPath[pkg.ImportPath] = pkg
	}
	sort.Slice(m.Packages, func(i, j int) bool { return m.Packages[i].ImportPath < m.Packages[j].ImportPath })
	if len(m.Packages) == 0 {
		return nil, errors.New("modtypes: go list answered no package of the module")
	}
	return m, nil
}

// check parses and type-checks one listed package of the module.
func (m *Module) check(p listedPackage, importers map[string]types.Importer) (*Package, error) {
	rel, err := filepath.Rel(m.Root, p.Dir)
	if err != nil {
		return nil, err
	}
	pkg := &Package{ImportPath: p.ImportPath, Name: p.Name, Dir: p.Dir, Rel: filepath.ToSlash(rel), Imports: p.Imports, Deps: p.Deps}
	for _, name := range p.IgnoredGoFiles {
		pkg.IgnoredGoFiles = append(pkg.IgnoredGoFiles, path(pkg.Rel, name))
	}
	for _, name := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
		f, err := parser.ParseFile(m.Fset, filepath.Join(p.Dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("modtypes: %s does not parse, so package %s was not read: %w", path(pkg.Rel, name), p.ImportPath, err)
		}
		pkg.Files = append(pkg.Files, f)
		pkg.FileRels = append(pkg.FileRels, path(pkg.Rel, name))
	}
	if len(pkg.Files) == 0 {
		return pkg, nil
	}
	pkg.Info = &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
		Implicits:  map[ast.Node]types.Object{},
	}
	conf := types.Config{Importer: m.importerFor(p.ImportMap, importers)}
	pkg.Types, err = conf.Check(p.ImportPath, m.Fset, pkg.Files, pkg.Info)
	if err != nil {
		return nil, fmt.Errorf("modtypes: package %s does not type-check against the toolchain's export data, so it was not read: %w", p.ImportPath, err)
	}
	return pkg, nil
}

// CheckSource type-checks src as the one file of a package named by its
// package clause, at import path importPath, against the module's export
// data: the guards' own tests check what a rule finds in a snippet.
func (m *Module) CheckSource(importPath, filename, src string) (*Package, error) {
	f, err := parser.ParseFile(m.Fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	pkg := &Package{ImportPath: importPath, Name: f.Name.Name, Files: []*ast.File{f}, FileRels: []string{filename}}
	pkg.Info = &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
		Implicits:  map[ast.Node]types.Object{},
	}
	conf := types.Config{Importer: importer.ForCompiler(m.Fset, "gc", m.lookup)}
	pkg.Types, err = conf.Check(importPath, m.Fset, pkg.Files, pkg.Info)
	return pkg, err
}

func (m *Module) lookup(p string) (io.ReadCloser, error) {
	export, ok := m.exports[p]
	if !ok {
		return nil, fmt.Errorf("modtypes: no export data for %s", p)
	}
	return os.Open(export) // #nosec G304 -- the build cache path go list just named
}

// importerFor is the importer of a package whose imports go list remapped
// by importMap (none for most), one per distinct map: the gc importer keeps
// what it read, so packages that share one share its reading.
func (m *Module) importerFor(importMap map[string]string, importers map[string]types.Importer) types.Importer {
	keys := make([]string, 0, len(importMap))
	for k, v := range importMap {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	key := strings.Join(keys, ",")
	if imp, ok := importers[key]; ok {
		return imp
	}
	imp := importer.ForCompiler(m.Fset, "gc", func(p string) (io.ReadCloser, error) {
		if mapped, ok := importMap[p]; ok {
			p = mapped
		}
		return m.lookup(p)
	})
	importers[key] = imp
	return imp
}

func path(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func tail(b []byte) []byte {
	if len(b) > 2000 {
		return b[len(b)-2000:]
	}
	return b
}

// Graph is the module's static reference graph: for every function of the
// module's production code — by Key — the functions its code references,
// called or taken as a value, as the type checker resolved them. A call
// through an interface or a function value bound elsewhere resolves to no
// function here: the graph is static.
type Graph struct {
	// Refs maps a referencing function to the functions it references.
	Refs map[string]map[string]bool
	// Where names a referencing function's file (slash path from the
	// module root).
	Where map[string]string
}

// Key names a function as the graph does: types.Func.FullName of its
// generic origin — "github.com/x/p.F", "(*github.com/x/p.T).M".
func Key(fn *types.Func) string { return fn.Origin().FullName() }

// Graph builds the module's static reference graph over its production
// files (none named *_test.go). The code of a function literal is its
// enclosing declaration's; a package-level variable's initializer is
// "var <package path>.<name>".
func (m *Module) Graph() *Graph {
	g := &Graph{Refs: map[string]map[string]bool{}, Where: map[string]string{}}
	for _, p := range m.Packages {
		if p.Info == nil {
			continue
		}
		for i, f := range p.Files {
			rel := p.FileRels[i]
			if strings.HasSuffix(rel, "_test.go") {
				continue
			}
			for _, u := range codeUnits(p, f) {
				g.Where[u.from] = rel
				for _, c := range u.code {
					ast.Inspect(c, func(n ast.Node) bool {
						id, ok := n.(*ast.Ident)
						if !ok {
							return true
						}
						if fn, ok := p.Info.Uses[id].(*types.Func); ok {
							if g.Refs[u.from] == nil {
								g.Refs[u.from] = map[string]bool{}
							}
							g.Refs[u.from][Key(fn)] = true
						}
						return true
					})
				}
			}
		}
	}
	return g
}

// codeUnit is a declaration's code as the graph attributes it.
type codeUnit struct {
	from string
	code []ast.Node
}

// codeUnits are a file's function bodies and package-level variable
// initializers, each under the key the graph gives it.
func codeUnits(p *Package, f *ast.File) []codeUnit {
	var out []codeUnit
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if fn, ok := p.Info.Defs[d.Name].(*types.Func); ok && d.Body != nil {
				out = append(out, codeUnit{Key(fn), []ast.Node{d.Body}})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				u := codeUnit{from: "var " + p.ImportPath + "." + vs.Names[0].Name}
				for _, v := range vs.Values {
					u.code = append(u.code, v)
				}
				out = append(out, u)
			}
		}
	}
	return out
}

// Reaching is every function of the graph whose code reaches one of
// targets — referencing it, or a function that does — each with the next
// function on its way there.
func (g *Graph) Reaching(targets ...string) map[string]string {
	callers := map[string][]string{}
	for from, tos := range g.Refs {
		for to := range tos {
			callers[to] = append(callers[to], from)
		}
	}
	for _, list := range callers {
		sort.Strings(list)
	}
	next := map[string]string{}
	queue := append([]string(nil), targets...)
	for len(queue) > 0 {
		to := queue[0]
		queue = queue[1:]
		for _, from := range callers[to] {
			if _, seen := next[from]; seen {
				continue
			}
			next[from] = to
			queue = append(queue, from)
		}
	}
	return next
}
