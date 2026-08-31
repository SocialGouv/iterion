package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

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
	return refAt(t, origin, "refs/heads/iterion/run-"+runID)
}

func refAt(t *testing.T, origin, ref string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", origin, "rev-parse", ref).CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

func TestBankRepoWorkspaceExportMismatchRefusesStaleTree(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	// The pod finished on a commit the export never delivered — the host
	// clone still reads the baseline.
	podHead := "feedfacefeedfacefeedfacefeedfacefeedface"

	r.bankRepoWorkspace(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: podHead}, "finished")

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
		runtime.WorkspaceIntegrity{Applicable: true, CaptureErr: "pod-side git rev-parse HEAD: pod gone"}, "finished")

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
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: base}, "finished")

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
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: head}, "finished")

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
		runtime.WorkspaceIntegrity{Applicable: true, CaptureErr: "pod gone"}, "finished")

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
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: podHead}, "finished")

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

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
		t.Fatalf("bank landed %q (present=%v) on origin, want %s — the push must resolve through origin's live credential, not the claim-time URL", got, ok, head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch == "" || run.FinalBranchError != "" {
		t.Fatalf("bank recorded %q with error %q, want a clean banked branch", run.FinalBranch, run.FinalBranchError)
	}
}

// The bank contract by classified outcome: a finished run banks, and so
// do the deaths whose successor would otherwise restart from the base
// commit (budget_exceeded, failed) — measured as nine manual
// store-snapshot recoveries in three days of one campaign before this.
// Pauses resume in place, an interrupted delivery re-clones and banks on
// its next attempt, a cancel is the operator refusing the work: none of
// those bank. Keyed on classifyExecResult's output so the
// budget-beats-interrupted precedence transfers verbatim.
func TestBankableStatusFollowsClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"finished banks", nil, true},
		{"budget exceeded banks", runtime.ErrBudgetExceeded, true},
		{"wrapped budget exceeded banks", fmt.Errorf("%w: duration (14401/14400)", runtime.ErrBudgetExceeded), true},
		{"bare runtime-error budget code banks", &runtime.RuntimeError{
			Code: runtime.ErrCodeBudgetExceeded, Message: "budget hard limit reached",
		}, true},
		{"generic failure banks", errors.New("boom"), true},
		{"paused does not bank", runtime.ErrRunPaused, false},
		{"operator pause does not bank", runtime.ErrRunPausedOperator, false},
		{"interrupted does not bank", runtime.ErrRunInterrupted, false},
		{"cancelled does not bank", runtime.ErrRunCancelled, false},
		// The one case a sentinel-order reimplementation would get wrong:
		// an interruption that also carries a spent budget classifies as
		// budget_exceeded — acked, never redelivered — so skipping the
		// bank here strands the work with no next attempt to bank it.
		{"budget-carrying interruption banks like a budget death",
			errors.Join(runtime.ErrRunInterrupted, runtime.ErrBudgetExceeded), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bankableStatus(classifyExecResult(c.err, "run-1").finalStatus)
			if got != c.want {
				t.Errorf("bankable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A budget death banks exactly like a success: branch on the forge,
// FinalCommit/FinalBranch on the run doc — so `runs merge` works on a
// dead run, and an operator can merge or branch from the banked head
// instead of replaying the snapshot. (Resume itself still re-clones at
// base — anchoring the successor on the banked head is follow-on work.)
func TestBankOnBudgetDeathLandsTheBranch(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "work paid for before the cap")
	head := gitOut(t, work, "rev-parse", "HEAD")

	deathErr := fmt.Errorf("%w: duration (14401/14400)", runtime.ErrBudgetExceeded)
	status := classifyExecResult(deathErr, msg.RunID).finalStatus
	if !bankableStatus(status) {
		t.Fatal("a budget death must be bankable")
	}
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, status)

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
		t.Fatalf("banked %q (present=%v), want %s on the forge", got, ok, head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch != "iterion/run-"+msg.RunID || run.FinalCommit != head || run.FinalBranchError != "" {
		t.Fatalf("run doc = branch %q commit %q err %q, want the banked branch recorded clean", run.FinalBranch, run.FinalCommit, run.FinalBranchError)
	}
}

// The gate and the bank, tested TOGETHER against a real bare remote —
// the adversarial pass proved by mutation that every direct
// bankRepoWorkspace test is blind to the gate (both `if false` and a
// revert to success-only passed the whole package). This table is the
// one that goes red.
func TestBankGateWiredToOutcome(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantBanked bool
	}{
		{"finished banks", nil, true},
		{"budget death banks", fmt.Errorf("%w: duration (14401/14400)", runtime.ErrBudgetExceeded), true},
		{"generic failure banks", errors.New("boom"), true},
		{"paused does not bank", runtime.ErrRunPaused, false},
		{"interrupted does not bank", runtime.ErrRunInterrupted, false},
		{"cancelled does not bank", runtime.ErrRunCancelled, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, msg, work, origin, base := bankFixture(t)
			gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
			head := gitOut(t, work, "rev-parse", "HEAD")

			r.bankIfBankable(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, c.err)

			got, present := bankedBranch(t, origin, msg.RunID)
			if c.wantBanked && (!present || got != head) {
				t.Fatalf("outcome %v must bank: branch %q (present=%v), want %s", c.err, got, present, head)
			}
			if !c.wantBanked && present {
				t.Fatalf("outcome %v must NOT bank, yet the branch exists at %q", c.err, got)
			}
		})
	}
}

