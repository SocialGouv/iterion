package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/liveledger"
)

// fixtureTaskfile is a minimal Taskfile with one recording live target
// and one non-recording one (a delegate), plus a ledger that carries a
// row for neither.
func fixtureTaskfile() string {
	return `version: "3"
tasks:
  test:live:bot:review-pr:
    cmds:
      - go test -v -tags live -count=1 -run 'TestLive_Bot_ReviewPR$' -timeout 45m ./e2e/...
  test:live:bots:
    cmds:
      - task: test:live:bot:review-pr
`
}

// fixtureLedger is a valid empty ledger.
func fixtureLedger() string {
	return "{\"schema\":1,\"rows\":[]}\n"
}

// runFrom executes the CLI's run in dir (t.Chdir) and returns exit code
// and stderr.
func runFrom(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	t.Chdir(dir)
	code := run(args, &bytes.Buffer{}, &stderr)
	return code, stderr.String()
}

// A status read is a READ: a ledger missing rows for enumerated targets
// must NOT be rewritten as a side effect of asking the question — the
// committed file's bytes stay untouched, and the hint on stderr is the
// operator's only trace.
func TestRun_ReadOnlyDoesNotDirtyTheLedger(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "Taskfile.yml")
	if err := os.WriteFile(tf, []byte(fixtureTaskfile()), 0o644); err != nil {
		t.Fatal(err)
	}
	lp := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(lp, []byte(fixtureLedger()), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}

	code, stderr := runFrom(t, dir, "-ledger", lp, "-taskfile", tf)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "missing from the ledger") {
		t.Fatalf("read-only hint missing from stderr: %s", stderr)
	}
	after, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("a read rewrote the ledger file:\nbefore=%s\nafter=%s", before, after)
	}
}

// Seeding is EXPLICIT: with -write-back the same invocation persists the
// missing never rows into the file.
func TestRun_WriteBackSeedsMissingRows(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "Taskfile.yml")
	if err := os.WriteFile(tf, []byte(fixtureTaskfile()), 0o644); err != nil {
		t.Fatal(err)
	}
	lp := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(lp, []byte(fixtureLedger()), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stderr := runFrom(t, dir, "-ledger", lp, "-taskfile", tf, "-write-back")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	l, err := liveledger.Load(lp)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := l.Get("test:live:bot:review-pr")
	if !ok {
		t.Fatal("recording target was not seeded")
	}
	if row.Verdict != liveledger.VerdictNever {
		t.Fatalf("seeded verdict %q, want never", row.Verdict)
	}
	if _, ok := l.Get("test:live:bots"); ok {
		t.Fatal("the delegating target was seeded — non-recording targets carry no row")
	}
}
