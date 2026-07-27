package cli

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/forfait"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// LAYER-2: bounded run-level auto-resume.
//
// After a run (or a manual resume) exits with status failed_resumable AND a
// retryable RuntimeError code, this loop waits (exponential backoff, capped)
// and re-invokes the engine's Resume in-process, up to N times — turning the
// 11 manual `iterion resume` invocations observed in the whole-improve-loop
// dogfood into automatic recovery. It re-uses the SAME engine (so the exact
// launch overrides — model/backend, budget flags, store-dir — carry over) and
// stops loudly when the cause is not auto-recoverable.
// ---------------------------------------------------------------------------

// autoResumeBackoffBase is the base delay for the auto-resume backoff. Larger
// than the in-executor per-node backoff (LAYER 1): a run-level failure means
// the fast retries already lost, so we wait a provider-reset-scale interval.
const autoResumeBackoffBase = 30 * time.Second

// autoResumeBackoffCap caps the per-attempt wait so a large N can't stall for
// hours between attempts.
const autoResumeBackoffCap = 5 * time.Minute

// autoResumeConfig bundles the knobs for the auto-resume loop.
type autoResumeConfig struct {
	// MaxAttempts is N (`--auto-resume` / ITERION_AUTO_RESUME). <= 0 disables.
	MaxAttempts int
	// BudgetRaised reports whether the operator passed any --max-* override.
	// Required to auto-resume a BUDGET_EXCEEDED failure (else the same cap
	// re-trips immediately — we stop with a clear message instead of looping).
	BudgetRaised bool
	// ForfaitCapPct gates auto-resume against the Claude Code OAuth forfait:
	// when 5h or 7d utilization >= this, we stop (staying failed_resumable)
	// rather than burn attempts against a wall. <= 0 disables the check.
	// Best-effort — an unavailable usage endpoint never blocks (see forfait).
	ForfaitCapPct float64
	// Retry is the shared retry contract (pkg/retrypolicy). It supplies the
	// horizon for a provider usage window, which is the one dimension this
	// loop could not express on its own: the cap used to be a hard 5h
	// constant — shorter than the weekly window it is meant to wait out.
	Retry retrypolicy.Policy
}

// resolveAutoResume builds the config from the flag + env + budget flags.
// A non-zero flag wins; a zero flag falls back to ITERION_AUTO_RESUME; the
// default is 0 (off).
func resolveAutoResume(flagN int, budget BudgetOverrides, override retrypolicy.Policy) autoResumeConfig {
	n := flagN
	if n == 0 {
		if env := envAutoResume(); env > 0 {
			n = env
		}
	}
	pol, _ := retrypolicy.Resolve(
		retrypolicy.Layer{Source: retrypolicy.SourceRunOverride, Policy: override},
		retrypolicy.Layer{Source: retrypolicy.SourceEnv, Policy: retrypolicy.FromEnv()},
	)
	// The CLI loop stays OPT-IN: --auto-resume / ITERION_AUTO_RESUME is the
	// gate, and n == 0 means off. The retry policy deliberately does NOT
	// turn it on — its defaults describe what an unattended cloud run should
	// do, and inheriting them here would start auto-resuming every local
	// `iterion run` that nobody asked to be retried. What the policy DOES
	// contribute is the horizon (max_wait) and the usage-window opt-out.
	if n > 0 {
		pol.MaxAttempts = n
	}
	return autoResumeConfig{
		MaxAttempts:   n,
		BudgetRaised:  !budget.IsZero(),
		ForfaitCapPct: forfait.CapPctFromEnv(),
		Retry:         pol,
	}
}

