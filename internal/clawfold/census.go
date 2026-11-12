package clawfold

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The fold's coordinates. The module path is the one iterion requires today;
// the destination keeps claw's own layout below pkg/claw, so Go's internal/
// visibility holds unchanged (pkg/claw/internal/… is importable from
// pkg/claw/… only, as claw's internal/ was from claw).
const (
	SourceModule = "github.com/SocialGouv/claw-code-go"
	DestModule   = "github.com/SocialGouv/iterion/pkg/claw"
	DestDir      = "pkg/claw"
	VendorDir    = "vendor/github.com/SocialGouv/claw-code-go"
)

// Fold is the import mapping the fold applies; its Inverse is `unrewrite`.
var Fold = Mapping{From: SourceModule, To: DestModule}

// Kind sorts a member file by what the toolchain does with it.
type Kind string

const (
	KindGo     Kind = "go"      // compiled into its package
	KindGoTest Kind = "go-test" // compiled by `go test` only
	KindAsset  Kind = "asset"   // never compiled: an embed, testdata, a licence
)

// Origin says which rule of the membership made a file a member.
type Origin string

const (
	// OriginVendored: the file is in the vendored set iterion links today.
	OriginVendored Origin = "vendored"
	// OriginPackage: another Go file of a member package — its tests, or the
	// code of a package only the tests reach.
	OriginPackage Origin = "package"
	// OriginAsset: a non-Go file a member package owns that vendoring drops
	// (testdata).
	OriginAsset Origin = "asset"
	// OriginFixture: a file outside every Go package that a member test reads
	// through a relative path.
	OriginFixture Origin = "fixture"
)

// Member is one file the fold carries, with the proof material for it.
type Member struct {
	Path           string `json:"path"` // in the source tree
	Dest           string `json:"dest"` // in iterion
	Mode           string `json:"mode"` // git mode, kept at the destination
	Kind           Kind   `json:"kind"`
	Origin         Origin `json:"origin"`
	Package        string `json:"package,omitempty"` // owning package dir; empty when none owns it
	Lines          int    `json:"lines"`             // as git counts an added file
	SrcBlob        string `json:"src_blob"`
	DestBlob       string `json:"dest_blob"`
	ImportSites    int    `json:"import_sites,omitempty"`
	RewrittenLines []int  `json:"rewritten_lines,omitempty"`

	src, dest  []byte
	extImports []string // imports outside the standard library and the module
}

// PackageOrigin says which rule made a Go package a member.
type PackageOrigin string

const (
	PackageVendored PackageOrigin = "vendored"       // iterion links it today
	PackageClosure  PackageOrigin = "import-closure" // a member's file (often a test) imports it
	PackageTestOnly PackageOrigin = "test-only"      // tests only, nested under a member package
)

// Package is one member Go package.
type Package struct {
	Dir    string        `json:"dir"`
	Origin PackageOrigin `json:"origin"`
	// Deps are the member packages it imports, from its sources AND its
	// tests: an external test package importing back is still an order
	// constraint on landing.
	Deps  []string `json:"deps,omitempty"`
	Files int      `json:"files"`
	Lines int      `json:"lines"` // Go files only; owned assets are separate units
}

// Residual is a line that names the source module path and that the fold
// leaves verbatim, because it is not an import spec.
type Residual struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Census is the computed membership of the fold at one pin.
type Census struct {
	Pin         string              `json:"pin"`
	Packages    map[string]*Package `json:"packages"`
	Members     []*Member           `json:"members"`
	ImportSites int                 `json:"import_sites"`
	Residuals   []Residual          `json:"residuals,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`

	vendorPkgs map[string]bool // the packages vendor/modules.txt holds
}

