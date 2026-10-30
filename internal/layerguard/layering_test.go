package layerguard

import (
	"bytes"
	"fmt"
	"go/build"
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
// judgedTrees — ./pkg/... and ./internal/... — read off a listing of the
// WHOLE module (listPatterns), unioned over every context treeUnion names
// (the host, GOOS=windows, GOOS=darwin, GOOS=freebsd, and the build tags
// the uncompiled files of the tree still demand). A family reached THROUGH
// another package is not a rule hit — what pkg/dsl/ir's direct imports
// really name, against what only its closure holds (iterion#2857), is one
// sentence of docs/layering.md rather than a second copy of the fact here,
// and TestTheLeafsOneHopReadingIsWhatThePageSays faces that sentence with
// the tree. A coupling that exists only in a _test.go file is out of scope
// by decision. docs/layering.md states the same bounds, one bullet each,
// and theBoundsThePageStates is the list both this godoc and
// TestThePageStatesTheBoundsTheGuardClaims read — never a count repeated
// here, which is how "the same three bounds" survived the page growing a
// fourth. Each bound has its verdict: the contexts are judged by
// TestEveryNonTestFileIsSeenByAListingContext, who decides what a build
// compiles by TestTheGuardAndTheGoToolReadTheSameFiles, the far end of an
// edge by TestEveryImportOfTheJudgedTreesIsAJudgedPackage, the one hop by
// TestTheGuardJudgesDirectImportsOnly.
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
// declaration of its scope: walkNonTestFiles walks them, and
// unjudgedImports reads an import that lands in one of them as a package
// the rules owe a verdict on. A tree added here is walked and judged in the
// same edit; an import outside them — bots and connectors, read by pkg/cli,
// pkg/server and pkg/connection — is outside this map by that same
// decision, and no rule of layers names either.
var judgedTrees = []string{"pkg", "internal"}

// listPatterns is what every listing context is given: the WHOLE module,
// not judgedTrees. The rules bind packages of judgedTrees, but an import
// INTO them comes from anywhere, and cmd/ — the composition root, the one
// tree the doctrine says imports in depth — is named by no pattern of
// judgedTrees. Listing the module instead feeds unjudgedImports the far end
// of every edge the module has, from the SAME listing the rules read, with
// no second source of truth to drift. Measured 2026-10-30: 230 packages
// against the 213 of `./pkg/... ./internal/...`, 1061 module-internal edges
// against 974, and not one rule hit more (the sources of every rule are
// paths of judgedTrees). The cost is a bound this guard carries: every
// package of the module must be listable in every context it judges, or
// listIn fails and says which context excluded a whole package.
var listPatterns = []string{"./..."}

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

// tagsDemandedBy returns the build tags the tree still asks for once the
// platform contexts have answered: for every non-test file of judgedTrees
// the go tool NAMED and none of those contexts compiled, the names its own
// build constraints carry, minus the names a platform context already
// decides — their GOOS and GOARCH values, the host's, and `unix`, which the
// toolchain satisfies from the GOOS alone — and minus the one name no
// listing may be given (reservedIgnoreTag). What is left is this project's
// vocabulary (goldens_record today), read off the files rather than kept
// here, so a tag landing tomorrow is listed the day it lands.
//
// DEMAND-driven, which is what keeps it honest at both ends. A file the go
// tool names in no listing — a fixture under testdata/, a file go/build
// drops by its name — demands nothing, so a module never carries its
// fixtures' private tags into its own listing. And a file the tool DOES
// name and no context compiles gets its tags whatever the expression puts
// around them: `//go:build ignore || mytag` demands mytag, which is the
// crossing iterion#2829's fourth round lost by dropping the file on the
// name instead of asking the go tool what it compiles.
//
// A name that is neither decided nor reserved — a platform this guard lists
// no context for — is passed as a tag: that widens what the listing sees,
// and a file still compiled by nothing is what
// TestEveryNonTestFileIsSeenByAListingContext reports.
func tagsDemandedBy(root string, platform listing) ([]string, error) {
	decided := map[string]bool{"unix": true, runtime.GOOS: true, runtime.GOARCH: true, reservedIgnoreTag: true}
	for _, c := range platformContexts {
		for _, kv := range c.env {
			if _, v, ok := strings.Cut(kv, "="); ok {
				decided[v] = true
			}
		}
	}
	tags := map[string]bool{}
	err := walkNonTestFiles(root, func(rel string, names []string) error {
		if !platform.known[rel] || platform.files[rel] {
			return nil // the go tool names it in no listing, or a context compiled it
		}
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
// No context of this guard ever carries it, which is the whole point:
// forced as a -tags value it becomes an ordinary tag and the listing of the
// module dies of the TOOLCHAIN's own generators —
// `go list -tags ignore ./...` exited 1 on 2026-10-30 with 38 file-level
// diagnostics, every one of them about a package of GOROOT ("found packages
// rand (auto_test.go) and main (gen_cooked.go) in $GOROOT/src/math/rand",
// `import "internal/goarch" is a program, not an importable package`); 36
// are anchored at a GOROOT file and 2 at the import site that reached it
// (vendor/… and internal/docsguard/diagrams.go), which is still GOROOT's
// package failing, not this module's. A generator of this module landing in
// its neighbour's package, which testdata/neverbuilt replays in isolation,
// is one more way in, not the reason
// (TestTheReservedIgnoreTagCannotBePassedToTheListing).
//
// So it is also the one name that can earn a file an exemption from the
// coverage demand — and the exemption is go/build's answer, not this file's
// reading of the expression: theReservedNameKeepsItOut asks MatchFile
// whether the file matches no context the guard judges and matches one the
// moment this name is satisfied. `//go:build ignore` and an empty
// `// +build` line earn it; `//go:build ignore || mytag` does not, because
// mytag is a context the guard can be given.
const reservedIgnoreTag = "ignore"

// buildConstraintsOf reads the build-constraint lines of a Go source —
// `//go:build` and the `// +build` go/build still honours (measured
// 2026-10-30: a file carrying the old line alone is in IgnoredGoFiles until
// its tag is given, and compiled the moment it is) — and answers ONE thing
// about the file: the tag names its lines use, negated ones included, BOTH
// sides of every && and ||, or a tag of a conjunction asks for a context
// nobody lists. The expression is walked through its own node types rather
// than evaluated: Expr.Eval calls its callback "as needed to find out
// whether a given build tag is satisfied", which is a contract about the
// verdict, not about the names it visits.
//
// Names only, and the names decide NOTHING about what compiles: which build
// a file is in is read off the go tool's own listing (listing.files,
// listing.known) and, for the one name no listing may carry, off
// go/build's MatchFile. Two rounds of iterion#2829 were lost to this
// function deciding it — first by the file-name convention, then by
// `slices.Contains(names, "ignore")`, which threw away a file
// `go build -tags mytag` really compiles.
//
// Every constraint-shaped line above the package clause is read, not only
// the one the toolchain honours: a name too many widens the listing, a name
// missed hides a file from the context that compiles it.
func buildConstraintsOf(src string) ([]string, error) {
	var names []string
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
			return nil, fmt.Errorf("unreadable build constraint %q: %w", line, err)
		}
		names = append(names, tagsOf(expr)...)
	}
	return names, nil
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

// walkNonTestFiles hands fn every non-test .go file under root's
// judgedTrees, by its module-relative path, with the build-constraint names
// it carries.
//
// It skips NOTHING but the _test.go suffix, and that is the fix of
// iterion#2829's fifth round: which of these files a build compiles is a
// question for the go tool, answered by the listing it writes
// (listing.files holds what a context compiled, listing.known what it named
// at all) and by go/build's MatchFile — never by a convention re-read here.
// The two rounds before it each re-implemented one of those rules and lost
// a real crossing to it: first the `_x.go` / `_x` name convention, then
// "the constraint says `ignore` somewhere", which threw away a file
// `go build -tags mytag` compiles.
//
// So the walk is the DEMAND side only — the files the guard owes a context
// for — and coverageOf sorts them into the go tool's own three answers. It
// feeds the tag contexts and the coverage proof; the layer rules read what
// `go list` returned, never this walk.
func walkNonTestFiles(root string, fn func(rel string, tags []string) error) error {
	for _, tree := range judgedTrees {
		err := filepath.WalkDir(filepath.Join(root, tree), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
			tags, err := buildConstraintsOf(string(src))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
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
// imports, the files the context compiled, and the files it NAMED and left
// out. The imports feed the rules; the two file lists are the go tool's own
// three-way answer about a file, which is why this guard asks for both —
// a file in GoFiles or CgoFiles is compiled here, a file in IgnoredGoFiles
// is one the tool read and its constraints excluded, and a file in NEITHER,
// in any context, is one go/build drops before it reads a constraint at all
// ("not due to build constraints — don't report", go/build) or one that
// sits where no pattern names a package. Measured 2026-10-30 on
// testdata/neverbuilt: Ignored=[gen.go genlinux.go], and _scratch.go /
// .scratch.go in no list of any context.
const listFormat = "{{.ImportPath}}|{{join .Imports \" \"}}|{{join .GoFiles \" \"}}|{{join .CgoFiles \" \"}}|{{join .IgnoredGoFiles \" \"}}"

// listing is what the contexts answered: the module-relative direct imports
// of each package, the module-relative path of every file they compiled,
// and every file they named, compiled or not.
type listing struct {
	imports map[string][]string
	files   map[string]bool // compiled by this context: GoFiles + CgoFiles
	known   map[string]bool // named by the go tool at all: the above + IgnoredGoFiles
}

func newListing() listing {
	return listing{imports: map[string][]string{}, files: map[string]bool{}, known: map[string]bool{}}
}

// merge unions other into l — one context's edges never erase another's.
func (l listing) merge(other listing) {
	for pkg, imps := range other.imports {
		l.imports[pkg] = append(l.imports[pkg], imps...)
	}
	for f := range other.files {
		l.files[f] = true
	}
	for f := range other.known {
		l.known[f] = true
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
		if len(fields) != 5 || !strings.HasPrefix(fields[0], modulePrefix) {
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
			got.known[path.Join(pkg, f)] = true
		}
		for _, f := range strings.Fields(fields[4]) {
			got.known[path.Join(pkg, f)] = true
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
	args = append(args, listPatterns...)
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
// root, in the context env and tags describe. Two readers, one producer:
// the one-hop sentence is measured AGAINST this closure — `go list -deps`
// is what publishes a transitive dependency, and the guard's own listing
// stops at one hop by decision — and
// TestTheGuardAndTheGoToolReadTheSameFiles asks it whether a crossing
// really ships, which is the oracle its scratch trees are judged by.
// stdout alone, for listIn's reason.
func depsOf(root, pkg string, env, tags []string) ([]string, error) {
	prefix, err := modulePrefixOf(root)
	if err != nil {
		return nil, err
	}
	args := []string{"list", "-deps"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "-f", "{{.ImportPath}}", "./"+pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(slices.Clone(os.Environ()), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps %v ./%s failed: %w\nstderr:\n%s", tags, pkg, err, stderr.String())
	}
	var deps []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if rel, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			deps = append(deps, rel)
		}
	}
	if len(deps) == 0 {
		return nil, fmt.Errorf("go list -deps %v ./%s named no package of %s — an empty closure certifies nothing", tags, pkg, prefix)
	}
	return deps, nil
}

// treeUnion lists root in every context the guard judges and unions what
// they answer, so a file one platform or one build tag compiles is judged
// by the same rules as the host's. It returns the union and the contexts
// that fed it.
//
// The platforms go first, and the tags context is what they still DEMAND:
// tagsDemandedBy reads the constraints of the files the go tool named and
// none of them compiled, so the tags the guard passes come from the gap
// the go tool itself reported, never from a reading of what a file "would"
// compile under.
func treeUnion(root, modulePrefix string, env []string) (listing, []buildContext, error) {
	union := newListing()
	ctxs := make([]buildContext, 0, len(platformContexts)+1)
	for _, ctx := range platformContexts {
		got, err := listIn(root, modulePrefix, env, ctx)
		if err != nil {
			return listing{}, nil, err
		}
		union.merge(got)
		ctxs = append(ctxs, ctx)
	}
	tags, err := tagsDemandedBy(root, union)
	if err != nil {
		return listing{}, nil, err
	}
	if len(tags) > 0 {
		ctx := buildContext{name: "tags=" + strings.Join(tags, ","), tags: tags}
		got, err := listIn(root, modulePrefix, env, ctx)
		if err != nil {
			return listing{}, nil, err
		}
		union.merge(got)
		ctxs = append(ctxs, ctx)
	}
	union.dedupe()
	return union, ctxs, nil
}

// contextNames spells the contexts a reader is told about, in the order
// they answered.
func contextNames(ctxs []buildContext) []string {
	names := make([]string, 0, len(ctxs))
	for _, c := range ctxs {
		names = append(names, c.name)
	}
	return names
}

var (
	treeOnce     sync.Once
	treeUnioned  listing
	treeContexts []buildContext
	treeErr      error
)

// treeListing lists this module once per test binary and hands every test
// the same union — five `go list` runs over 230 packages, 4.4 s measured
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
// crossing ships in the binary all the same. This is the other end of the
// coverage bucket the go tool names in no listing (coverage.offListing):
// demanding a context for a file in a directory no pattern names is an
// unclearable red, and this names the import that must not turn into
// silence instead.
//
// Every edge of the listing, whatever its SOURCE: listPatterns lists the
// whole module, so `cmd/x -> pkg/a/_old` is here too. Read off the judged
// trees alone — the shape of iterion#2829's fourth round — the composition
// root was the one importer nothing checked, and it is the tree the
// doctrine says reaches deepest.
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
// is named by no pattern and inside the binary, and the layer rules name no
// hit over it in either direction.
//
// Measured 2026-10-30 over this module, in the five contexts: 230 listed
// packages, 1061 module-internal edges, 1058 of them landing inside pkg/ or
// internal/, and every one of those 1058 names a package the listing holds.
// The 3 that land outside are `bots` and `connectors`, read by pkg/cli,
// pkg/server and pkg/connection: listed, since listPatterns names the whole
// module, and outside this map by the decision judgedTrees carries.
func TestEveryImportOfTheJudgedTreesIsAJudgedPackage(t *testing.T) {
	if unjudged := unjudgedImports(importsPairs(t)); len(unjudged) > 0 {
		t.Errorf("%d import(s) of %v name a package no listing context holds, so no rule reads what THEY import:\n%s\nrename the directory out of the conventions the go tool names no package in — `_x`, `.x`, `testdata` — so a pattern names it, or give the listing a pattern that does",
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
	raw := prefix + "pkg/dsl/ir|fmt " + prefix + "pkg/server github.com/spf13/cobra|ir.go||ir_windows.go\n" +
		prefix + "pkg/log|fmt|log.go||\n"
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
	// The ignored half is NAMED, not compiled: it must reach known and stay
	// out of files, or a file the go tool merely mentions would count as
	// covered and coverageOf would owe no context for it.
	if pairs.files["pkg/dsl/ir/ir_windows.go"] || !pairs.known["pkg/dsl/ir/ir_windows.go"] {
		t.Fatalf("parseListing holds the IgnoredGoFiles entry as compiled=%v named=%v, want compiled=false named=true", pairs.files["pkg/dsl/ir/ir_windows.go"], pairs.known["pkg/dsl/ir/ir_windows.go"])
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
	out := prefix + "pkg/log|fmt|log.go||\n" + diagnostic + "\n" + prefix + "pkg/store|" + prefix + "pkg/log|store.go||\n"
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
	closure, err := depsOf(repoRoot(t), leaf, nil, nil)
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
					t.Errorf("rule %q names %s family %q, which no package the listing of %v holds is under — that clause checks nothing", l.name, side.what, prefix, listPatterns)
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

// goBuildContextOf turns one listing context into the go/build context that
// answers for it: the same GOOS, the same GOARCH, the same build tags
// `go list` was given. What go/build then says about a file — the
// file-name conventions, the GOOS and GOARCH suffixes, `//go:build`,
// `// +build` and the evaluation of the expression — is the go tool's own
// reading, and the only reading this guard has.
func goBuildContextOf(c buildContext) build.Context {
	bc := build.Default
	bc.BuildTags = slices.Clone(c.tags)
	for _, kv := range c.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "GOOS":
			bc.GOOS = v
		case "GOARCH":
			bc.GOARCH = v
		}
	}
	return bc
}

// theReservedNameKeepsItOut reports whether the one name no listing may
// carry (reservedIgnoreTag) is what stands between dir/name and every
// context the guard judges: no context matches the file, and one of them
// does the moment that name is satisfied. Both halves are MatchFile's
// answer — "MatchFile reports whether the file ... would be included in a
// Package created by ImportDir" — so an `//go:build ignore` generator earns
// the exemption and `//go:build ignore || mytag` does not: mytag is a
// context the guard can be given, and tagsDemandedBy gives it.
//
// The second half is not decoration: a predicate that only asked "does the
// reserved name let it in" would exempt every file that mentions the name
// anywhere, which is exactly the crossing iterion#2829's fourth round lost.
func theReservedNameKeepsItOut(dir, name string, ctxs []buildContext) (bool, error) {
	var matched, matchedWithReserved bool
	for _, c := range ctxs {
		bc := goBuildContextOf(c)
		ok, err := bc.MatchFile(dir, name)
		if err != nil {
			return false, fmt.Errorf("go/build cannot read %s in context %s: %w", filepath.Join(dir, name), c.name, err)
		}
		matched = matched || ok
		bc.BuildTags = append(slices.Clone(bc.BuildTags), reservedIgnoreTag)
		ok, err = bc.MatchFile(dir, name)
		if err != nil {
			return false, fmt.Errorf("go/build cannot read %s in context %s with %s: %w", filepath.Join(dir, name), c.name, reservedIgnoreTag, err)
		}
		matchedWithReserved = matchedWithReserved || ok
	}
	return matchedWithReserved && !matched, nil
}

// coverage is what the go tool answered about the non-test files of root's
// judgedTrees. Every walked file lands in exactly one bucket, and which
// bucket is the TOOL's answer, never this file's reading of a convention:
//
//   - seen: a context of the listing compiled it (GoFiles or CgoFiles).
//   - offListing: no context NAMED it. go/build drops a file called _x.go
//     or .x.go before it reads a constraint, and a pattern names no package
//     in a directory called testdata, _x or .x — so the tool mentions such
//     a file in no list of any context. Nothing can reach a dropped file
//     name (no import names a file, and TestNoGuardIsReadAroundByTheBuild
//     in pkg/dsl/unit holds every Go file of the repository to a package of
//     the module); a directory it cannot name IS reachable, by an import,
//     and that edge is what TestEveryImportOfTheJudgedTreesIsAJudgedPackage
//     refuses. So this bucket buys no silence.
//   - reserved: the tool named it, no context compiled it, and
//     theReservedNameKeepsItOut says the one name a listing may not carry is
//     why. An `//go:build ignore` generator, which `go generate` compiles
//     alone through `go run`.
//   - uncovered: the tool named it and no context compiled it, with a
//     context that would — a `_plan9.go`, a tag behind a GOOS this guard
//     lists nothing for. The hole: its imports are judged by nobody, and
//     "add the context that compiles them" is a remedy a reader can carry
//     out.
type coverage struct {
	walked     int
	seen       int
	offListing []string
	reserved   []string
	uncovered  []string
}

// coverageOf sorts root's walked files into those four answers. THE reading
// of coverage: the tree guard, the fixture guard and the scratch-tree
// witness call this one, none re-walks the tree with its own idea of what
// counts.
func coverageOf(root string, got listing, ctxs []buildContext) (coverage, error) {
	var cov coverage
	err := walkNonTestFiles(root, func(rel string, _ []string) error {
		cov.walked++
		switch {
		case got.files[rel]:
			cov.seen++
			return nil
		case !got.known[rel]:
			cov.offListing = append(cov.offListing, rel)
			return nil
		}
		dir, name := filepath.Split(filepath.Join(root, filepath.FromSlash(rel)))
		kept, err := theReservedNameKeepsItOut(dir, name, ctxs)
		if err != nil {
			return err
		}
		if kept {
			cov.reserved = append(cov.reserved, rel)
		} else {
			cov.uncovered = append(cov.uncovered, rel)
		}
		return nil
	})
	slices.Sort(cov.offListing)
	slices.Sort(cov.reserved)
	slices.Sort(cov.uncovered)
	return cov, err
}

// coverageLine is what the coverage guard may claim about its own reach: the
// files a context COMPILED, never the files the walk handed out, and beside
// them the three answers that are not "compiled here" — a line that cannot
// contradict the verdict it accompanies.
func coverageLine(cov coverage, ctxs []buildContext) string {
	return fmt.Sprintf("%d of %d non-test files seen across %d contexts (%s); %d named in no listing, %d kept out by the reserved %q, %d uncovered",
		cov.seen, cov.walked, len(ctxs), strings.Join(contextNames(ctxs), ", "), len(cov.offListing), len(cov.reserved), reservedIgnoreTag, len(cov.uncovered))
}

// TestEveryNonTestFileIsSeenByAListingContext executes the scope sentence
// of docs/layering.md: the contexts treeUnion names compile every non-test
// Go file under pkg/ and internal/ the go tool can be asked to compile, so
// "walks every non-test import" is a measurement. A file no context
// compiles is imports no rule ever reads — iterion#2829's review drove a
// _windows.go and a build-tagged file through that hole, green under linux,
// red under GOOS=windows on the same tree and the same binary.
//
// The two buckets that are NOT red here are the go tool's own answers, and
// each has its own guard: a file the tool names in no listing is either a
// name nothing can reach or a directory TestEveryImportOfTheJudgedTreesIsAJudgedPackage
// refuses an import of, and a file the reserved name keeps out is one no
// build of its package compiles. Both counts are logged rather than pinned:
// they move with every fixture the tree adds, and neither can hide an edge
// — that is what those two guards are for.
func TestEveryNonTestFileIsSeenByAListingContext(t *testing.T) {
	cov, err := coverageOf(repoRoot(t), treeListing(t), treeContexts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.uncovered) > 0 {
		t.Errorf("%d of the %d non-test files under pkg/ and internal/ are named by the go tool and compiled by none of the listed contexts (%s) — add the context that compiles them, or their imports are judged by nothing:\n%s",
			len(cov.uncovered), cov.walked, strings.Join(contextNames(treeContexts), ", "), strings.Join(cov.uncovered, "\n"))
	}
	t.Log(coverageLine(cov, treeContexts))
}

// TestTheCoverageLineCountsOnlyWhatAContextCompiled pins the only count the
// guard above may claim. Logging the files WALKED as "seen" had it answer,
// two lines under a verdict naming three files no context compiles, "1704
// non-test files seen across 3 contexts" — a line that contradicts the
// verdict it accompanies, in a guard whose whole subject is an honest count.
// The three answers that are not "compiled here" are spelled out beside it,
// so a line can no longer read like full coverage over a walk that held
// files nobody compiled.
func TestTheCoverageLineCountsOnlyWhatAContextCompiled(t *testing.T) {
	cov := coverage{
		walked:     5,
		seen:       2,
		offListing: []string{"pkg/a/_old/old.go"},
		reserved:   []string{"pkg/a/gen.go"},
		uncovered:  []string{"pkg/a/lock_plan9.go"},
	}
	got := coverageLine(cov, []buildContext{{name: "host"}, {name: "windows"}})
	const want = `2 of 5 non-test files seen across 2 contexts (host, windows); 1 named in no listing, 1 kept out by the reserved "ignore", 1 uncovered`
	if got != want {
		t.Errorf("coverageLine(5 walked, 2 seen, 2 contexts) = %q, want %q — the files a context compiled, never the files the walk handed out", got, want)
	}
}

// theCrossbuildFixtureTags are the build tags testdata/crossbuild's own
// files carry, declared once and read twice: the fixture must be listed in
// a context carrying both, and THIS module must be listed in a context
// carrying neither. A fixture lives under testdata/, where the go tool
// names no package, so its files demand nothing — read the tags off every
// walked file instead and a module starts passing its fixtures' private
// vocabulary to its own listing.
var theCrossbuildFixtureTags = []string{"crossbuildonly", "crossbuildpair"}

// TestTheTagsContextIsWhatTheTreeStillDemands executes the page's sentence
// about the fifth context: the tags come from the files the four platform
// contexts left uncompiled, which on this tree is `goldens_record` —
// pkg/botreplay's three edges — and nothing else.
func TestTheTagsContextIsWhatTheTreeStillDemands(t *testing.T) {
	treeListing(t) // fills the memo treeContexts below
	names := contextNames(treeContexts)
	if len(names) != len(platformContexts)+1 || !strings.HasPrefix(names[len(names)-1], "tags=") {
		t.Fatalf("this module was listed in %v, want the %d platform contexts and one tags context", names, len(platformContexts))
	}
	demanded := strings.Split(strings.TrimPrefix(names[len(names)-1], "tags="), ",")
	if !slices.Contains(demanded, "goldens_record") {
		t.Errorf("the tags context carries %v, which does not include goldens_record — pkg/botreplay's files are then compiled by nothing", demanded)
	}
	for _, fixtureTag := range theCrossbuildFixtureTags {
		if slices.Contains(demanded, fixtureTag) {
			t.Errorf("the tags context carries %q, a tag of this package's own testdata fixture: the go tool names no package there, so no file of it may demand a context of this module", fixtureTag)
		}
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
	host, err := listIn(root, prefix, hermetic, buildContext{name: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if got := violationsOf(layers, host.imports, nil); len(got) != 0 {
		t.Errorf("the host context alone named %d hit(s) over the fixture:\n%s\nthe fixture is worthless unless a single-context guard calls it clean", len(got), strings.Join(got, "\n"))
	}
	union, ctxs, err := treeUnion(root, prefix, hermetic)
	if err != nil {
		t.Fatal(err)
	}
	names := contextNames(ctxs)
	if want := append(contextNames(platformContexts), "tags="+strings.Join(theCrossbuildFixtureTags, ",")); !slices.Equal(names, want) {
		t.Fatalf("the fixture was listed in %v, want %v: the fixture's //go:build linux is a platform the contexts cover, its own two tags are not, the second lives behind an && that a one-sided collector never reads, and the tags context exists because the platform contexts left those files uncompiled", names, want)
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

// TestTheGoToolSortsTheShapesTheGuardOwesNoContextFor replays, on a module
// of its own, every shape that is not an ordinary compiled file. Here: two
// `//go:build ignore` generators (one bare, one behind an `&&`), a `_old/`
// and a `.hidden/` directory, and a `_scratch.go` and a `.scratch.go` file.
// The walk hands out all eight non-test files — it skips nothing — and the
// go tool's own listing sorts them: two compiled, four it names in no list
// of any context, two it names and leaves to the reserved tag. Only the
// generators carry a constraint, so the tag assertion and the coverage
// assertion cannot cover for each other.
//
// Either generator's tag, forced into a -tags context, stops being reserved
// and the listing of the whole module dies (see reservedIgnoreTag), so
// EVERY test that lists the tree fatals on the error listIn returns — over
// a tree `go build ./...` compiles clean. The directories and the dropped
// file names, demanded a context for, would be an unclearable red. Either
// would be the guard's own, and iterion is one idiomatic generator away
// from both.
//
// The far end is here too: package a imports both directories, so the
// fixture holds the edges unjudgedImports must name. A package out of the
// listing is in the build the moment something imports it, and
// `_scratch.go` is the shape where that cannot happen — no import names a
// file.
func TestTheGoToolSortsTheShapesTheGuardOwesNoContextFor(t *testing.T) {
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
	union, ctxs, err := treeUnion(root, prefix, hermetic)
	if err != nil {
		t.Fatalf("listing the fixture failed, which is how a reserved tag fatals every test of this guard: %v", err)
	}
	names := contextNames(ctxs)
	if want := []string{"host", "windows", "darwin", "freebsd"}; !slices.Equal(names, want) {
		t.Fatalf("the fixture was listed in %v, want %v: `ignore` is the toolchain's reserved name, the two generators that carry it — one bare, one behind an && — are the fixture's only constrained files, and no context may be built from that name", names, want)
	}
	cov, err := coverageOf(root, union, ctxs)
	if err != nil {
		t.Fatal(err)
	}
	if want := 8; cov.walked != want {
		t.Errorf("the walk handed out %d non-test file(s) of the fixture, want all %d — it skips nothing, the go tool sorts them", cov.walked, want)
	}
	if want := 2; cov.seen != want {
		t.Errorf("%d file(s) of the fixture were compiled by a context, want the %d the toolchain compiles (pkg/a/a.go and internal/b/b.go)", cov.seen, want)
	}
	wantOff := []string{"pkg/a/.hidden/hidden.go", "pkg/a/.scratch.go", "pkg/a/_old/old.go", "pkg/a/_scratch.go"}
	if !slices.Equal(cov.offListing, wantOff) {
		t.Errorf("the go tool names %v in no listing of %v, want %v — the two dropped file names and the two directories no pattern names", cov.offListing, names, wantOff)
	}
	wantReserved := []string{"pkg/a/gen.go", "pkg/a/genlinux.go"}
	if !slices.Equal(cov.reserved, wantReserved) {
		t.Errorf("the reserved %q keeps %v out of every context, want %v — the bare generator and the one behind an &&", reservedIgnoreTag, cov.reserved, wantReserved)
	}
	if len(cov.uncovered) != 0 {
		t.Errorf("the guard demands a listing context for %d file(s) of the fixture that no context of %v can compile:\n%s", len(cov.uncovered), names, strings.Join(cov.uncovered, "\n"))
	}
	wantUnjudged := []string{"pkg/a -> pkg/a/.hidden", "pkg/a -> pkg/a/_old"}
	if got := unjudgedImports(union.imports); !slices.Equal(got, wantUnjudged) {
		t.Errorf("the guard names %v as imports no listing judges, want %v — package a imports both directories the go tool names no package in, and that must cost the alarm nothing", got, wantUnjudged)
	}
	t.Log(coverageLine(cov, ctxs))
}

// TestTheReservedIgnoreTagCannotBePassedToTheListing faces THIS module
// with the reason docs/layering.md and reservedIgnoreTag give for dropping
// a file that names `ignore`. testdata/neverbuilt proves the SITE — a
// generator of the module under test landing in its neighbour's package —
// and a reader could take that collision for the whole story. The
// guarantee is wider: the toolchain's own tree carries `//go:build ignore`
// generators, and the listing dies on those. Measured 2026-10-30 over
// `./...`, twice: rc=1, 230 lines of stdout, 592 of stderr, 38 file-level
// diagnostics among them, every one about a package of GOROOT and not one
// about a package of this module, 0.6 s per run.
//
// Red here means the page owes a new reason, not that the guard is wrong:
// the exemption itself is proved by the fixture, in isolation.
func TestTheReservedIgnoreTagCannotBePassedToTheListing(t *testing.T) {
	root := repoRoot(t)
	prefix, err := modulePrefixOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listIn(root, prefix, nil, buildContext{name: "tags=" + reservedIgnoreTag, tags: []string{reservedIgnoreTag}}); err == nil {
		t.Fatalf("`go list -tags %s` listed this module and exited 0 — the name is no longer reserved, so tagsDemandedBy could pass it as an ordinary tag and a generator would be compiled into its neighbour's package, and the page's reason is stale", reservedIgnoreTag)
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

// TestTheConstraintReaderNamesEveryTagWhereverItSits reads the shapes
// `go/build` honours and this guard must agree with, at the one function
// that reads them. Every name, both sides of every operator: a tag this
// reader loses is a context tagsDemandedBy never asks for, and the file it
// gates is then compiled by nobody — which is how iterion#2829's review
// drove a crossing through the guard behind `crossbuildonly &&
// crossbuildpair`.
//
// What the reader may NOT do is decide anything from those names. `ignore`
// is in the table twice over, bare and inside a conjunction, as an ordinary
// name among the others: whether a file carrying it is compiled anywhere is
// the go tool's answer (listing.known, listing.files,
// theReservedNameKeepsItOut), and the round that read it here dropped
// `//go:build ignore || mytag` — a file `go build -tags mytag` compiles —
// with the crossing it holds.
//
// The old `// +build` half is a string here, not a fixture: gofmt adds the
// `//go:build` line to a file that carries only the old one, so
// `gofmt -l` — a gate of this repository — refuses such a file anywhere in
// the tree. `go/build` honours it all the same (measured 2026-10-30 on a
// scratch module: ignored until its tag is given, compiled when it is),
// which is the agreement this function owes, and the empty `// +build` line
// is where the toolchain itself spells "never built" as the tag `ignore`.
func TestTheConstraintReaderNamesEveryTagWhereverItSits(t *testing.T) {
	for _, c := range []struct {
		name      string
		src       string
		wantNames []string
	}{
		{"bare go:build ignore", "//go:build ignore\n\npackage main\n", []string{"ignore"}},
		{"ignore inside a conjunction", "//go:build ignore && linux\n\npackage main\n", []string{"ignore", "linux"}},
		{"ignore inside a disjunction, with the tag that compiles it", "//go:build ignore || mytag\n\npackage a\n", []string{"ignore", "mytag"}},
		{"old-style +build ignore", "// +build ignore\n\npackage main\n", []string{"ignore"}},
		{"old-style comma conjunction", "// +build ignore,!windows\n\npackage main\n", []string{"ignore", "windows"}},
		{"old-style ordinary tag", "// +build goldens_record\n\npackage a\n", []string{"goldens_record"}},
		{"empty +build line, the toolchain's own `ignore`", "// +build\n\npackage a\n", []string{"ignore"}},
		{"a tag named after the reserved one is not it", "//go:build ignoreme\n\npackage a\n", []string{"ignoreme"}},
		{"both sides of an or", "//go:build linux || goldens_record\n\npackage a\n", []string{"linux", "goldens_record"}},
		{"prose naming the form is no constraint", "// a generator carries // +build ignore\n\npackage a\n", nil},
		{"past the package clause nothing is read", "package a\n\n//go:build ignore\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			names, err := buildConstraintsOf(c.src)
			if err != nil {
				t.Fatalf("buildConstraintsOf(%q) failed: %v", c.src, err)
			}
			if !slices.Equal(names, c.wantNames) {
				t.Errorf("names = %q, want %q — every name the lines use, both sides of every operator", names, c.wantNames)
			}
		})
	}
}

// scratchModule is the module path every scratch tree of the witness below
// declares. One banned pair runs through all of them, `pkg/store ->
// pkg/cli`, the eighth rule of layers: the shapes differ only in HOW the
// file that holds the crossing is gated.
const scratchModule = "example.com/shape"

// scratchFiles spells a module of four ordinary files plus whatever the
// shape adds: pkg/cli to be crossed into, pkg/store to cross from,
// internal/b so both judged trees exist — the walk fails on a tree that
// does not, and a module of this guard's own shape is what the shapes are
// about — and the go.mod. `go 1.24` is the fixtures' own floor: a scratch
// tree must not ask for more than the toolchain the gate runs.
func scratchFiles(extra map[string]string) map[string]string {
	files := map[string]string{
		"go.mod":             "module " + scratchModule + "\n\ngo 1.24\n",
		"pkg/cli/cli.go":     "package cli\n\n// Name answers something, so the package has a body.\nfunc Name() string { return \"cli\" }\n",
		"pkg/store/store.go": "package store\n\n// Name answers something, so the package has a body.\nfunc Name() string { return \"store\" }\n",
		"internal/b/b.go":    "package b\n\n// Name answers something, so the package has a body.\nfunc Name() string { return \"b\" }\n",
	}
	for rel, src := range extra {
		files[rel] = src
	}
	return files
}

// writeScratchModule writes one scratch tree under dir. Nothing is removed:
// dir is the framework's own temporary directory, and it cleans up.
func writeScratchModule(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(files[rel]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// bucketOf names the one answer the guard holds about a file: what a
// listing context compiled, and otherwise which of coverageOf's three
// buckets it landed in. "unwalked" is the fifth answer and the one no file
// of a judged tree may get — a walk that skips a shape instead of asking
// the go tool about it lands here, which is the defect of iterion#2829's
// third and fourth rounds.
func bucketOf(got listing, cov coverage, rel string) string {
	switch {
	case got.files[rel]:
		return "seen"
	case slices.Contains(cov.offListing, rel):
		return "offListing"
	case slices.Contains(cov.reserved, rel):
		return "reserved"
	case slices.Contains(cov.uncovered, rel):
		return "uncovered"
	}
	return "unwalked"
}

// TestTheGuardAndTheGoToolReadTheSameFiles writes a module per shape a file
// can be gated by and faces the guard with the GO TOOL's own answer about
// it, never with a reading of the convention. The oracle is
// `go list -deps`: under the tags and the environment the shape names, is
// the banned package really in the importer's closure — does the crossing
// SHIP?
//
// From that one answer the invariant of the whole guard follows, and this
// test executes it: if the crossing ships, the guard is not silent — it
// names the rule hit, or it names the import no rule can judge, or it says
// it owes a context for the file. If it ships nowhere, the guard says
// nothing. Two rounds of iterion#2829 broke that invariant by deciding
// here what the go tool decides there: the `_x`/`.x` name convention
// re-implemented, then "the constraint names `ignore`" — which threw away
// `//go:build ignore || mytag`, a file `go build -tags mytag` compiles,
// with the crossing it holds.
func TestTheGuardAndTheGoToolReadTheSameFiles(t *testing.T) {
	const banned = "pkg/cli"
	crossing := "import \"" + scratchModule + "/pkg/cli\"\n\n// Cross crosses the layer.\nfunc Cross() string { return cli.Name() }\n"
	oldDir := map[string]string{
		"pkg/store/store.go":    "package store\n\nimport old \"" + scratchModule + "/pkg/store/_old\"\n\n// Name reaches the package no pattern names.\nfunc Name() string { return old.Name() }\n",
		"pkg/store/_old/old.go": "package old\n\n" + strings.Replace(crossing, "// Cross crosses the layer.\nfunc Cross()", "// Name crosses the layer.\nfunc Name()", 1),
	}
	for _, c := range []struct {
		name string
		// files the scratch module holds beside scratchFiles' three.
		extra map[string]string
		// file is the one the shape is about, and bucket the single answer
		// the guard must hold about it.
		file   string
		bucket string
		// the oracle: does `go list -deps` of importer, in this context,
		// hold pkg/cli?
		importer string
		shipTags []string
		shipEnv  []string
		ships    bool
		// what the guard must then say, exactly.
		wantViolations []string
		wantUnjudged   []string
	}{
		{
			name:           "an ordinary crossing every context compiles",
			extra:          map[string]string{"pkg/store/lock.go": "package store\n\n" + crossing},
			file:           "pkg/store/lock.go",
			bucket:         "seen",
			importer:       "pkg/store",
			ships:          true,
			wantViolations: []string{layers[7].name + ": pkg/store -> pkg/cli"},
		},
		{
			name:           "the reserved name beside a tag a context can carry",
			extra:          map[string]string{"pkg/store/feat.go": "//go:build ignore || shapetag\n\npackage store\n\n" + crossing},
			file:           "pkg/store/feat.go",
			bucket:         "seen",
			importer:       "pkg/store",
			shipTags:       []string{"shapetag"},
			ships:          true,
			wantViolations: []string{layers[7].name + ": pkg/store -> pkg/cli"},
		},
		{
			name:           "the old +build line alone, with a tag a context can carry",
			extra:          map[string]string{"pkg/store/old.go": "// +build shapetag\n\npackage store\n\n" + crossing},
			file:           "pkg/store/old.go",
			bucket:         "seen",
			importer:       "pkg/store",
			shipTags:       []string{"shapetag"},
			ships:          true,
			wantViolations: []string{layers[7].name + ": pkg/store -> pkg/cli"},
		},
		{
			name:     "the reserved name inside a conjunction",
			extra:    map[string]string{"pkg/store/gen.go": "//go:build ignore && linux\n\npackage store\n\n" + crossing},
			file:     "pkg/store/gen.go",
			bucket:   "reserved",
			importer: "pkg/store",
			ships:    false,
		},
		{
			name:     "an empty +build line, the toolchain's own never-built",
			extra:    map[string]string{"pkg/store/empty.go": "// +build\n\npackage store\n\n" + crossing},
			file:     "pkg/store/empty.go",
			bucket:   "reserved",
			importer: "pkg/store",
			ships:    false,
		},
		{
			name:     "a file name the go tool drops before it reads a constraint",
			extra:    map[string]string{"pkg/store/_scratch.go": "package store\n\n" + crossing},
			file:     "pkg/store/_scratch.go",
			bucket:   "offListing",
			importer: "pkg/store",
			ships:    false,
		},
		{
			name:         "a directory the go tool names no package in, imported from the judged trees",
			extra:        oldDir,
			file:         "pkg/store/_old/old.go",
			bucket:       "offListing",
			importer:     "pkg/store",
			ships:        true,
			wantUnjudged: []string{"pkg/store -> pkg/store/_old"},
		},
		{
			name: "a testdata directory, imported from the judged trees",
			extra: map[string]string{
				"pkg/store/store.go":           "package store\n\nimport evil \"" + scratchModule + "/pkg/store/testdata/evil\"\n\n// Name reaches the package no pattern names.\nfunc Name() string { return evil.Name() }\n",
				"pkg/store/testdata/evil/e.go": "package evil\n\n" + strings.Replace(crossing, "// Cross crosses the layer.\nfunc Cross()", "// Name crosses the layer.\nfunc Name()", 1),
			},
			file:         "pkg/store/testdata/evil/e.go",
			bucket:       "offListing",
			importer:     "pkg/store",
			ships:        true,
			wantUnjudged: []string{"pkg/store -> pkg/store/testdata/evil"},
		},
		{
			name: "the same directory, imported from outside the judged trees",
			extra: map[string]string{
				"cmd/x/main.go":         "package main\n\nimport old \"" + scratchModule + "/pkg/store/_old\"\n\nfunc main() { println(old.Name()) }\n",
				"pkg/store/_old/old.go": oldDir["pkg/store/_old/old.go"],
			},
			file:         "pkg/store/_old/old.go",
			bucket:       "offListing",
			importer:     "cmd/x",
			ships:        true,
			wantUnjudged: []string{"cmd/x -> pkg/store/_old"},
		},
		{
			name:     "a platform suffix no context of this guard carries",
			extra:    map[string]string{"pkg/store/lock_plan9.go": "package store\n\n" + crossing},
			file:     "pkg/store/lock_plan9.go",
			bucket:   "uncovered",
			importer: "pkg/store",
			shipEnv:  []string{"GOOS=plan9"},
			ships:    true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeScratchModule(t, root, scratchFiles(c.extra))
			// A module of its own: GOWORK=off keeps an operator's workspace
			// out, and -buildvcs=false keeps a main package from asking a
			// temporary directory's enclosing repository for a revision.
			hermetic := []string{"GOWORK=off", "GOFLAGS=-buildvcs=false"}

			deps, err := depsOf(root, c.importer, append(slices.Clone(hermetic), c.shipEnv...), c.shipTags)
			if err != nil {
				t.Fatalf("the oracle failed, so the shape proves nothing: %v", err)
			}
			if ships := slices.Contains(deps, banned); ships != c.ships {
				t.Fatalf("`go list -deps %v %v ./%s` %s %s in its closure, the shape says it %s — the oracle and the shape disagree, so the guard is being compared to nothing",
					c.shipEnv, c.shipTags, c.importer, map[bool]string{true: "holds", false: "does not hold"}[ships], banned, map[bool]string{true: "does", false: "does not"}[c.ships])
			}

			prefix, err := modulePrefixOf(root)
			if err != nil {
				t.Fatal(err)
			}
			union, ctxs, err := treeUnion(root, prefix, hermetic)
			if err != nil {
				t.Fatal(err)
			}
			cov, err := coverageOf(root, union, ctxs)
			if err != nil {
				t.Fatal(err)
			}
			if got := bucketOf(union, cov, c.file); got != c.bucket {
				t.Errorf("the guard holds %s as %q, want %q — contexts %v, seen %d of %d, offListing %v, reserved %v, uncovered %v",
					c.file, got, c.bucket, contextNames(ctxs), cov.seen, cov.walked, cov.offListing, cov.reserved, cov.uncovered)
			}
			violations := violationsOf(layers, union.imports, nil)
			if c.wantViolations == nil {
				c.wantViolations = []string{}
			}
			if !slices.Equal(violations, c.wantViolations) {
				t.Errorf("the rules named:\n%s\nwant:\n%s", strings.Join(violations, "\n"), strings.Join(c.wantViolations, "\n"))
			}
			unjudged := unjudgedImports(union.imports)
			if c.wantUnjudged == nil {
				c.wantUnjudged = []string{}
			}
			if !slices.Equal(unjudged, c.wantUnjudged) {
				t.Errorf("the guard named %v as imports no listing judges, want %v", unjudged, c.wantUnjudged)
			}

			// The invariant the oracle buys: a crossing the go tool ships is
			// one this guard says SOMETHING about.
			said := len(violations) + len(unjudged) + len(cov.uncovered)
			if c.ships && said == 0 {
				t.Errorf("`go list -deps` ships %s into %s and the guard said nothing: no rule hit, no unjudged import, no file it owes a context for", banned, c.importer)
			}
			if !c.ships && said != 0 {
				t.Errorf("no build of this module holds %s in %s's closure, and the guard reported %d thing(s): violations=%v unjudged=%v uncovered=%v",
					banned, c.importer, said, violations, unjudged, cov.uncovered)
			}
		})
	}
}

// TestTheReservedNameEarnsNoExemptionWhereAContextCompilesTheFile drives
// theReservedNameKeepsItOut over contexts this test names, so both terms of
// its predicate are exercised on files written for them. The exemption is
// for a file no context of the guard can be given, NOT for a file whose
// constraint mentions the reserved name: `//go:build ignore || shapetag` is
// compiled by the tags context tagsDemandedBy asks for, and asking only
// "does the reserved name let it in" is what made iterion#2829's fourth
// round silent about the crossing such a file holds.
func TestTheReservedNameEarnsNoExemptionWhereAContextCompilesTheFile(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"bare.go":     "//go:build ignore\n\npackage main\n\nfunc main() {}\n",
		"beside.go":   "//go:build ignore || shapetag\n\npackage a\n",
		"plus.go":     "// +build\n\npackage a\n",
		"tagged.go":   "//go:build shapetag\n\npackage a\n",
		"plain.go":    "package a\n",
		"_scratch.go": "package a\n",
	}
	writeScratchModule(t, dir, files)
	// The contexts the guard would judge such a tree in: the platforms, plus
	// the one tags context a demand for shapetag produces.
	ctxs := append(slices.Clone(platformContexts), buildContext{name: "tags=shapetag", tags: []string{"shapetag"}})
	for _, c := range []struct {
		file string
		want bool
		why  string
	}{
		{"bare.go", true, "no context compiles it and the reserved name is the only thing that would"},
		{"plus.go", true, "the empty old-style line IS the reserved name, spelled by the toolchain itself"},
		{"beside.go", false, "the tags context compiles it, so the reserved name is not what keeps it out"},
		{"tagged.go", false, "the tags context compiles it, reserved name or no reserved name"},
		{"plain.go", false, "every context compiles it"},
		{"_scratch.go", false, "go/build drops the NAME in every context, with and without the reserved tag — this predicate is not what answers for that shape"},
	} {
		t.Run(c.file, func(t *testing.T) {
			got, err := theReservedNameKeepsItOut(dir, c.file, ctxs)
			if err != nil {
				t.Fatalf("theReservedNameKeepsItOut(%s) failed: %v", c.file, err)
			}
			if got != c.want {
				t.Errorf("theReservedNameKeepsItOut(%s) = %v, want %v — %s", c.file, got, c.want, c.why)
			}
		})
	}
}

// theBoundsThePageStates are the bounds docs/layering.md publishes about
// what this guard reads and what it does not, one bullet each, in the
// page's own order. ONE declaration: the godoc of layers names them from
// here instead of counting them, and the test below faces the page with it.
// The count was wrong twice — "the same three bounds" over a page carrying
// four — because it was a number in a comment that nothing executed.
var theBoundsThePageStates = []string{
	"Every non-test file, over five listing contexts.",
	"Which file a build compiles is the go tool's answer, never the guard's.",
	"Out of the listing is not out of the build.",
	"Direct imports, one hop.",
	"Test files are out of scope, by decision",
}

// TestThePageStatesTheBoundsTheGuardClaims reads the page's bounds section
// and demands it carry exactly those bullets: the same number, each one
// still said in the page's own words. A bound added to the page and not
// here is a promise this file never claimed; a bound dropped from the page
// while this list keeps it is the drift that made the godoc lie.
func TestThePageStatesTheBoundsTheGuardClaims(t *testing.T) {
	const heading = "## What the guard reads, and what it does not"
	page, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "layering.md")) // #nosec G304 -- the page of the module the test climbed to
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(page), heading)
	if !ok {
		t.Fatalf("docs/layering.md carries no %q section — the bounds this guard claims are published there", heading)
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	if got, want := strings.Count(section, "\n- **"), len(theBoundsThePageStates); got != want {
		t.Errorf("the page's bounds section carries %d bullet(s), and this guard claims %d — add the bound to theBoundsThePageStates or take it off the page", got, want)
	}
	said := proseText(section)
	for _, bound := range theBoundsThePageStates {
		if !strings.Contains(said, proseText(bound)) {
			t.Errorf("docs/layering.md no longer states the bound this guard claims:\n%s", bound)
		}
	}
}
