package git

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// gitSite is one git subprocess being built: start is the offset of the
// exec.Command call, end the offset just past the expression naming git.
// resolved says git is named by a value — a lookup's result, a constant —
// rather than by the literal "git" at the call.
type gitSite struct {
	start, end int
	resolved   bool
}

// gitFile is one swept Go file: its source and its git sites, in source order.
type gitFile struct {
	body  string
	sites []gitSite
}

// gitSweepSkips are the directories no sweep enters: vendored and generated
// trees, fixtures. Hidden directories are skipped too — they hold scratch
// clones of other people's code (.local, .works, .repos), and sweeping them
// reports on third-party source.
var gitSweepSkips = map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}

var (
	gitTreeOnce  sync.Once
	gitTreeFiles map[string]gitFile
	gitTreeErr   error
)

// gitSitesInTree returns every Go file under root that builds a git
// subprocess, with its sites. The three caller sweeps read the same tree, so
// it is computed once per test binary.
func gitSitesInTree(t *testing.T, root string) map[string]gitFile {
	t.Helper()
	gitTreeOnce.Do(func() { gitTreeFiles, gitTreeErr = scanGitSites(root) })
	if gitTreeErr != nil {
		t.Fatalf("sweep %s: %v", root, gitTreeErr)
	}
	return gitTreeFiles
}

// sortedGitFiles returns the paths of files, sorted, so a report reads the
// same on every run.
func sortedGitFiles(files map[string]gitFile) []string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func scanGitSites(root string) (map[string]gitFile, error) {
	dirs := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if gitSweepSkips[name] || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			dir := filepath.Dir(path)
			dirs[dir] = append(dirs[dir], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Directories are independent: each is read, filtered and type-checked
	// on its own, so they run concurrently. The error reported is the first
	// directory's in sorted order, whichever worker met it first.
	order := make([]string, 0, len(dirs))
	for dir := range dirs {
		order = append(order, dir)
	}
	sort.Strings(order)
	out := map[string]gitFile{}
	errs := make([]error, len(order))
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan int)
	for range min(runtime.GOMAXPROCS(0), 8) { // bounded: each worker holds a package's type information
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				files, err := gitSitesOfDir(dirs[order[i]])
				if err != nil {
					errs[i] = err
					continue
				}
				mu.Lock()
				maps.Copy(out, files)
				mu.Unlock()
			}
		}()
	}
	for i := range order {
		work <- i
	}
	close(work)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func gitSitesOfDir(paths []string) (map[string]gitFile, error) {
	sources := map[string][]byte{}
	for _, p := range paths {
		src, err := os.ReadFile(p) // #nosec G304 -- walking this repository's own tree
		if err != nil {
			return nil, err
		}
		sources[p] = src
	}
	sites, err := gitSpawnSitesInDir(sources)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Dir(paths[0]), err)
	}
	out := map[string]gitFile{}
	for p, s := range sites {
		out[p] = gitFile{body: string(sources[p]), sites: s}
	}
	return out, nil
}

// mentionsGit and importsExec are the prefilter: a directory none of whose
// files spells git as a string cannot name it, by literal, constant or
// lookup, and one none of whose files imports os/exec spawns nothing. It
// keeps the type check to the few directories that can do both.
func mentionsGit(src []byte) bool {
	return bytes.Contains(src, []byte(`"git"`)) || bytes.Contains(src, []byte("`git`"))
}

// importsExec reports a file importing os/exec, or golang.org/x/sys/execabs,
// its drop-in.
func importsExec(src []byte) bool {
	return bytes.Contains(src, []byte(`"os/exec"`)) || bytes.Contains(src, []byte(`"golang.org/x/sys/execabs"`))
}

