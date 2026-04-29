// Package recipe defines the RecipeSpec model and loading/application logic.
// A recipe is a first-class unit above a raw workflow: it bundles a workflow
// reference with preset variables, prompt overrides, budget limits, and an
// evaluation policy. Recipes are the unit of comparison for benchmarking.
package recipe

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// RecipeSpec is the top-level recipe definition. It wraps a workflow with
// presets that make it a self-contained, executable, and comparable unit.
type RecipeSpec struct {
	Name             string           `json:"name"`
	WorkflowRef      WorkflowRef      `json:"workflow_ref"`
	PresetVars       PresetVars       `json:"preset_vars,omitempty"`
	PromptPack       PromptPack       `json:"prompt_pack,omitempty"`
	Budget           *BudgetOverride  `json:"budget,omitempty"`
	ToolPolicy       []string         `json:"tool_policy,omitempty"`
	EvaluationPolicy EvaluationPolicy `json:"evaluation_policy,omitempty"`
}

// WorkflowRef identifies the workflow to execute.
type WorkflowRef struct {
	Name string `json:"name"`           // workflow name (must match ir.Workflow.Name)
	Path string `json:"path,omitempty"` // optional path to .iter file
}

// PresetVars is a map of variable name → preset value. These override
// workflow variable defaults and can themselves be overridden by run inputs.
type PresetVars map[string]interface{}

// PromptPack is a map of prompt name → template body override. When applied,
// these replace the corresponding prompts in the compiled workflow.
type PromptPack map[string]string

// BudgetOverride defines execution limits that override the workflow budget.
// Zero values are ignored (the workflow default is kept).
type BudgetOverride struct {
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
}

// EvaluationPolicy defines how a recipe's execution should be evaluated.
// The primary metric determines success; secondary metrics are collected
// for comparison but do not determine pass/fail.
type EvaluationPolicy struct {
	// PrimaryMetric is the field name in the terminal node's output that
	// determines success. For example "approved" (bool) or "verdict" (string).
	PrimaryMetric string `json:"primary_metric"`

	// SuccessValue is the expected value for the primary metric.
	// For bool fields use "true"; for string fields use the exact value.
	SuccessValue string `json:"success_value,omitempty"`

	// SecondaryMetrics lists additional metrics to collect for comparison.
	// These are field names from the terminal node's output or built-in
	// runtime metrics: "total_cost_usd", "total_duration", "total_iterations",
	// "total_tokens", "total_retries".
	SecondaryMetrics []string `json:"secondary_metrics,omitempty"`
}

// LoadFile loads a RecipeSpec from a JSON file.
func LoadFile(path string) (*RecipeSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("recipe: read file %q: %w", path, err)
	}
	return Load(data)
}

// Load parses a RecipeSpec from JSON bytes.
func Load(data []byte) (*RecipeSpec, error) {
	var spec RecipeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("recipe: parse: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate checks the recipe spec for basic consistency.
func (r *RecipeSpec) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("recipe: name is required")
	}
	if r.WorkflowRef.Name == "" {
		return fmt.Errorf("recipe %q: workflow_ref.name is required", r.Name)
	}
	return nil
}

// Apply merges recipe presets onto a compiled workflow, producing a new
// workflow ready for execution. The original workflow is not modified.
func (r *RecipeSpec) Apply(wf *ir.Workflow) (*ir.Workflow, error) {
	if wf.Name != r.WorkflowRef.Name {
		return nil, fmt.Errorf("recipe %q: workflow name mismatch: recipe references %q but got %q",
			r.Name, r.WorkflowRef.Name, wf.Name)
	}

	// Shallow-copy the workflow so we can replace fields without mutating the original.
	applied := *wf

	// --- Apply preset vars ---
	if len(r.PresetVars) > 0 {
		newVars := make(map[string]*ir.Var, len(wf.Vars))
		for k, v := range wf.Vars {
			cp := *v
			newVars[k] = &cp
		}
		for name, val := range r.PresetVars {
			v, ok := newVars[name]
			if !ok {
				return nil, fmt.Errorf("recipe %q: preset var %q not declared in workflow", r.Name, name)
			}
			cp := *v
			cp.HasDefault = true
			cp.Default = val
			newVars[name] = &cp
		}
		applied.Vars = newVars
	}

	// --- Apply prompt pack ---
	if len(r.PromptPack) > 0 {
		newPrompts := make(map[string]*ir.Prompt, len(wf.Prompts))
		for k, p := range wf.Prompts {
			newPrompts[k] = p
		}
		for name, body := range r.PromptPack {
			existing, ok := newPrompts[name]
			if !ok {
				return nil, fmt.Errorf("recipe %q: prompt pack references unknown prompt %q", r.Name, name)
			}
			cp := *existing
			cp.Body = body
			// Re-parse template refs is not needed at this layer;
			// the runtime resolves templates from Body at execution time.
			newPrompts[name] = &cp
		}
		applied.Prompts = newPrompts
	}

	// --- Apply tool policy ---
	if len(r.ToolPolicy) > 0 {
		applied.ToolPolicy = r.ToolPolicy
	}

	// --- Apply budget override ---
	if r.Budget != nil {
		newBudget := r.applyBudget(wf.Budget)
		applied.Budget = newBudget
	}

	return &applied, nil
}

// applyBudget merges recipe budget overrides onto the workflow budget.
// Non-zero recipe values take precedence; zero values preserve the original.
func (r *RecipeSpec) applyBudget(base *ir.Budget) *ir.Budget {
	result := &ir.Budget{}
	if base != nil {
		*result = *base
	}
	b := r.Budget
	if b.MaxParallelBranches > 0 {
		result.MaxParallelBranches = b.MaxParallelBranches
	}
	if b.MaxDuration != "" {
		result.MaxDuration = b.MaxDuration
	}
	if b.MaxCostUSD > 0 {
		result.MaxCostUSD = b.MaxCostUSD
	}
	if b.MaxTokens > 0 {
		result.MaxTokens = b.MaxTokens
	}
	if b.MaxIterations > 0 {
		result.MaxIterations = b.MaxIterations
	}
	return result
}
