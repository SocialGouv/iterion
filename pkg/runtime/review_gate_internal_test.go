package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The producer half of the review-gate mechanism had no coverage at all
// (Revi's R931159): every test in pkg/server hand-seeds the refs, so
// nothing verified that the ENGINE writes them, that the memoisation
// hands the companion and the human the same anchor, that the sequence
// continues across a resume instead of clobbering gate/0, or that
// reviewGateRange recovers the sequence from git when rs.gateAnchors is
// empty — which is every resumed review turn.

func gitRun(t *testing.T, dir string, args ...string) string {
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

// gateRepo is a git worktree with one commit, ready to be anchored.
func gateRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "iterion@example.test")
	gitRun(t, dir, "config", "user.name", "iterion")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = dir
	return cmd.Run() == nil
}

func TestMarkReviewGate_WritesTheRefAndIsIdempotent(t *testing.T) {
	dir := gateRepo(t)
	e := &Engine{workDir: dir, workflow: &ir.Workflow{}}
	rs := &runState{runID: "run-gate", isWorktree: true, loopCounters: map[string]int{}}

	first := e.markReviewGate(rs, "review")
	if first == nil {
		t.Fatal("markReviewGate returned nothing — the gate range has no anchor")
	}
	ref0 := store.ReviewGateRef(rs.runID, 0)
	if !refExists(t, dir, ref0) {
		t.Fatalf("%s was not written — the human panel resolves the range from this ref", ref0)
	}

	// The companion anchors when the gate STARTS and the pause asks again:
	// both must get the SAME seq, or the two judge different ranges.
	second := e.markReviewGate(rs, "review")
	if first["gate_seq"] != second["gate_seq"] {
		t.Errorf("gate_seq moved between the companion's anchor and the pause: %v → %v",
			first["gate_seq"], second["gate_seq"])
	}
	if refExists(t, dir, store.ReviewGateRef(rs.runID, 1)) {
		t.Error("a second anchor was taken for the same gate — the range would restart mid-review")
	}
}

func TestNextReviewGateSeq_ContinuesAcrossAResume(t *testing.T) {
	dir := gateRepo(t)
	const runID = "run-seq"
	if got := nextReviewGateSeq(dir, runID); got != 0 {
		t.Fatalf("first gate seq = %d, want 0", got)
	}
	gitRun(t, dir, "update-ref", store.ReviewGateRef(runID, 0), "HEAD")
	if got := nextReviewGateSeq(dir, runID); got != 1 {
		t.Errorf("second gate seq = %d, want 1 — restarting at 0 clobbers gate/0 "+
			"and the second reviewer re-reads the first's approved work", got)
	}
	gitRun(t, dir, "update-ref", store.ReviewGateRef(runID, 1), "HEAD")
	// A resume rebuilds runState from the checkpoint, which carries no
	// gateAnchors — the sequence must still come from git.
	if got := nextReviewGateSeq(dir, runID); got != 2 {
		t.Errorf("post-resume gate seq = %d, want 2", got)
	}
	// Another run's gates are not counted.
	if got := nextReviewGateSeq(dir, "other-run"); got != 0 {
		t.Errorf("unrelated run's gate seq = %d, want 0", got)
	}
}

func TestReviewGateRange_RecoversSeqWhenAnchorsAreEmpty(t *testing.T) {
	dir := gateRepo(t)
	const runID = "run-range"
	gitRun(t, dir, "update-ref", store.ReviewGateRef(runID, 0), "HEAD")
	gitRun(t, dir, "update-ref", store.ReviewGateRef(runID, 1), "HEAD")

	e := &Engine{workDir: dir, workflow: &ir.Workflow{}}
	// The resumed shape: fresh runState, gateAnchors empty.
	rs := &runState{runID: runID, isWorktree: true, loopCounters: map[string]int{}}
	wt := &worktreeContext{originalTip: "HEAD~0"}

	base, head := e.reviewGateRange(rs, wt)
	wantHead := store.ReviewGateRef(runID, 1)
	wantBase := store.ReviewGateRef(runID, 0)
	if head != wantHead || base != wantBase {
		t.Errorf("range = %s..%s, want %s..%s — an empty gateAnchors fell back to "+
			"originalTip..HEAD, so the companion judged the whole run while the "+
			"human panel read gate/0..gate/1", base, head, wantBase, wantHead)
	}
}