// gitSpawnSitesInDir returns, per file, every git subprocess the Go files of
// one directory build.
//
// git is named two ways. The literal "git" at the call — as the command, or
// as the first argument of a wrapper named by a value (`exec.Command(nice,
// "git", …)`). And a value: a lookup's result (`bin, err :=
// envtrust.LookPath("git")`, which the PATH seam asks callers to spawn), a
// constant or variable holding "git", carried through assignments, `var`
// declarations, struct fields (keyed or positional) and the results of a
// function that returns one. A sweep that knew only the literal lost exactly
// the sites that follow the seam. golang.org/x/sys/execabs counts as os/exec.
//
// The files of a package are type-checked together, without an importer:
// locals, package variables, fields and functions of the package resolve to
// their go/types objects wherever they are declared, and a value is tracked
// by its object, never its name — a `bin` resolved for another tool, or the
// `bin` field of another type, is not git. A bare "git" string names git at a
// call or in a constant or variable, never through a struct field: a table of
// tools whose rows include git is not a git spawn. Production files are
// marked and read before test files join the marking, so a test that hands
// git to a generic production runner does not make that runner a git site.
// A package that spawns by value is type-checked in every distinct build its
// files make on linux, darwin and windows, cgo off and on, under every subset
// of its own build tags (buildConfigurations), and the sites are merged: a
// stub for another platform, a method another tag adds, or a value carried
// through a file only one build holds cannot hide the build that spawns git.
// A file no such build holds (`_linux_arm64.go`, `!linux && !darwin &&
// !windows`, `//go:build ignore`) is still read for its literal spawns.
//
// Not followed — this recognizer's limits: a value stored in another
// package's field; git passed as a parameter, to a generic runner
// (`run(t, dir, "git", …)`) or to a git-specific helper; a method value,
// a sync.OnceValues lookup, a function literal held in a variable; a field
// filled through an elided composite type or a generic instance; an external
// test package calling the package's exported helper; an argv handed to a
// transport; git after a wrapper named by a literal (`exec.Command("nice",
// "git")`); a value spawn in a file no build holds, or that only builds
// beside a twin in the package's own test files (the tests are checked with
// the package, so that build never compiles here). A build that does not
// compile is read with the bindings it has: a name or selector it binds to
// git unlike every build that compiles makes a false site — a guard that
// judges the file turns red on it unless the call already carries what it
// asks; the resolved-site floor could be met by one. A
// package with more than gitSweepMaxTags build tags fails the sweep rather
// than be swept in part.
func gitSpawnSitesInDir(sources map[string][]byte) (map[string][]gitSite, error) {
	spells, spawns := false, false
	for _, src := range sources {
		spells = spells || mentionsGit(src)
		spawns = spawns || importsExec(src)
	}
	if !spells || !spawns {
		return nil, nil
	}
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	packages := map[string][]*ast.File{}
	var names []string
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, sources[p], parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		if _, seen := packages[f.Name.Name]; !seen {
			names = append(names, f.Name.Name)
		}
		packages[f.Name.Name] = append(packages[f.Name.Name], f)
	}
	out := map[string][]gitSite{}
	for _, name := range names {
		if !spawnsByValue(packages[name]) {
			// Every command this package spawns is a literal: no value can
			// name git here, so its sites are read without a type check.
			for path, sites := range gitSpawnSitesInPackage(fset, packages[name], nil) {
				out[path] = append(out[path], sites...)
			}
			continue
		}
		seen := map[string]map[int]bool{} // path → site starts already reported
		sets, leftover, err := buildConfigurations(sources, packages[name], fset)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", name, err)
		}
		checks := make([]map[string][]gitSite, 0, len(sets)+1)
		for _, set := range sets {
			checks = append(checks, typeCheck(fset, name, set))
		}
		if len(leftover) > 0 {
			// Files no configuration builds are still read, for their
			// literal spawns, without a type check.
			checks = append(checks, gitSpawnSitesInPackage(fset, leftover, nil))
		}
		for _, check := range checks {
			for path, sites := range check {
				for _, site := range sites {
					if seen[path] == nil {
						seen[path] = map[int]bool{}
					}
					if !seen[path][site.start] {
						seen[path][site.start] = true
						out[path] = append(out[path], site)
					}
				}
			}
		}
	}
	for path := range out {
		slices.SortFunc(out[path], func(a, b gitSite) int { return a.start - b.start })
	}
	return out, nil
}