// A redelivered attempt that ends with a strictly poorer chain must not
// force-push over the richer branch an earlier attempt banked: attempt 1
// banks two commits; attempt 2 re-clones from base, produces one
// divergent commit, and its bank is refused — branch and run doc keep
// pointing at the richer chain.
func TestBankRefusesToClobberRicherAttempt(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	work2 := filepath.Join(t.TempDir(), "attempt2")
	gitOut(t, filepath.Dir(work2), "clone", work, work2)
	gitOut(t, work2, "config", "user.email", "t@test.invalid")
	gitOut(t, work2, "config", "user.name", "t")
	gitOut(t, work2, "remote", "set-url", "origin", origin)

	gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1: one")
	gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1: two")
	richHead := gitOut(t, work, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "budget_exceeded")

	gitOut(t, work2, "commit", "--allow-empty", "-m", "attempt 2: poorer divergent retry")
	r.bankRepoWorkspace(context.Background(), msg, work2, base, runtime.WorkspaceIntegrity{}, "failed")

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != richHead {
		t.Fatalf("branch = %q (present=%v), want the richer attempt-1 head %s kept", got, ok, richHead)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalCommit != richHead {
		t.Fatalf("FinalCommit = %q, want the richer head %s kept", run.FinalCommit, richHead)
	}
	// The refusal must not be log-only: the doc now names a head THIS
	// attempt never produced, and nothing else on it says the two diverge.
	ev := findEvent(t, r, msg.RunID, store.EventRunBankRefused)
	if ev == nil {
		t.Fatal("a refused bank left no run_bank_refused on the timeline — the dropped head is invisible outside the pod log")
	}
	poorHead := gitOut(t, work2, "rev-parse", "HEAD")
	if got := ev.Data["kept_head"]; got != richHead {
		t.Errorf("kept_head = %v, want the richer banked head %s", got, richHead)
	}
	if got := ev.Data["dropped_head"]; got != poorHead {
		t.Errorf("dropped_head = %v, want this attempt's head %s", got, poorHead)
	}
	// FinalBranchError stays empty on purpose: its documented meaning is
	// "FinalCommit has no persistent branch guarding it", and here the pair
	// is valid and mergeable — just a different attempt's. Overloading it
	// would raise an alarming branch failure over a perfectly good branch.
	if run.FinalBranchError != "" {
		t.Errorf("FinalBranchError = %q, want the field pair left coherent and the divergence carried by the event", run.FinalBranchError)
	}
}

func findEvent(t *testing.T, r *Runner, runID string, typ store.EventType) *store.Event {
	t.Helper()
	evs, err := r.cfg.Store.LoadEvents(store.WithIdentity(context.Background(), "team-a", ""), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, e := range evs {
		if e.Type == typ {
			return e
		}
	}
	return nil
}

// A resume that carried the banked work FORWARD (descendant head)
// supersedes: the branch advances.
func TestBankDescendantSupersedes(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "first outcome")
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	gitOut(t, work, "commit", "--allow-empty", "-m", "resume carried it further")
	newHead := gitOut(t, work, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != newHead {
		t.Fatalf("branch = %q (present=%v), want advanced to %s", got, ok, newHead)
	}
}

// An operator cancel that lands while the bank is in flight must not
// become merge-eligible: the branch may reach the forge, but the run doc
// — the surface `runs merge` trusts — stays unrecorded.
func TestBankLeavesCancelledRunUnrecorded(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "work the operator then refused")
	run := loadRun(t, r, msg.RunID)
	run.Status = store.RunStatusCancelled
	if err := r.cfg.Store.SaveRun(store.WithIdentity(context.Background(), msg.TenantID, ""), run); err != nil {
		t.Fatalf("flip to cancelled: %v", err)
	}

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "failed")

	if _, present := bankedBranch(t, origin, msg.RunID); !present {
		t.Fatalf("the push itself precedes the doc check and should have landed")
	}
	after := loadRun(t, r, msg.RunID)
	if after.FinalBranch != "" || after.FinalCommit != "" {
		t.Fatalf("cancelled run recorded FinalBranch=%q FinalCommit=%q, want both empty (not merge-eligible)", after.FinalBranch, after.FinalCommit)
	}
}

