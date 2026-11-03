package envtrust

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// The trees the sweeps in this file subtract, each with the reason it is
// out. One list for both: two lists required to agree is the drift class
// internal/sweepwalk was extracted for, and a sweep judging a narrower tree
// than its neighbour would be green for a reason no reader could name.
var lookPathSweepExclusions = map[string]string{
	"third_party/": "a `replace`d module compiled into the binary, whose own discovery.go does call exec.LookPath — " +
		"out for a REACHABILITY reason, not an ownership one: every iterion entry point passes `WithCliPath` with a " +
		"path pkg/internal/clilocate resolved through the seam, and the SDK returns an explicit path before it searches PATH",
	"scripts/": "out for the opposite reason: scripts/ci/testcaps observes the real runner's PATH by construction, " +
		"which is its whole job",
	"e2e/fixtures/": "sample application sources an e2e scenario hands a bot to work on, each under its own go.mod — " +
		"another project's code, never this binary's launcher",
}

// sweepNonTestGo visits, parsed, every non-test Go file the sweeps in this
// file judge: everything the shared walk reaches minus
// [lookPathSweepExclusions], by SUBTRACTION rather than from a list of
// blessed directories — an allowlist of tree names is a silent hole one
// tree later, and `contrib/mattermost-clarify`, a `package main` of this
// very module, took a plain `exec.LookPath("docker")` green under one.
//
// It holds the exclusions to being REACHED, so a reason cannot outlive its
// tree, and skips files whose bytes lack `LookPath`: the identifier has to
// appear literally in the source to be referenced — no splicing forms a Go
// identifier — so such a file needs no parse.
func sweepNonTestGo(t *testing.T, visit func(rel string, file *ast.File)) {
	t.Helper()
	excludedSeen := map[string]bool{}
	root := sweepwalk.ModuleRoot(t)
	sweepwalk.GoFiles(t, root, func(rel, path string) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		// Every matching exclusion is marked, not the first one a map range
		// happens to yield: the prefixes are disjoint today, and a verdict
		// that would change the day two overlap is a verdict read off an
		// iteration order.
		excluded := false
		for dir := range lookPathSweepExclusions {
			if strings.HasPrefix(rel, dir) {
				excludedSeen[dir] = true
				excluded = true
			}
		}
		if excluded {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(raw), "LookPath") {
			return
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		visit(rel, file)
	})

	// An exclusion the walk never reached is a reason outliving its tree —
	// the shape that leaves a guard green over nothing, and the same defect
	// as an exception whose lookup is gone. (That the walk itself is alive
	// is proven from the other side: each sweep below names files it is
	// required to FIND, and TestTheSweepJudgesTheTreeTheTestRunsIn pins the
	// root walked.)
	for dir, reason := range lookPathSweepExclusions {
		if !excludedSeen[dir] {
			t.Errorf("%s is subtracted from these sweeps (%s) and the walk reached no non-test Go file under it: the tree "+
				"moved or emptied, so the exclusion now names nothing — drop the entry, or fix the prefix.", dir, reason)
		}
	}
}