// execNamesOf is every name f imports os/exec or execabs by — a file may
// import both — leaving out blank and dot imports.
func execNamesOf(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err == nil && (p == "os/exec" || p == "golang.org/x/sys/execabs") {
			name := path.Base(p)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name != "_" && name != "." {
				names[name] = true
			}
		}
	}
	return names
}

// execCommandArg returns the command argument of an exec.Command /
// CommandContext call under a name os/exec (or execabs) is imported by, or nil.
func execCommandArg(call *ast.CallExpr, execNames map[string]bool) ast.Expr {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || !execNames[pkg.Name] {
		return nil
	}
	arg := -1
	switch sel.Sel.Name {
	case "Command":
		arg = 0
	case "CommandContext":
		arg = 1
	}
	if arg < 0 || len(call.Args) <= arg {
		return nil
	}
	return call.Args[arg]
}

// spawnsByValue reports whether some exec.Command in the package names its
// command by anything but a string literal — the only case a value can name
// git, and so the only one that needs the type check.
func spawnsByValue(files []*ast.File) bool {
	found := false
	for _, f := range files {
		execNames := execNamesOf(f)
		if len(execNames) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if arg := execCommandArg(call, execNames); arg != nil {
					if _, literal := ast.Unparen(arg).(*ast.BasicLit); !literal {
						found = true
					}
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// gitConfigurations are the platforms a package's configurations are drawn
// from, each with cgo off and on.
var gitConfigurations = [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}}

// knownBuildWords are the build-constraint words that are not a package's own
// tags: operating systems, architectures, and the toolchain's.
var knownBuildWords = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true,
	"ios": true, "js": true, "linux": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true,
	"wasip1": true, "windows": true, "zos": true, "unix": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true, "mips64": true,
	"mips64le": true, "mipsle": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
	"cgo": true, "gc": true, "gccgo": true, "ignore": true,
}

// customBuildTags returns the tags the constraint lines of a package's file
// headers name that are not platform or toolchain words, sorted.
func customBuildTags(sources map[string][]byte, files []*ast.File, fset *token.FileSet) []string {
	tags := map[string]bool{}
	for _, f := range files {
		for _, tag := range fileBuildTags(buildHeader(sources, f, fset)) {
			tags[tag] = true
		}
	}
	return slices.Sorted(maps.Keys(tags))
}

// fileName is the name of the file pos lies in, as read from disk: a `//line`
// directive renames positions for messages, never the file the sweep keys its
// sources, builds and sites by.
func fileName(fset *token.FileSet, pos token.Pos) string {
	return fset.PositionFor(pos, false).Filename
}

// buildHeader is the part of a file go/build reads constraints from: the
// text before its package clause, a leading UTF-8 byte order mark dropped as
// go/build drops it.
func buildHeader(sources map[string][]byte, f *ast.File, fset *token.FileSet) []byte {
	src := sources[fileName(fset, f.Pos())]
	return bytes.TrimPrefix(src[:min(fset.Position(f.Package).Offset, len(src))], []byte("\uFEFF"))
}

// gitSweepMaxTags bounds the package-own build tags buildConfigurations
// enumerates the subsets of: a package with more fails the sweep, naming its
// directory, rather than be swept in part. Raise it to sweep such a package — the cost
// doubles with each tag.
const gitSweepMaxTags = 10

// buildConfigurations returns every distinct file set the go tool would
// select for a package on each platform of gitConfigurations, cgo off then
// on, under every subset of the package's own build tags — whether the set
// compiles or not — and the files no such set holds. A package's tags are
// read from each file's header, the text before its package clause
// (buildHeader, fileBuildTags). Every build is read rather than a covering few chosen:
// which build binds a selector, holds a lookup beside its spawn, or leaves a
// twin out cannot be told without checking it. With cgo off, a file importing
// "C" is left out, as the go tool does.
func buildConfigurations(sources map[string][]byte, files []*ast.File, fset *token.FileSet) (sets [][]*ast.File, leftover []*ast.File, err error) {
	tags := customBuildTags(sources, files, fset)
	if len(tags) > gitSweepMaxTags {
		return nil, nil, fmt.Errorf("%d build tags %v: the git sweep builds every subset of them; raise gitSweepMaxTags to sweep this package", len(tags), tags)
	}
	importsC := func(f *ast.File) bool {
		for _, spec := range f.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && p == "C" {
				return true
			}
		}
		return false
	}
	// A file whose constraint names none of the package's tags matches the
	// same way under every subset: it is matched once per platform and cgo
	// setting.
	tagged := map[*ast.File]bool{}
	for _, f := range files {
		tagged[f] = len(fileBuildTags(buildHeader(sources, f, fset))) > 0
	}
	match := func(ctxt *build.Context, f *ast.File) bool {
		path := fileName(fset, f.Pos())
		ok, err := ctxt.MatchFile(filepath.Dir(path), filepath.Base(path))
		return err == nil && ok && (ctxt.CgoEnabled || !importsC(f))
	}
	var subsets [][]string
	for size := 0; size <= len(tags); size++ {
		subsets = append(subsets, subsetsOfSize(tags, size)...)
	}
	covered := map[*ast.File]bool{}
	seen := map[string]bool{}
	for _, platform := range gitConfigurations {
		for _, cgo := range []bool{false, true} {
			ctxt := build.Default
			ctxt.GOOS, ctxt.GOARCH, ctxt.CgoEnabled = platform[0], platform[1], cgo
			ctxt.OpenFile = func(path string) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(sources[path])), nil
			}
			untagged := map[*ast.File]bool{}
			for _, f := range files {
				if !tagged[f] {
					untagged[f] = match(&ctxt, f)
				}
			}
			for _, subset := range subsets {
				ctxt.BuildTags = subset
				var set []*ast.File
				key := ""
				for _, f := range files {
					if in, fixed := untagged[f]; fixed && !in || !fixed && !match(&ctxt, f) {
						continue
					}
					set = append(set, f)
					key += fileName(fset, f.Pos()) + "\x00"
				}
				if len(set) == 0 || seen[key] {
					continue
				}
				seen[key] = true
				sets = append(sets, set)
				for _, f := range set {
					covered[f] = true
				}
			}
		}
	}
	for _, f := range files {
		if !covered[f] {
			leftover = append(leftover, f)
		}
	}
	return sets, leftover, nil
}