// The wall-clock death is the class the death bank most exists for, and
// the one the run ctx cannot serve: executeRun deadlines ctx, the engine
// returns a sentinel-free error (classified `failed`, hence bankable),
// and exec.CommandContext refuses to Start on a done ctx — so every git
// op of the bank fails instantly and the branch never reaches the forge.
// The detach is NARROW on purpose: a ctx cancelled for lease loss or an
// operator cancel must stay dead, because another pod may already own
// the run. The cause is the oracle, never the returned error — the
// interrupted path LOSES its sentinel when the store write behind it
// fails (run_failure.go falls back to failRun), which is exactly the
// shape the second case pins.
func TestBankDetachesOnlyFromOurOwnDeadline(t *testing.T) {
	deadlined := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		t.Cleanup(cancel)
		<-ctx.Done()
		return ctx
	}
	causedCancel := func(cause error) func() context.Context {
		return func() context.Context {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(cause)
			t.Cleanup(func() { cancel(nil) })
			return ctx
		}
	}
	cases := []struct {
		name       string
		ctx        func() context.Context
		err        error
		wantBanked bool
	}{
		{"our own wall-clock deadline still banks", deadlined, errors.New("timeout: context deadline exceeded"), true},
		// The sentinel-free shape: FailRunResumable failed, so the engine
		// returned a bare RuntimeError and classification says `failed`.
		// Only the cancel CAUSE still tells us the lease may have moved.
		{"lease loss does not bank even when the sentinel was lost",
			causedCancel(runtime.ErrRunInterrupted), errors.New("boom"), false},
		{"operator cancel does not bank", causedCancel(runtime.ErrRunCancelled), errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, msg, work, origin, base := bankFixture(t)
			gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
			head := gitOut(t, work, "rev-parse", "HEAD")

			r.bankIfBankable(c.ctx(), msg, work, base, runtime.WorkspaceIntegrity{}, c.err)

			got, present := bankedBranch(t, origin, msg.RunID)
			if c.wantBanked && (!present || got != head) {
				t.Fatalf("must bank: branch %q (present=%v), want %s", got, present, head)
			}
			if !c.wantBanked && present {
				t.Fatalf("must NOT bank, yet the branch exists at %q", got)
			}
			if c.wantBanked {
				if run := loadRun(t, r, msg.RunID); run.FinalBranchError != "" {
					t.Fatalf("FinalBranchError = %q, want the bank to have run on a live ctx", run.FinalBranchError)
				}
			}
		})
	}
}

