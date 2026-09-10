package ir

import (
	"fmt"
	"time"
)

// BudgetOverrides carries launch-time budget limits that override a
// workflow's declared `budget:` block at run time. Each field uses the
// "non-zero wins, zero inherits" convention shared with recipe budget
// overrides (see recipe.applyBudget). This lets any bot be re-budgeted at
// launch without editing its .bot — e.g.
//
//	iterion run bots/foo/main.bot --max-cost-usd 120 --max-duration 4h
//
// or the equivalent `budget` object on POST /api/runs. Precedence is DSL
// budget: → recipe/preset → launch overrides (overrides win, which is the
// intent of an at-run override).
type BudgetOverrides struct {
	MaxCostUSD          float64
	MaxTokens           int
	MaxDuration         string
	MaxIterations       int
	MaxParallelBranches int
	// CapImposed travels with an override whose value was clamped by an
	// external authority (pool-grant allowance) so the runtime knows the
	// resulting cap is absolute — see ir.Budget.CapImposed.
	CapImposed bool
}

// IsZero reports whether no override was supplied. CapImposed alone is
// not an override: it marks a clamp that already lowered a cap, so it
// only ever travels next to that cap — a value carrying nothing but the
// marker has nothing to apply, nothing to put on the wire and nothing to
// persist (it used to persist as an empty `budget_overrides: {}`).
func (o BudgetOverrides) IsZero() bool {
	return o.MaxCostUSD <= 0 && o.MaxTokens <= 0 && o.MaxDuration == "" &&
		o.MaxIterations <= 0 && o.MaxParallelBranches <= 0
}

// Validate rejects a malformed MaxDuration early with an actionable
// error, rather than letting newSharedBudget silently drop an unparseable
// duration (which would look like the override had no effect).
func (o BudgetOverrides) Validate() error {
	if o.MaxDuration != "" {
		if _, err := time.ParseDuration(ExpandEnvWithDefault(o.MaxDuration)); err != nil {
			return fmt.Errorf("max_duration %q: %w (use a Go duration like 30m, 2h, 90m)", o.MaxDuration, err)
		}
	}
	return nil
}

// ApplyBudgetOverrides mutates wf.Budget in place with any non-zero
// override. A nil wf.Budget is allocated only when at least one override is
// supplied; otherwise the workflow keeps its (possibly nil) budget so
// newSharedBudget continues to treat the run as unbudgeted. Mirrors
// recipe.applyBudget's precedence: non-zero override wins, zero inherits.
//
// It must run AFTER the workflow is resolved (recipe/preset budget already
// folded in) but BEFORE the executor is built — the executor snapshots
// Budget at construction time, so a later mutation would be invisible to
// the model/cost layer.
func ApplyBudgetOverrides(wf *Workflow, o BudgetOverrides) {
	if wf == nil || o.IsZero() {
		return
	}
	if wf.Budget == nil {
		wf.Budget = &Budget{}
	}
	if o.MaxCostUSD > 0 {
		wf.Budget.MaxCostUSD = o.MaxCostUSD
	}
	if o.MaxTokens > 0 {
		wf.Budget.MaxTokens = o.MaxTokens
	}
	if o.MaxDuration != "" {
		wf.Budget.MaxDuration = o.MaxDuration
	}
	if o.MaxIterations > 0 {
		wf.Budget.MaxIterations = o.MaxIterations
	}
	if o.MaxParallelBranches > 0 {
		wf.Budget.MaxParallelBranches = o.MaxParallelBranches
	}
	if o.CapImposed {
		wf.Budget.CapImposed = true
	}
}