// The completeness guard on the EXPLICIT PATH-lookup class. [LookPath] is
// exec.LookPath that refuses a PATH a project `.env` planted, and the class
// was closed site by site (#2809, then #2876) — but nothing held it closed:
// .golangci.yml enables six linters, none of which can forbid a call, and
// it excludes cmd/iterion-desktop from linting altogether. The twelfth
// exec.LookPath would land green, in a tree whose every other lookup reads
// the seam, and the only trace would be a grep nobody runs.
//
// So: every exec.LookPath in this repository's own non-test sources is
// either the seam itself or an exception ON THE RECORD, with the count of
// calls its reason covers and the reason in full. A file is not a licence —
// a second lookup added beside an exempted one arrives red, because the
// count no longer matches.
//
// What this guard does NOT judge, and why:
//
//   - _test.go files. A skip-guarded `exec.LookPath` in a test is the
//     capability-probe idiom the CI census reads (docs/test-capability-ledger.md):
//     it observes the real runner on purpose, and a test's own PATH is the
//     test's business.
//   - The IMPLICIT class. `exec.Command("docker", …)` is also a PATH lookup
//     answering the same question — Go resolves a bare name against the
//     parent's PATH at Command() time, consulting no seam. That class is
//     larger, needs a seam of its own, and is tracked separately; this
//     guard covers the explicit lookups and says so in
//     docs/environment-variables.md rather than implying more.
//   - The trees named in [lookPathSweepExclusions], each with its reason.
//     vendor/, node_modules/, studio/, testdata/ and the operator's parked
//     checkouts never reach the predicate at all: the shared walk prunes
//     them (internal/sweepwalk). Everything else the walk reaches IS
//     judged — [sweepNonTestGo] owns that rule and the reason it is a
//     subtraction.
//
// Blind spot, stated: the walk reads the lookup through the file's own
// `os/exec` import names, so a lookup reached through ANOTHER package's
// re-export is invisible here — and judged at that re-export's own site,
// which lives in a tree this sweep walks. A dot-import of os/exec would
// hide the call from the matcher entirely, so it is reported rather than
// miscounted — including when a named import of the same path precedes it,
// which is legal Go that compiles.
func TestEveryExecLookPathOutsideTheSeamIsOnTheRecord(t *testing.T) {
	// An exception names a file, HOW MANY lookups its reason covers, and the
	// reason. Counting the calls rather than trusting the file is the point:
	// `external_cli.go` holds two, and a third added beside them would
	// otherwise inherit a reason written for the first two.
	type exception struct {
		sites  int
		reason string
	}
	exceptions := map[string]exception{
		// The desktop app's tool detection, and the one file in this list
		// NO linter sees (.golangci.yml excludes cmd/iterion-desktop: the
		// Wails/cgo build needs a toolchain the core lint has not got), so
		// this sweep is its only reader. Two lookups, one call shape: probe
		// PATH, fall back to the well-known install locations, then re-probe
		// so the caller gets the canonical exec.ErrNotFound. It reports
		// which claude/codex/git the DESKTOP process would run, and that
		// process plants PATH deliberately unmarked — the operator's own
		// `~/.iterion/env`, whose values keep the operator's authority
		// (cmd/iterion-desktop/env_file.go) — so the seam would resolve
		// exactly the same thing while implying a refusal that cannot fire
		// here.
		"cmd/iterion-desktop/external_cli.go": {sites: 2,
			reason: "the desktop wrapper's CLI detection, in the process whose dotenv plant is operator-owned and unmarked on purpose"},
		// Windows' process-tree kill. `//go:build windows`, so no CI job
		// compiles it; it resolves taskkill from the system PATH to end a
		// subtree iterion already started, which is not a question about
		// which program to RUN on the operator's behalf.
		"pkg/backend/delegate/claudesdk/process_windows.go": {sites: 1,
			reason: "Windows taskkill, ending a subtree iterion already started"},
		// The seam itself: the one place allowed to reach the real lookup,
		// and only once the planted-PATH question has been answered.
		"internal/envtrust/lookpath.go": {sites: 1,
			reason: "the seam, reached only when the PATH is the operator's own or the name already carries a path"},
	}

	var offenders []string
	var dotImports []string
	found := map[string]int{}
	sweepNonTestGo(t, func(rel string, file *ast.File) {
		sites, dotImported := execLookPathSites(file)
		if dotImported {
			dotImports = append(dotImports, rel)
			return
		}
		if sites == 0 {
			return
		}
		found[rel] = sites
		if _, ok := exceptions[rel]; !ok {
			offenders = append(offenders, rel)
		}
	})

	slices.Sort(offenders)
	for _, rel := range offenders {
		t.Errorf("%s calls exec.LookPath, which resolves against the live PATH — a PATH a project `.env` can have planted, "+
			"making the repository under review choose the executable. Call envtrust.LookPath instead "+
			"(internal/envtrust/lookpath.go): same signature, same result on the operator's own PATH, a loud "+
			"exec.ErrNotFound on a planted one. If the lookup genuinely must answer the live PATH, add %q to this "+
			"test's exceptions with the number of lookups and the reason.", rel, rel)
	}
	for _, rel := range dotImports {
		t.Errorf("%s dot-imports os/exec, and this guard reads the lookup through the file's import name — the call is "+
			"invisible to it. Import os/exec under a name (the whole tree does) so the class stays judged.", rel)
	}

	// The exceptions are held to their count, in BOTH directions: an extra
	// lookup beside an exempted one is unjudged, and an entry the walk no
	// longer finds is a reason outliving its site — the shape that leaves a
	// guard green over nothing.
	for rel, want := range exceptions {
		got, ok := found[rel]
		if !ok {
			t.Errorf("%s is on the record for %d exec.LookPath lookup(s) (%s) and the walk finds none: either the file "+
				"moved, the lookup went through the seam, or the walk stopped reading it — drop the entry if the site "+
				"is gone.", rel, want.sites, want.reason)
			continue
		}
		if got != want.sites {
			t.Errorf("%s is on the record for %d exec.LookPath lookup(s) (%s) and now holds %d: the reason covers those "+
				"lookups, not the file. Route the new one through envtrust.LookPath, or raise the count with the reason "+
				"it deserves.", rel, want.sites, want.reason, got)
		}
	}
}