// A bankable death naks, so a redelivered attempt banks into the SAME run
// doc — and `runs merge` reads FinalBranch and FinalCommit TOGETHER
// (PerformDeferredMerge takes BranchToMerge + FinalSHA; BuildSquashMessage
// resolves FinalCommit in a clone that only fetched the branch). The two
// orders a second attempt can arrive in must each leave the trio coherent.
func TestBankKeepsTheRunDocCoherentAcrossAttempts(t *testing.T) {
	t.Run("a failed second push never orphans the first attempt's pair", func(t *testing.T) {
		r, msg, work, _, base := bankFixture(t)
		gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1")
		bankedHead := gitOut(t, work, "rev-parse", "HEAD")
		r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

		// Attempt 2 produces a head that never reaches the forge.
		gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 2, unreachable forge")
		gitOut(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-remote.git"))
		r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

		run := loadRun(t, r, msg.RunID)
		if run.FinalBranch != "iterion/run-"+msg.RunID || run.FinalCommit != bankedHead {
			t.Fatalf("branch %q / commit %q — want attempt 1's forge-backed pair %s intact, not a SHA the merge clone cannot resolve",
				run.FinalBranch, run.FinalCommit, bankedHead)
		}
		// The failure goes on the timeline, NOT on FinalBranchError: the
		// studio hides the branch row whenever the error field is set, so
		// naming this attempt's failure there would hide a branch that
		// exists on the forge and merges.
		if run.FinalBranchError != "" {
			t.Fatalf("FinalBranchError = %q — a later attempt's push failure must not mask the valid banked pair", run.FinalBranchError)
		}
	})

	t.Run("a clean bank clears an earlier attempt's recorded failure", func(t *testing.T) {
		r, msg, work, origin, base := bankFixture(t)
		gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1, unreachable forge")
		good := gitOut(t, work, "remote", "get-url", "origin")
		gitOut(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-remote.git"))
		r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")
		if run := loadRun(t, r, msg.RunID); run.FinalBranchError == "" {
			t.Fatal("precondition: the first attempt must have recorded a bank failure")
		}

		gitOut(t, work, "remote", "set-url", "origin", good)
		gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 2 lands")
		head := gitOut(t, work, "rev-parse", "HEAD")
		r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

		if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != head {
			t.Fatalf("branch %q (present=%v), want %s", got, ok, head)
		}
		run := loadRun(t, r, msg.RunID)
		if run.FinalBranchError != "" {
			t.Fatalf("FinalBranchError = %q, want a clean bank to clear the earlier refusal", run.FinalBranchError)
		}
		if run.FinalCommit != head || run.FinalBranch != "iterion/run-"+msg.RunID {
			t.Fatalf("branch %q / commit %q, want the freshly banked pair", run.FinalBranch, run.FinalCommit)
		}
	})
}

// runGitOutEnv returns COMBINED output, so the ls-remote parse has to
// survive whatever git writes to stderr on a network read. Taking the
// first token of the output turns a redirect warning into the "prior
// head", whose fetch then fails and collapses the anti-clobber guard
// into the blind force-push it exists to prevent.
func TestParseLsRemoteHeadIgnoresStderrNoise(t *testing.T) {
	const sha = "9a1b2c3d4e5f60718293a4b5c6d7e8f901234567"
	branch := "iterion/run-run-bank-1"
	cases := []struct {
		name, out, want string
	}{
		{"clean advertisement", sha + "\trefs/heads/" + branch + "\n", sha},
		{"redirect warning first",
			"warning: redirecting to https://forge.example/org/repo.git/\n" + sha + "\trefs/heads/" + branch + "\n", sha},
		{"remote banner first",
			"remote: Announcing a scheduled maintenance window\n" + sha + "\trefs/heads/" + branch + "\n", sha},
		{"branch absent, noise only", "warning: redirecting to https://forge.example/x\n", ""},
		{"a different ref is not this branch",
			sha + "\trefs/heads/iterion/run-someone-else\n", ""},
		{"empty output", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseLsRemoteHead(c.out, branch); got != c.want {
				t.Errorf("parseLsRemoteHead = %q, want %q", got, c.want)
			}
		})
	}
}

// The richer-chain guard is a read-compare-write — ls-remote, compare,
// force-push — with nothing making it atomic. A sibling attempt that
// advances the branch in between is clobbered anyway, which is exactly
// the loss the guard exists to prevent. The lease moves the decision
// into the push itself.
func TestBankPushArgsLeaseTheComparedRefState(t *testing.T) {
	const branch, head, old = "iterion/run-x", "aaaa1111", "bbbb2222"
	t.Run("a known prior head is leased", func(t *testing.T) {
		got := strings.Join(bankPushArgs(branch, head, old), " ")
		want := "push --force-with-lease=refs/heads/" + branch + ":" + old + " origin " + head + ":refs/heads/" + branch
		if got != want {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})
	t.Run("an unknown prior head keeps failing OPEN, never leases absence", func(t *testing.T) {
		// "" as the lease <expect> means "the ref must not exist": it would
		// break the first bank and turn bankedBranchHead's documented
		// unreadable-remote degradation into a hard failure.
		got := strings.Join(bankPushArgs(branch, head, ""), " ")
		if strings.Contains(got, "force-with-lease") {
			t.Fatalf("args = %q, want a plain force push when the prior head is unknown", got)
		}
		if want := "push --force origin " + head + ":refs/heads/" + branch; got != want {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})
}

// The lease's teeth, against real git: a push whose expectation no longer
// matches the remote must be REFUSED, and the branch left alone. This is
// also what pins that the lease is not quietly overridden — a `--force`
// alongside it, or a git that ranked force above the lease, shows up here.
func TestBankPushLeaseRejectsAStaleExpectation(t *testing.T) {
	_, msg, work, origin, _ := bankFixture(t)
	branch := "iterion/run-" + msg.RunID
	gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1")
	first := gitOut(t, work, "rev-parse", "HEAD")
	gitOut(t, work, "push", "--force", "origin", first+":refs/heads/"+branch)

	// A sibling attempt advances the branch after our ls-remote read it.
	gitOut(t, work, "commit", "--allow-empty", "-m", "the sibling's work")
	sibling := gitOut(t, work, "rev-parse", "HEAD")
	gitOut(t, work, "push", "--force", "origin", sibling+":refs/heads/"+branch)

	// Our push still carries the stale expectation.
	gitOut(t, work, "checkout", "-q", first)
	gitOut(t, work, "commit", "--allow-empty", "-m", "our divergent head")
	ours := gitOut(t, work, "rev-parse", "HEAD")
	args := append([]string{"-C", work}, bankPushArgs(branch, ours, first)...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err == nil {
		t.Fatalf("the stale-lease push succeeded and clobbered the sibling: %s", out)
	}
	if got, _ := bankedBranch(t, origin, msg.RunID); got != sibling {
		t.Fatalf("branch = %q, want the sibling's head %s untouched", got, sibling)
	}
}

// The integrity refusal has the same cross-attempt hazard as the push
// arm: recordBankFailure's invariant ("FinalCommit is set but no
// persistent branch guards it") held only while the bank ran once, on
// success. Now a bankable death naks, so a clean attempt can be followed
// by one whose export fails the integrity check — and overwriting
// FinalBranchError there would raise an alarming branch failure over the
// valid, forge-backed pair the first attempt banked.
func TestBankIntegrityRefusalKeepsAnEarlierAttemptsPair(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "attempt 1")
	banked := gitOut(t, work, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	// Attempt 2 is an export-based sandbox whose copy never delivered the
	// pod's final tree.
	work2 := filepath.Join(t.TempDir(), "attempt2")
	gitOut(t, filepath.Dir(work2), "clone", origin, work2)
	r.bankRepoWorkspace(context.Background(), msg, work2, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: "feedfacefeedfacefeedfacefeedfacefeedface"}, "failed")

	run := loadRun(t, r, msg.RunID)
	if run.FinalBranch != "iterion/run-"+msg.RunID || run.FinalCommit != banked {
		t.Fatalf("branch %q / commit %q, want attempt 1's forge-backed pair %s intact", run.FinalBranch, run.FinalCommit, banked)
	}
	if run.FinalBranchError != "" {
		t.Fatalf("FinalBranchError = %q — a refusal for THIS attempt must not report a branch failure over a branch that exists and merges", run.FinalBranchError)
	}
	ev := findEvent(t, r, msg.RunID, store.EventRunBankRefused)
	if ev == nil {
		t.Fatal("the integrity refusal left no run_bank_refused — it became a silent no-op, which is what recordBankFailure exists to prevent")
	}
	if cause, _ := ev.Data["cause"].(string); !strings.Contains(cause, "bank refused") {
		t.Errorf("event cause = %q, want the integrity refusal named", cause)
	}
	if got := ev.Data["kept_head"]; got != banked {
		t.Errorf("kept_head = %v, want %s", got, banked)
	}
}

// The flagship scenario of the outcome-aware supersede: attempt 1 dies
// at the budget cap holding a LONGER chain; the manual resume re-clones
// at base, completes the remaining work in fewer commits, and FINISHES.
// The finished outcome must supersede the dead attempt's longer chain —
// a count-only comparison would keep the dead tree and `runs merge`
// would land it over the finished run's.
func TestBankFinishedResumeSupersedesLongerDeadAttempt(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	work2 := filepath.Join(t.TempDir(), "resume")
	gitOut(t, filepath.Dir(work2), "clone", work, work2)
	gitOut(t, work2, "config", "user.email", "t@test.invalid")
	gitOut(t, work2, "config", "user.name", "t")
	gitOut(t, work2, "remote", "set-url", "origin", origin)

	gitOut(t, work, "commit", "--allow-empty", "-m", "dead attempt: one")
	gitOut(t, work, "commit", "--allow-empty", "-m", "dead attempt: two")
	gitOut(t, work, "commit", "--allow-empty", "-m", "dead attempt: three")
	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "budget_exceeded")

	deadHead := gitOut(t, work, "rev-parse", "HEAD")

	gitOut(t, work2, "commit", "--allow-empty", "-m", "finished resume: the remaining unit")
	finishedHead := gitOut(t, work2, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work2, base, runtime.WorkspaceIntegrity{}, "finished")

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != finishedHead {
		t.Fatalf("branch = %q (present=%v), want the FINISHED head %s superseding the dead attempt's longer chain", got, ok, finishedHead)
	}
	if run := loadRun(t, r, msg.RunID); run.FinalCommit != finishedHead {
		t.Fatalf("FinalCommit = %q, want the finished head %s", run.FinalCommit, finishedHead)
	}
	// The finished chain does not contain the dead attempt's commits, so
	// the supersede force-pushed away their only forge-side copy — the
	// bank must have archived them first, and said so on the timeline.
	archive := "refs/heads/iterion/run-" + msg.RunID + "-attempt-" + deadHead[:12]
	if got, ok := refAt(t, origin, archive); !ok || got != deadHead {
		t.Fatalf("archive ref = %q (present=%v), want the dead attempt's head %s preserved at %s", got, ok, deadHead, archive)
	}
	ev := findEvent(t, r, msg.RunID, store.EventRunBankSuperseded)
	if ev == nil {
		t.Fatal("the divergent supersede left no run_bank_superseded — the takeover is invisible outside the pod log")
	}
	if ref, _ := ev.Data["archived_ref"].(string); ref == "" {
		t.Fatalf("run_bank_superseded carries no archived_ref, data=%v", ev.Data)
	}
}

// A finished chain that CONTAINS the banked head loses nothing by
// superseding it: no archive ref, no timeline event.
func TestBankFinishedContainedChainLeavesNoArchive(t *testing.T) {
	r, msg, work, origin, _ := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "work before the death")
	deadHead := gitOut(t, work, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work, "", runtime.WorkspaceIntegrity{}, "failed")

	gitOut(t, work, "commit", "--allow-empty", "-m", "the finishing unit on top")
	finishedHead := gitOut(t, work, "rev-parse", "HEAD")
	r.bankRepoWorkspace(context.Background(), msg, work, "", runtime.WorkspaceIntegrity{}, "finished")

	if got, ok := bankedBranch(t, origin, msg.RunID); !ok || got != finishedHead {
		t.Fatalf("branch = %q (present=%v), want the finished head %s", got, ok, finishedHead)
	}
	archive := "refs/heads/iterion/run-" + msg.RunID + "-attempt-" + deadHead[:12]
	if got, ok := refAt(t, origin, archive); ok {
		t.Fatalf("a contained chain must not be archived, found %s @ %s", archive, got)
	}
	if ev := findEvent(t, r, msg.RunID, store.EventRunBankSuperseded); ev != nil {
		t.Fatalf("a contained supersede must stay silent, got run_bank_superseded with data=%v", ev.Data)
	}
}

// ── Attempt-ref parking (interrupted / paused / lease-loss deaths) ──
//
// Those outcomes must not touch the STORAGE branch (another pod may own
// the lease; FinalBranch on a half-done run would be merge-eligible) —
// but their work is as stranded as a death's, so it parks on a
// uniquely-named ref with the run doc left untouched. Falsified both
// ways: the parking outcomes leave the ref + the timeline event and
// nothing on the doc; an operator cancel and a verified no-op leave
// NOTHING; an unverifiable export refuses loudly on the timeline.

func attemptRefAt(t *testing.T, origin, runID, head string) (string, bool) {
	t.Helper()
	return refAt(t, origin, "refs/heads/iterion/run-"+runID+"-attempt-"+head[:12])
}

func TestBankAttemptRefParksInterruptedAndPaused(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		cause string
	}{
		{"interrupted delivery parks", runtime.ErrRunInterrupted, "interrupted"},
		{"paused run parks", runtime.ErrRunPaused, "paused"},
		{"operator pause parks", runtime.ErrRunPausedOperator, "paused_operator"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, msg, work, origin, base := bankFixture(t)
			gitOut(t, work, "commit", "--allow-empty", "-m", "work in flight")
			head := gitOut(t, work, "rev-parse", "HEAD")

			r.bankIfBankable(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, c.err)

			if got, present := bankedBranch(t, origin, msg.RunID); present {
				t.Fatalf("the storage branch must stay untouched, found it at %q", got)
			}
			if got, ok := attemptRefAt(t, origin, msg.RunID, head); !ok || got != head {
				t.Fatalf("attempt ref = %q (present=%v), want the in-flight head %s parked", got, ok, head)
			}
			run := loadRun(t, r, msg.RunID)
			if run.FinalBranch != "" || run.FinalCommit != "" || run.FinalBranchError != "" {
				t.Fatalf("run doc = %q/%q/%q, want it untouched (an attempt ref must not be merge-eligible)", run.FinalBranch, run.FinalCommit, run.FinalBranchError)
			}
			ev := findEvent(t, r, msg.RunID, store.EventRunBankAttempt)
			if ev == nil {
				t.Fatal("no run_bank_attempt on the timeline — the parked ref is invisible outside the pod log")
			}
			if ev.Data["head"] != head || ev.Data["cause"] != c.cause {
				t.Errorf("event data = %v, want head %s and cause %q", ev.Data, head, c.cause)
			}
		})
	}
}

