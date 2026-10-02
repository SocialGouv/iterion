package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPerformMerge_ConflictPath drives the full happy path of the
// conflict-resolver chain end-to-end at the service layer:
//
//  1. Seed a repo with a head branch + a divergent storage branch
//     that conflicts on the squash.
//  2. PerformMergeCtx hits the conflict — returns the typed error,
//     persists MergeStatusConflicted, stashes the pending message
//     and target.
//  3. GetMergeConflicts returns the right files + content.
//  4. ResolveMergeConflictFile rejects an out-of-set path and
//     accepts the real one.
//  5. FinalizeMergeAfterConflict commits, flips status to merged.
func TestPerformMerge_ConflictPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")

	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Build the repo with a real conflict between main and a storage
	// branch shaped like "iterion/run/<run id>".
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("file.txt", "alpha\nbravo\ncharlie\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	// Storage branch (the equivalent of iterion/run/<run id>).
	runGit("checkout", "-qb", "iterion/run/test-conflict")
	writeRepo("file.txt", "alpha\nBRAVO-INCOMING\ncharlie\ndelta-incoming\n")
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	// Back to main, with a divergent change that will conflict.
	runGit("checkout", "-q", "main")
	writeRepo("file.txt", "alpha\nbravo-main\ncharlie\n")
	runGit("commit", "-qam", "main-change")

	// Seed the run record so PerformMergeCtx has something to load.
	ctx := context.Background()
	runID := "run-conflict-test"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/test-conflict"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 1. Trigger the merge — expect a conflict error.
	_, mergeErr := svc.PerformMergeCtx(ctx, runID, MergeRequest{})
	if mergeErr == nil {
		t.Fatal("expected merge to fail with conflict")
	}
	if !strings.Contains(mergeErr.Error(), "conflict") {
		t.Errorf("error message %q should mention conflict", mergeErr.Error())
	}

	// 2. Run should now be MergeStatusConflicted.
	loaded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-conflict: %v", err)
	}
	if loaded.MergeStatus != store.MergeStatusConflicted {
		t.Fatalf("MergeStatus=%q, want conflicted", loaded.MergeStatus)
	}
	if loaded.PendingMergeMessage == "" {
		t.Error("PendingMergeMessage should be set")
	}
	if loaded.PendingMergeInto != "main" {
		t.Errorf("PendingMergeInto=%q, want main", loaded.PendingMergeInto)
	}

	// 3. GetMergeConflicts returns the right files.
	det, err := svc.GetMergeConflicts(ctx, runID)
	if err != nil {
		t.Fatalf("GetMergeConflicts: %v", err)
	}
	if len(det.Files) != 1 {
		t.Fatalf("Files=%d, want 1", len(det.Files))
	}
	if det.Files[0].Path != "file.txt" {
		t.Errorf("Path=%q, want file.txt", det.Files[0].Path)
	}
	if len(det.Files[0].Hunks) != 1 {
		t.Errorf("Hunks=%d, want 1", len(det.Files[0].Hunks))
	}

	// 4. Path validation: out-of-set path is rejected.
	if err := svc.ResolveMergeConflictFile(ctx, runID, "other.txt", "x"); err == nil {
		t.Error("expected out-of-set path to be rejected")
	}

	// Real path: accepted — the run's own version of the file, the only
	// resolution a contract file the verdict names can carry.
	resolved := "alpha\nBRAVO-INCOMING\ncharlie\ndelta-incoming\n"
	if err := svc.ResolveMergeConflictFile(ctx, runID, "file.txt", resolved); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}

	// Conflict set is now empty.
	det2, err := svc.GetMergeConflicts(ctx, runID)
	if err != nil {
		t.Fatalf("GetMergeConflicts post-resolve: %v", err)
	}
	if len(det2.Files) != 0 {
		t.Errorf("after resolve Files=%d, want 0", len(det2.Files))
	}

	// The verdict the landing answers to: judged at the run's own landing
	// commit, whose file.txt carries the resolution's exact bytes.
	judged := storageSHA
	loaded2, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun for the verdict: %v", err)
	}
	loaded2.Checkpoint = &store.Checkpoint{Outputs: map[string]map[string]any{
		"lot_verify": {"contract_head": judged, "contract_tree": `{"table":{"file.txt":{"w":"file:x"}},"rest":{"count":0,"digest":"x"}}`},
	}}
	if err := st.SaveRun(ctx, loaded2); err != nil {
		t.Fatalf("SaveRun with the verdict: %v", err)
	}

	// 5. Finalize commits the squash and flips status.
	res, err := svc.FinalizeMergeAfterConflict(ctx, runID, "")
	if err != nil {
		t.Fatalf("FinalizeMergeAfterConflict: %v", err)
	}
	if res.MergeStatus != store.MergeStatusMerged {
		t.Errorf("MergeStatus=%q, want merged", res.MergeStatus)
	}
	if res.MergedCommit == "" {
		t.Error("MergedCommit should be set")
	}
	if res.MergedInto != "main" {
		t.Errorf("MergedInto=%q, want main", res.MergedInto)
	}

	// Verify run.json reflects the final state with pending fields
	// cleared.
	final, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun final: %v", err)
	}
	if final.PendingMergeMessage != "" {
		t.Errorf("PendingMergeMessage should be cleared, got %q", final.PendingMergeMessage)
	}
	if final.PendingMergeInto != "" {
		t.Errorf("PendingMergeInto should be cleared, got %q", final.PendingMergeInto)
	}

	// HEAD on main should reflect the resolved squash.
	headFile, err := os.ReadFile(filepath.Join(repoDir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt post-merge: %v", err)
	}
	if string(headFile) != resolved {
		t.Errorf("file.txt content=%q, want %q", string(headFile), resolved)
	}
}

