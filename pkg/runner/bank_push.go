package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

func (r *Runner) bankPushPolicy() (int, time.Duration) {
	attempts, delay := 2, 2*time.Second
	if raw := os.Getenv("ITERION_RUNNER_BANK_ATTEMPTS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			attempts = n
		} else {
			r.cfg.Logger.Warn("runner: invalid ITERION_RUNNER_BANK_ATTEMPTS=%q; using %d", raw, attempts)
		}
	}
	if raw := os.Getenv("ITERION_RUNNER_BANK_RETRY_DELAY"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			delay = d
		} else {
			r.cfg.Logger.Warn("runner: invalid ITERION_RUNNER_BANK_RETRY_DELAY=%q; using %s", raw, delay)
		}
	}
	return attempts, delay
}

// pushBankWithRetry rechecks remote ownership before every retry. A lost push
// acknowledgement is success when the remote already has head. A changed tip
// must pass the same richer-chain guard as the first attempt; an unreadable
// retry inspection never degrades a known lease into an unguarded force push.
// allowed=false means the guard recorded a refusal and the caller must keep
// the earlier banked pair untouched.
func (r *Runner) pushBankWithRetry(ctx context.Context, msg *queue.RunMessage, workDir, tok, branch, head, finalStatus string) (attempts int, allowed bool, pushErr error) {
	maxAttempts, delay := r.bankPushPolicy()
	// The tip this call already archived and compared. A failed push leaves
	// the remote untouched, so without this the retry re-fetches, re-archives
	// and emits a SECOND run_bank_superseded — against the very forge that
	// just failed, spending the bank budget before the retry push is issued.
	// A tip that MOVED is not this one, and passes the guard again.
	var guarded string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var oldHead string
		if attempt == 1 {
			oldHead = r.bankedBranchHead(ctx, workDir, tok, branch)
		} else {
			var err error
			oldHead, err = r.readBankedBranchHead(ctx, workDir, tok, branch)
			if err != nil {
				return attempts, true, fmt.Errorf("bank retry inspection failed: %w", errors.Join(pushErr, err))
			}
		}
		if oldHead == head {
			return attempts, true, nil
		}
		if oldHead != "" && oldHead != guarded {
			if finalStatus == "finished" {
				// A finished chain may be shorter than a dead attempt's;
				// preserve its old commits before superseding it, as before.
				r.preserveSupersededChain(ctx, msg, workDir, tok, branch, oldHead, head)
			} else if !r.bankSupersedes(ctx, msg, workDir, tok, branch, oldHead, head) {
				return attempts, false, nil
			}
			guarded = oldHead
		}
		args := bankPushArgs(branch, head, oldHead)
		if attempt > 1 && oldHead == "" {
			// The retry read confirmed absence. Bind that observation too,
			// so a concurrent first banker cannot be overwritten in the gap.
			args = []string{"push", "--force-with-lease=refs/heads/" + branch + ":", "origin", head + ":refs/heads/" + branch}
		}
		// origin reads the clone's live credential store on each attempt;
		// tok is only the redaction key, never a frozen push credential.
		pushErr = r.runGit(ctx, workDir, tok, args...)
		attempts++
		if pushErr == nil || attempt == maxAttempts || ctx.Err() != nil {
			return attempts, true, pushErr
		}
		data := bankPushErrorData(branch, head, pushErr)
		data["attempt"], data["max_attempts"], data["delay_ms"] = attempt, maxAttempts, delay.Milliseconds()
		r.recordBankEvent(msg, store.EventRunBankRetry, data)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempts, true, fmt.Errorf("bank retry cancelled during backoff: %w", errors.Join(pushErr, ctx.Err()))
		case <-timer.C:
		}
	}
	return attempts, true, pushErr
}

func (r *Runner) readBankedBranchHead(ctx context.Context, workDir, tok, branch string) (string, error) {
	out, err := r.runGitOutEnv(ctx, workDir, tok, nil, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return parseLsRemoteHead(out, branch), nil
}

func bankPushErrorData(branch, head string, err error) map[string]any {
	data := map[string]any{"branch": branch, "head": head, "reason": "push_failed", "error": err.Error()}
	kind := "git_error"
	var exitErr *exec.ExitError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "cancelled"
	case errors.As(err, &exitErr):
		kind = "process_exit"
		data["exit_code"] = exitErr.ExitCode()
	}
	data["failure_kind"] = kind
	return data
}

func (r *Runner) recordBankEvent(msg *queue.RunMessage, kind store.EventType, data map[string]any) {
	wctx, cancel := context.WithTimeout(context.Background(), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{Type: kind, Data: data}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit %s: %v", msg.RunID, kind, err)
	}
}
