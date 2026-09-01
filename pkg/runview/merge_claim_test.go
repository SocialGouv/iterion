package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedMergeableRun builds a real repo whose storage branch squashes
// cleanly onto main, plus a run record pointing at it. Returns the
// store, the service and the run id.
func seedMergeableRun(t *testing.T) (*store.FilesystemRunStore, *Service, string) {
	t.Helper()
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
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	write("file.txt", "alpha\n")
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	baseSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))

	runGit("checkout", "-qb", "iterion/run/claim-test")
	write("file.txt", "alpha\nbravo\n")
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "main")

	ctx := context.Background()
	runID := "run-claim-test"
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
	r.FinalBranch = "iterion/run/claim-test"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun seed: %v", err)
	}

	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return st, svc, runID
}

// Two concurrent PerformMergeCtx calls on the same run: exactly one
// wins; the loser is rejected AT THE CLAIM (or sees "already merged"),
// and the persisted record carries the winner's outcome. Before the
// claim existed this was the measured double-squash TOCTOU: both
// callers passed the merged-check, both built a squash, and the loser
// overwrote the winner's "merged" with "failed".
func TestPerformMerge_ConcurrentClaims(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.PerformMergeCtx(ctx, runID, MergeRequest{})
		}(i)
	}
	wg.Wait()

	okCount := 0
	for _, e := range errs {
		if e == nil {
			okCount++
			continue
		}
		msg := e.Error()
		if !strings.Contains(msg, "merge in progress") && !strings.Contains(msg, "already merged") {
			t.Errorf("loser error should be a claim rejection, got: %v", e)
		}
	}
	if okCount != 1 {
		t.Fatalf("exactly one merge must win, got %d (errs: %v)", okCount, errs)
	}

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("MergeStatus=%q, want merged — the loser overwrote the winner", r.MergeStatus)
	}
	if r.MergedCommit == "" || r.MergedInto != "main" {
		t.Fatalf("merged bookkeeping incomplete: commit=%q into=%q", r.MergedCommit, r.MergedInto)
	}
}

// A fresh "merging" claim held by someone else rejects the caller
// without touching git or the record.
func TestPerformMerge_HeldClaimRejects(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	claimed, prior, _, err := st.ClaimMerge(ctx, runID, time.Now().Add(-mergeClaimStaleAfter))
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	if prior != "" {
		t.Fatalf("prior=%q, want empty (never merged)", prior)
	}

	_, mergeErr := svc.PerformMergeCtx(ctx, runID, MergeRequest{})
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "merge in progress") {
		t.Fatalf("want claim rejection, got: %v", mergeErr)
	}
	r, _ := st.LoadRun(ctx, runID)
	if r.MergeStatus != store.MergeStatusMerging {
		t.Fatalf("MergeStatus=%q, want merging untouched", r.MergeStatus)
	}
}

// A stale "merging" (the previous claimant crashed mid-merge) does not
// wedge the run: the next caller steals the claim and completes.
func TestPerformMerge_StaleClaimIsStolen(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	if claimed, _, _, err := st.ClaimMerge(ctx, runID, time.Now().Add(-mergeClaimStaleAfter)); err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	// Age the claim past the staleness bound.
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.MergeClaimedAt = time.Now().Add(-mergeClaimStaleAfter - time.Minute)
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun age claim: %v", err)
	}

	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err != nil {
		t.Fatalf("stale claim must be stealable, got: %v", err)
	}
	r, _ = st.LoadRun(ctx, runID)
	if r.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("MergeStatus=%q, want merged", r.MergeStatus)
	}
}

// An attempt that dies before any outcome (unresolvable repo root)
// releases the claim and restores the pre-claim state — the run does
// not stay wedged in "merging".
func TestPerformMerge_EarlyErrorReleasesClaim(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.RepoRoot = ""
	r.WorkDir = filepath.Join(t.TempDir(), "gone")
	r.MergeStatus = store.MergeStatusPending
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	_, mergeErr := svc.PerformMergeCtx(ctx, runID, MergeRequest{})
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "no resolvable repo root") {
		t.Fatalf("want repo-root error, got: %v", mergeErr)
	}
	r, _ = st.LoadRun(ctx, runID)
	if r.MergeStatus != store.MergeStatusPending {
		t.Fatalf("MergeStatus=%q, want pending restored (claim released)", r.MergeStatus)
	}
}

// Once "merged" is persisted, no conditional writer expecting an
// in-flight state can clobber it — the exact overwrite the TOCTOU
// allowed (loser persisting "failed" over the winner's "merged").
func TestUpdateRunMergeIf_CannotClobberMerged(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	changed, err := st.UpdateRunMergeIf(ctx, runID,
		store.RunMergeUpdate{Status: store.MergeStatusFailed},
		[]store.MergeStatus{store.MergeStatusMerging})
	if err != nil {
		t.Fatalf("UpdateRunMergeIf: %v", err)
	}
	if changed {
		t.Fatal("a writer expecting 'merging' must not overwrite 'merged'")
	}
	r, _ := st.LoadRun(ctx, runID)
	if r.MergeStatus != store.MergeStatusMerged || r.MergedCommit == "" {
		t.Fatalf("merged record damaged: status=%q commit=%q", r.MergeStatus, r.MergedCommit)
	}
}

// A run RecoverFinalize left as "skipped" stays mergeable — on main it
// was, and refusing it would strand every recovered run.
func TestPerformMerge_SkippedIsMergeable(t *testing.T) {
	st, svc, runID := seedMergeableRun(t)
	ctx := context.Background()

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.MergeStatus = store.MergeStatusSkipped
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err != nil {
		t.Fatalf("a skipped run must stay mergeable, got: %v", err)
	}
	r, _ = st.LoadRun(ctx, runID)
	if r.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("MergeStatus=%q, want merged", r.MergeStatus)
	}
}

// cancelSensitiveStore refuses merge-state writes on a dead context —
// the behaviour of a real remote store (Mongo), which the filesystem
// store cannot exhibit because it ignores ctx.
type cancelSensitiveStore struct {
	store.RunStore
}

func (c cancelSensitiveStore) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, expectedFrom []store.MergeStatus) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.RunStore.UpdateRunMergeIf(ctx, id, upd, expectedFrom)
}

// The merge outcome must not ride the request context: once the claim
// is held, a client disconnect (cancelled request ctx) must neither
// lose the outcome nor leak the claim. Against a ctx-honouring store,
// a merge driven by an already-cancelled request still lands and
// persists — proof the state writes are detached.
func TestPerformMerge_OutcomeSurvivesRequestCancel(t *testing.T) {
	st, _, runID := seedMergeableRun(t)

	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(cancelSensitiveStore{st}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.PerformMergeCtx(cancelled, runID, MergeRequest{}); err != nil {
		t.Fatalf("merge under a cancelled request ctx must still persist, got: %v", err)
	}
	r, _ := st.LoadRun(context.Background(), runID)
	if r.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("MergeStatus=%q, want merged (outcome lost to the request ctx)", r.MergeStatus)
	}
}
