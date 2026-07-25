package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func finalizeRecoveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func seedFinishedRecoveryWorktree(t *testing.T, runID string) (store.RunStore, string, string, string) {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	finalizeRecoveryGit(t, repo, "init", "-b", "main")
	finalizeRecoveryGit(t, repo, "config", "user.email", "test@example.com")
	finalizeRecoveryGit(t, repo, "config", "user.name", "Test")
	finalizeRecoveryGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	finalizeRecoveryGit(t, repo, "add", "README.md")
	finalizeRecoveryGit(t, repo, "commit", "-m", "base")
	baseSHA := finalizeRecoveryGit(t, repo, "rev-parse", "HEAD")

	st, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	wt := filepath.Join(st.Root(), "worktrees", runID)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	worktreeAuthoritySince := time.Now().UTC()
	finalizeRecoveryGit(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
	})
	if err := os.WriteFile(filepath.Join(wt, "result.go"), []byte("package result\n"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	finalizeRecoveryGit(t, wt, "add", "result.go")
	finalizeRecoveryGit(t, wt, "commit", "-m", "feat: recovered result")
	finalSHA := finalizeRecoveryGit(t, wt, "rev-parse", "HEAD")

	r, err := st.CreateRun(ctx, runID, "recovery-lock-test", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.Name = runID
	r.Status = store.RunStatusFinished
	r.Worktree = true
	r.WorkDir = wt
	r.RepoRoot = repo
	r.BaseCommit = baseSHA
	r.WorktreeCreatedAt = worktreeAuthoritySince
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return st, repo, wt, finalSHA
}

func assertRecoveryPending(t *testing.T, st store.RunStore, runID, wt string) {
	t.Helper()
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" {
		t.Fatalf("live finalization was raced: %+v", r)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("live worktree was removed: %v", err)
	}
}

func assertRecoveryComplete(t *testing.T, st store.RunStore, repo, runID, wt, finalSHA string) {
	t.Helper()
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.FinalCommit != finalSHA || r.FinalBranch == "" {
		t.Fatalf("recovery metadata mismatch: %+v, want commit %s", r, finalSHA)
	}
	if got := finalizeRecoveryGit(t, repo, "rev-parse", r.FinalBranch); got != finalSHA {
		t.Fatalf("storage branch tip=%q, want %q", got, finalSHA)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("recovered worktree still exists: %v", err)
	}
}

// A terminal status can become visible before the live engine's deferred
// finalizer runs. The in-process manager is therefore a liveness authority even
// though the row already says "finished".
func TestReconcileOrphans_DoesNotRecoverManagerActiveFinishedRun(t *testing.T) {
	const runID = "run_finished_manager_active"
	st, repo, wt, finalSHA := seedFinishedRecoveryWorktree(t, runID)
	svc := &Service{store: st, manager: NewManager(), logger: iterlog.Nop()}
	if _, err := svc.manager.Register(context.Background(), runID); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	svc.reconcileOrphans()
	assertRecoveryPending(t, st, runID, wt)

	svc.manager.Deregister(runID)
	svc.reconcileOrphans()
	assertRecoveryComplete(t, st, repo, runID, wt, finalSHA)
}

// A different Iterion process is invisible to this Service's manager but holds
// the shared per-run flock. Recovery must acquire that lock before touching Git.
func TestReconcileOrphans_DoesNotRecoverLockedFinishedRun(t *testing.T) {
	const runID = "run_finished_lock_held"
	st, repo, wt, finalSHA := seedFinishedRecoveryWorktree(t, runID)
	svc := &Service{store: st, manager: NewManager(), logger: iterlog.Nop()}
	lock, err := st.LockRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}

	svc.reconcileOrphans()
	assertRecoveryPending(t, st, runID, wt)

	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	svc.reconcileOrphans()
	assertRecoveryComplete(t, st, repo, runID, wt, finalSHA)
}
