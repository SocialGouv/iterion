package liveledger

import (
	"os"
	"path/filepath"
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
	if len(got) != 4 {
		t.Fatalf("got %d live targets, want 4: %+v", len(got), got)
	}
	if got[0].Name != "test:live:aggregate" || got[0].RunPattern != "TestLive_" {
		t.Fatalf("aggregate: %+v", got[0])
	}
	if got[1].Name != "test:live:bot:review-pr" || got[1].RunPattern != "TestLive_Bot_ReviewPR$" {
		t.Fatalf("review-pr: %+v", got[1])
	}
	if got[2].Name != "test:live:feat:permission" || got[2].RunPattern != "TestLive_Feat_Permission$" {
		t.Fatalf("permission: %+v", got[2])
	}
	if got[3].Name != "test:live:notest" || got[3].RunPattern != "" {
		t.Fatalf("notest: %+v", got[3])
	}
}

// TargetNames is a small helper — its shape (names extracted, order
// preserved) is contract for callers that feed EnsureNeverRows.
func TestTargetNames_PreservesOrder(t *testing.T) {
	in := []LiveTarget{
		{Name: "test:live:a"},
		{Name: "test:live:b"},
		{Name: "test:live:c"},
	}
	got := TargetNames(in)
	if len(got) != 3 || got[0] != "test:live:a" || got[1] != "test:live:b" || got[2] != "test:live:c" {
		t.Fatalf("TargetNames = %v, want [a b c]", got)
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