func envAutoResume() int {
	raw := os.Getenv("ITERION_AUTO_RESUME")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// autoResumeRetryableCodes is the allow-list of RuntimeError codes for which a
// run-level auto-resume is meaningful. Everything NOT here (SCHEMA_VALIDATION,
// AUTH_FAILED, WORKSPACE_SAFETY, LOOP_EXHAUSTED, NODE_NOT_FOUND, …) fails loud:
// resuming would just re-hit the same deterministic wall.
var autoResumeRetryableCodes = map[runtime.ErrorCode]bool{
	runtime.ErrCodeExecutionFailed:     true, // transient backend error surfaced after in-executor retries
	runtime.ErrCodeBudgetExceeded:      true, // special-cased: needs a raised cap
	runtime.ErrCodeTimeout:             true, // context deadline (max_duration / --timeout)
	runtime.ErrCodeRateLimited:         true, // provider throttle (short backoff)
	runtime.ErrCodeUsageLimitBlocked:   true, // forfait window exhausted — special-cased: reset-aware delay
	runtime.ErrCodeNetworkTransient:    true, // connectivity blip beyond the LAYER-1 budget
	runtime.ErrCodeToolFailedTransient: true, // transient tool failure
}

// autoResumeGate is the pure per-attempt decision (status/forfait I/O stays in
// the loop): given the failure code + config + whether budget was already
// auto-resumed once, should we resume, and if not, why.
type autoResumeGate struct {
	proceed       bool
	consumeBudget bool // caller flips budgetResumed when true
	reason        string
}

// gateAutoResume decides, for a retryable-vs-terminal failure code, whether an
// auto-resume is warranted. It encodes the code allow-list and the
// BUDGET_EXCEEDED special-case (needs a raised cap; only one budget retry so
// the same cap can't loop).
func gateAutoResume(code runtime.ErrorCode, cfg autoResumeConfig, budgetResumed bool) autoResumeGate {
	if !autoResumeRetryableCodes[code] {
		return autoResumeGate{reason: "not auto-recoverable (code " + nonEmptyCode(code) + ") — leaving run failed_resumable for manual review"}
	}
	if code == runtime.ErrCodeUsageLimitBlocked && !cfg.Retry.Enabled() {
		return autoResumeGate{reason: "provider usage window exhausted and retry.usage_window is off — leaving run failed_resumable"}
	}
	if code == runtime.ErrCodeBudgetExceeded {
		if !cfg.BudgetRaised {
			return autoResumeGate{reason: "budget exceeded and no raised --max-* cap provided — stopping (re-run with a higher cap, e.g. --max-duration 4h)"}
		}
		if budgetResumed {
			return autoResumeGate{reason: "budget still exceeded with the raised cap — stopping (raise it further to continue)"}
		}
		return autoResumeGate{proceed: true, consumeBudget: true}
	}
	return autoResumeGate{proceed: true}
}

// runtimeCode extracts the RuntimeError code from err, or "" when err is not a
// RuntimeError.
func runtimeCode(err error) runtime.ErrorCode {
	var re *runtime.RuntimeError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

// autoResumeBackoff returns the delay before the (0-indexed) nth auto-resume
// attempt: exponential from the base, capped, with 0.5x–1.5x jitter.
func autoResumeBackoff(attempt int) time.Duration {
	d := float64(autoResumeBackoffBase) * math.Pow(2, float64(attempt))
	if d > float64(autoResumeBackoffCap) {
		d = float64(autoResumeBackoffCap)
	}
	return time.Duration(d * (0.5 + rand.Float64()))
}

// usageLimitFallbackDelay is the wait before retrying a usage-window
// block whose reset instant could not be parsed: window-scale, not
// backoff-scale (retrying a 5h forfait cap after 30s just burns an
// attempt).
const usageLimitFallbackDelay = 15 * time.Minute

// usageLimitDelay picks the wait for a USAGE_LIMIT_BLOCKED resume:
// honor the provider's parsed reset instant (plus a small margin) when
// present, else the window-scale fallback. Clamped to
// [fallback floor when hint is in the past, usageLimitMaxDelay].
func usageLimitDelay(err error, pol retrypolicy.Policy, now time.Time) time.Duration {
	delay := usageLimitFallbackDelay
	var rl *delegate.ErrRateLimited
	if errors.As(err, &rl) && !rl.ResetAt.IsZero() {
		if until := rl.ResetAt.Sub(now) + time.Minute; until > 0 {
			delay = until
		}
	} else if at, ok := delegate.ParseResetHint(err.Error(), now); ok {
		// The typed error did not survive to here, but the notice text may
		// still carry the instant — the same structure-first, string-last
		// order the cloud runner uses.
		if until := at.Sub(now) + time.Minute; until > 0 {
			delay = until
		}
	}
	if max := pol.MaxWaitDuration(); delay > max {
		delay = max
	}
	return delay
}

// autoResumeLoop drives the bounded run-level auto-resume. It re-uses eng (so
// launch overrides carry over) and returns the final engine error. A no-op
// (returns err unchanged) when disabled, when err is nil/paused/cancelled, or
// when the failure is not auto-recoverable.
func autoResumeLoop(
	ctx context.Context,
	eng *runtime.Engine,
	s store.RunStore,
	runID string,
	cfg autoResumeConfig,
	err error,
	logger *iterlog.Logger,
) error {
	if cfg.MaxAttempts <= 0 || err == nil {
		return err
	}
	// A paused (human) or cancelled (user Ctrl-C) exit is never auto-resumed.
	if errors.Is(err, runtime.ErrRunPaused) || errors.Is(err, runtime.ErrRunCancelled) {
		return err
	}

	budgetResumed := false
	for attempt := 1; attempt <= cfg.MaxAttempts && err != nil; attempt++ {
		// The store status is the authoritative discriminator: only a
		// failed_resumable run carries a checkpoint to resume from. FailNode
		// (→ failed), paused, cancelled and finished all fall out here.
		r, loadErr := s.LoadRun(ctx, runID)
		if loadErr != nil {
			logger.Warn("auto-resume: load run %s: %v — stopping", runID, loadErr)
			return err
		}
		if r.Status != store.RunStatusFailedResumable {
			return err
		}

		code := runtimeCode(err)
		gate := gateAutoResume(code, cfg, budgetResumed)
		if !gate.proceed {
			logger.Info("auto-resume: %s", gate.reason)
			return err
		}
		if gate.consumeBudget {
			budgetResumed = true
		}

		// Forfait-cap awareness (best-effort): don't auto-resume into a
		// near-exhausted OAuth subscription window. Skips silently when it
		// can't tell (no token, API-key run, endpoint down).
		if d := forfait.Check(ctx, cfg.ForfaitCapPct); d.Blocked {
			logger.Warn("auto-resume: %s — leaving run failed_resumable", d.Reason)
			return err
		} else if d.Skipped {
			logger.Debug("auto-resume: forfait cap check skipped (%s)", d.Reason)
		}

		delay := autoResumeBackoff(attempt - 1)
		if code == runtime.ErrCodeUsageLimitBlocked {
			// Reset-aware wait: retrying inside the forfait window can
			// never succeed, so the delay tracks the provider's reset
			// hint instead of the exponential backoff.
			delay = usageLimitDelay(err, cfg.Retry, time.Now())
			logger.Warn("auto-resume: provider usage window exhausted; waiting %s for the quota reset",
				delay.Round(time.Minute))
		}
		logger.Warn("auto-resume %d/%d: run %s failed_resumable (%s); waiting %s then resuming",
			attempt, cfg.MaxAttempts, runID, nonEmptyCode(code), delay.Round(time.Second))
		emitAutoResume(ctx, s, runID, attempt, cfg.MaxAttempts, code, delay, logger)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}

		// Re-use the engine; failed_resumable resumes need no answers.
		err = eng.Resume(ctx, runID, nil)
		if err == nil {
			logger.Info("auto-resume %d/%d: run %s recovered", attempt, cfg.MaxAttempts, runID)
			return nil
		}
		// A resume that pauses for human input or is cancelled must not be
		// re-driven — hand it back to the reporter.
		if errors.Is(err, runtime.ErrRunPaused) || errors.Is(err, runtime.ErrRunCancelled) {
			return err
		}
	}

	if err != nil {
		logger.Warn("auto-resume: exhausted %d attempt(s); run %s remains failed_resumable — resume manually once the cause is addressed", cfg.MaxAttempts, runID)
	}
	return err
}

// nonEmptyCode renders an empty code as a readable token for logs.
func nonEmptyCode(c runtime.ErrorCode) string {
	if c == "" {
		return "UNCLASSIFIED"
	}
	return string(c)
}

// emitAutoResume records an observable run_auto_resumed event so the timeline
// shows the automation. Best-effort — a persistence hiccup must not abort the
// recovery.
func emitAutoResume(ctx context.Context, s store.RunStore, runID string, attempt, max int, code runtime.ErrorCode, delay time.Duration, logger *iterlog.Logger) {
	if _, err := s.AppendEvent(ctx, runID, store.Event{
		Type:      store.EventRunAutoResumed,
		RunID:     runID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"attempt":  attempt,
			"max":      max,
			"code":     nonEmptyCode(code),
			"delay_ms": delay.Milliseconds(),
			"reason":   "auto",
		},
	}); err != nil {
		logger.Debug("auto-resume: emit event: %v", err)
	}
}
