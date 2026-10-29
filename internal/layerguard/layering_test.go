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
// another package is not a rule hit: pkg/dsl/ir's direct imports name no
// pkg/forge and no pkg/dispatcher package, and its transitive closure holds
// four of them (iterion#2857). A coupling that exists only in a _test.go
// file is out of scope by decision. docs/layering.md carries the same three
// bounds; the contexts are judged by
// TestEveryNonTestFileIsSeenByAListingContext, the one-hop reading by
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
// instead is what TestEveryNonTestFileIsSeenByAListingContext reports.
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
	err := walkNonTestFiles(root, func(rel string, src []byte) error {
		names, err := buildTagsOf(string(src))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
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

// buildTagsOf returns every name the //go:build lines of a Go source use,
// negated ones included — BOTH sides of every && and ||, or a tag of a
// conjunction asks for a context nobody lists. The expression is walked
// through its own node types rather than evaluated: Expr.Eval calls its
// callback "as needed to find out whether a given build tag is satisfied",
// which is a contract about the verdict, not about the names it visits.
func buildTagsOf(src string) ([]string, error) {
	var names []string
	for line := range strings.SplitSeq(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break // the toolchain reads no constraint past the package clause
		}
		if !constraint.IsGoBuild(line) {
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

// walkNonTestFiles hands fn every non-test .go file under root's pkg/ and
// internal/ trees, by its module-relative path. testdata/ is skipped: the
// go tool lists no package there, so a fixture's build tag would ask for a
// context no listing can use, and a fixture's file would be reported as
// unseen for ever.
func walkNonTestFiles(root string, fn func(rel string, src []byte) error) error {
	for _, tree := range []string{"pkg", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
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
			return fn(filepath.ToSlash(rel), src)
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
	args = append(args, "-f", listFormat, "./pkg/...", "./internal/...")
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
// so a banned family reached through a third package is no rule hit. The
// tree pays this today: `go list -deps ./pkg/dsl/ir` holds four packages of
// three banned families its direct imports do not name — pkg/forge,
// pkg/dispatcher/native, pkg/dispatcher/tracker and
// pkg/backend/tool/privacy/detector (iterion#2857) — so a reader who takes
// "is a leaf" for a transitive contract reads a promise this guard does not
// make.
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
					t.Errorf("rule %q names %s family %q, which no package of ./pkg/... or ./internal/... is under — that clause checks nothing", l.name, side.what, prefix)
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

// TestEveryNonTestFileIsSeenByAListingContext executes the scope sentence
// of docs/layering.md: the contexts listContexts names compile every
// non-test Go file under pkg/ and internal/, so "walks every non-test
// import" is a measurement. A file no context compiles is imports no rule
// ever reads — iterion#2829's review drove a _windows.go and a build-tagged
// file through that hole, green under linux, red under GOOS=windows on the
// same tree and the same binary.
func TestEveryNonTestFileIsSeenByAListingContext(t *testing.T) {
	seen := treeListing(t).files
	var unseen []string
	total := 0
	if err := walkNonTestFiles(repoRoot(t), func(rel string, _ []byte) error {
		total++
		if !seen[rel] {
			unseen = append(unseen, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(unseen) > 0 {
		t.Errorf("%d of the %d non-test files under pkg/ and internal/ are compiled by none of the listed contexts (%s) — add the context that compiles them, or their imports are judged by nothing:\n%s",
			len(unseen), total, strings.Join(treeContexts, ", "), strings.Join(unseen, "\n"))
	}
	t.Logf("%d non-test files seen across %d contexts (%s)", total, len(treeContexts), strings.Join(treeContexts, ", "))
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
