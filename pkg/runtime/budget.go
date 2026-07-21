package runtime

import (
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrBudgetExceeded is returned when a budget limit has been reached.
var ErrBudgetExceeded = fmt.Errorf("runtime: budget exceeded")

// SharedBudget tracks resource consumption across a workflow run.
// It is safe for concurrent use by parallel branches (first-come-first-served).
//
// Budget enforcement is "soft": because nodes are checked before execution
// and recorded after, concurrent branches may slightly exceed limits when
// multiple nodes pass the pre-check simultaneously. This is by design —
// hard enforcement would require holding the lock across the entire node
// execution, which would serialize all parallel branches.
type SharedBudget struct {
	mu     sync.Mutex
	logger *iterlog.Logger

	// Limits (0 means unlimited).
	maxTokens     int
	maxCostUSD    float64
	maxIterations int
	maxDuration   time.Duration
	// warnTokens is advisory-only: crossing it emits a single
	// budget_warning (advisory=true) but never hard-limits the run.
	warnTokens int

	// Consumed.
	tokensUsed     int
	costUsed       float64
	iterationsUsed int
	startedAt      time.Time

	// Warning tracking — each dimension warns at most once (re-armed
	// per axis by RaiseCaps so a raised ceiling gets fresh warnings).
	warningsEmitted map[string]bool

	// everRaised records that at least one live raise_budget landed —
	// the trigger for persisting the (absolute) caps on the run record.
	everRaised bool
}

const (
	budgetWarningThreshold = 0.8
	budgetHardThreshold    = 0.9 // refuse new node executions at 90% to limit concurrent overage
)

// newSharedBudget creates a SharedBudget from an IR Budget definition.
// Returns nil if budget is nil or has no enforceable limits.
func newSharedBudget(b *ir.Budget, logger *iterlog.Logger) *SharedBudget {
	if b == nil {
		return nil
	}

	var maxDur time.Duration
	if b.MaxDuration != "" {
		// Expand ${VAR:-default} forms so a bot's budget can be tuned per
		// run/env (e.g. a longer max_duration for remediation on a large
		// repo) without editing the .bot — the budget block, unlike the
		// effort/model fields, was not previously env-resolved.
		parsed, err := time.ParseDuration(ir.ExpandEnvWithDefault(b.MaxDuration))
		if err == nil {
			maxDur = parsed
		}
	}

	// If no limits are set beyond MaxParallelBranches (handled elsewhere),
	// skip. A warn-only threshold still needs the tracker: it never blocks,
	// but consumption must be counted for the advisory to fire.
	if b.MaxTokens == 0 && b.MaxCostUSD == 0 && b.MaxIterations == 0 && maxDur == 0 && b.WarnTokens == 0 {
		return nil
	}

	return &SharedBudget{
		logger:          logger,
		maxTokens:       b.MaxTokens,
		warnTokens:      b.WarnTokens,
		maxCostUSD:      b.MaxCostUSD,
		maxIterations:   b.MaxIterations,
		maxDuration:     maxDur,
		startedAt:       time.Now(),
		warningsEmitted: make(map[string]bool),
	}
}

// RaiseCaps raises any of the four caps to the supplied ABSOLUTE values
// (live steering, raise_budget). Raise-only: a value lower than or equal
// to the current cap — or zero — is ignored, so a stale/duplicate command
// can never SHRINK a running budget. Each raised axis re-arms its
// warning so the operator gets a fresh 80% tick against the new ceiling.
// Returns the effective caps after clamping and whether anything
// changed. Nil-safe (no-op, raised=false): the caller maps a nil budget
// to its "no budget declared" error before ever calling this.
func (b *SharedBudget) RaiseCaps(o ir.BudgetOverrides) (effective ir.BudgetOverrides, raised bool) {
	if b == nil {
		return ir.BudgetOverrides{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// An axis at 0 means UNLIMITED: "raising" it to a number would in
	// fact constrain the run, so each clause requires a currently-set
	// (> 0) limit before applying a strictly-greater value.
	if b.maxCostUSD > 0 && o.MaxCostUSD > b.maxCostUSD {
		b.maxCostUSD = o.MaxCostUSD
		delete(b.warningsEmitted, "cost_usd")
		raised = true
	}
	if b.maxTokens > 0 && o.MaxTokens > b.maxTokens {
		b.maxTokens = o.MaxTokens
		delete(b.warningsEmitted, "tokens")
		raised = true
	}
	if b.maxIterations > 0 && o.MaxIterations > b.maxIterations {
		b.maxIterations = o.MaxIterations
		delete(b.warningsEmitted, "iterations")
		raised = true
	}
	if b.maxDuration > 0 && o.MaxDuration != "" {
		if d, err := time.ParseDuration(ir.ExpandEnvWithDefault(o.MaxDuration)); err == nil && d > b.maxDuration {
			b.maxDuration = d
			delete(b.warningsEmitted, "duration")
			raised = true
		}
	}
	if raised {
		b.everRaised = true
	}
	return b.capsLocked(), raised
}

// Raises returns the current ABSOLUTE caps and whether any live raise
// ever landed on this budget — the persistence trigger. Nil-safe.
func (b *SharedBudget) Raises() (ir.BudgetOverrides, bool) {
	if b == nil {
		return ir.BudgetOverrides{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capsLocked(), b.everRaised
}

// capsLocked snapshots the limit fields as BudgetOverrides. Caller
// holds b.mu.
func (b *SharedBudget) capsLocked() ir.BudgetOverrides {
	caps := ir.BudgetOverrides{
		MaxCostUSD:    b.maxCostUSD,
		MaxTokens:     b.maxTokens,
		MaxIterations: b.maxIterations,
	}
	if b.maxDuration > 0 {
		caps.MaxDuration = b.maxDuration.String()
	}
	return caps
}

// Snapshot returns the budget's consumed amounts and elapsed active time so
// they can be persisted in a checkpoint and restored on resume. Safe on a nil
// budget (returns zeros). elapsed is time.Since(startedAt) at call time.
func (b *SharedBudget) Snapshot() (tokens int, cost float64, iterations int, elapsed time.Duration) {
	if b == nil {
		return 0, 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokensUsed, b.costUsed, b.iterationsUsed, time.Since(b.startedAt)
}

// Restore seeds a freshly-built budget with consumption carried over from a
// checkpoint so a resumed run continues from where it left off rather than
// with a full allowance. elapsed shifts startedAt back so the duration budget
// counts prior active time (the pause gap itself is excluded). Safe on a nil
// budget (no-op). Called once, before the resumed run executes any node.
func (b *SharedBudget) Restore(tokens int, cost float64, iterations int, elapsed time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokensUsed = tokens
	b.costUsed = cost
	b.iterationsUsed = iterations
	if elapsed > 0 {
		b.startedAt = time.Now().Add(-elapsed)
	}
}

// budgetCheckResult holds the outcome of a single dimension check.
type budgetCheckResult struct {
	exceeded    bool
	hardLimited bool // true when 90% <= ratio < 100%
	warning     bool
	advisory    bool   // warning came from a warn-only threshold (warn_tokens)
	dimension   string // "tokens", "cost_usd", "iterations", "duration"
	used        float64
	limit       float64
}

// RecordUsage records resource consumption from a node execution and returns
// check results. tokens and costUSD may be zero if the executor does not
// report them.
//
// Because budget enforcement is soft (pre-check and post-record are not
// atomic), concurrent branches may push usage past the limit. When overage
// exceeds 20% of the limit, a warning is logged to aid debugging.
func (b *SharedBudget) RecordUsage(tokens int, costUSD float64) []budgetCheckResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.iterationsUsed++
	b.tokensUsed += tokens
	b.costUsed += costUSD

	checks := b.checkLocked()

	// Log a warning when soft enforcement allows significant overage.
	for _, c := range checks {
		if c.exceeded && c.limit > 0 {
			overage := (c.used - c.limit) / c.limit
			if overage > 0.2 {
				b.logger.Warn("budget %s exceeded by %.0f%% (%.0f/%.0f) — concurrent branches may have passed pre-check simultaneously",
					c.dimension, overage*100, c.used, c.limit)
			}
		}
	}

	return checks
}

// Check checks current budget status without recording usage.
func (b *SharedBudget) Check() []budgetCheckResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkLocked()
}

// RemainingDuration reports the wall-clock time left before the run's
// max_duration budget is exhausted, and whether a duration budget is set.
// When no duration limit is configured (or the budget is nil) it returns
// (0, false) so callers skip deadline bounding. An already-overrun budget
// returns (0, true). This is the basis for the engine's per-node hard
// deadline: the boundary budget check only blocks NEW node starts, so a
// single long or hung node would otherwise run unbounded past max_duration.
func (b *SharedBudget) RemainingDuration() (time.Duration, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxDuration <= 0 {
		return 0, false
	}
	rem := b.maxDuration - time.Since(b.startedAt)
	if rem < 0 {
		rem = 0
	}
	return rem, true
}

// DurationStatus returns the elapsed time and the duration limit (both in
// nanoseconds, as float64 to match budgetCheckResult), plus whether a
// duration budget is set. Used to surface a per-node deadline expiry as a
// BUDGET_EXCEEDED(duration) failure with the same shape as the boundary check.
func (b *SharedBudget) DurationStatus() (used, limit float64, bounded bool) {
	if b == nil {
		return 0, 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxDuration <= 0 {
		return 0, 0, false
	}
	return float64(time.Since(b.startedAt)), float64(b.maxDuration), true
}

func (b *SharedBudget) checkLocked() []budgetCheckResult {
	var results []budgetCheckResult

	check := func(dimension string, used, limit float64) {
		if limit <= 0 {
			return
		}
		ratio := used / limit
		if ratio >= 1.0 {
			results = append(results, budgetCheckResult{
				exceeded: true, dimension: dimension, used: used, limit: limit,
			})
		} else if ratio >= budgetHardThreshold {
			results = append(results, budgetCheckResult{
				hardLimited: true, dimension: dimension, used: used, limit: limit,
			})
		} else if ratio >= budgetWarningThreshold && !b.warningsEmitted[dimension] {
			b.warningsEmitted[dimension] = true
			results = append(results, budgetCheckResult{
				warning: true, dimension: dimension, used: used, limit: limit,
			})
		}
	}

	check("iterations", float64(b.iterationsUsed), float64(b.maxIterations))
	check("tokens", float64(b.tokensUsed), float64(b.maxTokens))
	check("cost_usd", b.costUsed, b.maxCostUSD)
	check("duration", float64(time.Since(b.startedAt)), float64(b.maxDuration))

	// The advisory token threshold reports once on the tokens axis and
	// never blocks: the operator asked to be told (audit hint), not
	// stopped. It shares the axis dedupe with the ratio warning above so
	// a run surfaces at most one tokens warning.
	if b.warnTokens > 0 && b.tokensUsed >= b.warnTokens && !b.warningsEmitted["tokens"] {
		b.warningsEmitted["tokens"] = true
		results = append(results, budgetCheckResult{
			warning: true, advisory: true, dimension: "tokens",
			used: float64(b.tokensUsed), limit: float64(b.warnTokens),
		})
	}

	return results
}

// findBudgetCheck returns the first result matching pick, or nil.
func findBudgetCheck(results []budgetCheckResult, pick func(*budgetCheckResult) bool) *budgetCheckResult {
	for i := range results {
		if pick(&results[i]) {
			return &results[i]
		}
	}
	return nil
}

// findExceeded returns the first exceeded result, or nil.
func findExceeded(results []budgetCheckResult) *budgetCheckResult {
	return findBudgetCheck(results, func(r *budgetCheckResult) bool { return r.exceeded })
}

// findHardLimited returns the first hard-limited result, or nil.
func findHardLimited(results []budgetCheckResult) *budgetCheckResult {
	return findBudgetCheck(results, func(r *budgetCheckResult) bool { return r.hardLimited })
}

// findWarnings returns all warning results.
func findWarnings(results []budgetCheckResult) []budgetCheckResult {
	var warnings []budgetCheckResult
	for _, r := range results {
		if r.warning {
			warnings = append(warnings, r)
		}
	}
	return warnings
}

// ---------------------------------------------------------------------------
// Budget helpers
// ---------------------------------------------------------------------------

// checkBudgetBeforeExec checks budget limits before a node runs.
// It enforces both hard exceeded (100%) and hard limited (90%) thresholds.
func (e *Engine) checkBudgetBeforeExec(rs *runState, nodeID string) error {
	// Daily spend cap: pause (resumable) before executing the next node
	// when the shared per-day ledger is over the cap. Checked before the
	// per-run budget — and independently of whether a per-run budget is
	// declared — so a cap trip pauses (resumable) rather than fails.
	if e.dailyCap != nil {
		if st, err := e.dailyCap.Status(rs.ctx); err != nil {
			e.logger.Warn("daily spend cap: status check failed: %v", err)
		} else if st.Exceeded {
			return e.handleCostCapPause(rs, nodeID, st)
		}
	}

	if rs.budget == nil {
		return nil
	}
	checks := rs.budget.Check()

	// Hard exceeded (100%+).
	if exc := findExceeded(checks); exc != nil {
		return e.failBudgetExceeded(rs, nodeID, exc)
	}

	// Hard limit (90%+) — refuse new node executions to prevent concurrent overage.
	if hl := findHardLimited(checks); hl != nil {
		_ = e.emit(rs.ctx, rs.runID, store.EventBudgetExceeded, nodeID, map[string]any{
			"dimension":  hl.dimension,
			"used":       hl.used,
			"limit":      hl.limit,
			"hard_limit": true,
		})
		return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget hard limit reached: %s at %.0f%% (%.0f/%.0f)", hl.dimension, (hl.used/hl.limit)*100, hl.used, hl.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow; new executions are blocked at 90%% to prevent concurrent overage", hl.dimension),
		})
	}

	return nil
}

// recordAndCheckBudget records usage from a node execution and emits
// budget_warning / budget_exceeded events as needed.
func (e *Engine) recordAndCheckBudget(rs *runState, nodeID string, output map[string]any) error {
	tokens, costUSD := extractUsage(output)

	// Daily spend cap accounting (independent of the per-run budget so it
	// works for workflows with no budget: block). We only record — the
	// pause decision happens on the pre-exec path so we anchor the
	// checkpoint at a not-yet-executed node.
	if e.dailyCap != nil && costUSD > 0 {
		rs.costUSDTotal += costUSD
		if _, err := e.dailyCap.Record(rs.ctx, rs.runID, rs.costUSDTotal); err != nil {
			e.logger.Warn("daily spend cap: record failed: %v", err)
		}
	}

	if rs.budget == nil {
		return nil
	}

	checks := rs.budget.RecordUsage(tokens, costUSD)

	// Emit warnings.
	for _, w := range findWarnings(checks) {
		_ = e.emit(rs.ctx, rs.runID, store.EventBudgetWarning, nodeID, map[string]any{
			"dimension": w.dimension,
			"advisory":  w.advisory,
			"used":      w.used,
			"limit":     w.limit,
		})
	}

	// Fail on exceeded.
	if exc := findExceeded(checks); exc != nil {
		return e.failBudgetExceeded(rs, nodeID, exc)
	}

	return nil
}

// failBudgetExceeded emits a budget_exceeded event for a hard-exceeded
// (100%+) dimension and fails the run with a checkpoint-preserving
// RuntimeError. Shared by the pre-exec (checkBudgetBeforeExec) and
// post-exec (recordAndCheckBudget) budget checks, which produced this
// identical event+error pair. The 90% hard-limit case stays inline in
// checkBudgetBeforeExec — it has a distinct message, hint, and event
// field, and is reached from only one site.
func (e *Engine) failBudgetExceeded(rs *runState, nodeID string, exc *budgetCheckResult) error {
	_ = e.emit(rs.ctx, rs.runID, store.EventBudgetExceeded, nodeID, map[string]any{
		"dimension": exc.dimension,
		"used":      exc.used,
		"limit":     exc.limit,
	})
	return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
		Code:    ErrCodeBudgetExceeded,
		Message: fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
		NodeID:  nodeID,
		Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow", exc.dimension),
	})
}