// A bankable death on a ctx cancelled for LEASE LOSS: the storage branch
// stays with whoever owns the lease, but the work parks on the attempt
// ref — the case that used to strand it in the snapshot outright.
func TestBankAttemptRefParksLeaseLossDeath(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "work the dying pod holds")
	head := gitOut(t, work, "rev-parse", "HEAD")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(runtime.ErrRunInterrupted)
	t.Cleanup(func() { cancel(nil) })

	r.bankIfBankable(ctx, msg, work, base, runtime.WorkspaceIntegrity{}, errors.New("boom"))

	if got, present := bankedBranch(t, origin, msg.RunID); present {
		t.Fatalf("a lease-loss death must not touch the storage branch, found %q", got)
	}
	if got, ok := attemptRefAt(t, origin, msg.RunID, head); !ok || got != head {
		t.Fatalf("attempt ref = %q (present=%v), want %s parked", got, ok, head)
	}
	if run := loadRun(t, r, msg.RunID); run.FinalBranch != "" || run.FinalBranchError != "" {
		t.Fatalf("run doc = %q/%q, want it untouched", run.FinalBranch, run.FinalBranchError)
	}
}

// The operator refusing the work is the one outcome that parks NOTHING —
// on both roads a cancel can arrive by (classified status, and a
// bankable death whose ctx cause is the cancel sentinel).
func TestBankAttemptRefOperatorCancelParksNothing(t *testing.T) {
	t.Run("classified cancelled", func(t *testing.T) {
		r, msg, work, origin, base := bankFixture(t)
		gitOut(t, work, "commit", "--allow-empty", "-m", "refused work")
		head := gitOut(t, work, "rev-parse", "HEAD")

		r.bankIfBankable(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, runtime.ErrRunCancelled)

		if _, present := bankedBranch(t, origin, msg.RunID); present {
			t.Fatal("cancelled must not bank the storage branch")
		}
		if _, ok := attemptRefAt(t, origin, msg.RunID, head); ok {
			t.Fatal("cancelled must not park an attempt ref either")
		}
		if ev := findEvent(t, r, msg.RunID, store.EventRunBankAttempt); ev != nil {
			t.Fatalf("cancelled left a run_bank_attempt event: %v", ev.Data)
		}
	})
	t.Run("bankable death on an operator-cancelled ctx", func(t *testing.T) {
		r, msg, work, origin, base := bankFixture(t)
		gitOut(t, work, "commit", "--allow-empty", "-m", "refused work")
		head := gitOut(t, work, "rev-parse", "HEAD")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(runtime.ErrRunCancelled)
		t.Cleanup(func() { cancel(nil) })

		r.bankIfBankable(ctx, msg, work, base, runtime.WorkspaceIntegrity{}, errors.New("boom"))

		if _, present := bankedBranch(t, origin, msg.RunID); present {
			t.Fatal("an operator-cancelled ctx must not bank the storage branch")
		}
		if _, ok := attemptRefAt(t, origin, msg.RunID, head); ok {
			t.Fatal("an operator-cancelled ctx must not park an attempt ref")
		}
		if ev := findEvent(t, r, msg.RunID, store.EventRunBankAttempt); ev != nil {
			t.Fatalf("operator cancel left a run_bank_attempt event: %v", ev.Data)
		}
	})
}

