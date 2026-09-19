package liveledger

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryLiveTestRecordsToTheLedger enforces the class invariant of
// #1422: every `test:live:*` Taskfile target that runs a Go test
// function must have that test function record to the last-green
// ledger, either directly (a `liveledger.Track(t)` call in the
// function body) or transitively through `runBotLive` (which calls
// Track at its top). A new target added to the Taskfile without one
// of those two calls fires this test — a "silent live target" is the
// bug this exists to catch.
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

	// Aggregate targets whose -run pattern names no specific test
	// function (empty pattern, or a broad prefix like "TestLive_") are
	// out of scope — they run whatever passes the wider filter, and
	// each of those tests is enforced on its own row.
	specific := map[string]bool{} // test function name -> seen
	for _, tt := range targets {
		name := testFuncNameFromRun(tt.RunPattern)
		if name == "" {
			continue
		}
		specific[name] = true
	}
	if len(specific) < 10 {
		t.Fatalf("only %d target(s) named a specific test function — the Taskfile parser is probably wrong or the -run patterns changed shape", len(specific))
	}

	e2eDir := filepath.Join(root, "e2e")
	sources, err := readLiveTestSources(e2eDir)
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}

	var missing []string
	for fn := range specific {
		if hasLedgerHook(sources, fn) {
			continue
		}
		missing = append(missing, fn)
	}
	if len(missing) > 0 {
		t.Fatalf("the following %d live test function(s) do not record to the ledger — add `liveledger.Track(t)` at the top of each, or ensure they go through runBotLive:\n  - %s\n\nThis is enforced by the class rule of #1422: a new `task test:live:*` target that runs a test without ledger-recording is a silent live target the operator cannot see in `task test:live:status`.",
			len(missing), strings.Join(missing, "\n  - "))
	}
}

// testFuncNameFromRun strips the `$` anchor from a `-run` pattern and
// returns the test function name if the pattern names a specific one.
// A pattern like "TestLive_" (no anchor) is treated as aggregate and
// returns "" (out of scope for the enforcement above).
func testFuncNameFromRun(runPattern string) string {
	if runPattern == "" {
		return ""
	}
	// A specific-function pattern ends in `$` to anchor the match.
	if !strings.HasSuffix(runPattern, "$") {
		return ""
	}
	name := strings.TrimSuffix(runPattern, "$")
	// A pattern like "^TestLive_Foo$" (with leading anchor) is common.
	name = strings.TrimPrefix(name, "^")
	// Guard: the result must look like a Go identifier prefixed with
	// TestLive_; anything else is a wider filter we don't enforce.
	if !strings.HasPrefix(name, "TestLive_") {
		return ""
	}
	return name
}

// readLiveTestSources concatenates every _test.go file's source under
// e2e/ into ONE string keyed by nothing — the enforcement only needs
// substring matches ("func <name>(", `liveledger.Track`, `runBotLive`)
// scoped to the same file, so we return a map filename → source.
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
// A test could be declared with a receiver, but Go tests never are, so
// this shape is sufficient.
var funcHeaderRE = regexp.MustCompile(`(?m)^func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// hasLedgerHook returns true when the source file that DECLARES fn also
// contains either a direct liveledger.Track call OR a runBotLive call.
// The check is per-file, not per-function-body: a test file with even
// ONE runBotLive-using test implies the harness is in scope, and the
// alternative (parse Go source into ASTs to scope to fn's body) is
// expensive and out of proportion to the enforcement. Adding a NEW
// test to a file whose siblings already have the hook is genuinely
// safe — the reflex is "call runBotLive" in almost every case, and
// the guard reddens the moment the file has ONLY tests that skip the
// harness AND do not call Track directly.
func hasLedgerHook(sources map[string]string, fn string) bool {
	// First, find the file that declares fn.
	declRE := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(fn) + `\s*\(`)
	for _, src := range sources {
		if !declRE.MatchString(src) {
			continue
		}
		if strings.Contains(src, "liveledger.Track(") {
			return true
		}
		if strings.Contains(src, "runBotLive(") {
			return true
		}
		return false
	}
	// Not found at all — the -run pattern names a function the source
	// tree does not declare, which is a separate class of bug.
	return false
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

// Placeholder assertion to silence "regexp declared and not used" if
// the guard's constants ever collapse to zero usage during refactor.
var _ = funcHeaderRE
