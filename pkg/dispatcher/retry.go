package dispatcher

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// sandboxBackoffSchedule overrides the default exponential backoff
// when the failure is a sandbox-setup error (devcontainer postCreate
// killed, docker daemon refused, image pull failed). These don't
// recover within milliseconds — the host is the bottleneck. Pause
// longer between attempts so the operator's docker daemon and OS
// have room to breathe, and so a runaway OOM cycle doesn't pin the
// dispatcher in a "spawn-die-spawn-die" loop that exhausts further
// resources.
var sandboxBackoffSchedule = []time.Duration{
	60 * time.Second,
	180 * time.Second,
	300 * time.Second,
}

// sandboxParkDelay is the delay queued AFTER the schedule is
// exhausted. We don't strictly stop retrying (the operator may fix
// the host without touching the dispatcher), but we wait long enough
// that the model spend / docker churn settles to zero. A retry entry
// with a 1h DueAt is also visible on the studio's dispatcher view so
// the operator can manually clear it after fixing the underlying
// issue.
const sandboxParkDelay = 1 * time.Hour

// scheduleRetry queues a retry for the given issue, using exponential
// backoff capped by cfg.MaxRetryBackoff. Must be called from the actor.
//
// When the prior run terminated in a resumable status (failed_resumable,
// cancelled, paused_operator), prev.RunID is captured on the retry
// entry so the next dispatch resumes the same run via
// runtime.Engine.Resume instead of minting a fresh one. The engine's
// resume machinery picks up at the failing node, reuses the worktree
// the prior run created, and avoids re-executing upstream nodes
// (which can be expensive — a feature_dev plan node can spend $5 and
// 10 minutes on workspace exploration before producing its first
// artifact).
func (c *Dispatcher) scheduleRetry(issueID string, prev *runningEntry, runErr error) {
	cfg := c.cfg.Load()
	// prevAttempt is the attempt index of the run that just failed. It
	// lives on the runningEntry — dispatch() carries it forward from the
	// consumed retry entry — so reading it here is what makes the counter
	// ACCUMULATE across retries. Reading c.state.retries[issueID] alone (the
	// prior behaviour) always missed: dispatch() deletes that entry when it
	// picks the run up, so prevAttempt reset to 0 every cycle and the
	// attempt count was frozen at 1 — the dashboard never advanced past
	// "attempt 1" and any attempt ceiling would never trigger.
	prevAttempt := prev.Attempt
	if cur, ok := c.state.retries[issueID]; ok {
		if cur.Attempt > prevAttempt {
			prevAttempt = cur.Attempt
		}
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
	}
	attempt := prevAttempt + 1
	delay := computeBackoff(attempt, cfg.MaxRetryBackoff())
	sandboxFail := isSandboxSetupError(runErr)
	parked := false
	if sandboxFail {
		var sbParked bool
		delay, sbParked = sandboxBackoff(attempt)
		parked = sbParked
	}
	due := time.Now().Add(delay)

	timer := time.AfterFunc(delay, func() {
		select {
		case c.cmds <- cmdRetryDue{issueID: issueID}:
		case <-c.stop:
		}
	})
	errStr := ""
	if runErr != nil {
		errStr = runErr.Error()
	}
	prevRunID := c.resumableRunID(prev.RunID)
	// The prior run may be resumable by status (failed_resumable /
	// cancelled / paused_operator), but if resume already FAILED because
	// the bot's workflow source changed since that run started, resuming
	// again is futile — the runtime rejects it identically every attempt
	// ("workflow source has changed ... re-run from scratch or use
	// --force", pkg/runtime/resume.go). Drop the resume pointer so the
	// next attempt mints a fresh runID instead of looping on the same
	// doomed resume. Bot edits are routine in a dev/dogfood loop, so this
	// otherwise strands every issue whose last run is failed_resumable.
	if isResumeSourceChanged(runErr) {
		if prevRunID != "" {
			c.logger.Info("dispatcher: %s prior run %s not resumable (bot source changed) — retrying from scratch", prev.Identifier, prevRunID)
		}
		prevRunID = ""
	}
	c.state.retries[issueID] = &retryEntry{
		IssueID:    issueID,
		Identifier: prev.Identifier,
		Attempt:    attempt,
		DueAt:      due,
		LastError:  errStr,
		Timer:      timer,
		PrevRunID:  prevRunID,
	}
	switch {
	case parked:
		c.logger.Warn("dispatcher: %s parked after %d sandbox-setup failures — next retry in %s. Investigate host (docker daemon, OOM, disk) before then or clear the retry from the studio.", prev.Identifier, attempt-1, delay)
	case sandboxFail:
		c.logger.Warn("dispatcher: %s sandbox setup failed (attempt=%d/%d) — backing off %s before retry", prev.Identifier, attempt, len(sandboxBackoffSchedule), delay)
	default:
		c.logger.Info("dispatcher: %s retry queued (attempt=%d, in=%s, resume=%s)", prev.Identifier, attempt, delay, prev.RunID)
	}
}