// Lines counts data the way git counts the lines of an added file.
func Lines(data []byte) int {
	// git's own test: a NUL in the first 8000 bytes makes a file binary,
	// which a diff counts as no added line at all.
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func dirOf(p string) string { return path.Dir(p) }

// isPackageFile reports whether the go tool compiles p into a package: a .go
// file none of whose directories is testdata or starts with "." or "_" —
// the go tool ignores those, so a Go file there is data, carried verbatim.
func isPackageFile(p string) bool {
	if !strings.HasSuffix(p, ".go") || strings.HasPrefix(path.Base(p), ".") || strings.HasPrefix(path.Base(p), "_") {
		return false
	}
	return !inDataDir(p)
}

// inDataDir reports whether a directory of p is one the go tool ignores:
// testdata, or a name starting with "." or "_".
func inDataDir(p string) bool {
	dir := dirOf(p)
	if dir == "." {
		return false // the root itself, not a directory named "."
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "testdata" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

// Compute derives the fold's membership from the vendored set (what iterion
// links, embeds included) and the source tree at the pin (which also carries
// the tests). vendored maps paths relative to the vendored module root to
// their bytes.
func Compute(tree *Tree, vendored map[string][]byte) (*Census, error) {
	c := &Census{Pin: tree.Commit, Packages: map[string]*Package{}}

	goFiles := map[string][]string{} // package dir -> its .go files at the pin
	for p := range tree.Entries {
		if isPackageFile(p) {
			goFiles[dirOf(p)] = append(goFiles[dirOf(p)], p)
		}
	}
	for _, files := range goFiles {
		sort.Strings(files)
	}

	// Rule 1 — the core: the vendored set, proven identical to the pin.
	if len(vendored) == 0 {
		return nil, fmt.Errorf("the vendored set is empty: %s holds no file", VendorDir)
	}
	var mismatched []string
	for p, data := range vendored {
		e, ok := tree.Entries[p]
		if !ok {
			mismatched = append(mismatched, p+" (absent at the pin)")
			continue
		}
		if got := BlobID(data, len(e.Blob)); got != e.Blob {
			mismatched = append(mismatched, fmt.Sprintf("%s (vendored %s, pin %s)", p, got, e.Blob))
		}
	}
	if len(mismatched) > 0 {
		sort.Strings(mismatched)
		return nil, fmt.Errorf("the vendored set and the pin %s disagree on %d file(s) — the membership source and the content source must be the same commit: %s",
			tree.Commit, len(mismatched), strings.Join(firstN(mismatched, 10), "; "))
	}

	importsCache := map[string][]string{}
	clawImports := func(file string) ([]string, error) {
		if deps, ok := importsCache[file]; ok {
			return deps, nil
		}
		data, _ := tree.Content(file)
		specs, err := importSpecs(file, data)
		if err != nil {
			return nil, err
		}
		var deps []string
		for _, s := range specs {
			rel, under := Mapping{From: SourceModule, To: "."}.mapPath(s.path)
			if !under {
				continue
			}
			dir := path.Clean(rel)
			if _, ok := goFiles[dir]; !ok {
				return nil, fmt.Errorf("%s:%d imports %s, which the pin holds no Go package for", file, s.line, s.path)
			}
			deps = append(deps, dir)
		}
		importsCache[file] = deps
		return deps, nil
	}

	// Rule 2 — close over the imports of every Go file of every member
	// package, sources and tests.
	addPackage := func(dir string, origin PackageOrigin) bool {
		if _, ok := c.Packages[dir]; ok {
			return false
		}
		c.Packages[dir] = &Package{Dir: dir, Origin: origin}
		return true
	}
	for p := range vendored {
		if isPackageFile(p) {
			addPackage(dirOf(p), PackageVendored)
		}
	}
	closeOver := func() error {
		queue := sortedKeys(c.Packages)
		for len(queue) > 0 {
			dir := queue[0]
			queue = queue[1:]
			for _, f := range goFiles[dir] {
				deps, err := clawImports(f)
				if err != nil {
					return err
				}
				for _, d := range deps {
					if addPackage(d, PackageClosure) {
						queue = append(queue, d)
					}
				}
			}
		}
		return nil
	}
	if err := closeOver(); err != nil {
		return nil, err
	}

	// Rule 3 — a tests-only package nested under a member package is the
	// library's own test code, and nothing imports it for the closure to
	// find. Repeat until no new one appears.
	for {
		added := false
		for _, dir := range sortedKeys(goFiles) {
			if _, member := c.Packages[dir]; member || !allTests(goFiles[dir]) || !underMember(dir, c.Packages) {
				continue
			}
			addPackage(dir, PackageTestOnly)
			added = true
		}
		if !added {
			break
		}
		if err := closeOver(); err != nil {
			return nil, err
		}
	}

	// Rules 4 and 6 — every vendored file, every file a member package owns.
	members := map[string]Origin{}
	owner := func(p string) string {
		for d := dirOf(p); ; d = dirOf(d) {
			if _, ok := goFiles[d]; ok {
				return d
			}
			if d == "." {
				return ""
			}
		}
	}
	for p := range vendored {
		members[p] = OriginVendored
	}
	for p := range tree.Entries {
		if _, ok := members[p]; ok {
			continue
		}
		o := owner(p)
		if _, member := c.Packages[o]; !member || o == "" {
			continue
		}
		if isPackageFile(p) {
			members[p] = OriginPackage
			continue
		}
		members[p] = OriginAsset
	}

	// Rule 5 — files outside every package that a member test reads.
	fixtures, warnings, err := testFixtures(tree, members, owner)
	if err != nil {
		return nil, err
	}
	c.Warnings = append(c.Warnings, warnings...)
	for _, p := range fixtures {
		if _, ok := members[p]; !ok {
			members[p] = OriginFixture
		}
	}

	// Rule 4b — what a member's //go:embed patterns name: vendoring keeps
	// the embeds of the files it vendors, not those of a test or of a
	// package only the tests reach.
	embedded, embedWarnings, err := embedFiles(tree, members)
	if err != nil {
		return nil, err
	}
	c.Warnings = append(c.Warnings, embedWarnings...)
	for _, p := range embedded {
		if _, ok := members[p]; !ok {
			members[p] = OriginAsset
		}
	}

	// A go.mod or go.work landing outside a data directory would make its
	// directory another module: the go tool would then skip the fold
	// silently instead of building it.
	for p := range members {
		if base := path.Base(p); (base == "go.mod" || base == "go.work") && !inDataDir(p) {
			return nil, fmt.Errorf("%s would land as %s and make that directory a module of its own; the fold refuses to carry it", p, path.Join(DestDir, p))
		}
	}

	// The manifest: per file, its destination and the proof material.
	for _, p := range sortedKeys(members) {
		m, err := c.member(tree, p, members[p], owner(p))
		if err != nil {
			return nil, err
		}
		c.Members = append(c.Members, m)
		if m.Kind != KindAsset {
			pkg := c.Packages[m.Package]
			pkg.Files++
			pkg.Lines += m.Lines
			deps, _ := clawImports(p)
			for _, d := range deps {
				if d != m.Package && !contains(pkg.Deps, d) {
					pkg.Deps = append(pkg.Deps, d)
				}
			}
		}
	}
	for _, pkg := range c.Packages {
		sort.Strings(pkg.Deps)
	}
	return c, nil
}

func (c *Census) member(tree *Tree, p string, origin Origin, owner string) (*Member, error) {
	e := tree.Entries[p]
	// Load's module view has no link left; this guards a direct caller of
	// Compute handing it a tree that does.
	if e.Mode == "120000" {
		return nil, fmt.Errorf("%s is a symbolic link; the fold carries regular files only", p)
	}
	src, _ := tree.Content(p)
	m := &Member{
		Path:    p,
		Dest:    path.Join(DestDir, p),
		Mode:    e.Mode,
		Origin:  origin,
		Lines:   Lines(src),
		SrcBlob: e.Blob,
		src:     src,
	}
	switch {
	case isPackageFile(p) && strings.HasSuffix(p, "_test.go"):
		m.Kind = KindGoTest
	case isPackageFile(p):
		m.Kind = KindGo
	default:
		m.Kind = KindAsset
	}
	if m.Kind != KindAsset {
		m.Package = owner
	} else if _, member := c.Packages[owner]; member && origin != OriginFixture {
		m.Package = owner
	}

	m.dest = src
	if m.Kind != KindAsset {
		taken, err := Fold.ImportsUnderDestination(p, src)
		if err != nil {
			return nil, err
		}
		if len(taken) > 0 {
			return nil, fmt.Errorf("%s already imports under %s (line(s) %v): the rewrite would not have an exact inverse", p, DestModule, taken)
		}
		dest, lines, sites, err := Fold.rewrite(p, src)
		if err != nil {
			return nil, err
		}
		specs, err := importSpecs(p, src)
		if err != nil {
			return nil, err
		}
		for _, spec := range specs {
			if _, under := Fold.mapPath(spec.path); !under && strings.Contains(strings.Split(spec.path, "/")[0], ".") {
				m.extImports = append(m.extImports, spec.path)
			}
		}
		m.dest, m.RewrittenLines, m.ImportSites = dest, lines, sites
		c.ImportSites += sites
		if err := gofmtVerdict(p, src, dest, &c.Warnings); err != nil {
			return nil, err
		}
	}
	if m.Kind == KindAsset && strings.HasSuffix(p, ".go") {
		// Data to the go tool, but CI's gofmt step reads every .go file.
		if clean, err := GofmtStable(src); err != nil || !clean {
			c.Warnings = append(c.Warnings, p+": a Go file the go tool ignores, not gofmt-clean; CI's gofmt step reads it, and the fold keeps it byte for byte")
		}
	}
	m.DestBlob = BlobID(m.dest, len(e.Blob))
	if bytes.IndexByte(src, '\r') >= 0 {
		c.Warnings = append(c.Warnings, p+": holds a carriage return; a .gitattributes text rule would normalise it at `git add` and the committed blob would not be the planned one")
	}
	for i, line := range bytes.Split(m.dest, []byte("\n")) {
		if bytes.Contains(line, []byte(SourceModule)) {
			c.Residuals = append(c.Residuals, Residual{File: p, Line: i + 1, Text: strings.TrimSpace(string(line))})
		}
	}
	return m, nil
}

// gofmtVerdict refuses a rewrite that moves gofmt's import order, and
// records — without refusing — a source the pin already ships unformatted:
// that is the source's state, not the rewrite's doing.
func gofmtVerdict(p string, src, dest []byte, warnings *[]string) error {
	srcStable, err := GofmtStable(src)
	if err != nil {
		return fmt.Errorf("gofmt %s: %w", p, err)
	}
	destStable, err := GofmtStable(dest)
	if err != nil {
		return fmt.Errorf("gofmt rewritten %s: %w", p, err)
	}
	switch {
	case srcStable && !destStable:
		return fmt.Errorf("%s: the rewrite changes what gofmt does to the file (the import order moved); a gofmt pass would move lines the proof does not cover", p)
	case !srcStable && destStable:
		return fmt.Errorf("%s: the pin ships it unformatted and the rewrite makes it gofmt-clean: `clawfold unrewrite`, which re-sorts a formatted input, would not give it back", p)
	case !srcStable:
		*warnings = append(*warnings, p+": the pin ships this file not gofmt-clean; the fold keeps it so")
	}
	return nil
}

// testFixtures finds the files a member test reads through a relative path
// leaving its package: a string literal starting with "../", or a
// filepath.Join / path.Join whose arguments are all literals and join to
// one. A path that resolves to a file — or a directory holding files — of
// the tree, owned by no Go package, is a fixture. One that lands in a
// package the fold does not carry is reported, not carried.
func testFixtures(tree *Tree, members map[string]Origin, owner func(string) string) ([]string, []string, error) {
	found := map[string]bool{}
	var warnings []string
	for _, p := range sortedKeys(members) {
		if !isPackageFile(p) || !strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, _ := tree.Content(p)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", p, err)
		}
		joins := pathPackageNames(file)
		var candidates []relPath
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, ok := stringLit(n); ok {
					candidates = append(candidates, relPath{v, fset.Position(n.Pos()).Line})
				}
			case *ast.CallExpr:
				if v, ok := literalJoin(n, joins); ok {
					candidates = append(candidates, relPath{v, fset.Position(n.Pos()).Line})
				}
			}
			return true
		})
		for _, cand := range candidates {
			if !strings.HasPrefix(cand.value, "../") {
				continue
			}
			target := path.Clean(path.Join(dirOf(p), cand.value))
			if target == "." || target == ".." || strings.HasPrefix(target, "../") {
				continue
			}
			for _, f := range filesAt(tree, target) {
				switch o := owner(f); {
				case o == "":
					found[f] = true
				case o != "" && members[f] == "":
					warnings = append(warnings, fmt.Sprintf("%s:%d reads %s, which %s owns and the fold does not carry", p, cand.line, f, o))
				}
			}
		}
	}
	return sortedKeys(found), dedupeStrings(warnings), nil
}