// fileBuildTags returns the package-own tags the constraint lines of a file
// header name, `//go:build` or `// +build`, trimmed: every tag go/build could
// read there, and possibly more (a `+build` beside a `//go:build`, a line
// inside a `/* */`) — the tags only enumerate the builds; MatchFile decides
// which files each one holds.
func fileBuildTags(src []byte) []string {
	tags := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			continue
		}
		expr.Eval(func(tag string) bool {
			if !knownBuildWords[tag] && !strings.HasPrefix(tag, "go1") && !strings.HasPrefix(tag, "goexperiment.") {
				tags[tag] = true
			}
			return false
		})
	}
	return slices.Sorted(maps.Keys(tags))
}

// subsetsOfSize returns the subsets of tags with n elements, in order.
func subsetsOfSize(tags []string, n int) [][]string {
	if n == 0 {
		return [][]string{nil}
	}
	var out [][]string
	var walk func(start int, acc []string)
	walk = func(start int, acc []string) {
		if len(acc) == n {
			out = append(out, slices.Clone(acc))
			return
		}
		for i := start; i < len(tags); i++ {
			walk(i+1, append(acc, tags[i]))
		}
	}
	walk(0, nil)
	return out
}

// typeCheck type-checks one build's files as a package, without an
// importer, and reads their git sites through its objects. A set declaring a
// package-level name or a method twice — twins of exclusive tags set
// together, which no build compiles — is read for literal spawns only: its
// objects would lend one twin's git to the other, and every build that
// compiles is read on its own. Two imports whose paths end alike
// (`math/rand/v2`, `…/miniredis/v2`) are not such a twin: without an
// importer, each declares its fake package's last path element in the file
// scope.
func typeCheck(fset *token.FileSet, name string, files []*ast.File) map[string][]gitSite {
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{},
	}
	redeclared := false
	conf := types.Config{Error: func(err error) {
		te, ok := err.(types.Error)
		if ok && (strings.Contains(te.Msg, "redeclared") || strings.Contains(te.Msg, "already declared at")) && !inImport(files, te.Pos) {
			redeclared = true
		}
	}, FakeImportC: true}
	_, _ = conf.Check(name, fset, files, info)
	if redeclared {
		info = nil
	}
	return gitSpawnSitesInPackage(fset, files, info)
}