// execLookPathSites counts the references this file makes to os/exec's
// LookPath, and reports whether the file dot-imports os/exec (which hides
// the reference from the matcher).
//
// It counts a CALL and a function VALUE alike: `locate(exec.LookPath)`
// hands the lookup on without calling it here, and a call-only matcher
// would miss it — scripts/ci/testcaps/main.go passes exactly that value
// today, so the shape is not hypothetical.
//
// It reads the SOURCE, not the bytes: the nineteen
// `// envtrust.LookPath, not exec.LookPath: …` justifications a grep
// reports are comments, which no AST node carries here, and so is the name
// inside a string. The reference is matched through EVERY name the file
// binds to os/exec, so an aliased `import xexec "os/exec"` counts and a
// `LookPath` method on an unrelated receiver does not.
func execLookPathSites(file *ast.File) (sites int, dotImported bool) {
	names, dotImported := importNames(file, "os/exec")
	if dotImported {
		return 0, true
	}
	if len(names) == 0 {
		return 0, false // the file does not import os/exec at all
	}
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LookPath" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && names[pkg.Name] {
			sites++
		}
		return true
	})
	return sites, false
}

// importNames returns every local name the file binds to the import path,
// and whether ANY of those imports is a dot-import. One path can be bound
// several times — `import ( "os/exec"; xexec "os/exec" )` is legal Go that
// compiles and gofmt leaves alone — so the FIRST import is not the answer:
// reading it alone misses a reference made through a second name, and
// misses the dot-import that hides a reference entirely whenever a named
// import precedes it. A dot-import is reported rather than counted: there
// is no name to match, so a sweep says so instead of returning a zero that
// reads as "no reference here".
//
// One matcher for both sweeps: the second would otherwise re-derive the
// same rule, and the two would be required to agree.
func importNames(file *ast.File, path string) (names map[string]bool, dotImported bool) {
	quoted := strconv.Quote(path)
	base := path[strings.LastIndex(path, "/")+1:]
	names = map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != quoted {
			continue
		}
		switch {
		case imp.Name == nil:
			names[base] = true
		case imp.Name.Name == ".":
			dotImported = true
		case imp.Name.Name == "_":
			// Imported for side effects; nothing to reference.
		default:
			names[imp.Name.Name] = true
		}
	}
	return names, dotImported
}

