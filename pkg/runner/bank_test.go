package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{})

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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{})

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
	// The bank pushes through `origin` (live credential store) — break
	// THAT to exercise the refusal path.
	gitOut(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-remote.git"))

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{})

	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch != "" {
		t.Errorf("failed push still set FinalBranch %q", run.FinalBranch)
	}
	if !strings.Contains(run.FinalBranchError, "bank push") {
		t.Errorf("FinalBranchError = %q, want it to name the failed bank push", run.FinalBranchError)
	}
}

// The four integrity cases of an export-based sandbox (kubernetes): the
// host clone is a COPY of the pod workspace, so "HEAD == baseline" only
// means "no commits" when the pod-side capture agrees. Falsified both
// ways: the two loss shapes refuse loudly, the two verified shapes stay
// exactly as clean as before.

func bankedBranch(t *testing.T, origin, runID string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", origin, "rev-parse", "refs/heads/iterion/run-"+runID).CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

func TestBankRepoWorkspaceExportMismatchRefusesStaleTree(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	// The pod finished on a commit the export never delivered — the host
	// clone still reads the baseline.
	podHead := "feedfacefeedfacefeedfacefeedfacefeedface"

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: podHead})

	if got, ok := bankedBranch(t, origin, msg.RunID); ok {
		t.Errorf("a stale exported tree was banked anyway at %s", got)
	}
	run := loadRun(t, r, msg.RunID)
	if !strings.Contains(run.FinalBranchError, podHead) || !strings.Contains(run.FinalBranchError, "export") {
		t.Errorf("FinalBranchError = %q, want it to name the pod-side HEAD the export failed to deliver", run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceNoopRefusedWhenPodHeadUnknown(t *testing.T) {
	// Workspace at the baseline AND no pod-side truth: indistinguishable
	// from a lost export — never a silent clean no-op.
	r, msg, work, origin, base := bankFixture(t)

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, CaptureErr: "pod-side git rev-parse HEAD: pod gone"})

	if got, ok := bankedBranch(t, origin, msg.RunID); ok {
		t.Errorf("an unverifiable baseline tree was banked anyway at %s", got)
	}
	run := loadRun(t, r, msg.RunID)
	if !strings.Contains(run.FinalBranchError, "cannot tell") {
		t.Errorf("FinalBranchError = %q, want the refusal to say the no-op is unverifiable", run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceVerifiedNoopStaysClean(t *testing.T) {
	// The pod confirms HEAD == baseline: a genuine no-commit run must
	// stay a clean no-op — no branch, no error, no false alarm.
	r, msg, work, origin, base := bankFixture(t)

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: base})

	if got, ok := bankedBranch(t, origin, msg.RunID); ok {
		t.Errorf("a pod-confirmed no-op banked a branch at %s", got)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError != "" || run.FinalBranch != "" || run.FinalCommit != "" {
		t.Errorf("pod-confirmed no-op still recorded %q/%q/%q", run.FinalBranch, run.FinalCommit, run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceVerifiedWorkBanksNormally(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
	head := gitOut(t, work, "rev-parse", "HEAD")

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: head})

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
		t.Errorf("banked branch = %q (present=%v), want %s", got, ok, head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch == "" || run.FinalBranchError != "" {
		t.Errorf("verified work bank recorded %q with error %q", run.FinalBranch, run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceUnverifiedWorkStillBanks(t *testing.T) {
	// Capture failed but the exported tree HAS new commits: preserving
	// the visible work wins (the warn log carries the caveat).
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "salvaged work")
	head := gitOut(t, work, "rev-parse", "HEAD")

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, CaptureErr: "pod gone"})

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
		t.Errorf("banked branch = %q (present=%v), want %s", got, ok, head)
	}
	if run := loadRun(t, r, msg.RunID); run.FinalBranchError != "" {
		t.Errorf("salvage bank recorded an error anyway: %q", run.FinalBranchError)
	}
}

func TestBankRepoWorkspaceRefShadowRecoversBySHA(t *testing.T) {
	// The export delivered every object but a stale host loose ref
	// shadows the pod's packed ref: HEAD reads the baseline while the
	// pod's final commit IS present. The bank must push that exact
	// commit by SHA instead of refusing.
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
	podHead := gitOut(t, work, "rev-parse", "HEAD")
	// Park HEAD back on the baseline while the work commit stays present
	// (the ref-shadow read: rev-parse HEAD == base, object podHead exists).
	gitOut(t, work, "checkout", "-q", "--detach", base)

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: podHead})

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != podHead {
		t.Errorf("recovered branch = %q (present=%v), want the pod-side commit %s", got, ok, podHead)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch == "" || run.FinalCommit != podHead {
		t.Errorf("recovery recorded FinalBranch=%q FinalCommit=%q, want branch + %s", run.FinalBranch, run.FinalCommit, podHead)
	}
	if run.FinalBranchError != "" {
		t.Errorf("successful SHA recovery still recorded an error: %q", run.FinalBranchError)
	}
}


// The bank must push through the clone's `origin` remote — whose
// credential the mid-run refresher keeps LIVE — never through a URL
// carrying the claim-time token: a GitHub App installation token lives
// one hour and a paused run's bank then died on a dead credential
// (run 01a0335f-af54). Proven by divergence: origin points at a live
// bare, msg.RepoURL at a dead path — the old shape pushed at RepoURL
// and failed, the fixed one lands the branch on origin.
func TestBankPushesThroughOriginNotClaimTimeURL(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
	head := gitOut(t, work, "rev-parse", "HEAD")
	msg.RepoURL = filepath.Join(t.TempDir(), "dead-claim-url.git")

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{})

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
		t.Fatalf("bank landed %q (present=%v) on origin, want %s — the push must resolve through origin's live credential, not the claim-time URL", got, ok, head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch == "" || run.FinalBranchError != "" {
		t.Fatalf("bank recorded %q with error %q, want a clean banked branch", run.FinalBranch, run.FinalBranchError)
	}
}
