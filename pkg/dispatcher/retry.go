package dispatcher

import (
	"context"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

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
	prevAttempt := 0
	if cur, ok := c.state.retries[issueID]; ok {
		prevAttempt = cur.Attempt
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
	}
	attempt := prevAttempt + 1
	delay := computeBackoff(attempt, cfg.MaxRetryBackoff())
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
	c.state.retries[issueID] = &retryEntry{
		IssueID:    issueID,
		Identifier: prev.Identifier,
		Attempt:    attempt,
		DueAt:      due,
		LastError:  errStr,
		Timer:      timer,
		PrevRunID:  c.resumableRunID(prev.RunID),
	}
	c.logger.Info("dispatcher: %s retry queued (attempt=%d, in=%s, resume=%s)", prev.Identifier, attempt, delay, prev.RunID)
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