// The other half of the class, and the half a lookup census cannot see: a
// seam lookup whose answer is thrown away, after which the spawn asks the
// live PATH the very question the seam had just answered. It is the shape
// pkg/sandbox/kubernetes/validate.go carried in full (the seam at :81, a
// bare `kubectl` at :57) and pkg/dryrun/shell.go after it — the refusal
// still gates such a site, so it is a class left open rather than a live
// bypass, which is exactly how it survived a census that only counted
// lookups. docs/environment-variables.md states that what the seam resolved
// is CARRIED rather than asked again; this holds the statement.
//
// A DISCARD is `_, err := envtrust.LookPath(…)`, or a call whose results go
// nowhere at all. Keeping the path is where the judgement stops: no AST can
// tell a path stored and spawned from one stored and ignored, so the sweep
// proves the answer is KEPT and leaves "and spawned" to the packages that
// can separate the two calls in time — pkg/sandbox/docker's
// detect_spawn_path_test.go moves PATH between Detect and the spawn and
// reads which binary answered.
//
// Where the lookup and the spawn are adjacent statements, as in
// pkg/dryrun, no in-process witness can separate the shapes at all: Go
// fills cmd.Path from a lookup of its own that reads the same PATH an
// instant later, only cmd.Args[0] differs, and a child reached through a
// shebang reports the exec path as its `$0`. This sweep is the whole guard
// at such a site, which is why it judges the shape and says so.
func TestASeamLookupKeepsThePathItResolved(t *testing.T) {
	// A probe that answers a yes/no has no spawn for the path to reach, so
	// discarding it is the honest shape — on the record, with its count,
	// like the lookup exceptions above.
	type discard struct {
		sites  int
		reason string
	}
	onTheRecord := map[string]discard{
		"pkg/sandbox/factory.go": {sites: 1,
			reason: "HostHasDocker answers `iterion sandbox doctor` with the runtime's NAME and spawns nothing: the probe's " +
				"result is the name, and the driver that does spawn resolves its own path through the seam"},
		"pkg/backend/delegate/cliagent_image_gap.go": {sites: 1,
			reason: "refuseAbsentImageCLI only decides whether a hostless dispatch is refused for a backend CLI absent from " +
				"the operator's PATH and spawns nothing; the spawn that follows (cliagent.go) runs the bare name through " +
				"exec.CommandContext, the implicit class #2988 converts"},
	}

	var offenders []string
	var dotImports []string
	found := map[string]int{}
	sweepNonTestGo(t, func(rel string, file *ast.File) {
		discards, dotImported := seamLookPathDiscards(file)
		if dotImported {
			dotImports = append(dotImports, rel)
			return
		}
		if discards == 0 {
			return
		}
		found[rel] = discards
		if _, ok := onTheRecord[rel]; !ok {
			offenders = append(offenders, rel)
		}
	})

	slices.Sort(offenders)
	for _, rel := range offenders {
		t.Errorf("%s calls envtrust.LookPath and throws the resolved path away. Whatever it spawns next resolves the name "+
			"against the live PATH again — the question the seam had just answered, asked of a PATH that can have moved in "+
			"between, so the binary probed is not provably the binary run. Bind the path and spawn THAT. If the lookup only "+
			"answers whether a binary exists, add %q to this test's record with the number of discards and the reason.", rel, rel)
	}
	for _, rel := range dotImports {
		t.Errorf("%s dot-imports internal/envtrust, and this guard reads the lookup through the file's import name — the "+
			"call is invisible to it. Import the seam under a name (the whole tree does) so the class stays judged.", rel)
	}

	for rel, want := range onTheRecord {
		got, ok := found[rel]
		if !ok {
			t.Errorf("%s is on the record for %d discarded seam lookup(s) (%s) and the walk finds none: either the file "+
				"moved or the lookup now keeps its path — drop the entry if the site is gone.", rel, want.sites, want.reason)
			continue
		}
		if got != want.sites {
			t.Errorf("%s is on the record for %d discarded seam lookup(s) (%s) and now holds %d: the reason covers those "+
				"discards, not the file. Carry the path at the new one, or raise the count with the reason it deserves.",
				rel, want.sites, want.reason, got)
		}
	}
}

// seamLookPathDiscards counts the places this file asks [LookPath] for a
// path and keeps none of it, and reports a dot-import of the seam's package
// (which hides the call from the matcher). The seam's own package calls
// LookPath unqualified, so it imports nothing to match and counts zero —
// its exemption is the lookup census's business, not this one's.
func seamLookPathDiscards(file *ast.File) (discards int, dotImported bool) {
	names, dotImported := importNames(file, "github.com/SocialGouv/iterion/internal/envtrust")
	if dotImported {
		return 0, true
	}
	if len(names) == 0 {
		return 0, false
	}
	isSeamLookup := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LookPath" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && names[pkg.Name]
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch stmt := node.(type) {
		case *ast.AssignStmt:
			// `_, err := …` and `_, _ = …`: the path has no name to be used
			// under. A single-valued Lhs cannot be the lookup's two results.
			if len(stmt.Rhs) != 1 || len(stmt.Lhs) != 2 || !isSeamLookup(stmt.Rhs[0]) {
				return true
			}
			if blank, ok := stmt.Lhs[0].(*ast.Ident); ok && blank.Name == "_" {
				discards++
			}
		case *ast.ExprStmt:
			// The call standing alone as a statement: both results dropped.
			if isSeamLookup(stmt.X) {
				discards++
			}
		}
		return true
	})
	return discards, false
}

