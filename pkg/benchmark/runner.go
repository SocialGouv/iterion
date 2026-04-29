package benchmark

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RunnerConfig configures a benchmark run.
type RunnerConfig struct {
	// CaseLabel is a human-readable label for the benchmark case (e.g. PR title).
	CaseLabel string

	// Workflow is the base compiled workflow all recipes share.
	Workflow *ir.Workflow

	// Recipes to compare. At least two are required for a meaningful benchmark.
	Recipes []*recipe.RecipeSpec

	// Inputs are the common run-time inputs passed to every recipe.
	Inputs map[string]interface{}

	// ExecutorFactory creates a fresh NodeExecutor for each recipe run,
	// ensuring complete isolation (no shared caches, sessions, etc.).
	ExecutorFactory func() runtime.NodeExecutor
}

// Runner orchestrates multi-recipe benchmarks with isolated workspaces.
type Runner struct {
	config RunnerConfig
}

// NewRunner creates a benchmark runner from the given configuration.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if len(cfg.Recipes) < 2 {
		return nil, fmt.Errorf("benchmark: at least 2 recipes required, got %d", len(cfg.Recipes))
	}
	if cfg.Workflow == nil {
		return nil, fmt.Errorf("benchmark: workflow is required")
	}
	if cfg.ExecutorFactory == nil {
		return nil, fmt.Errorf("benchmark: executor factory is required")
	}
	return &Runner{config: cfg}, nil
}

// RecipeRun holds the result of a single recipe's execution.
type RecipeRun struct {
	Recipe  *recipe.RecipeSpec
	RunID   string
	Store   *store.RunStore
	Metrics *RunMetrics
	Err     error
}

// Run executes all recipes sequentially, each in its own isolated store,
// collects metrics, and returns a BenchmarkReport.
func (r *Runner) Run(ctx context.Context, storeRoot string) (*BenchmarkReport, error) {
	benchID := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	results := make([]*RecipeRun, len(r.config.Recipes))

	for i, rec := range r.config.Recipes {
		result := r.runRecipe(ctx, storeRoot, benchID, rec, i)
		results[i] = result
	}

	// Build report.
	report := &BenchmarkReport{
		ID:        benchID,
		CreatedAt: time.Now().UTC(),
		CaseLabel: r.config.CaseLabel,
	}

	for _, res := range results {
		if res.Err != nil {
			// Still include the metrics (partial) with error status.
			m := &RunMetrics{
				RecipeName: res.Recipe.Name,
				RunID:      res.RunID,
				Status:     "error",
				Verdict:    fmt.Sprintf("error: %v", res.Err),
			}
			report.Results = append(report.Results, m)
			continue
		}
		report.Results = append(report.Results, res.Metrics)
	}

	return report, nil
}

// runRecipe executes a single recipe in an isolated store workspace.
func (r *Runner) runRecipe(ctx context.Context, storeRoot, benchID string, rec *recipe.RecipeSpec, index int) *RecipeRun {
	result := &RecipeRun{Recipe: rec}

	// Each recipe gets its own isolated store directory.
	isoRoot := fmt.Sprintf("%s/%s/recipe-%d-%s", storeRoot, benchID, index, rec.Name)
	isoStore, err := store.New(isoRoot)
	if err != nil {
		result.Err = fmt.Errorf("create isolated store: %w", err)
		return result
	}
	result.Store = isoStore

	// Create engine from recipe with a fresh executor.
	executor := r.config.ExecutorFactory()
	engine, err := runtime.NewFromRecipe(rec, r.config.Workflow, isoStore, executor)
	if err != nil {
		result.Err = fmt.Errorf("create engine for recipe %q: %w", rec.Name, err)
		return result
	}

	// Generate unique run ID.
	runID := fmt.Sprintf("%s-%s", benchID, rec.Name)
	result.RunID = runID

	// Execute.
	if err := engine.Run(ctx, runID, r.config.Inputs); err != nil {
		result.Err = err
		// Still try to collect partial metrics.
	}

	// Collect metrics.
	primaryMetric := ""
	if rec.EvaluationPolicy.PrimaryMetric != "" {
		primaryMetric = rec.EvaluationPolicy.PrimaryMetric
	}
	metrics, err := CollectMetrics(isoStore, runID, rec.Name, primaryMetric)
	if err != nil && result.Err == nil {
		result.Err = fmt.Errorf("collect metrics for recipe %q: %w", rec.Name, err)
		return result
	}
	if metrics != nil {
		result.Metrics = metrics
	}

	return result
}