// A verified no-op (pod-side HEAD confirms the baseline) parks nothing
// and stays silent — the attempt ref must not manufacture refs for runs
// that produced no commits.
func TestBankAttemptRefVerifiedNoopStaysSilent(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)

	r.bankIfBankable(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: base}, runtime.ErrRunPaused)

	if out, err := exec.Command("git", "-C", origin, "for-each-ref", "refs/heads/iterion/").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a workless pause parked something anyway: %q (%v)", out, err)
	}
	if ev := findEvent(t, r, msg.RunID, store.EventRunBankAttempt); ev != nil {
		t.Fatalf("a verified no-op left a run_bank_attempt event: %v", ev.Data)
	}
}

// The attempt ref rides the SAME integrity oracle as the storage bank: an
// export that did not deliver the pod's final tree is refused — but the
// refusal lands on the TIMELINE, never on FinalBranchError (the run is
// not terminally unbanked; it may resume, and the field would raise a
// terminal alarm over a non-terminal outcome).
func TestBankAttemptRefIntegrityRefusalIsLoudOnTheTimeline(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	podHead := "feedfacefeedfacefeedfacefeedfacefeedface"

	r.bankIfBankable(context.Background(), msg, work, base,
		runtime.WorkspaceIntegrity{Applicable: true, PodHead: podHead}, runtime.ErrRunInterrupted)

	if out, err := exec.Command("git", "-C", origin, "for-each-ref", "refs/heads/iterion/").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("an unverifiable export parked a ref anyway: %q (%v)", out, err)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError != "" {
		t.Fatalf("FinalBranchError = %q, want the attempt-path refusal kept OFF the run doc", run.FinalBranchError)
	}
	ev := findEvent(t, r, msg.RunID, store.EventRunBankAttempt)
	if ev == nil {
		t.Fatal("the integrity refusal left no run_bank_attempt — a silent loss of the parked-work promise")
	}
	if errStr, _ := ev.Data["error"].(string); !strings.Contains(errStr, "export") {
		t.Errorf("event error = %v, want the export refusal named", ev.Data["error"])
	}
	if _, hasRef := ev.Data["ref"]; hasRef {
		t.Errorf("refusal event carries a ref: %v — exactly one of ref/error may be present", ev.Data)
	}
}