// The predicate above is a BLOCKING gate run over the repository's own
// sources, where a false accusation shows up as a red build on correct code
// and a miss shows up as nothing at all. Each case below is source the
// matcher must read a specific way, so a change to it reddens here instead
// of in someone's PR — or, worse, stays green while judging nothing.
func TestTheLookPathSweepReadsTheShapesItClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		sites int
		dot   bool
	}{
		{
			name: "a plain call",
			src: `package p
import "os/exec"
func f() { _, _ = exec.LookPath("docker") }`,
			sites: 1,
		},
		{
			name: "two calls in one file are two sites",
			src: `package p
import "os/exec"
func f() { _, _ = exec.LookPath("docker") }
func g() { _, _ = exec.LookPath("podman") }`,
			sites: 2,
		},
		{
			// The shape a call-only matcher misses, and the shape the tree
			// already uses (scripts/ci/testcaps/main.go).
			name: "the lookup passed on as a function value",
			src: `package p
import "os/exec"
func observe(look func(string) (string, error)) {}
func f() { observe(exec.LookPath) }`,
			sites: 1,
		},
		{
			name: "an aliased import is still os/exec",
			src: `package p
import xexec "os/exec"
func f() { _, _ = xexec.LookPath("docker") }`,
			sites: 1,
		},
		{
			// Legal Go that compiles and gofmt leaves alone: one path, two
			// local names. A matcher that stopped at the FIRST os/exec
			// import counted one of these two lookups and called the file
			// judged.
			name: "every name bound to os/exec is a reference",
			src: `package p
import (
	"os/exec"
	xexec "os/exec"
)
func f() (string, error) { _, _ = exec.LookPath("docker"); return xexec.LookPath("podman") }`,
			sites: 2,
		},
		{
			// The blind spot the header states, in the shape that hides it:
			// a dot-import BESIDE a named one. Stopping at the first import
			// returns a name, so the report never fires and the bare
			// `LookPath` — no selector to match — counts zero.
			name: "a dot-import beside a named one is still reported",
			src: `package p
import (
	"os/exec"
	. "os/exec"
)
func f() (string, error) { _ = exec.Command("docker"); return LookPath("podman") }`,
			dot: true,
		},
		{
			// The nineteen justification comments in the tree, and the
			// reason this reads the AST rather than the bytes.
			name: "the name in a comment or a string is not a reference",
			src: `package p
import "os/exec"
// envtrust.LookPath, not exec.LookPath: the comment every converted site carries.
const doc = "exec.LookPath"
func f() { _ = exec.Command("docker") }`,
		},
		{
			name: "the seam's own call is not os/exec's",
			src: `package p
import "github.com/SocialGouv/iterion/internal/envtrust"
func f() { _, _ = envtrust.LookPath("docker") }`,
		},
		{
			// cmd/iterion-desktop's `looker` interface has a LookPath
			// method; a call on it is not a PATH lookup of this class.
			// The spawn beside it names docker, never git: pkg/git's
			// test-caller sweep reads the TEXT of every _test.go, so a
			// `git` subprocess quoted in a fixture is an offender to it.
			name: "a LookPath method on an unrelated receiver",
			src: `package p
import "os/exec"
type looker interface{ LookPath(string) (string, error) }
func f(l looker) { _, _ = l.LookPath("claude"); _ = exec.Command("docker") }`,
		},
		{
			name: "a file that does not import os/exec",
			src: `package p
func f(l interface{ LookPath(string) (string, error) }) { _, _ = l.LookPath("docker") }`,
		},
		{
			name: "a blank import references nothing",
			src: `package p
import _ "os/exec"
func f() {}`,
		},
		{
			// Reported, not counted: there is no import name to match, so a
			// zero here would read as "no lookup in this file".
			name: "a dot-import is reported, never silently zero",
			src: `package p
import . "os/exec"
func f() { _, _ = LookPath("docker") }`,
			dot: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sites, dotImported := execLookPathSites(file)
			if sites != tc.sites {
				t.Errorf("sites = %d, want %d", sites, tc.sites)
			}
			if dotImported != tc.dot {
				t.Errorf("dotImported = %v, want %v", dotImported, tc.dot)
			}
		})
	}
}

