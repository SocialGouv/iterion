package liveledger

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A synthetic Taskfile with a few live targets and one non-live target
// tests the parser without depending on the real Taskfile's evolving
// shape. Adding a new `test:live:*` target to the real Taskfile
// therefore does NOT redden this test — the real-Taskfile invariant is
// covered separately by TestEnumerateLiveTargets_RealTaskfile.
func TestEnumerateLiveTargets_ParsesRunPatterns(t *testing.T) {
	yamlContent := `
version: "3"

tasks:
  build:
    cmds:
      - go build ./...

  test:live:bot:review-pr:
    desc: 'a live target'
    cmds:
      - go test -v -tags live -count=1 -run 'TestLive_Bot_ReviewPR$' -timeout 45m ./e2e/...

  test:live:feat:permission:
    desc: 'another live target, double-quoted'
    cmds:
      - go test -tags live -count=1 -run "TestLive_Feat_Permission$" ./e2e/...

  test:live:aggregate:
    cmds:
      - go test -tags live -count=1 -run 'TestLive_' ./e2e/...

  test:live:unquoted:
    cmds:
      - go test -v -tags live -count=1 -run TestLive_Unquoted -timeout 15m ./e2e/...

  test:live:equals:
    cmds:
      - go test -v -tags live -count=1 -run=TestLive_Equals$ -timeout 15m ./e2e/...

  test:live:multitag:
    cmds:
      - go test -v -tags "live extra" -count=1 -run 'TestLive_MultiTag$' -timeout 15m ./e2e/...

  test:live:notest:
    cmds:
      - echo 'this target invokes no test binary'

  unrelated:
    cmds:
      - echo hi
`
	path := filepath.Join(t.TempDir(), "Taskfile.yml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := EnumerateLiveTargets(path)
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}
	byName := map[string]LiveTarget{}
	for _, tt := range got {
		byName[tt.Name] = tt
	}
	if len(got) != 7 {
		t.Fatalf("got %d live targets, want 7: %+v", len(got), got)
	}
	wantPatterns := map[string]string{
		"test:live:aggregate":       "TestLive_",
		"test:live:bot:review-pr":   "TestLive_Bot_ReviewPR$",
		"test:live:equals":          "TestLive_Equals$",
		"test:live:feat:permission": "TestLive_Feat_Permission$",
		"test:live:multitag":        "TestLive_MultiTag$",
		"test:live:notest":          "",
		"test:live:unquoted":        "TestLive_Unquoted",
	}
	for name, want := range wantPatterns {
		got2, ok := byName[name]
		if !ok {
			t.Fatalf("target %s missing from enumeration", name)
		}
		if got2.RunPattern != want {
			t.Fatalf("%s pattern = %q, want %q", name, got2.RunPattern, want)
		}
	}
	for _, name := range []string{"aggregate", "bot:review-pr", "equals", "feat:permission", "multitag", "unquoted"} {
		tt := byName["test:live:"+name]
		if !tt.Records {
			t.Fatalf("%s must classify as recording", tt.Name)
		}
	}
	if byName["test:live:notest"].Records {
		t.Fatalf("a target that runs no test must classify as non-recording")
	}
}

// The ledger's row set is the set of RECORDING targets — the class of
// non-recording targets is pinned exactly, because each entry here is a
// decision with a reason:
//
//   - test:live:bots / test:live:bots-real delegate every cmd through
//     `task:` and the sub-task overrides {{.TASK}}, so the parent's
//     name never reaches a Track call — the sub-targets are the record;
//   - test:live:status runs the (read-only) status binary, no test;
//   - test:live:compile runs `go test -run '^$'` — compiles, runs
//     nothing;
//   - test:live:quality:unit runs the quality engine's unit tests —
//     go test, but neither -tags live nor ./e2e, so no Track hook.
//
// A target that joins this list without a reason, or a recording target
// that lands here, must redden — the enumeration decides which rows the
// committed ledger carries.
func TestEnumerateLiveTargets_RealTaskfile_RecordsClassification(t *testing.T) {
	got, err := EnumerateLiveTargets(realTaskfilePath(t))
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}
	var non []string
	for _, tt := range got {
		if !tt.Records {
			non = append(non, tt.Name)
		}
	}
	want := []string{
		"test:live:bots",
		"test:live:bots-real",
		"test:live:compile",
		"test:live:quality:unit",
		"test:live:status",
	}
	if !slices.Equal(non, want) {
		t.Fatalf("non-recording set = %v, want %v — the ledger seeds rows only for recording targets", non, want)
	}
}

// The real Taskfile at the checkout root parses AND has more than one
// live target. If a future refactor moves the file or breaks the YAML
// shape the parser reads, this test surfaces it before the ledger goes
// silent. The specific count is not the assertion (the real Taskfile
// evolves); presence + non-zero is.
func TestEnumerateLiveTargets_RealTaskfile(t *testing.T) {
	path := realTaskfilePath(t)
	got, err := EnumerateLiveTargets(path)
	if err != nil {
		t.Fatalf("EnumerateLiveTargets(%s): %v", path, err)
	}
	if len(got) < 10 {
		t.Fatalf("EnumerateLiveTargets(%s) returned %d targets, want at least 10 — the parser missed live targets", path, len(got))
	}
	for _, tt := range got {
		if !strings.HasPrefix(tt.Name, LiveTargetPrefix) {
			t.Errorf("target %q slipped through the prefix filter", tt.Name)
		}
	}
}

// realTaskfilePath resolves the checkout's Taskfile.yml from the test's
// working directory. Tests always run under their package directory,
// so we walk up until we see go.mod.
func realTaskfilePath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, TaskfileRelPath)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found — cannot locate the repository root")
		}
		dir = parent
	}
}
