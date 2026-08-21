package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bankRepoWorkspace is the repo-targeted twin of worktree finalization:
// a successful run's commits must land on the forge as a per-run branch,
// or a finished cloud run's work exists nowhere the server can reach.
// Falsified both ways: work is pushed and recorded; no work is a clean
// no-op; a push failure is recorded as FinalBranchError, never silence.

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bankFixture(t *testing.T) (r *Runner, msg *queue.RunMessage, work, origin string, base string) {
	t.Helper()
	tmp := t.TempDir()
	origin = filepath.Join(tmp, "origin.git")
	gitOut(t, tmp, "init", "--bare", origin)
	work = filepath.Join(tmp, "clone")
	gitOut(t, tmp, "clone", origin, work)
	gitOut(t, work, "config", "user.email", "t@test.invalid")
	gitOut(t, work, "config", "user.name", "t")
	gitOut(t, work, "commit", "--allow-empty", "-m", "baseline")
	base = gitOut(t, work, "rev-parse", "HEAD")

	st, err := store.New(filepath.Join(tmp, "store"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	msg = &queue.RunMessage{RunID: "run-bank-1", TenantID: "team-a", RepoURL: origin}
	if serr := st.SaveRun(context.Background(), &store.Run{ID: msg.RunID, TenantID: msg.TenantID}); serr != nil {
		t.Fatalf("seed run: %v", serr)
	}
	r = &Runner{cfg: Config{Logger: iterlog.Nop(), Store: st}}
	return r, msg, work, origin, base
}

func loadRun(t *testing.T, r *Runner, id string) *store.Run {
	t.Helper()
	run, err := r.cfg.Store.LoadRun(store.WithIdentity(context.Background(), "team-a", ""), id)
	if err != nil || run == nil {
		t.Fatalf("load run: %v", err)
	}
	return run
}

func TestBankRepoWorkspacePushesAndRecords(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
	head := gitOut(t, work, "rev-parse", "HEAD")

	r.bankRepoWorkspace(context.Background(), msg, work, base)

	branchHead := gitOut(t, origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID)
	if branchHead != head {
		t.Errorf("banked branch at %s, want %s", branchHead, head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch != "iterion/run-"+msg.RunID || run.FinalCommit != head {
		t.Errorf("FinalBranch/FinalCommit = %q/%q, want branch + %s", run.FinalBranch, run.FinalCommit, head)
	}
	if run.FinalBranchError != "" {
		t.Errorf("unexpected FinalBranchError: %q", run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceNoWorkIsNoop(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)

	r.bankRepoWorkspace(context.Background(), msg, work, base)

	if out, err := exec.Command("git", "-C", origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID).CombinedOutput(); err == nil {
		t.Errorf("a workless run banked a branch anyway: %s", out)
	}
	if run := loadRun(t, r, msg.RunID); run.FinalBranch != "" || run.FinalCommit != "" {
		t.Errorf("no-op bank still recorded %q/%q", run.FinalBranch, run.FinalCommit)
	}
}

func TestBankRepoWorkspacePushFailureIsNamed(t *testing.T) {
	r, msg, work, _, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "work")
	msg.RepoURL = filepath.Join(t.TempDir(), "no-such-remote.git")

	r.bankRepoWorkspace(context.Background(), msg, work, base)

	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch != "" {
		t.Errorf("failed push still set FinalBranch %q", run.FinalBranch)
	}
	if !strings.Contains(run.FinalBranchError, "bank push") {
		t.Errorf("FinalBranchError = %q, want it to name the failed bank push", run.FinalBranchError)
	}
}