// TestAbortMergeConflict_RestoresWorktree exercises the abort path:
// after a conflict + abort, the worktree is back at main's clean
// state and merge_status flips to failed (not conflicted).
func TestAbortMergeConflict_RestoresWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")

	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("file.txt", "alpha\nbravo\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	runGit("checkout", "-qb", "iterion/run/abort-test")
	writeRepo("file.txt", "alpha\nBRAVO-INCOMING\n")
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	runGit("checkout", "-q", "main")
	writeRepo("file.txt", "alpha\nbravo-main\n")
	runGit("commit", "-qam", "main-change")
	mainCleanContent := "alpha\nbravo-main\n"

	ctx := context.Background()
	runID := "run-abort-test"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/abort-test"
	r.Status = store.RunStatusFinished
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil {
		t.Fatal("expected conflict")
	}

	if err := svc.AbortMergeConflict(ctx, runID); err != nil {
		t.Fatalf("AbortMergeConflict: %v", err)
	}
	loaded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-abort: %v", err)
	}
	if loaded.MergeStatus != store.MergeStatusFailed {
		t.Errorf("MergeStatus=%q, want failed (abort should not leave conflicted state)", loaded.MergeStatus)
	}
	if loaded.PendingMergeMessage != "" {
		t.Errorf("PendingMergeMessage should be cleared post-abort, got %q", loaded.PendingMergeMessage)
	}
	// Worktree should be back to main's clean state.
	head, err := os.ReadFile(filepath.Join(repoDir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(head) != mainCleanContent {
		t.Errorf("worktree not restored: got %q, want %q", string(head), mainCleanContent)
	}
}

func captureGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gittest.Run(t, dir, args...)
}