// isResumeSourceChanged reports whether runErr is the runtime's refusal
// to resume because the bot's workflow source changed since the prior
// run started (pkg/runtime/resume.go: "workflow source has changed ...
// re-run from scratch or use --force"). Such a run cannot be resumed as-
// is; the dispatcher must retry FRESH rather than reschedule the same
// doomed resume. Matches the message substring because the error is a
// plain fmt.Errorf, not a typed code, and survives the in-process
// boundary as text.
func isResumeSourceChanged(err error) bool {
	return err != nil && strings.Contains(err.Error(), "workflow source has changed")
}

// resumableRunID returns the runID iff the corresponding run record
// can be resumed by the runtime — i.e. its on-disk status is
// failed_resumable, cancelled, or paused_operator. Returns "" if the
// run is missing, terminal-without-checkpoint (failed), or any error
// reading the store; the dispatcher then falls back to a fresh run.
// Best-effort: store IO errors are debug-logged, never fatal.
func (c *Dispatcher) resumableRunID(runID string) string {
	if runID == "" || c.storeDir == "" {
		return ""
	}
	s, err := store.New(c.storeDir, store.WithLogger(c.logger))
	if err != nil {
		c.logger.Debug("dispatcher: open store for resume check: %v", err)
		return ""
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		c.logger.Debug("dispatcher: cannot read run %s for resume check: %v", runID, err)
		return ""
	}
	switch r.Status {
	case store.RunStatusFailedResumable,
		store.RunStatusCancelled,
		store.RunStatusPausedOperator:
		return runID
	}
	return ""
}

// isSandboxSetupError reports whether the run failed before the
// runtime engine got to execute any node — devcontainer postCreate
// exited non-zero, docker daemon refused, image pull timed out. These
// are NOT transients the per-node recovery dispatch can mask; they
// fail deterministically until the host is fixed. The dispatcher
// applies sandboxBackoffSchedule instead of the default exponential
// so consecutive failures don't pile docker churn on a stressed host.
//
// Match strings are intentionally broad and lowercase — claw's claude
// CLI, claude_code, the runtime's sandbox driver, and a few buildkit
// edge cases all wrap their errors with slightly different prefixes,
// and matching too tightly here means a stress-induced postCreate
// failure slips into the default 10s exponential and re-spawns the
// container before the host has recovered.
func isSandboxSetupError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "sandbox start:") {
		return true
	}
	if strings.Contains(msg, "postcreate") {
		return true
	}
	if strings.Contains(msg, "post_create") {
		return true
	}
	if strings.Contains(msg, "image pull") {
		return true
	}
	if strings.Contains(msg, "container start") {
		return true
	}
	// A broken/partial CLI install inside the sandbox surfaces as an
	// "exec format error" when the runtime first invokes it (observed:
	// npm install -g claude-code exits 0 leaving a claude.exe symlink
	// whose target wasn't fully written → EFORMAT on the first
	// claude_code node — native:c6d93a2a). The hardened post_create now
	// fails the boot cleanly, but if a broken binary still slips through
	// to node execution, treat it as a sandbox-setup error so the retry
	// uses the backoff + a fresh container (where the reinstall takes)
	// rather than the default exponential against the same broken image.
	if strings.Contains(msg, "exec format error") {
		return true
	}
	return false
}

// sandboxBackoff returns the delay for the given retry attempt under
// the sandbox-setup-error schedule + a parked flag once the schedule
// is exhausted. attempt is 1-indexed (first retry = attempt 1).
func sandboxBackoff(attempt int) (delay time.Duration, parked bool) {
	if attempt < 1 {
		return time.Second, false
	}
	if attempt <= len(sandboxBackoffSchedule) {
		return sandboxBackoffSchedule[attempt-1], false
	}
	return sandboxParkDelay, true
}

// computeBackoff returns min(10s * 2^(attempt-1), cap), with attempt=0
// treated as a continuation (fixed 1s).
func computeBackoff(attempt int, cap time.Duration) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	const base = 10 * time.Second
	// Cap the exponent to avoid int overflow on absurd attempt counts.
	if attempt > 10 {
		attempt = 10
	}
	mult := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * mult)
	if cap > 0 && d > cap {
		return cap
	}
	return d
}
