package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=iterion", "GIT_AUTHOR_EMAIL=iterion@example.test",
		"GIT_COMMITTER_NAME=iterion", "GIT_COMMITTER_EMAIL=iterion@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// snapshotAs mimics what the engine records at a boundary: stage
// everything, commit the tree onto HEAD without moving HEAD, point a ref
// at it.
func snapshotAs(t *testing.T, dir, ref string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	tree := gitIn(t, dir, "write-tree")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	commit := gitIn(t, dir, "commit-tree", tree, "-p", head, "-m", "snapshot "+ref)
	gitIn(t, dir, "update-ref", ref, commit)
	gitIn(t, dir, "reset", "--mixed", "HEAD")
	return commit
}

// TestReviewScope_GroupsByNodeAndKeepsUnattributedWork is the core
// contract of the gate-range design.
//
// The RANGE is a workspace before/after, so it contains everything the run
// did — including work by node kinds that record no boundary (subbots,
// fan-out branches, computes). Grouping by node is presentation on top;
// what cannot be attributed must still appear, or a reviewer approves less
// than what changed.
func TestReviewScope_GroupsByNodeAndKeepsUnattributedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	wt := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	writeIn(t, wt, "README.md", "base\n")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	base := gitIn(t, wt, "rev-parse", "HEAD")

	const runID = "run-review-scope"

	// --- gate 0: the run starts here.
	snapshotAs(t, wt, store.ReviewGateRef(runID, 0))

	// `implement` runs: bracketed by both boundaries.
	snapshotAs(t, wt, store.NodePreSnapshotRef(runID, "implement", 0))
	writeIn(t, wt, "src/feature.go", "package main // by implement\n")
	snapshotAs(t, wt, store.NodeSnapshotRef(runID, "implement", 0))

	// A subbot runs: NO boundary refs at all — the case the per-node
	// design could not cover.
	writeIn(t, wt, "docs/from_subbot.md", "written by a delegated child\n")

	// `write_docs` runs: bracketed.
	snapshotAs(t, wt, store.NodePreSnapshotRef(runID, "write_docs", 0))
	writeIn(t, wt, "docs/guide.md", "by write_docs\n")
	snapshotAs(t, wt, store.NodeSnapshotRef(runID, "write_docs", 0))

	// --- gate 1: the reviewer is paused here.
	snapshotAs(t, wt, store.ReviewGateRef(runID, 1))

	run := &store.Run{ID: runID, WorkDir: wt, BaseCommit: base}
	scope := buildReviewScope(run, -1)

	if !scope.Available {
		t.Fatalf("scope unavailable: %s", scope.Reason)
	}
	if scope.GateSeq != 1 {
		t.Errorf("GateSeq = %d, want the latest gate (1)", scope.GateSeq)
	}
	// Completeness first: every file changed since gate 0 is in the range.
	if scope.TotalFiles != 3 {
		t.Fatalf("TotalFiles = %d, want 3 (feature.go, from_subbot.md, guide.md)", scope.TotalFiles)
	}

	seen := map[string]string{} // path -> group label
	for _, g := range scope.Groups {
		for _, f := range g.Files {
			seen[f.Path] = g.Label
		}
	}
	if got := seen["src/feature.go"]; got != "implement" {
		t.Errorf("src/feature.go grouped under %q, want implement", got)
	}
	if got := seen["docs/guide.md"]; got != "write_docs" {
		t.Errorf("docs/guide.md grouped under %q, want write_docs", got)
	}
	// The subbot's file has no boundary to attribute it to — it must still
	// be shown, under the catch-all.
	got := seen["docs/from_subbot.md"]
	if got == "" {
		t.Fatal("the subbot's file vanished from the review — a reviewer would approve work they never saw")
	}
	if !strings.Contains(got, "Other changes") {
		t.Errorf("docs/from_subbot.md grouped under %q, want the catch-all group", got)
	}
	// The groups partition the range exactly.
	var total int
	for _, g := range scope.Groups {
		total += len(g.Files)
	}
	if total != scope.TotalFiles {
		t.Errorf("groups hold %d files, range has %d — grouping must partition, not filter", total, scope.TotalFiles)
	}
}

// TestReviewScope_RangeStartsAtPreviousGate: a second reviewer sees only
// what happened since the first approved, not the whole run.
func TestReviewScope_RangeStartsAtPreviousGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	wt := t.TempDir()
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	writeIn(t, wt, "README.md", "base\n")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	base := gitIn(t, wt, "rev-parse", "HEAD")

	const runID = "run-two-gates"
	snapshotAs(t, wt, store.ReviewGateRef(runID, 0))
	writeIn(t, wt, "phase_one.md", "approved by the first reviewer\n")
	snapshotAs(t, wt, store.ReviewGateRef(runID, 1))
	writeIn(t, wt, "phase_two.md", "the second reviewer's business\n")
	snapshotAs(t, wt, store.ReviewGateRef(runID, 2))

	run := &store.Run{ID: runID, WorkDir: wt, BaseCommit: base}
	scope := buildReviewScope(run, -1)
	if !scope.Available {
		t.Fatalf("unavailable: %s", scope.Reason)
	}
	if scope.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d, want 1 — the second gate must not re-show what the first approved", scope.TotalFiles)
	}
	if scope.Groups[0].Files[0].Path != "phase_two.md" {
		t.Errorf("range shows %q, want phase_two.md", scope.Groups[0].Files[0].Path)
	}

	// And an explicit gate selects its own range.
	first := buildReviewScope(run, 1)
	if !first.Available || first.TotalFiles != 1 || first.Groups[0].Files[0].Path != "phase_one.md" {
		t.Errorf("gate 1 range = %+v, want just phase_one.md", first.Groups)
	}
}

// TestReviewScope_ReportsWhyItIsEmpty: a review panel that shows nothing
// without saying why is worse than no panel.
func TestReviewScope_ReportsWhyItIsEmpty(t *testing.T) {
	scope := buildReviewScope(&store.Run{ID: "no-workspace"}, -1)
	if scope.Available {
		t.Fatal("expected unavailable for a run with no workspace")
	}
	if !strings.Contains(scope.Reason, "no workspace") {
		t.Errorf("Reason = %q, want it to name the missing workspace", scope.Reason)
	}

	wt := t.TempDir()
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	writeIn(t, wt, "a.txt", "x")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")

	scope = buildReviewScope(&store.Run{ID: "no-gates", WorkDir: wt}, -1)
	if scope.Available {
		t.Fatal("expected unavailable when no gate was reached")
	}
	if !strings.Contains(scope.Reason, "no review gate") {
		t.Errorf("Reason = %q, want it to say no gate was reached", scope.Reason)
	}
}