// The re-judge compares the staged resolution with the commit the gate's
// word landed on — not with the pre-flip head the verifier judged: the run's
// own `done` plan differs from it by exactly the verdict's one line, and a
// conflicted landing of a converged run carries that flip.
func TestFinalizeMergeAfterConflict_LandsTheRunsOwnDonePlan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("file.txt", "alpha\nbravo\ncharlie\n")
	if err := os.MkdirAll(filepath.Join(repoDir, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: todo\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	// The run's branch: the lot's work, and mark_done's flip committed.
	runGit("checkout", "-qb", "iterion/run/test-done-plan")
	writeRepo("file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n")
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: done\n")
	runGit("commit", "-qam", "the lot and the gate's word")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "main")
	writeRepo("file.txt", "alpha\nbravo-main\ncharlie\n")
	runGit("commit", "-qam", "main-change")

	ctx := context.Background()
	runID := "run-done-plan-test"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/test-done-plan"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil {
		t.Fatal("expected merge to fail with conflict")
	}
	// The resolver takes the run's version of the conflicted file; the plan
	// rides staged as the run's own flip wrote it.
	if err := svc.ResolveMergeConflictFile(ctx, runID, "file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n"); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}

	// The verdict: judged at the pre-flip base, landing on the run's commit.
	loaded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun for the verdict: %v", err)
	}
	loaded.Checkpoint = &store.Checkpoint{Outputs: map[string]map[string]any{
		"lot_verify": {"contract_head": baseSHA,
			"contract_tree": `{"table":{".modernize/plan.yaml":{"w":"file:p"},"file.txt":{"w":"file:f"}},"rest":{"count":0,"digest":"x"}}`},
	}}
	if err := st.SaveRun(ctx, loaded); err != nil {
		t.Fatalf("SaveRun with the verdict: %v", err)
	}

	res, err := svc.FinalizeMergeAfterConflict(ctx, runID, "")
	if err != nil {
		t.Fatalf("the run's own done plan was refused on its landing: %v", err)
	}
	if res.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("MergeStatus=%q, want merged", res.MergeStatus)
	}
	landed := strings.TrimSpace(captureGitOutput(t, repoDir, "show", res.MergedCommit+":.modernize/plan.yaml"))
	if !strings.Contains(landed, "status: done") {
		t.Fatalf("the landing lost the gate's word:\n%s", landed)
	}
}

// stalePairSetup is the fixture the stale-pair tests share: a repo whose
// main moved both a product file and the plan, a storage branch carrying
// attempt 1's landing (C1: the plan with the stale marker), attempt 2's
// landing C2 on a since-deleted scratch ref (the plan re-recorded), and a
// run doc whose FinalCommit is C1 — the pair the merge meets.
func stalePairSetup(t *testing.T) (svc *Service, repoDir, runID, c1, c2 string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir = filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("file.txt", "alpha\nbravo\ncharlie\n")
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: todo\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	runGit("checkout", "-qb", "iterion/run/stale-pair")
	writeRepo("file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n")
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: done\n    note: stale-attempt-one\n")
	runGit("commit", "-qam", "attempt one's done")
	c1 = strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-qb", "cg-scratch-attempt-two")
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: done\n    note: fresh-attempt-two\n")
	runGit("commit", "-qam", "attempt two's done")
	c2 = strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "iterion/run/stale-pair")
	runGit("branch", "-qD", "cg-scratch-attempt-two")
	runGit("checkout", "-q", "main")
	writeRepo("file.txt", "alpha\nbravo-main\ncharlie\n")
	writeRepo(".modernize/plan.yaml", "lots:\n  - id: L1\n    status: todo\n    owner-edited: true\n")
	runGit("commit", "-qam", "main-change")

	ctx := context.Background()
	runID = "run-stale-pair"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = c1
	r.FinalBranch = "iterion/run/stale-pair"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}
	svc, err = NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil {
		t.Fatal("expected merge to fail with conflict")
	}
	loaded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun for the verdict: %v", err)
	}
	loaded.Checkpoint = &store.Checkpoint{Outputs: map[string]map[string]any{
		"lot_verify": {"contract_head": c2,
			"contract_tree": `{"table":{".modernize/plan.yaml":{"w":"file:p"},"file.txt":{"w":"file:f"}},"rest":{"count":0,"digest":"x"}}`},
	}}
	if err := st.SaveRun(ctx, loaded); err != nil {
		t.Fatalf("SaveRun with the verdict: %v", err)
	}
	return svc, repoDir, runID, c1, c2
}

