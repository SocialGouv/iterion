package runtime

import (
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/internal/log"
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

	// Consumed.
	tokensUsed     int
	costUsed       float64
	iterationsUsed int
	startedAt      time.Time

	// Warning tracking — each dimension warns at most once.
	warningsEmitted map[string]bool
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
		parsed, err := time.ParseDuration(b.MaxDuration)
		if err == nil {
			maxDur = parsed
		}
	}

	// If no limits are set beyond MaxParallelBranches (handled elsewhere), skip.
	if b.MaxTokens == 0 && b.MaxCostUSD == 0 && b.MaxIterations == 0 && maxDur == 0 {
		return nil
	}

	return &SharedBudget{
		logger:          logger,
		maxTokens:       b.MaxTokens,
		maxCostUSD:      b.MaxCostUSD,
		maxIterations:   b.MaxIterations,
		maxDuration:     maxDur,
		startedAt:       time.Now(),
		warningsEmitted: make(map[string]bool),
	}
}

// budgetCheckResult holds the outcome of a single dimension check.
type budgetCheckResult struct {
	exceeded    bool
	hardLimited bool // true when 90% <= ratio < 100%
	warning     bool
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

	return results
}

// findExceeded returns the first exceeded result, or nil.
func findExceeded(results []budgetCheckResult) *budgetCheckResult {
	for i := range results {
		if results[i].exceeded {
			return &results[i]
		}
	}
	return nil
}

// findHardLimited returns the first hard-limited result, or nil.
func findHardLimited(results []budgetCheckResult) *budgetCheckResult {
	for i := range results {
		if results[i].hardLimited {
			return &results[i]
		}
	}
	return nil
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
