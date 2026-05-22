package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
//
// This is the regression test for the whole feature — if anything
// breaks in the parsing, persistence, or commit chain, this fails.
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
	// branch shaped like "iterion/run/<friendly>".
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"LC_ALL=C",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\noutput: %s", args, err, string(out))
		}
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

	// Storage branch (the equivalent of iterion/run/<friendly>).
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

	// Real path: accepted.
	resolved := "alpha\nresolved\ncharlie\ndelta-resolved\n"
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
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"LC_ALL=C",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\noutput: %s", args, err, string(out))
		}
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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
