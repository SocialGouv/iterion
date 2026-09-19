package liveledger

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

// TestEveryLiveTestRecordsToTheLedger enforces the class invariant of
// #1422: every `test:live:*` Taskfile target that runs a Go test
// function must have that test function record to the last-green
// ledger, either directly (a `liveledger.Track(t)` call in the
// function's own body) or through `runBotLive` (which calls Track at
// its top). A new target added to the Taskfile without one of those two
// calls fires this test — a "silent live target" is the bug this exists
// to catch.
//
// The class enumerates from ONE source (Taskfile.yml), as #1422
// mandates; a hand list would drift.
func TestEveryLiveTestRecordsToTheLedger(t *testing.T) {
	root := repoRoot(t)
	taskfile := filepath.Join(root, TaskfileRelPath)
	targets, err := EnumerateLiveTargets(taskfile)
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}

	e2eDir := filepath.Join(root, "e2e")
	sources, err := readLiveTestSources(e2eDir)
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}
	declared := declaredTestFuncs(sources)
	hooks := bodyScopedHooks(sources)

	var specific []string
	var drifted []string
	for _, tt := range targets {
		sp, dr := classifyRunPattern(tt.RunPattern, declared)
		specific = append(specific, sp...)
		drifted = append(drifted, dr...)
	}
	specific = uniqueSorted(specific)
	if len(specific) < 10 {
		t.Fatalf("only %d target(s) named a specific test function — the Taskfile parser is probably wrong or the -run patterns changed shape", len(specific))
	}
	if len(drifted) > 0 {
		sort.Strings(drifted)
		t.Fatalf("the following -run pattern member(s) name no declared test function — the target would run ZERO tests while its ledger row waits forever for a run that cannot happen:\n  - %s",
			strings.Join(drifted, "\n  - "))
	}

	// The hook is a property of the TEST, not of the target: every
	// declared TestLive_* function runs under the `test:live` aggregate
	// (-run "TestLive_") whether or not a specific target names it, and
	// an unhooked member makes the aggregate's merged row lie (its
	// failure never reaches any tracker). Enforcement set = specific
	// targets' functions ∪ every declared TestLive_* function.
	mustHook := make(map[string]bool, len(specific))
	for _, fn := range specific {
		mustHook[fn] = true
	}
	for fn := range declared {
		if strings.HasPrefix(fn, "TestLive_") {
			mustHook[fn] = true
		}
	}

	var missing []string
	for fn := range mustHook {
		if !hooks[fn] {
			missing = append(missing, fn)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the following %d live test function(s) do not record to the ledger — call `liveledger.Track(t)` ONCE in the test body itself, or route the test through runBotLive:\n  - %s\n\nThis is enforced by the class rule of #1422: a new `task test:live:*` target that runs a test without ledger-recording is a silent live target the operator cannot see in `task test:live:status`.",
			len(missing), strings.Join(missing, "\n  - "))
	}
}

// classifyRunPattern resolves one -run pattern against the set of test
// functions the e2e sources declare. An alternative (patterns split on
// `|`, each stripped of its own ^/$ anchors) is:
//   - SPECIFIC when it is exactly a declared function — enforcement
//     scope;
//   - AGGREGATE when it is a prefix of at least one declared function
//     (a broad filter like "TestLive_Bot_") — out of scope here, each
//     member is enforced through its own specific target;
//   - DRIFT when it matches nothing — the target would run zero tests.
//
// Existence-in-source, not the presence of a `$` anchor, is what makes
// a pattern specific: half the real Taskfile's single-test targets
// write the pattern unquoted and unanchored, and an anchor-based rule
// skipped them all.
func classifyRunPattern(pattern string, declared map[string]bool) (specific, drifted []string) {
	for _, alt := range strings.Split(pattern, "|") {
		alt = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(alt), "$"), "^")
		if alt == "" {
			continue
		}
		switch {
		case declared[alt]:
			specific = append(specific, alt)
		case prefixesAny(alt, declared):
			// aggregate — out of enforcement scope
		default:
			drifted = append(drifted, alt)
		}
	}
	return specific, drifted
}

