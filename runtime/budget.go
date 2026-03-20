package runtime

import (
	"fmt"
	"sync"
	"time"

	"github.com/iterion-ai/iterion/ir"
)

// ErrBudgetExceeded is returned when a budget limit has been reached.
var ErrBudgetExceeded = fmt.Errorf("runtime: budget exceeded")

// SharedBudget tracks resource consumption across a workflow run.
// It is safe for concurrent use by parallel branches (first-come-first-served).
type SharedBudget struct {
	mu sync.Mutex

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

const budgetWarningThreshold = 0.8

// newSharedBudget creates a SharedBudget from an IR Budget definition.
// Returns nil if budget is nil or has no enforceable limits.
func newSharedBudget(b *ir.Budget) *SharedBudget {
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
	exceeded  bool
	warning   bool
	dimension string // "tokens", "cost_usd", "iterations", "duration"
	used      float64
	limit     float64
}

// RecordUsage records resource consumption from a node execution and returns
// check results. tokens and costUSD may be zero if the executor does not
// report them.
func (b *SharedBudget) RecordUsage(tokens int, costUSD float64) []budgetCheckResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.iterationsUsed++
	b.tokensUsed += tokens
	b.costUsed += costUSD

	return b.checkLocked()
}

// Check checks current budget status without recording usage.
func (b *SharedBudget) Check() []budgetCheckResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkLocked()
}

func (b *SharedBudget) checkLocked() []budgetCheckResult {
	var results []budgetCheckResult

	// Iterations.
	if b.maxIterations > 0 {
		ratio := float64(b.iterationsUsed) / float64(b.maxIterations)
		if ratio >= 1.0 {
			results = append(results, budgetCheckResult{
				exceeded: true, dimension: "iterations",
				used: float64(b.iterationsUsed), limit: float64(b.maxIterations),
			})
		} else if ratio >= budgetWarningThreshold && !b.warningsEmitted["iterations"] {
			b.warningsEmitted["iterations"] = true
			results = append(results, budgetCheckResult{
				warning: true, dimension: "iterations",
				used: float64(b.iterationsUsed), limit: float64(b.maxIterations),
			})
		}
	}

	// Tokens.
	if b.maxTokens > 0 {
		ratio := float64(b.tokensUsed) / float64(b.maxTokens)
		if ratio >= 1.0 {
			results = append(results, budgetCheckResult{
				exceeded: true, dimension: "tokens",
				used: float64(b.tokensUsed), limit: float64(b.maxTokens),
			})
		} else if ratio >= budgetWarningThreshold && !b.warningsEmitted["tokens"] {
			b.warningsEmitted["tokens"] = true
			results = append(results, budgetCheckResult{
				warning: true, dimension: "tokens",
				used: float64(b.tokensUsed), limit: float64(b.maxTokens),
			})
		}
	}

	// Cost.
	if b.maxCostUSD > 0 {
		ratio := b.costUsed / b.maxCostUSD
		if ratio >= 1.0 {
			results = append(results, budgetCheckResult{
				exceeded: true, dimension: "cost_usd",
				used: b.costUsed, limit: b.maxCostUSD,
			})
		} else if ratio >= budgetWarningThreshold && !b.warningsEmitted["cost_usd"] {
			b.warningsEmitted["cost_usd"] = true
			results = append(results, budgetCheckResult{
				warning: true, dimension: "cost_usd",
				used: b.costUsed, limit: b.maxCostUSD,
			})
		}
	}

	// Duration.
	if b.maxDuration > 0 {
		elapsed := time.Since(b.startedAt)
		ratio := float64(elapsed) / float64(b.maxDuration)
		if ratio >= 1.0 {
			results = append(results, budgetCheckResult{
				exceeded: true, dimension: "duration",
				used: float64(elapsed), limit: float64(b.maxDuration),
			})
		} else if ratio >= budgetWarningThreshold && !b.warningsEmitted["duration"] {
			b.warningsEmitted["duration"] = true
			results = append(results, budgetCheckResult{
				warning: true, dimension: "duration",
				used: float64(elapsed), limit: float64(b.maxDuration),
			})
		}
	}

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
