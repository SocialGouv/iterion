package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The shim faults only the push/inspection boundary; the clone, commits,
// remote updates, and persisted bank state use real git and the real store.
func bankFaultGit(t *testing.T, action string) func() int {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX git fault shim")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "pushes")
	t.Setenv("BANK_TEST_REAL_GIT", realGit)
	t.Setenv("BANK_TEST_COUNT", count)
	script := `#!/bin/sh
op=
for arg in "$@"; do
 case "$arg" in push|ls-remote) op="$arg"; break;; esac
done
n=0
if test -f "$BANK_TEST_COUNT"; then read -r n < "$BANK_TEST_COUNT"; fi
if test "$op" = push; then
 n=$((n + 1))
 printf '%s\n' "$n" > "$BANK_TEST_COUNT"
fi
` + action + `
exec "$BANK_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, err := os.ReadFile(count)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if _, err := fmt.Sscan(string(b), &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func TestBankPushRetriesWithObservableCause(t *testing.T) {
	for _, mode := range []string{"transient", "exhausted", "lost_ack", "inspection_failed", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			r, msg, work, origin, base := bankFixture(t)
			run := loadRun(t, r, msg.RunID)
			run.Status = store.RunStatusFinished
			if err := r.cfg.Store.SaveRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				t.Setenv("ITERION_RUNNER_BANK_ATTEMPTS", "1")
			}
			t.Setenv("BANK_TEST_MODE", mode)
			pushes := bankFaultGit(t, `
if test "$op" = ls-remote && test "$n" -gt 0 && test "$BANK_TEST_MODE" = inspection_failed; then
 echo "retry remote unavailable" >&2; exit 9
fi
if test "$op" = push; then
 if test "$BANK_TEST_MODE" = lost_ack; then "$BANK_TEST_REAL_GIT" "$@" || exit; fi
 if test "$n" -eq 1 || test "$BANK_TEST_MODE" = exhausted; then
  echo "transport broke fake-bank-token" >&2; exit 12
 fi
fi
`)
			ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{Generic: map[string]string{"forge_token": "fake-bank-token"}})
			r.pushBank(ctx, msg, work, base, "finished")
			got := loadRun(t, r, msg.RunID)
			if got.Status != store.RunStatusFinished {
				t.Fatalf("bank changed workflow status: %s", got.Status)
			}
			wantPushes := 2
			if mode == "lost_ack" || mode == "inspection_failed" || mode == "disabled" {
				wantPushes = 1
			}
			if pushes() != wantPushes {
				t.Fatalf("pushes=%d, want %d", pushes(), wantPushes)
			}
			succeeded := mode == "transient" || mode == "lost_ack"
			if succeeded {
				if got.BankState() != store.BankStateReady || got.FinalCommit != base {
					t.Fatalf("bank=%+v", got)
				}
				// Bypass the shim's faulting ls-remote when observing the real remote.
				if head := gitOut(t, origin, "rev-parse", "refs/heads/"+got.FinalBranch); head != base {
					t.Fatalf("remote=%s", head)
				}
				if e := findEvent(t, r, msg.RunID, store.EventRunBankFailed); e != nil {
					t.Fatalf("successful bank emitted failure: %+v", e)
				}
			} else {
				if got.BankState() != store.BankStateFailed || got.FinalCommit != base || got.FinalBranch != "" {
					t.Fatalf("bank=%+v", got)
				}
				e := findEvent(t, r, msg.RunID, store.EventRunBankFailed)
				if e == nil || e.Data["recorded"] != true || fmt.Sprint(e.Data["attempts"]) != fmt.Sprint(wantPushes) || e.Data["bank_state"] != "bank_failed" {
					t.Fatalf("failure event=%+v", e)
				}
				if mode == "inspection_failed" && !strings.Contains(got.FinalBranchError, "retry inspection failed") {
					t.Fatal(got.FinalBranchError)
				}
			}
			retry := findEvent(t, r, msg.RunID, store.EventRunBankRetry)
			if mode == "disabled" {
				if retry != nil {
					t.Fatal("retry disabled but announced")
				}
			} else {
				if retry == nil || retry.Data["failure_kind"] != "process_exit" || fmt.Sprint(retry.Data["exit_code"]) != "12" || !strings.Contains(fmt.Sprint(retry.Data["error"]), "transport broke ***") {
					t.Fatalf("retry=%+v", retry)
				}
			}
			evs, err := r.cfg.Store.LoadEvents(store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID), msg.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fmt.Sprint(evs), "fake-bank-token") || strings.Contains(got.FinalBranchError, "fake-bank-token") {
				t.Fatal("bank diagnostics leaked credential")
			}
		})
	}
}

func TestBankRetryRechecksRicherConcurrentChain(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "richer one")
	gitOut(t, work, "commit", "--allow-empty", "-m", "richer two")
	richer := gitOut(t, work, "rev-parse", "HEAD")
	branch := "iterion/run-" + msg.RunID
	t.Setenv("BANK_TEST_RICHER", richer)
	t.Setenv("BANK_TEST_BRANCH", branch)
	pushes := bankFaultGit(t, `
if test "$op" = push; then
 "$BANK_TEST_REAL_GIT" push origin "$BANK_TEST_RICHER:refs/heads/$BANK_TEST_BRANCH" || exit
 echo 'connection reset' >&2; exit 12
fi
`)
	r.pushBank(context.Background(), msg, work, base, "failed_resumable")
	if pushes() != 1 {
		t.Fatalf("retried push over richer concurrent chain: %d", pushes())
	}
	if got := gitOut(t, origin, "rev-parse", "refs/heads/"+branch); got != richer {
		t.Fatalf("lost richer tip %s", got)
	}
	e := findEvent(t, r, msg.RunID, store.EventRunBankRefused)
	if e == nil {
		t.Fatal("missing richer-chain refusal")
	}
}

func TestBankRetryBackoffHonorsCancellation(t *testing.T) {
	r, msg, work, _, base := bankFixture(t)
	t.Setenv("ITERION_RUNNER_BANK_RETRY_DELAY", "1h")
	pushes := bankFaultGit(t, `if test "$op" = push; then echo failure >&2; exit 12; fi`)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel when the retry event is durable: no sleep or elapsed-time oracle.
	st := &bankRetryCancelStore{RunStore: r.cfg.Store, cancel: cancel}
	r.cfg.Store = st
	defer cancel()
	r.pushBank(ctx, msg, work, base, "finished")
	if pushes() != 1 {
		t.Fatal("push retried after cancellation")
	}
	e := findEvent(t, r, msg.RunID, store.EventRunBankFailed)
	if e == nil || e.Data["failure_kind"] != "cancelled" {
		t.Fatalf("failure=%+v", e)
	}
}

type bankRetryCancelStore struct {
	store.RunStore
	cancel context.CancelFunc
}

func (s *bankRetryCancelStore) AppendEvent(ctx context.Context, id string, e store.Event) (*store.Event, error) {
	out, err := s.RunStore.AppendEvent(ctx, id, e)
	if e.Type == store.EventRunBankRetry {
		s.cancel()
	}
	return out, err
}

func TestBankGitDeadlineNamesItsActualBound(t *testing.T) {
	r, _, work, _, _ := bankFixture(t)
	bankFaultGit(t, `if test "$op" = push; then exec sleep 60; fi`)
	previous := gitOpTimeout
	t.Cleanup(func() { gitOpTimeout = previous })
	for _, parentDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(parentDeadline), func(t *testing.T) {
			gitOpTimeout = 30 * time.Millisecond
			ctx := context.Background()
			if parentDeadline {
				gitOpTimeout = time.Minute
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			_, err := r.runGitOutEnv(ctx, work, "", nil, "push", "origin", "HEAD:refs/heads/test")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline cause lost: %v", err)
			}
			if parentDeadline && (!strings.Contains(err.Error(), "caller context ended") || strings.Contains(err.Error(), "timed out after 1m")) {
				t.Fatal(err)
			}
			if !parentDeadline && !strings.Contains(err.Error(), "ITERION_RUNNER_GIT_TIMEOUT") {
				t.Fatal(err)
			}
		})
	}
}

func TestBankRetryLeasesObservedAbsence(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "commit", "--allow-empty", "-m", "concurrent bank")
	concurrent := gitOut(t, work, "rev-parse", "HEAD")
	branch := "iterion/run-" + msg.RunID
	t.Setenv("BANK_TEST_RICHER", concurrent)
	t.Setenv("BANK_TEST_BRANCH", branch)
	pushes := bankFaultGit(t, `
if test "$op" = push; then
 if test "$n" -eq 1; then echo 'transport failed' >&2; exit 12; fi
 # The retry already observed absence. Race a new tip into that gap.
 "$BANK_TEST_REAL_GIT" push origin "$BANK_TEST_RICHER:refs/heads/$BANK_TEST_BRANCH" || exit
fi
`)
	r.pushBank(context.Background(), msg, work, base, "finished")
	if pushes() != 2 {
		t.Fatalf("pushes=%d", pushes())
	}
	if got := gitOut(t, origin, "rev-parse", "refs/heads/"+branch); got != concurrent {
		t.Fatalf("retry overwrote concurrent bank: %s", got)
	}
	run := loadRun(t, r, msg.RunID)
	if run.BankState() != store.BankStateFailed || !strings.Contains(run.FinalBranchError, "stale info") {
		t.Fatalf("missing lease rejection: %+v", run)
	}
}

// TestALiveRunBoundsItsBankSequenceToo.
//
// gitOpTimeout bounds one git subprocess; nothing bounds the SEQUENCE the
// bank runs (ls-remote, fetch, archive push, push) once the push retries
// multiply it. On the deadlined path the detached ctx carries bankBudget,
// but a run launched without --timeout has no deadline for the live path
// to sit under, so attempts×ops×gitOpTimeout pins the pod that carries it.
func TestALiveRunBoundsItsBankSequenceToo(t *testing.T) {
	previous := gitOpTimeout
	t.Cleanup(func() { gitOpTimeout = previous })
	gitOpTimeout = time.Minute

	ctx, cancel, ok := bankContext(context.Background())
	defer cancel()
	if !ok {
		t.Fatal("a live ctx must still bank")
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		t.Fatal("a run without --timeout banks under no deadline at all")
	}
	if budget := time.Until(deadline); budget > bankBudget {
		t.Fatalf("bank budget = %s, want <= %s", budget, bankBudget)
	}

	// A shorter run deadline is not EXTENDED to the budget.
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	kept, cancelKept, ok := bankContext(short)
	defer cancelKept()
	if !ok {
		t.Fatal("a live ctx must still bank")
	}
	if got, _ := kept.Deadline(); time.Until(got) > 2*time.Second {
		t.Fatalf("the bank outlives the run's own deadline by %s", time.Until(got))
	}

	// The operator who disabled per-op bounds keeps unbounded git ops.
	gitOpTimeout = 0
	unbounded, cancelUnbounded, ok := bankContext(context.Background())
	defer cancelUnbounded()
	if !ok {
		t.Fatal("a live ctx must still bank")
	}
	if _, has := unbounded.Deadline(); has {
		t.Fatal("ITERION_RUNNER_GIT_TIMEOUT<=0 asked for unbounded git ops")
	}
}

func TestBankFailureEventSurvivesDocumentWriteFailure(t *testing.T) {
	for _, integrity := range []bool{false, true} {
		t.Run(fmt.Sprint(integrity), func(t *testing.T) {
			r, msg, work, _, base := bankFixture(t)
			r.cfg.Store = &parkIOProbeStore{RunStore: r.cfg.Store, saveErr: errors.New("store down")}
			if integrity {
				r.recordBankFailure(msg, "export refused")
			} else {
				bankFaultGit(t, `if test "$op" = push; then echo 'push refused' >&2; exit 12; fi`)
				r.pushBank(context.Background(), msg, work, base, "finished")
			}
			ev := findEvent(t, r, msg.RunID, store.EventRunBankFailed)
			if ev == nil || ev.Data["recorded"] != false || ev.Data["bank_state"] != "bank_failed" || ev.Data["error"] == "" {
				t.Fatalf("event=%+v", ev)
			}
		})
	}
}