// A stale pair — an earlier attempt's landing (FinalCommit=C1) with a later
// attempt's verdict (judged at C2, its bank push failed) — is refused by
// name in both directions: the landing's bytes were never judged, and the
// verdict's own bytes are not on the landing either. The nominal flow (the
// verdict judged the head the landing carries) passes the guard.
func TestFinalizeMergeAfterConflict_RefusesAStalePair(t *testing.T) {
	svc, _, runID, _, _ := stalePairSetup(t)
	ctx := context.Background()

	// The resolver takes attempt 1's bytes — what the landing carries.
	if err := svc.ResolveMergeConflictFile(ctx, runID, "file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n"); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}
	resolved := "lots:\n  - id: L1\n    status: done\n    note: stale-attempt-one\n"
	if err := svc.ResolveMergeConflictFile(ctx, runID, ".modernize/plan.yaml", resolved); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}
	_, err := svc.FinalizeMergeAfterConflict(ctx, runID, "")
	if err == nil || !strings.Contains(err.Error(), "the verdict is stale for this landing") {
		t.Fatalf("a stale pair landed, or was refused for another cause: err=%v", err)
	}
}

// The same stale pair, resolved with the VERDICT's own bytes (attempt two's
// plan): still refused — the landing commit does not carry the head the
// verdict judged, so the comparison has no honest base.
func TestFinalizeMergeAfterConflict_RefusesTheVerdictsOwnBytesOnAStalePair(t *testing.T) {
	svc, repoDir, runID, _, c2 := stalePairSetup(t)
	ctx := context.Background()

	resolved := strings.TrimSpace(captureGitOutput(t, repoDir, "show", c2+":.modernize/plan.yaml"))
	if err := svc.ResolveMergeConflictFile(ctx, runID, "file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n"); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}
	if err := svc.ResolveMergeConflictFile(ctx, runID, ".modernize/plan.yaml", resolved); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}
	_, err := svc.FinalizeMergeAfterConflict(ctx, runID, "")
	if err == nil || !strings.Contains(err.Error(), "the verdict is stale for this landing") {
		t.Fatalf("a stale pair landed, or was refused for another cause: err=%v", err)
	}
}

// The landing is re-judged against the stored verdict: a resolution of a
// contract file the verdict names must carry the bytes that verdict judged.
// Whoever fills the resolver, a landing that rewrites the contract is refused
// by name — the owner changes the contract between runs, by hand.
func TestFinalizeMergeAfterConflict_RefusesAResolutionThatRewritesTheContract(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")

	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	writeRepo := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	writeRepo("file.txt", "alpha\nbravo\ncharlie\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	runGit("checkout", "-qb", "iterion/run/test-rewrite")
	writeRepo("file.txt", "alpha\nBRAVO-INCOMING\ncharlie\n")
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "main")
	writeRepo("file.txt", "alpha\nbravo-main\ncharlie\n")
	runGit("commit", "-qam", "main-change")

	ctx := context.Background()
	runID := "run-rewrite-test"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.BaseCommit = baseSHA
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/test-rewrite"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil {
		t.Fatal("expected merge to fail with conflict")
	}
	// The resolver takes neither side: a rewrite nobody judged.
	if err := svc.ResolveMergeConflictFile(ctx, runID, "file.txt", "alpha\nthe resolver's own\ncharlie\n"); err != nil {
		t.Fatalf("ResolveMergeConflictFile: %v", err)
	}
	// The verdict's judged HEAD carries the run's version of file.txt.
	resolvedSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "iterion/run/test-rewrite"))
	loaded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun for the verdict: %v", err)
	}
	loaded.Checkpoint = &store.Checkpoint{Outputs: map[string]map[string]any{
		"lot_verify": {"contract_head": strings.TrimSpace(resolvedSHA),
			"contract_tree": `{"table":{"file.txt":{"w":"file:x"}},"rest":{"count":0,"digest":"x"}}`},
	}}
	if err := st.SaveRun(ctx, loaded); err != nil {
		t.Fatalf("SaveRun with the verdict: %v", err)
	}

	_, err = svc.FinalizeMergeAfterConflict(ctx, runID, "")
	if err == nil {
		t.Fatal("the landing committed a resolution that rewrites the contract the verdict judged")
	}
	if !strings.Contains(err.Error(), "not the contract the verdict judged") {
		t.Fatalf("the refusal does not name the rewritten contract: %v", err)
	}
}