// prefixesAny reports whether prefix is a proper broad filter — a
// prefix of at least one declared function name.
func prefixesAny(prefix string, declared map[string]bool) bool {
	for name := range declared {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// declaredTestFuncs collects every function name the given test-file
// sources declare.
func declaredTestFuncs(sources map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, src := range sources {
		for _, m := range funcHeaderRE.FindAllStringSubmatch(src, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// bodyScopedHooks parses every source file and reports, per function,
// whether ITS OWN body reaches a recording hook: a liveledger.Track
// call or a runBotLive call. Body-scoped, not file-scoped — a sibling's
// hook must not vouch for a function that reaches none (measured:
// TestLive_FeatureDev and TestLive_VibeReviewAlternating were
// "covered" by their _Real sibling's Track while reaching no hook
// themselves, and an unanchored -run runs both variants in one process
// under one ledger row).
func bodyScopedHooks(sources map[string]string) map[string]bool {
	out := map[string]bool{}
	for name, src := range sources {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hooked := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "liveledger" && x.Sel.Name == "Track" {
						hooked = true
					}
				case *ast.Ident:
					if x.Name == "runBotLive" {
						hooked = true
					}
				}
				return !hooked
			})
			out[fn.Name.Name] = hooked
		}
	}
	return out
}

// readLiveTestSources reads every _test.go file under dir, keyed by
// file name.
func readLiveTestSources(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = string(b)
	}
	return out, nil
}

// funcHeaderRE matches "func <Name>(" — the test-function declaration.
var funcHeaderRE = regexp.MustCompile(`(?m)^func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// uniqueSorted dedupes and sorts names so the guard's messages are
// deterministic.
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// repoRoot walks up from the test's cwd until go.mod is found.
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
			t.Fatal("no go.mod on the way up")
		}
		dir = parent
	}
}

// classifyRunPattern's table, pinned: the shapes the real Taskfile
// writes today plus the two that escaped the previous rules (drift,
// alternation).
func TestClassifyRunPattern(t *testing.T) {
	declared := map[string]bool{"TestLive_A": true, "TestLive_Abc": true, "TestLive_B": true}

	if sp, dr := classifyRunPattern("TestLive_A$", declared); len(sp) != 1 || sp[0] != "TestLive_A" || len(dr) != 0 {
		t.Fatalf("anchored exact: sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("TestLive_A", declared); len(sp) != 1 || sp[0] != "TestLive_A" || len(dr) != 0 {
		t.Fatalf("unanchored exact (the Taskfile's most common shape): sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("TestLive_A$|TestLive_B$", declared); len(sp) != 2 || len(dr) != 0 {
		t.Fatalf("alternation of declared names: sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("TestLive_", declared); len(sp) != 0 || len(dr) != 0 {
		t.Fatalf("broad prefix is aggregate, not drift: sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("^$", declared); len(sp) != 0 || len(dr) != 0 {
		t.Fatalf("compile-only pattern is out of scope: sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("TestLive_Abcx$", declared); len(sp) != 0 || len(dr) != 1 || dr[0] != "TestLive_Abcx" {
		t.Fatalf("typo must be drift: sp=%v dr=%v", sp, dr)
	}
	if sp, dr := classifyRunPattern("TestLive_A$|TestLive_Nope$", declared); len(sp) != 1 || len(dr) != 1 || dr[0] != "TestLive_Nope" {
		t.Fatalf("mixed alternation: the drifted member must surface: sp=%v dr=%v", sp, dr)
	}
}

// The hook check is per BODY: two functions in one file, only one
// hooked — the file-scope rule this replaces passed both because a
// sibling carried the call.
func TestBodyScopedHooks(t *testing.T) {
	src := `package e2e

func TestLive_Hooked(t *testing.T) {
	liveledger.Track(t)
}

func TestLive_Unhooked(t *testing.T) {
	_ = t
}

func TestLive_ViaHarness(t *testing.T) {
	runBotLive(t, liveSpec{})
}
`
	hooks := bodyScopedHooks(map[string]string{"probe_live_test.go": src})
	if !hooks["TestLive_Hooked"] {
		t.Fatal("a direct Track in the body was not seen")
	}
	if hooks["TestLive_Unhooked"] {
		t.Fatal("an unhooked function was blessed — file-scope regression")
	}
	if !hooks["TestLive_ViaHarness"] {
		t.Fatal("runBotLive in the body was not seen")
	}
}
