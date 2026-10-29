package layerguard

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// repoRoot climbs from the test's working directory to the go.mod — the
// guard must run `go list` from the module root, wherever the test runs from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test — run from inside the module")
		}
		dir = parent
	}
}

// modulePrefixOf reads the module path from root/go.mod — the one place that
// names it — and returns it with the trailing slash parseListing trims.
// Nothing else in this file spells a module path: a rule's bare
// `pkg/dsl/ir` and `go list`'s full import path are comparable only because
// the prefix comes off at that one seam (iterion#2829).
func modulePrefixOf(root string) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- the module root the test climbed to
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(p) + "/", nil
		}
	}
	return "", fmt.Errorf("%s/go.mod carries no module line", root)
}

// layerRule forbids a package family from importing another. Both sides are
// MODULE-RELATIVE path prefixes (pkg/dsl/ir, internal), the form
// parseListing hands out.
type layerRule struct {
	name    string
	sources []string // package path prefixes the rule binds
	banned  []string // package path prefixes they must not import
}

// The layer map, as an executable guard. Each rule forbids a package family
// from importing another — the map codifies what the tree already does, so
// the guard bites on the FIRST new violation, not after a refactor.
//
// What it judges, exactly: the DIRECT imports of the non-test files of
// ./pkg/... and ./internal/..., unioned over every context listContexts
// names (the host, GOOS=windows, GOOS=darwin, GOOS=freebsd, and the build
// tags the tree's own non-test files carry). A family reached THROUGH
// another package is not a rule hit — what pkg/dsl/ir's direct imports
// really name, against what only its closure holds (iterion#2857), is one
// sentence of docs/layering.md rather than a second copy of the fact here,
// and TestTheLeafsOneHopReadingIsWhatThePageSays faces that sentence with
// the tree. A coupling that exists only in a _test.go file is out of scope
// by decision. docs/layering.md carries the same three bounds; the contexts
// are judged by TestEveryNonTestFileIsSeenByAListingContext, the one-hop
// evaluation by TestTheGuardJudgesDirectImportsOnly.
//
// Verdict faced on 2026-10-29 at e59110ed5, over the 213 packages and 974
// module-internal import edges the union named there: 12 violating pairs,
// every one of them accepted into allowExceptions below with its reason.
// Those counts are that reading's provenance — expectedAllowCount and
// expectedRuleHitCount are the two numbers this file keeps true. The first
// reading of this file ("verified 2026-10-23 over 903 internal import
// pairs: zero violations") was not a verdict: the guard compared `go list`'s
// full import path to these bare prefixes, so no rule ever bound a package
// and nothing was checked.
//
// The ratchet: a violation that review accepts goes into allowExceptions
// with its reason, and expectedAllowCount moves with it — both are edits a
// reviewer sees. The list can only shrink without a review.
var layers = []layerRule{
	{"pkg/dsl/ir is a leaf — no executor, server, dispatcher, CLI, backend, forge or runner imports", []string{"pkg/dsl/ir"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend", "pkg/forge", "pkg/runner"}},
	{"pkg/dsl compiles without an executor behind it", []string{"pkg/dsl"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend"}},
	{"no pkg package imports the composition root — the server is reached through cmd/ and the seams", []string{"pkg"}, []string{"pkg/server"}},
	{"pkg/runtime never imports the server, the dispatcher or the CLI", []string{"pkg/runtime"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the backends never import the server, the dispatcher or the CLI", []string{"pkg/backend"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the dispatcher never imports the server or the CLI", []string{"pkg/dispatcher"}, []string{"pkg/server", "pkg/cli"}},
	{"internal never imports the composition root", []string{"internal"}, []string{"pkg/server"}},
	{"the store stays a leaf — no executor, server, dispatcher, backend or CLI imports", []string{"pkg/store"}, []string{"pkg/server", "pkg/runtime", "pkg/dispatcher", "pkg/backend", "pkg/cli"}},
}

// allowExceptions maps "source -> banned" violations that review has
// accepted, each with the reason that keeps it honest. expectedAllowCount
// pins the count so an addition is an edit of this file, visible in review,
// never a silent rot; TestEveryAcceptedExceptionIsALiveViolation refuses an
// entry the tree no longer carries, so the list can only shrink.
var allowExceptions = map[string]string{
	// The IR validates a .bot against what the chosen backend can do, so it
	// reads the capability vocabulary where it is defined instead of keeping
	// a second copy of it (AGENTS.md: never a second list). Each package
	// below is vocabulary — constants, Parse, SupportsBackend — never an
	// executor, and the five leaves carry 1 to 3 module packages each.
	"pkg/dsl/ir -> pkg/backend/backendcaps":        "capability-aware validation: validate_provider/command/fallbacks/async_backend/sandbox_image_presence refuse a .bot asking a backend for what the registry says it cannot do (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/toolcatalog":        "validate_tools/fallbacks/permission_rules resolve tool names against THE catalogue, never a second list (leaf: 2 module packages)",
	"pkg/dsl/ir -> pkg/backend/permission":         "validate_permission_rules parses a rule with the permission grammar that enforces it (leaf: 3 module packages)",
	"pkg/dsl/ir -> pkg/backend/modelroute":         "validate_gateway/nodes decide gateway shape with modelroute.Parse, the one reader of a model reference (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/ambient":            "validate_nodes names the backends that honour an ambient-context policy from ambient.SupportOf, the measurement's source (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/automemory":         "validate_nodes asks automemory.SupportsBackend which backends serve durable memory — vocabulary, but the package drags pkg/store and pkg/knowledge (10 module packages): filed as iterion#2857",
	"pkg/dsl/ir -> pkg/dispatcher/native/boardops": "validate_capabilities names the board capabilities from boardops.CapBoard* — seven constants that drag the native dispatcher, pkg/store, pkg/forge and pkg/mail (43 module packages): filed as iterion#2857",

	// pkg/cli composes and starts an in-process server (server.New in
	// studio.go and workspace_studio.go) and reads its response types; its
	// only importers are cmd/iterion and cmd/iterion-desktop, so the
	// composition stays in the command layer. Accepted, not corrected away:
	// a third entry here would mean the CLI is reaching deeper, which is
	// the signal this list exists to carry.
	"pkg/cli -> pkg/server":          "iterion studio/dispatch compose the in-process server (server.New, server.Config, BrowserGuard, ImportForgeIssues) and the CLI aliases its response types",
	"pkg/cli -> pkg/server/projects": "iterion studio --workspace loads the project registry that NewWorkspaceHost is built from",

	// The claw backend exposes the board as in-process tools; every
	// operation goes through boardops so validation and event emission
	// match the stdio and HTTP transports byte-for-byte.
	"pkg/backend/tool -> pkg/dispatcher/native/boardops":     "RegisterClawBoardTools runs each board operation through boardops.Call, THE board implementation the other two transports use",
	"pkg/backend/tool -> pkg/dispatcher/native":              "BoardConfig.Store is the dispatcher's own *native.Store handle, shared so a tool call is visible to the dispatcher immediately",
	"pkg/backend/delegate -> pkg/dispatcher/native/boardops": "the MCP board bridge serves the same operations through the same boardops entry as the claw tools",
}

const expectedAllowCount = 12

// expectedRuleHitCount pins how many times a RULE fires over the tree,
// exceptions ignored: 19 hits for the 12 accepted pairs, because several
// rules name the same crossing. The pair count cannot see a rule born MUTE
// — a rule whose only targets are pairs allowExceptions already holds is
// green the day it lands, with no edit to this file and nothing in review
// to read. This number moves instead.
const expectedRuleHitCount = 19

// underPrefix reports whether a module-relative package path IS prefix or
// lives below it. THE comparison of this guard: both arguments come from
// parseListing or from a rule, never from `go list` raw.
func underPrefix(pkg, prefix string) bool {
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

// inFamily reports whether pkg belongs to any of the families.
func inFamily(pkg string, prefixes []string) bool {
	for _, p := range prefixes {
		if underPrefix(pkg, p) {
			return true
		}
	}
	return false
}

// judgedTrees are the module trees this guard judges, as the one
// declaration of its scope: listIn asks `go list` for ./<tree>/...,
// walkNonTestFiles walks the same trees, and unjudgedImports reads an
// import that lands in one of them as a package the rules owe a verdict
// on. A tree added here is listed, walked and judged in the same edit; an
// import outside them — bots and connectors, read by pkg/cli, pkg/server
// and pkg/connection — is outside this map by that same decision, and no
// rule of layers names either.
var judgedTrees = []string{"pkg", "internal"}

// buildContext is one listing context: what `go list` is told about the
// platform and the build tags. The zero value is the host's own.
type buildContext struct {
	name string
	env  []string // GOOS/GOARCH overrides, appended to the test's environment
	tags []string // -tags values
}

// platformContexts are the platforms the guard lists. The tree's non-test
// files constrain themselves to four: linux, darwin and windows by name,
// and a fourth that is none of the three (pkg/authoringlock/platform_other.go,
// pkg/server/assistant_authoring_local_other.go and
// pkg/store/rename_noreplace_other.go read `!linux && !darwin && !windows`)
// — freebsd is such a platform, and it keeps `unix` true, so the unix half
// of the tree stays compiled there too. This list is not the guarantee:
// TestEveryNonTestFileIsSeenByAListingContext refuses a file no context
// compiles, which is the hole iterion#2829's review attacked with a
// _windows.go and a build tag.
var platformContexts = []buildContext{
	{name: "host"},
	{name: "windows", env: []string{"GOOS=windows"}},
	{name: "darwin", env: []string{"GOOS=darwin"}},
	{name: "freebsd", env: []string{"GOOS=freebsd"}},
}

// listContexts returns the contexts whose union the guard judges: one per
// platform above, plus one carrying every project build tag the tree's own
// non-test files ask for — read from those files by projectTags, never a
// list kept here.
func listContexts(root string) ([]buildContext, error) {
	tags, err := projectTags(root)
	if err != nil {
		return nil, err
	}
	ctxs := slices.Clone(platformContexts)
	if len(tags) > 0 {
		ctxs = append(ctxs, buildContext{name: "tags=" + strings.Join(tags, ","), tags: tags})
	}
	return ctxs, nil
}

// projectTags reads the build tags the tree asks of its own non-test files
// under pkg/ and internal/, minus the names the platform contexts already
// decide: their GOOS and GOARCH values, the host's, and `unix`, which the
// toolchain satisfies from the GOOS alone. What is left is this project's
// vocabulary (goldens_record), and it is read from the files rather than
// kept here so a tag landing tomorrow is listed the day it lands.
//
// A name that is neither — a platform this guard lists no context for — is
// passed as a tag: that widens what the listing sees, and a file it hides
// instead is what TestEveryNonTestFileIsSeenByAListingContext reports. The
// name the toolchain RESERVES never reaches a context: walkNonTestFiles
// hands out no file whose constraint NAMES `ignore`, bare or inside a
// conjunction, which is the one door that name had
// (TestTheGuardSkipsWhatTheToolchainNeverCompiles).
func projectTags(root string) ([]string, error) {
	decided := map[string]bool{"unix": true, runtime.GOOS: true, runtime.GOARCH: true}
	for _, c := range platformContexts {
		for _, kv := range c.env {
			if _, v, ok := strings.Cut(kv, "="); ok {
				decided[v] = true
			}
		}
	}
	tags := map[string]bool{}
	err := walkNonTestFiles(root, func(_ string, names []string) error {
		for _, name := range names {
			if !decided[name] {
				tags[name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(tags)), nil
}

// reservedIgnoreTag is the name the toolchain keeps for a file no build of
// its PACKAGE compiles — `//go:build ignore` over a `package main`
// generator, which `go generate` then compiles alone through `go run`.
// go/build spells that meaning with this very name: an empty `// +build`
// line, which never builds, parses to the tag `ignore`.
//
// Nothing sets the name, which is the whole point: forced as a -tags value
// it becomes an ordinary tag and the listing of the module dies of the
// TOOLCHAIN's own generators — `go list -tags ignore ./pkg/... ./internal/...`
// exited 1 on 2026-10-30, and all 38 of its file-level diagnostics name a
// file of GOROOT, none of this module ("found packages rand (auto_test.go)
// and main (gen_cooked.go)", `import "internal/goarch" is a program, not an
// importable package`). A generator of this module landing in its
// neighbour's package, which testdata/neverbuilt replays in isolation, is
// one more way in, not the reason
// (TestTheReservedIgnoreTagCannotBePassedToTheListing).
const reservedIgnoreTag = "ignore"

// buildConstraintsOf reads the build-constraint lines of a Go source —
// `//go:build` and the `// +build` go/build still honours — and answers two
// things about the file. Its names: every one the lines use, negated ones
// included — BOTH sides of every && and ||, or a tag of a conjunction asks
// for a context nobody lists. And whether any line NAMES
// reservedIgnoreTag, bare or inside a conjunction: that name cannot be
// passed as a tag at all, so the guard builds no context from the file and
// demands none for it. The expression is walked through its own node types
// rather than evaluated: Expr.Eval calls its callback "as needed to find
// out whether a given build tag is satisfied", which is a contract about
// the verdict, not about the names it visits.
//
// Every constraint-shaped line above the package clause is read, not only
// the one the toolchain honours: a name too many widens the listing, a name
// missed hides a file from the context that compiles it. A file whose
// constraint names `ignore` beside a term a listed context satisfies loses
// the coverage DEMAND, never the verdict — the rules read `go list`, which
// holds the file wherever it was compiled.
func buildConstraintsOf(src string) (names []string, namesReservedIgnore bool, err error) {
	for line := range strings.SplitSeq(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break // the toolchain reads no constraint past the package clause
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return nil, false, fmt.Errorf("unreadable build constraint %q: %w", line, err)
		}
		lineNames := tagsOf(expr)
		if slices.Contains(lineNames, reservedIgnoreTag) {
			namesReservedIgnore = true
		}
		names = append(names, lineNames...)
	}
	return names, namesReservedIgnore, nil
}

// tagsOf collects the tag names of a parsed build constraint.
func tagsOf(e constraint.Expr) []string {
	switch x := e.(type) {
	case *constraint.TagExpr:
		return []string{x.Tag}
	case *constraint.NotExpr:
		return tagsOf(x.X)
	case *constraint.AndExpr:
		return append(tagsOf(x.X), tagsOf(x.Y)...)
	case *constraint.OrExpr:
		return append(tagsOf(x.X), tagsOf(x.Y)...)
	}
	return nil
}

// droppedByName reports whether the go tool drops an entry of this name
// from a listing of a package pattern: go/build ignores a FILE whose name
// starts with `_` or `.` in every build context, and a pattern names no
// package inside a DIRECTORY so named. One convention, one predicate, both
// halves: honouring the directory half alone leaves the file half as a
// hole, which iterion#2829's third round measured.
func droppedByName(name string) bool {
	return strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// walkNonTestFiles hands fn every non-test .go file of root's judgedTrees
// that a listing of those trees can name, by its module-relative path, with
// the build-constraint names it carries. Its skips are the go tool's own
// conventions, and they cost the RULES nothing: this walk feeds the tag
// contexts and the coverage proof, never the layer rules, which read what
// `go list` answers.
//
//   - testdata/: the go tool lists no package there, so a fixture's build tag
//     would ask for a context no listing can use, and a fixture's file would
//     be reported as unseen for ever.
//   - a FILE named _x.go or .x.go (droppedByName): go/build drops that name
//     in every context, and nothing can import a file, so no build of a
//     package of this module compiles it — `go build ./...` leaves it out,
//     and only an explicit `go run _x.go` ever reads it. Nor is it a file
//     the repository forgets: TestNoGuardIsReadAroundByTheBuild
//     (pkg/dsl/unit) holds every Go file of the tree to a package of the
//     module, and a planted pkg/dsl/ir/_scratch.go is red there.
//   - a DIRECTORY named _x or .x (droppedByName): a pattern names no package
//     there, so no context can compile its files and "add the context that
//     compiles them" is a remedy nobody can carry out. Out of the listing is
//     not out of the build, though: such a package is importable, and
//     `go build ./...` ships it the moment a listed package imports it.
//     That edge is what TestEveryImportOfTheJudgedTreesIsAJudgedPackage
//     refuses, so skipping the files here buys no silence.
//   - a file whose constraint names the reserved `ignore` (see
//     reservedIgnoreTag): passing that name as a tag kills the listing of
//     the whole module, so every test of this guard would fatal over a tree
//     the toolchain builds clean.
func walkNonTestFiles(root string, fn func(rel string, tags []string) error) error {
	for _, tree := range judgedTrees {
		err := filepath.WalkDir(filepath.Join(root, tree), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if name == "testdata" || droppedByName(name) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || droppedByName(name) {
				return nil
			}
			src, err := os.ReadFile(p) // #nosec G304 -- a .go file of the tree the test walks
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			tags, namesReservedIgnore, err := buildConstraintsOf(string(src))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if namesReservedIgnore {
				return nil
			}
			return fn(rel, tags)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// listFormat is what `go list` is asked for: the package, its direct
// imports, and the files the context compiled — the imports feed the rules,
// the files feed the proof that every non-test file of the tree was read.
const listFormat = "{{.ImportPath}}|{{join .Imports \" \"}}|{{join .GoFiles \" \"}}|{{join .CgoFiles \" \"}}"

// listing is what the contexts answered: the module-relative direct imports
// of each package, and the module-relative path of every file they compiled.
type listing struct {
	imports map[string][]string
	files   map[string]bool
}

func newListing() listing {
	return listing{imports: map[string][]string{}, files: map[string]bool{}}
}

// merge unions other into l — one context's edges never erase another's.
func (l listing) merge(other listing) {
	for pkg, imps := range other.imports {
		l.imports[pkg] = append(l.imports[pkg], imps...)
	}
	for f := range other.files {
		l.files[f] = true
	}
}

// dedupe sorts and compacts every import list, so a pair unioned from two
// contexts is one pair and every reader gets the same order.
func (l listing) dedupe() {
	for pkg, imps := range l.imports {
		slices.Sort(imps)
		l.imports[pkg] = slices.Compact(imps)
	}
}

// parseListing turns what `go list -f listFormat` wrote into MODULE-RELATIVE
// pairs: the package and every import lose the module prefix here, once, and
// imports outside the module are dropped — they belong to no layer of this
// map. It also returns the lines it could NOT read.
//
// The trim is this seam's job, not each comparison's. A rule's sources and
// banned families are bare paths (pkg/dsl/ir); a full import path reaching
// underPrefix binds nothing and the map goes unenforced in silence, which
// is what iterion#2829 measured. No full path survives this function.
func parseListing(modulePrefix, out string) (listing, []string) {
	got := newListing()
	var rejected []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 4 || !strings.HasPrefix(fields[0], modulePrefix) {
			rejected = append(rejected, line)
			continue
		}
		pkg := strings.TrimPrefix(fields[0], modulePrefix)
		rel := []string{}
		for _, imp := range strings.Fields(fields[1]) {
			if !strings.HasPrefix(imp, modulePrefix) {
				continue
			}
			rel = append(rel, strings.TrimPrefix(imp, modulePrefix))
		}
		got.imports[pkg] = rel
		for _, f := range append(strings.Fields(fields[2]), strings.Fields(fields[3])...) {
			got.files[path.Join(pkg, f)] = true
		}
	}
	return got, rejected
}

// listIn runs the guard's producer in one context and reads what it wrote.
// stdout alone: `go list` writes diagnostics to stderr — on this machine
// every run carries one ("go: writing stat cache: ... read-only file
// system") — and a parser fed the two merged can lose a package's imports
// to a line that landed inside another. Every non-empty line of stdout must
// parse, and the context must name a package: a listing that answers
// nothing certifies nothing.
func listIn(root, modulePrefix string, env []string, ctx buildContext) (listing, error) {
	args := []string{"list"}
	if len(ctx.tags) > 0 {
		args = append(args, "-tags", strings.Join(ctx.tags, ","))
	}
	args = append(args, "-f", listFormat)
	for _, tree := range judgedTrees {
		args = append(args, "./"+tree+"/...")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(append(slices.Clone(os.Environ()), env...), ctx.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return listing{}, fmt.Errorf("go list in context %s (%v %v) failed: %w\nstderr:\n%s\na context that excludes every file of a package fails here: give the tag or the platform a context of its own", ctx.name, ctx.env, ctx.tags, err, stderr.String())
	}
	got, rejected := parseListing(modulePrefix, string(out))
	if len(rejected) > 0 {
		return listing{}, fmt.Errorf("go list in context %s wrote %d line(s) this parser cannot read — dropping one loses a package's imports in silence:\n%s", ctx.name, len(rejected), strings.Join(rejected, "\n"))
	}
	if len(got.imports) == 0 {
		return listing{}, fmt.Errorf("go list in context %s named no package of %s — the guard would certify an empty tree", ctx.name, modulePrefix)
	}
	return got, nil
}

// depsOf returns the module-relative transitive closure of one package of
// root, the reading the one-hop sentence is measured AGAINST: `go list
// -deps` is what publishes a transitive dependency, and the guard's own
// listing stops at one hop by decision. stdout alone, for listIn's reason.
func depsOf(root, pkg string) ([]string, error) {
	prefix, err := modulePrefixOf(root)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./"+pkg)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps ./%s failed: %w\nstderr:\n%s", pkg, err, stderr.String())
	}
	var deps []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if rel, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			deps = append(deps, rel)
		}
	}
	if len(deps) == 0 {
		return nil, fmt.Errorf("go list -deps ./%s named no package of %s — an empty closure certifies nothing", pkg, prefix)
	}
	return deps, nil
}

// treeUnion lists root in every context listContexts names and unions what
// they answer, so a file one platform or one build tag compiles is judged
// by the same rules as the host's. It returns the union and the names of
// the contexts that fed it.
func treeUnion(root, modulePrefix string, env []string) (listing, []string, error) {
	ctxs, err := listContexts(root)
	if err != nil {
		return listing{}, nil, err
	}
	union := newListing()
	names := make([]string, 0, len(ctxs))
	for _, ctx := range ctxs {
		got, err := listIn(root, modulePrefix, env, ctx)
		if err != nil {
			return listing{}, nil, err
		}
		union.merge(got)
		names = append(names, ctx.name)
	}
	union.dedupe()
	return union, names, nil
}

var (
	treeOnce     sync.Once
	treeUnioned  listing
	treeContexts []string
	treeErr      error
)

// treeListing lists this module once per test binary and hands every test
// the same union — five `go list` runs over 213 packages, 4.2 s measured
// over five runs of the package, paid once per binary instead of once per
// test.
func treeListing(t *testing.T) listing {
	t.Helper()
	root := repoRoot(t)
	treeOnce.Do(func() {
		prefix, err := modulePrefixOf(root)
		if err != nil {
			treeErr = err
			return
		}
		treeUnioned, treeContexts, treeErr = treeUnion(root, prefix, nil)
	})
	if treeErr != nil {
		t.Fatal(treeErr)
	}
	return treeUnioned
}

func importsPairs(t *testing.T) map[string][]string {
	t.Helper()
	return treeListing(t).imports
}

// eachViolation walks the rules over module-relative pairs — sorted, so
// every reader gets the same order — and hands each rule hit to visit as
// (rule name, "<source> -> <import>"). THE evaluation: violationsOf and
// violatingPairs are its two readers, so neither reconstructs a verdict by
// re-parsing the other's message.
func eachViolation(rules []layerRule, pairs map[string][]string, visit func(rule, key string)) {
	for _, l := range rules {
		for _, pkg := range slices.Sorted(maps.Keys(pairs)) {
			if !inFamily(pkg, l.sources) {
				continue
			}
			for _, imp := range pairs[pkg] {
				for _, b := range l.banned {
					if !underPrefix(imp, b) {
						continue
					}
					// A rule forbids CROSSING into a family; a package of
					// that family importing its own family is cohesion, not
					// a layer crossing (pkg/server reads pkg/server/projects).
					if underPrefix(pkg, b) {
						continue
					}
					visit(l.name, pkg+" -> "+imp)
				}
			}
		}
	}
}

// violationsOf returns one line per rule hit, "<rule>: <source> -> <import>",
// minus the pairs accepted holds. accepted may be nil — the raw verdict.
func violationsOf(rules []layerRule, pairs map[string][]string, accepted map[string]string) []string {
	violations := []string{}
	eachViolation(rules, pairs, func(rule, key string) {
		if _, ok := accepted[key]; ok {
			return
		}
		violations = append(violations, rule+": "+key)
	})
	return violations
}

// violatingPairs is the set of "source -> import" keys the rules name,
// exceptions ignored — what allowExceptions is allowed to hold.
func violatingPairs(rules []layerRule, pairs map[string][]string) map[string]bool {
	keys := map[string]bool{}
	eachViolation(rules, pairs, func(_, key string) { keys[key] = true })
	return keys
}

// unjudgedImports returns the "<source> -> <import>" edges whose IMPORT
// lands inside judgedTrees and names a package the listing does not hold —
// sorted, so every reader gets the same order. Such a package has no entry
// of its own in pairs, so every rule that would bind it reads nothing: what
// IT imports crosses no layer anybody checks, and the file that holds the
// crossing ships in the binary all the same. This is the other end of
// walkNonTestFiles' `_x`/`.x` directory skip — the skip spares an
// unclearable red, this names the import that skip must not turn into
// silence.
func unjudgedImports(pairs map[string][]string) []string {
	unjudged := []string{}
	for _, pkg := range slices.Sorted(maps.Keys(pairs)) {
		for _, imp := range pairs[pkg] {
			if !inFamily(imp, judgedTrees) {
				continue
			}
			if _, listed := pairs[imp]; !listed {
				unjudged = append(unjudged, pkg+" -> "+imp)
			}
		}
	}
	return unjudged
}

func TestThePackageLayersHold(t *testing.T) {
	pairs := importsPairs(t)
	if hits := violationsOf(layers, pairs, nil); len(hits) != expectedRuleHitCount {
		t.Errorf("the rules fire %d time(s) over the tree, want the pinned %d — a crossing appeared or left, or a rule was born mute over pairs allowExceptions already holds:\n%s", len(hits), expectedRuleHitCount, strings.Join(hits, "\n"))
	}
	if len(allowExceptions) != expectedAllowCount {
		t.Errorf("allowExceptions holds %d entries, want the pinned %d — an exception leaves only by shrinking this list", len(allowExceptions), expectedAllowCount)
	}
	if violations := violationsOf(layers, pairs, allowExceptions); len(violations) != 0 {
		t.Errorf("the layer map is violated %d time(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestEveryImportOfTheJudgedTreesIsAJudgedPackage demands that the map
// reach the far end of every edge it is handed: an import under pkg/ or
// internal/ that no listing context names is a package whose own imports
// no rule reads, while `go build ./...` compiles it. The go tool makes that
// shape one rename away — a `pkg/dsl/ir/_old/old.go` importing pkg/server
// is outside `./pkg/...` and inside the binary, and the layer rules name no
// hit over it in either direction.
//
// Measured 2026-10-30 over this module: 213 listed packages, 971
// module-internal edges, and the only imports no pattern names are `bots`
// and `connectors` — two top-level trees outside judgedTrees, so outside
// this map by the decision judgedTrees carries.
func TestEveryImportOfTheJudgedTreesIsAJudgedPackage(t *testing.T) {
	if unjudged := unjudgedImports(importsPairs(t)); len(unjudged) > 0 {
		t.Errorf("%d import(s) of %v name a package no listing context holds, so no rule reads what THEY import:\n%s\nrename the directory out of the go tool's `_x`/`.x` convention so a pattern names it, or give the listing a pattern that does",
			len(unjudged), judgedTrees, strings.Join(unjudged, "\n"))
	}
}

// TestTheGuardBindsAPackageByItsModuleRelativePath drives the real seam
// with the shape `go list` really emits — full import paths on BOTH sides —
// and demands the rule name the pair. Trim the module prefix on one side
// only, the way this file read before iterion#2829, and the guard binds
// nothing: it reports zero violations over a tree that holds one, and this
// test is the only thing that says so.
func TestTheGuardBindsAPackageByItsModuleRelativePath(t *testing.T) {
	prefix, err := modulePrefixOf(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	raw := prefix + "pkg/dsl/ir|fmt " + prefix + "pkg/server github.com/spf13/cobra|ir.go|\n" +
		prefix + "pkg/log|fmt|log.go|\n"
	pairs, rejected := parseListing(prefix, raw)
	if len(rejected) != 0 {
		t.Fatalf("parseListing rejected %d line(s) of the shape go list emits: %q", len(rejected), rejected)
	}
	if got := pairs.imports["pkg/dsl/ir"]; !slices.Equal(got, []string{"pkg/server"}) {
		t.Fatalf("parseListing(pkg/dsl/ir) = %q, want the module-relative [pkg/server] with stdlib and third-party dropped", got)
	}
	if !pairs.files["pkg/dsl/ir/ir.go"] {
		t.Fatalf("parseListing kept no file for pkg/dsl/ir: %v — the files are what proves every context was read", slices.Sorted(maps.Keys(pairs.files)))
	}
	got := violationsOf(layers, pairs.imports, nil)
	want := []string{
		layers[0].name + ": pkg/dsl/ir -> pkg/server",
		layers[1].name + ": pkg/dsl/ir -> pkg/server",
		layers[2].name + ": pkg/dsl/ir -> pkg/server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the guard named %d violation(s) for pkg/dsl/ir -> pkg/server:\n%s\nwant the three rules that bind it:\n%s",
			len(got), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheParserRefusesALineItCannotRead: a line the parser steps over is a
// package's imports nobody judges. `go list` writes its diagnostics to
// stderr, the guard used to read stderr and stdout merged, and a line that
// landed mid-stream would have been dropped in silence — only the twelve
// accepted pairs would have noticed, by going stale.
func TestTheParserRefusesALineItCannotRead(t *testing.T) {
	const prefix = "example.com/m/"
	diagnostic := "go: writing stat cache: open /nowhere/.tmp: read-only file system"
	out := prefix + "pkg/log|fmt|log.go|\n" + diagnostic + "\n" + prefix + "pkg/store|" + prefix + "pkg/log|store.go|\n"
	got, rejected := parseListing(prefix, out)
	if !slices.Equal(rejected, []string{diagnostic}) {
		t.Errorf("parseListing reported %q as unreadable, want exactly the diagnostic line %q", rejected, diagnostic)
	}
	if len(got.imports) != 2 {
		t.Errorf("parseListing read %d package(s) around the diagnostic, want 2", len(got.imports))
	}
}

// TestARuleDoesNotForbidAFamilyFromImportingItself fixes the other half of
// the verdict: "no pkg package imports the composition root" is about
// crossing INTO pkg/server, so pkg/server reading pkg/server/projects is
// cohesion. Drop the intra-family skip and the composition root is reported
// as violating the rule that describes it.
func TestARuleDoesNotForbidAFamilyFromImportingItself(t *testing.T) {
	pairs := map[string][]string{
		"pkg/server":  {"pkg/server/projects", "pkg/server/cloudpublisher"},
		"pkg/runtime": {"pkg/server"},
	}
	got := violationsOf(layers, pairs, nil)
	want := []string{
		layers[2].name + ": pkg/runtime -> pkg/server",
		layers[3].name + ": pkg/runtime -> pkg/server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("verdict on the intra-family fixture:\n%s\nwant only the crossing from outside pkg/server:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheGuardJudgesDirectImportsOnly executes the bound docs/layering.md
// publishes with the table: a rule reads the DIRECT imports of a package,
// so a banned family reached through a third package is no rule hit — a
// reader who takes "is a leaf" for a transitive contract reads a promise
// this guard does not make. What the tree pays for that today is measured,
// not restated here: theLeafClosureOnly and
// TestTheLeafsOneHopReadingIsWhatThePageSays (iterion#2857).
func TestTheGuardJudgesDirectImportsOnly(t *testing.T) {
	pairs := map[string][]string{
		"pkg/dsl/ir":     {"pkg/vocabulary"},
		"pkg/vocabulary": {"pkg/forge", "pkg/runner"},
	}
	if got := violationsOf(layers, pairs, nil); len(got) != 0 {
		t.Errorf("the guard named %d hit(s) on a transitive reach:\n%s\nwant none — pkg/dsl/ir imports pkg/vocabulary, and only that", len(got), strings.Join(got, "\n"))
	}
	direct := map[string][]string{"pkg/dsl/ir": {"pkg/forge"}}
	if got := violationsOf(layers, direct, nil); len(got) != 1 {
		t.Errorf("the same edge taken DIRECTLY named %d hit(s), want the one rule that bans pkg/forge: %v", len(got), got)
	}
}

// theLeafsAcceptedDispatcherImports are the pkg/dispatcher packages
// pkg/dsl/ir imports DIRECTLY, each one held by allowExceptions with its
// reason. ONE declaration: the tree is faced with this list and the page's
// sentence is spelled from it, so accepting a second import is a single
// edit a reviewer sees. Two copies would let the page keep saying "only"
// about one of them with the test green.
var theLeafsAcceptedDispatcherImports = []string{"pkg/dispatcher/native/boardops"}

// theLeafOneHopSentence is what docs/layering.md publishes about the one
// hop, in the page's own words minus its code ticks, with the accepted
// imports spelled from theLeafsAcceptedDispatcherImports. The fact lives on
// the page once and this guard faces it with the tree — the reading it
// replaces ("its direct imports name no pkg/forge and no pkg/dispatcher
// package") was false the day it landed: pkg/dsl/ir imports
// pkg/dispatcher/native/boardops DIRECTLY, which is the very pair
// allowExceptions accepts below.
func theLeafOneHopSentence() string {
	return "its direct imports name no pkg/forge package and, of the pkg/dispatcher family, only the accepted " + andList(theLeafsAcceptedDispatcherImports)
}

// theLeafClosureOnly are the packages of the leaf rule's banned families
// that `go list -deps ./pkg/dsl/ir` holds and its direct imports do not —
// the other half of the same bullet, and the weight filed as iterion#2857.
// When that weight is lifted the measurement moves, this list and the page's
// sentence are edited together, and a reviewer sees both.
var theLeafClosureOnly = []string{
	"pkg/backend/tool/privacy/detector",
	"pkg/dispatcher/native",
	"pkg/dispatcher/tracker",
	"pkg/forge",
}

// proseText normalizes a page, or a sentence, to what it SAYS: markdown code
// ticks off, every run of whitespace one space. The line breaks a page needs
// for its width then hide nothing from a comparison.
func proseText(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ")
}

// andList spells a list the way a page does: "a, b and c".
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// TestTheLeafsOneHopReadingIsWhatThePageSays executes the bullet
// docs/layering.md publishes about the one hop, both halves, against the
// union the rules are judged on. A page that says the leaf's direct imports
// name no pkg/dispatcher package tells a reader of iterion#2857 that the
// coupling is transitive, when seven boardops.CapBoard* constants are one
// hop away — and the page is where #2829 ends, so a sentence on it is a
// claim this guard owes a verdict on, not prose.
func TestTheLeafsOneHopReadingIsWhatThePageSays(t *testing.T) {
	const leaf = "pkg/dsl/ir"
	leafRule := layers[0]
	if !slices.Equal(leafRule.sources, []string{leaf}) {
		t.Fatalf("layers[0] binds %q, not %s — this test reads the banned families off that rule", leafRule.sources, leaf)
	}
	direct := treeListing(t).imports[leaf]
	if len(direct) == 0 {
		t.Fatalf("the union named no import of %s — the sentence below would be about nothing", leaf)
	}
	var forge, dispatcher []string
	for _, imp := range direct {
		if underPrefix(imp, "pkg/forge") {
			forge = append(forge, imp)
		}
		if underPrefix(imp, "pkg/dispatcher") {
			dispatcher = append(dispatcher, imp)
		}
	}
	if len(forge) != 0 {
		t.Errorf("%s imports %v one hop away, and the page says it names no pkg/forge package: accept the pair with its reason and edit the sentence, or drop the import", leaf, forge)
	}
	accepted := theLeafsAcceptedDispatcherImports
	if !slices.Equal(dispatcher, accepted) {
		t.Errorf("%s's direct pkg/dispatcher imports are %v, want exactly %v — the page's sentence is spelled from that list and names no other", leaf, dispatcher, accepted)
	}
	for _, imp := range dispatcher {
		if _, ok := allowExceptions[leaf+" -> "+imp]; !ok {
			t.Errorf("%s -> %s is a direct crossing allowExceptions does not hold, and the page calls it accepted", leaf, imp)
		}
	}
	closure, err := depsOf(repoRoot(t), leaf)
	if err != nil {
		t.Fatal(err)
	}
	var closureOnly []string
	for _, pkg := range closure {
		if pkg != leaf && inFamily(pkg, leafRule.banned) && !slices.Contains(direct, pkg) {
			closureOnly = append(closureOnly, pkg)
		}
	}
	slices.Sort(closureOnly)
	if !slices.Equal(closureOnly, theLeafClosureOnly) {
		t.Errorf("go list -deps ./%s holds %v of the rule's banned families that its direct imports do not name, want the pinned %v — iterion#2857 moved: edit this list and the page's sentence with it", leaf, closureOnly, theLeafClosureOnly)
	}
	page, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "layering.md")) // #nosec G304 -- the page of the module the test climbed to
	if err != nil {
		t.Fatal(err)
	}
	said := proseText(string(page))
	if sentence := theLeafOneHopSentence(); !strings.Contains(said, proseText(sentence)) {
		t.Errorf("docs/layering.md does not carry the sentence this guard executes:\n%s\nthe page and the tree owe the same reading of the one hop (iterion#2829)", sentence)
	}
	if list := andList(theLeafClosureOnly); !strings.Contains(said, proseText(list)) {
		t.Errorf("docs/layering.md does not name the closure the measurement holds:\n%s", list)
	}
}

// TestEveryLayerRuleBindsAPackageOfTheTree: a prefix that binds no package
// certifies nothing. This is the property the asymmetric comparison broke
// for every rule at once, silently, and the one a future refactor breaks
// for a single clause by renaming a package family out from under it —
// BOTH sides, because a banned family is where seven of the eight clauses
// of the first rule live, and a rename there leaves a rule that cannot bite.
func TestEveryLayerRuleBindsAPackageOfTheTree(t *testing.T) {
	pairs := importsPairs(t)
	for _, l := range layers {
		for _, side := range []struct {
			what     string
			prefixes []string
		}{{"source", l.sources}, {"banned", l.banned}} {
			for _, prefix := range side.prefixes {
				bound := 0
				for pkg := range pairs {
					if underPrefix(pkg, prefix) {
						bound++
					}
				}
				if bound == 0 {
					t.Errorf("rule %q names %s family %q, which no package of %v is under — that clause checks nothing", l.name, side.what, prefix, judgedTrees)
				}
			}
		}
	}
}

// TestEveryAcceptedExceptionIsALiveViolation keeps the ratchet from rotting
// the other way: an entry whose import is gone, or whose rule no longer
// names it, inflates expectedAllowCount and buys silence for a pair nobody
// reviewed.
func TestEveryAcceptedExceptionIsALiveViolation(t *testing.T) {
	live := violatingPairs(layers, importsPairs(t))
	for _, key := range slices.Sorted(maps.Keys(allowExceptions)) {
		if !live[key] {
			t.Errorf("allowExceptions accepts %q, which no rule names on today's tree — drop the entry and lower expectedAllowCount", key)
		}
	}
	if len(allowExceptions) != len(live) {
		t.Logf("%d accepted exception(s) for %d violating pair(s) — the difference is what TestThePackageLayersHold reports", len(allowExceptions), len(live))
	}
}

// unseenFiles walks root's non-test files and reports the ones seen does
// not hold — compiled by no listing context — with the number walked, so a
// caller can say how many WERE compiled. THE reading of coverage: the tree
// guard and the fixture guard call this one, neither re-walks the tree with
// its own idea of what counts.
func unseenFiles(root string, seen map[string]bool) (unseen []string, total int, err error) {
	err = walkNonTestFiles(root, func(rel string, _ []string) error {
		total++
		if !seen[rel] {
			unseen = append(unseen, rel)
		}
		return nil
	})
	return unseen, total, err
}

// coverageLine is what the coverage guard may claim about its own reach: the
// files a context compiled, which is the walk MINUS the unseen.
func coverageLine(total int, unseen []string, contexts []string) string {
	return fmt.Sprintf("%d of %d non-test files seen across %d contexts (%s)", total-len(unseen), total, len(contexts), strings.Join(contexts, ", "))
}

// TestEveryNonTestFileIsSeenByAListingContext executes the scope sentence
// of docs/layering.md: the contexts listContexts names compile every
// non-test Go file under pkg/ and internal/, so "walks every non-test
// import" is a measurement. A file no context compiles is imports no rule
// ever reads — iterion#2829's review drove a _windows.go and a build-tagged
// file through that hole, green under linux, red under GOOS=windows on the
// same tree and the same binary.
func TestEveryNonTestFileIsSeenByAListingContext(t *testing.T) {
	unseen, total, err := unseenFiles(repoRoot(t), treeListing(t).files)
	if err != nil {
		t.Fatal(err)
	}
	if len(unseen) > 0 {
		t.Errorf("%d of the %d non-test files under pkg/ and internal/ are compiled by none of the listed contexts (%s) — add the context that compiles them, or their imports are judged by nothing:\n%s",
			len(unseen), total, strings.Join(treeContexts, ", "), strings.Join(unseen, "\n"))
	}
	t.Log(coverageLine(total, unseen, treeContexts))
}

// TestTheCoverageLineCountsOnlyWhatAContextCompiled pins the only count the
// guard above may claim. Logging the files WALKED as "seen" had it answer,
// two lines under a verdict naming three files no context compiles, "1704
// non-test files seen across 3 contexts" — a line that contradicts the
// verdict it accompanies, in a guard whose whole subject is an honest count.
func TestTheCoverageLineCountsOnlyWhatAContextCompiled(t *testing.T) {
	got := coverageLine(3, []string{"pkg/a/_old/old.go"}, []string{"host", "windows"})
	const want = "2 of 3 non-test files seen across 2 contexts (host, windows)"
	if got != want {
		t.Errorf("coverageLine(3 walked, 1 unseen, 2 contexts) = %q, want %q — the files a context compiled, never the files the walk handed out", got, want)
	}
}

// TestAWindowsOnlyAndATagOnlyCrossingAreNamed replays iterion#2829's review
// attacks hermetically. testdata/crossbuild is a module of its own whose
// pkg/store/lock_windows.go imports pkg/cli and whose pkg/dsl/ir/tagged.go,
// behind a build tag no file of this repository carries, imports pkg/server:
// both are non-test files, both violate a rule, and the host context alone
// sees neither. The union names all four rule hits — and the tags it needed
// came out of the fixture's own files, one of them from behind an `&&`,
// which is why neither a hard-coded tag list nor a collector that reads one
// side of a conjunction can answer this test.
func TestAWindowsOnlyAndATagOnlyCrossingAreNamed(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "crossbuild"))
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := modulePrefixOf(root)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is its own module: GOWORK=off and an empty GOFLAGS keep an
	// operator's workspace or -mod=vendor from reaching into it.
	hermetic := []string{"GOWORK=off", "GOFLAGS="}
	if tags, err := projectTags(root); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(tags, []string{"crossbuildonly", "crossbuildpair"}) {
		t.Fatalf("projectTags(fixture) = %q, want [crossbuildonly crossbuildpair]: the fixture's //go:build linux is a platform the contexts cover, its own two tags are not, and the second lives behind an && that a one-sided collector never reads", tags)
	}
	host, err := listIn(root, prefix, hermetic, buildContext{name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if got := violationsOf(layers, host.imports, nil); len(got) != 0 {
		t.Errorf("the host context alone named %d hit(s) over the fixture:\n%s\nthe fixture is worthless unless a single-context guard calls it clean", len(got), strings.Join(got, "\n"))
	}
	union, names, err := treeUnion(root, prefix, hermetic)
	if err != nil {
		t.Fatal(err)
	}
	got := violationsOf(layers, union.imports, nil)
	want := []string{
		layers[0].name + ": pkg/dsl/ir -> pkg/server",
		layers[1].name + ": pkg/dsl/ir -> pkg/server",
		layers[2].name + ": pkg/dsl/ir -> pkg/server",
		layers[7].name + ": pkg/store -> pkg/cli",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the union of %v named:\n%s\nwant the windows-only and the tag-only crossing:\n%s", names, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheGuardSkipsWhatTheToolchainNeverCompiles replays, on a module of
// its own, every shape the walk skips but `testdata/` itself — which is
// where this fixture lives, so the tree's own 1704-file count proves that
// one. Here: two `//go:build ignore` generators (one bare, one behind an
// `&&`), a `_old/` and a `.hidden/` directory, and a `_scratch.go` and a
// `.scratch.go` file. Walked as ordinary files they break this guard two
// ways at once, and the fixture keeps the halves apart — only the
// generators carry a constraint, so the tag assertion and the coverage
// assertion cannot cover for each other.
//
// Either tag, forced into a -tags context, stops being reserved and the
// listing of the whole module dies (see reservedIgnoreTag), so EVERY test
// that lists the tree fatals on the error listIn returns — over a tree
// `go build ./...` compiles clean. The directories and the dropped file
// names would be reported as compiled by no context, with "add the context
// that compiles them" for a remedy no context can satisfy. Either red would
// be the guard's own, and iterion is one idiomatic generator away from
// both.
//
// The other end of the directory skip is here too: package a imports both
// directories, so the fixture holds the edges unjudgedImports must name. A
// package out of the listing is in the build the moment something imports
// it, and `_scratch.go` is the shape where that cannot happen — no import
// names a file.
func TestTheGuardSkipsWhatTheToolchainNeverCompiles(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "neverbuilt"))
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := modulePrefixOf(root)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is its own module: GOWORK=off and an empty GOFLAGS keep an
	// operator's workspace or -mod=vendor from reaching into it.
	hermetic := []string{"GOWORK=off", "GOFLAGS="}
	tags, err := projectTags(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatalf("projectTags(fixture) = %q, want none: `ignore` is the toolchain's reserved name, and the two generators that carry it — one bare, one behind an && — are the fixture's only constrained files", tags)
	}
	union, names, err := treeUnion(root, prefix, hermetic)
	if err != nil {
		t.Fatalf("listing the fixture failed, which is how a reserved tag fatals every test of this guard: %v", err)
	}
	unseen, total, err := unseenFiles(root, union.files)
	if err != nil {
		t.Fatal(err)
	}
	if len(unseen) != 0 {
		t.Errorf("the guard demands a listing context for %d file(s) of the fixture that no context of %v can compile:\n%s", len(unseen), names, strings.Join(unseen, "\n"))
	}
	if want := 2; total != want {
		t.Errorf("the walk handed out %d non-test file(s) of the fixture, want the %d the toolchain compiles (pkg/a/a.go and internal/b/b.go)", total, want)
	}
	wantUnjudged := []string{"pkg/a -> pkg/a/.hidden", "pkg/a -> pkg/a/_old"}
	if got := unjudgedImports(union.imports); !slices.Equal(got, wantUnjudged) {
		t.Errorf("the guard names %v as imports no listing judges, want %v — package a imports both directories the walk skips, and the skip must cost the alarm nothing", got, wantUnjudged)
	}
	t.Log(coverageLine(total, unseen, names))
}

// TestTheReservedIgnoreTagCannotBePassedToTheListing faces THIS module
// with the reason docs/layering.md and reservedIgnoreTag give for dropping
// a file that names `ignore`. testdata/neverbuilt proves the SITE — a
// generator of the module under test landing in its neighbour's package —
// and a reader could take that collision for the whole story. The
// guarantee is wider: the toolchain's own tree carries `//go:build ignore`
// generators, and the listing dies on those. Measured 2026-10-30: 592
// stderr lines, the 38 file-level diagnostics among them all naming a file
// of GOROOT and none a file of this module, 0.5 s per run.
//
// Red here means the page owes a new reason, not that the guard is wrong:
// the skip itself is proved by the fixture, in isolation.
func TestTheReservedIgnoreTagCannotBePassedToTheListing(t *testing.T) {
	root := repoRoot(t)
	prefix, err := modulePrefixOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listIn(root, prefix, nil, buildContext{name: "tags=" + reservedIgnoreTag, tags: []string{reservedIgnoreTag}}); err == nil {
		t.Fatalf("`go list -tags %s` listed this module and exited 0 — the name is no longer reserved, so walkNonTestFiles could hand a file carrying it to a context instead of dropping it, and the page's reason is stale", reservedIgnoreTag)
	} else {
		goroot := exec.Command("go", "env", "GOROOT")
		goroot.Dir = root
		out, gerr := goroot.Output()
		if gerr != nil {
			t.Fatalf("go env GOROOT failed, so the cause of the failure above cannot be located: %v", gerr)
		}
		if g := strings.TrimSpace(string(out)); !strings.Contains(err.Error(), g) {
			t.Errorf("the listing failed, but no diagnostic names GOROOT (%s) — the page says the toolchain's OWN generators carry `%s`, so that sentence is owed a fresh measurement:\n%v", g, reservedIgnoreTag, err)
		}
	}
}

// TestTheConstraintReaderNamesTheReservedIgnoreWhereverItSits reads the
// shapes `go/build` honours and this guard must agree with, at the one
// function that reads them. The composed form is the live defect of
// iterion#2829's third round: recognised by the bare `*constraint.TagExpr`
// alone, a `//go:build ignore && linux` file puts `ignore` in projectTags
// and the listing of the whole module dies.
//
// The old `// +build` half is a string here, not a fixture: gofmt adds the
// `//go:build` line to a file that carries only the old one, so
// `gofmt -l` — a gate of this repository — refuses such a file anywhere in
// the tree. `go/build` honours it all the same, which is the agreement this
// function owes, and the empty `// +build` line is where the toolchain
// itself spells "never built" as the tag `ignore`.
func TestTheConstraintReaderNamesTheReservedIgnoreWhereverItSits(t *testing.T) {
	for _, c := range []struct {
		name       string
		src        string
		wantNames  []string
		wantIgnore bool
	}{
		{"bare go:build ignore", "//go:build ignore\n\npackage main\n", []string{"ignore"}, true},
		{"ignore inside a conjunction", "//go:build ignore && linux\n\npackage main\n", []string{"ignore", "linux"}, true},
		{"old-style +build ignore", "// +build ignore\n\npackage main\n", []string{"ignore"}, true},
		{"old-style comma conjunction", "// +build ignore,!windows\n\npackage main\n", []string{"ignore", "windows"}, true},
		{"old-style ordinary tag", "// +build goldens_record\n\npackage a\n", []string{"goldens_record"}, false},
		{"empty +build line, the toolchain's own `ignore`", "// +build\n\npackage a\n", []string{"ignore"}, true},
		{"a tag named after the reserved one is not it", "//go:build ignoreme\n\npackage a\n", []string{"ignoreme"}, false},
		{"prose naming the form is no constraint", "// a generator carries // +build ignore\n\npackage a\n", nil, false},
		{"past the package clause nothing is read", "package a\n\n//go:build ignore\n", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			names, namesIgnore, err := buildConstraintsOf(c.src)
			if err != nil {
				t.Fatalf("buildConstraintsOf(%q) failed: %v", c.src, err)
			}
			if !slices.Equal(names, c.wantNames) {
				t.Errorf("names = %q, want %q", names, c.wantNames)
			}
			if namesIgnore != c.wantIgnore {
				t.Errorf("namesReservedIgnore = %v, want %v — the file is dropped from the walk on the NAME, wherever the expression puts it", namesIgnore, c.wantIgnore)
			}
		})
	}
}