type relPath struct {
	value string
	line  int
}

func stringLit(lit *ast.BasicLit) (string, bool) {
	if lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// pathPackageNames returns the names under which file imports path and
// path/filepath.
func pathPackageNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || (p != "path" && p != "path/filepath") {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = true
	}
	return names
}

func literalJoin(call *ast.CallExpr, joins map[string]bool) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !joins[pkg.Name] || len(call.Args) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(call.Args))
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok {
			return "", false
		}
		v, ok := stringLit(lit)
		if !ok {
			return "", false
		}
		parts = append(parts, v)
	}
	return path.Join(parts...), true
}

// filesAt returns the file at target, or every file below the directory
// target names.
func filesAt(tree *Tree, target string) []string {
	if _, ok := tree.Entries[target]; ok {
		return []string{target}
	}
	var out []string
	for p := range tree.Entries {
		if strings.HasPrefix(p, target+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func allTests(files []string) bool {
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			return false
		}
	}
	return len(files) > 0
}

func underMember(dir string, pkgs map[string]*Package) bool {
	for d := dirOf(dir); d != "."; d = dirOf(d) {
		if _, ok := pkgs[d]; ok {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func dedupeStrings(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func firstN(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return append(list[:n:n], fmt.Sprintf("… and %d more", len(list)-n))
}

// embedFiles expands the //go:embed patterns of every member Go file
// against the tree, the way the go tool does: a pattern names files or
// directories relative to the file's package; a directory brings every file
// below it except those under a name starting with "." or "_", unless the
// pattern carries the "all:" prefix. A match that is a Go file of a package
// is reported, not carried: embedding another package's source is not data.
func embedFiles(tree *Tree, members map[string]Origin) ([]string, []string, error) {
	found := map[string]bool{}
	var warnings []string
	for _, p := range sortedKeys(members) {
		if !isPackageFile(p) {
			continue
		}
		src, _ := tree.Content(p)
		pkg := dirOf(p)
		// Directives are comments: parsed, so a "//go:embed" inside a string
		// literal (a test's fixture source) is not one.
		file, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", p, err)
		}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				rest, ok := strings.CutPrefix(comment.Text, "//go:embed ")
				if !ok {
					continue
				}
				patterns, err := embedPatterns(rest)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", p, err)
				}
				for _, pattern := range patterns {
					all := strings.HasPrefix(pattern, "all:")
					pattern = strings.TrimPrefix(pattern, "all:")
					for f := range tree.Entries {
						rel, ok := strings.CutPrefix(f, pkg+"/")
						if pkg == "." {
							rel, ok = f, true
						}
						if !ok || !embedMatches(pattern, rel, all) {
							continue
						}
						if isPackageFile(f) && dirOf(f) != pkg {
							warnings = append(warnings, fmt.Sprintf("%s embeds %s, a Go file of another package; not carried", p, f))
							continue
						}
						found[f] = true
					}
				}
			}
		}
	}
	return sortedKeys(found), dedupeStrings(warnings), nil
}

// embedMatches reports whether the file at rel (relative to the embedding
// package) is named by pattern: directly, or through a directory pattern.
func embedMatches(pattern, rel string, all bool) bool {
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		if ok, _ := path.Match(pattern, dir); !ok {
			continue
		}
		if all {
			return true
		}
		for _, seg := range parts[i:] {
			if strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
				return false
			}
		}
		return true
	}
	return false
}

// embedPatterns splits a //go:embed argument list: space-separated
// patterns, each optionally a Go string literal.
func embedPatterns(args string) ([]string, error) {
	var out []string
	for args = strings.TrimSpace(args); args != ""; args = strings.TrimLeftFunc(args, unicode.IsSpace) {
		if args[0] == '"' || args[0] == '`' {
			end := strings.IndexByte(args[1:], args[0])
			if end < 0 {
				return nil, fmt.Errorf("unterminated //go:embed pattern %s", args)
			}
			v, err := strconv.Unquote(args[:end+2])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			args = args[end+2:]
			continue
		}
		// The go tool splits on any Unicode space, a tab included.
		end := strings.IndexFunc(args, unicode.IsSpace)
		if end < 0 {
			end = len(args)
		}
		out = append(out, args[:end])
		args = args[end:]
	}
	return out, nil
}