// The carry predicate is the second blocking gate over the same sources,
// and it judges a STATEMENT shape rather than a reference — so the shapes
// the tree actually writes are the ones it must read, each a case here.
func TestTheCarrySweepReadsTheShapesItClaims(t *testing.T) {
	const seam = `import "github.com/SocialGouv/iterion/internal/envtrust"` + "\n"
	for _, tc := range []struct {
		name     string
		src      string
		discards int
		dot      bool
	}{
		{
			// pkg/dryrun/shell.go's shape before this change, and
			// pkg/sandbox/factory.go's today.
			name:     "the path dropped into a blank",
			src:      "package p\n" + seam + `func f() error { _, err := envtrust.LookPath("dash"); return err }`,
			discards: 1,
		},
		{
			name:     "the same inside an if statement's init",
			src:      "package p\n" + seam + `func f() string { if _, err := envtrust.LookPath("docker"); err == nil { return "docker" }; return "" }`,
			discards: 1,
		},
		{
			name:     "both results dropped, the call standing alone",
			src:      "package p\n" + seam + `func f() { envtrust.LookPath("docker") }`,
			discards: 1,
		},
		{
			// The shape every converted site now has.
			name: "the path bound to a name is kept",
			src:  "package p\n" + seam + `func f() (string, error) { bin, err := envtrust.LookPath("dash"); return bin, err }`,
		},
		{
			name: "the path returned to the caller is kept",
			src:  "package p\n" + seam + `func f() (string, error) { return envtrust.LookPath("iterion") }`,
		},
		{
			// pkg/runtime/devbox_host.go's indirection.
			name: "the lookup wrapped in a function value is kept",
			src:  "package p\n" + seam + `var look = func() (string, error) { return envtrust.LookPath("devbox") }`,
		},
		{
			name:     "an aliased import of the seam is still the seam",
			src:      "package p\nimport et \"github.com/SocialGouv/iterion/internal/envtrust\"\nfunc f() error { _, err := et.LookPath(\"dash\"); return err }",
			discards: 1,
		},
		{
			// os/exec's own lookup is the OTHER sweep's business: this one
			// would otherwise accuse an exception already on the record.
			name: "a discarded os/exec lookup is not this sweep's class",
			src:  "package p\nimport \"os/exec\"\nfunc f() error { _, err := exec.LookPath(\"dash\"); return err }",
		},
		{
			name: "a LookPath on an unrelated package is not the seam",
			src:  "package p\nimport \"example.com/other\"\nfunc f() error { _, err := other.LookPath(\"dash\"); return err }",
		},
		{
			name: "a file that does not import the seam",
			src:  "package p\nfunc f(l interface{ LookPath(string) (string, error) }) { _, _ = l.LookPath(\"dash\") }",
		},
		{
			name: "a dot-import is reported, never silently zero",
			src:  "package p\nimport . \"github.com/SocialGouv/iterion/internal/envtrust\"\nfunc f() error { _, err := LookPath(\"dash\"); return err }",
			dot:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			discards, dotImported := seamLookPathDiscards(file)
			if discards != tc.discards {
				t.Errorf("discards = %d, want %d", discards, tc.discards)
			}
			if dotImported != tc.dot {
				t.Errorf("dotImported = %v, want %v", dotImported, tc.dot)
			}
		})
	}
}

// The sweeps subtract three directory names, and a sweep that judged the
// wrong trees would be green for the wrong reason. The walk's own rules are
// proven by internal/sweepwalk's tests; what belongs here is that the root
// it judges is the tree the test PROCESS runs in — this file's package
// directory is inside it, and the seam it guards is reachable from it.
func TestTheSweepJudgesTheTreeTheTestRunsIn(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	for _, rel := range []string{
		filepath.Join("internal", "envtrust", "lookpath.go"),
		filepath.Join("cmd", "iterion-desktop", "external_cli.go"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("the module root the sweep walks (%s) does not hold %s: %v — the sweep is judging another tree", root, rel, err)
		}
	}
}