// gitSpawnSitesInPackage reads the git sites of files through a type check's
// objects, or for literal spawns only when info is nil.
func gitSpawnSitesInPackage(fset *token.FileSet, files []*ast.File, info *types.Info) map[string][]gitSite {
	if info == nil {
		info = &types.Info{
			Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{},
		}
	}
	objectOf := func(id *ast.Ident) types.Object {
		if o := info.Defs[id]; o != nil {
			return o
		}
		return info.Uses[id]
	}

	objects := map[types.Object]bool{} // variables, fields and functions that hold or return git
	var isGit func(e ast.Expr) bool
	callsGitSource := func(call *ast.CallExpr) bool {
		if isGitLookup(call, isGit) {
			return true
		}
		var fn *ast.Ident
		switch f := ast.Unparen(call.Fun).(type) {
		case *ast.Ident:
			fn = f
		case *ast.SelectorExpr:
			fn = f.Sel
		}
		if fn == nil {
			return false
		}
		o, ok := info.Uses[fn].(*types.Func)
		return ok && objects[o]
	}
	isGit = func(e ast.Expr) bool {
		switch v := ast.Unparen(e).(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			return v.Kind == token.STRING && err == nil && s == "git"
		case *ast.Ident:
			o := objectOf(v)
			return o != nil && objects[o]
		case *ast.SelectorExpr:
			o := info.Uses[v.Sel]
			return o != nil && objects[o]
		case *ast.CallExpr:
			return callsGitSource(v)
		}
		return false
	}
	// fieldValue: a value that makes a field git — a resolved one, never a
	// bare "git" string (a table of tools is not a git spawn).
	fieldValue := func(e ast.Expr) bool {
		if _, literal := ast.Unparen(e).(*ast.BasicLit); literal {
			return false
		}
		return isGit(e)
	}
	mark := func(o types.Object) bool {
		if o == nil || objects[o] {
			return false
		}
		objects[o] = true
		return true
	}
	markExpr := func(e ast.Expr) bool {
		switch v := ast.Unparen(e).(type) {
		case *ast.Ident:
			if v.Name == "_" {
				return false
			}
			return mark(objectOf(v))
		case *ast.SelectorExpr:
			return mark(info.Uses[v.Sel])
		}
		return false
	}
	markFlow := func(lhs, rhs []ast.Expr) bool {
		changed := false
		switch {
		case len(lhs) == len(rhs):
			for i := range rhs {
				value := isGit(rhs[i])
				if _, field := ast.Unparen(lhs[i]).(*ast.SelectorExpr); field {
					value = fieldValue(rhs[i])
				}
				if value && markExpr(lhs[i]) {
					changed = true
				}
			}
		case len(rhs) == 1 && len(lhs) == 2:
			// bin, err := LookPath("git") — or a function returning one.
			if call, ok := ast.Unparen(rhs[0]).(*ast.CallExpr); ok && callsGitSource(call) && markExpr(lhs[0]) {
				changed = true
			}
		}
		return changed
	}
	markLiteral := func(lit *ast.CompositeLit) bool {
		var name *ast.Ident
		switch t := lit.Type.(type) {
		case *ast.Ident:
			name = t
		case *ast.SelectorExpr:
			name = t.Sel
		}
		if name == nil {
			return false // an elided or generic type: not followed
		}
		tn, ok := info.Uses[name].(*types.TypeName)
		if !ok {
			return false
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			return false // a map's key or a slice's element is not a field
		}
		changed := false
		for i, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && fieldValue(kv.Value) && mark(info.Uses[key]) {
					changed = true
				}
				continue
			}
			if i < st.NumFields() && fieldValue(elt) && mark(st.Field(i)) {
				changed = true
			}
		}
		return changed
	}
	markReturns := func(fn *ast.FuncDecl) bool {
		o := info.Defs[fn.Name]
		if o == nil || objects[o] || fn.Body == nil {
			return false
		}
		returns := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncLit:
				return false // its returns are its own
			case *ast.ReturnStmt:
				if len(v.Results) == 0 {
					// A bare return hands on the named results.
					if res := fn.Type.Results; res != nil && len(res.List) > 0 && len(res.List[0].Names) > 0 {
						if o := info.Defs[res.List[0].Names[0]]; o != nil && objects[o] {
							returns = true
						}
					}
					return !returns
				}
				first := v.Results[0]
				if len(v.Results) == 1 {
					// `return LookPath("git")` hands both results on.
					if call, ok := ast.Unparen(first).(*ast.CallExpr); ok && callsGitSource(call) {
						returns = true
					}
				}
				if isGit(first) {
					returns = true
				}
			}
			return !returns
		})
		return returns && mark(o)
	}
	// A value travels through assignments, literals and returns in any order
	// relative to its use, so marking repeats until nothing new is learnt.
	markAll := func(files []*ast.File) {
		for changed := true; changed; {
			changed = false
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					switch v := n.(type) {
					case *ast.AssignStmt:
						changed = markFlow(v.Lhs, v.Rhs) || changed
					case *ast.ValueSpec:
						lhs := make([]ast.Expr, len(v.Names))
						for i, n := range v.Names {
							lhs[i] = n
						}
						changed = markFlow(lhs, v.Values) || changed
					case *ast.CompositeLit:
						changed = markLiteral(v) || changed
					case *ast.FuncDecl:
						changed = markReturns(v) || changed
					}
					return true
				})
			}
		}
	}

	out := map[string][]gitSite{}
	detect := func(files []*ast.File) {
		for _, f := range files {
			execNames := execNamesOf(f)
			if len(execNames) == 0 {
				continue
			}
			path := fileName(fset, f.Pos())
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				arg := execCommandArg(call, execNames)
				if arg != nil && !isGit(arg) {
					// A wrapper named by a value (`exec.Command(nice, "git",
					// …)`): git runs as its child, with the same environment.
					if _, literal := ast.Unparen(arg).(*ast.BasicLit); !literal {
						if i := slices.Index(call.Args, arg); i+1 < len(call.Args) {
							if next, ok := ast.Unparen(call.Args[i+1]).(*ast.BasicLit); ok && isGit(next) {
								arg = next
							}
						}
					}
				}
				if arg == nil || !isGit(arg) {
					return true
				}
				_, literal := ast.Unparen(arg).(*ast.BasicLit)
				out[path] = append(out[path], gitSite{
					start:    fset.Position(call.Pos()).Offset,
					end:      fset.Position(arg.End()).Offset,
					resolved: !literal,
				})
				return true
			})
		}
	}
	// Production first: its sites are read before a test's values join the
	// marking, so a test handing git to a production runner does not turn
	// that runner into a git site.
	var production, tests []*ast.File
	for _, f := range files {
		if strings.HasSuffix(fileName(fset, f.Pos()), "_test.go") {
			tests = append(tests, f)
		} else {
			production = append(production, f)
		}
	}
	markAll(production)
	detect(production)
	markAll(files)
	detect(tests)
	return out
}

// isGitLookup reports a call of any LookPath (exec's, envtrust's) asked for
// git — the literal, or a value holding it.
func isGitLookup(call *ast.CallExpr, isGit func(ast.Expr) bool) bool {
	name := ""
	switch fn := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	return name == "LookPath" && len(call.Args) == 1 && isGit(call.Args[0])
}

// inImport reports whether pos lies in an import declaration of files.
func inImport(files []*ast.File, pos token.Pos) bool {
	for _, f := range files {
		for _, spec := range f.Imports {
			if spec.Pos() <= pos && pos < spec.End() {
				return true
			}
		}
	}
	return false
}
