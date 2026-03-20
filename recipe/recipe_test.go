package recipe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/iterion-ai/iterion/ir"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sampleWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "pr_refine",
		Entry: "review",
		Nodes: map[string]*ir.Node{
			"review": {ID: "review", Kind: ir.NodeAgent, SystemPrompt: "review_sys", UserPrompt: "review_usr"},
			"act":    {ID: "act", Kind: ir.NodeAgent},
			"done":   {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "review", To: "act"},
			{From: "act", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{
			"review_sys": {Name: "review_sys", Body: "You are a code reviewer."},
			"review_usr": {Name: "review_usr", Body: "Review this PR: {{vars.pr_title}}"},
		},
		Vars: map[string]*ir.Var{
			"pr_title":   {Name: "pr_title", Type: ir.VarString},
			"model_name": {Name: "model_name", Type: ir.VarString, HasDefault: true, Default: "claude"},
		},
		Loops: map[string]*ir.Loop{},
		Budget: &ir.Budget{
			MaxTokens:  100000,
			MaxCostUSD: 10.0,
		},
	}
}

func writeJSON(t *testing.T, dir, name string, v interface{}) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Test: Load from JSON
// ---------------------------------------------------------------------------

func TestLoadValidRecipe(t *testing.T) {
	spec := &RecipeSpec{
		Name:        "fast_review",
		WorkflowRef: WorkflowRef{Name: "pr_refine", Path: "examples/pr_refine.iter"},
		PresetVars: PresetVars{
			"model_name": "gpt-4o",
		},
		PromptPack: PromptPack{
			"review_sys": "You are a fast code reviewer. Be concise.",
		},
		Budget: &BudgetOverride{
			MaxCostUSD: 5.0,
			MaxTokens:  50000,
		},
		EvaluationPolicy: EvaluationPolicy{
			PrimaryMetric:    "approved",
			SuccessValue:     "true",
			SecondaryMetrics: []string{"total_cost_usd", "total_duration", "total_iterations"},
		},
	}

	dir := t.TempDir()
	path := writeJSON(t, dir, "fast_review.json", spec)

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if loaded.Name != "fast_review" {
		t.Errorf("name = %q, want %q", loaded.Name, "fast_review")
	}
	if loaded.WorkflowRef.Name != "pr_refine" {
		t.Errorf("workflow_ref.name = %q, want %q", loaded.WorkflowRef.Name, "pr_refine")
	}
	if loaded.PresetVars["model_name"] != "gpt-4o" {
		t.Errorf("preset model_name = %v, want %q", loaded.PresetVars["model_name"], "gpt-4o")
	}
	if loaded.PromptPack["review_sys"] == "" {
		t.Error("prompt pack review_sys should not be empty")
	}
	if loaded.Budget.MaxCostUSD != 5.0 {
		t.Errorf("budget.max_cost_usd = %v, want 5.0", loaded.Budget.MaxCostUSD)
	}
	if loaded.EvaluationPolicy.PrimaryMetric != "approved" {
		t.Errorf("eval.primary_metric = %q, want %q", loaded.EvaluationPolicy.PrimaryMetric, "approved")
	}
	if len(loaded.EvaluationPolicy.SecondaryMetrics) != 3 {
		t.Errorf("eval.secondary_metrics len = %d, want 3", len(loaded.EvaluationPolicy.SecondaryMetrics))
	}
}

func TestLoadMinimalRecipe(t *testing.T) {
	data := []byte(`{"name":"minimal","workflow_ref":{"name":"wf"}}`)
	spec, err := Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.Name != "minimal" {
		t.Errorf("name = %q, want %q", spec.Name, "minimal")
	}
}

// ---------------------------------------------------------------------------
// Test: Validation
// ---------------------------------------------------------------------------

func TestValidateMissingName(t *testing.T) {
	_, err := Load([]byte(`{"workflow_ref":{"name":"wf"}}`))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestValidateMissingWorkflowRef(t *testing.T) {
	_, err := Load([]byte(`{"name":"r"}`))
	if err == nil {
		t.Fatal("expected error for missing workflow_ref.name")
	}
}

func TestValidateMissingWorkflowRefName(t *testing.T) {
	_, err := Load([]byte(`{"name":"r","workflow_ref":{}}`))
	if err == nil {
		t.Fatal("expected error for empty workflow_ref.name")
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	_, err := Load([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadFileMissing(t *testing.T) {
	_, err := LoadFile("/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — preset vars
// ---------------------------------------------------------------------------

func TestApplyPresetVars(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		PresetVars: PresetVars{
			"model_name": "gpt-4o",
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Preset var should have new default.
	v := applied.Vars["model_name"]
	if !v.HasDefault || v.Default != "gpt-4o" {
		t.Errorf("model_name default = %v, want gpt-4o", v.Default)
	}

	// Original workflow should not be mutated.
	if wf.Vars["model_name"].Default != "claude" {
		t.Error("original workflow was mutated")
	}

	// Non-preset var should be unchanged.
	if applied.Vars["pr_title"].HasDefault {
		t.Error("pr_title should not have a default")
	}
}

func TestApplyPresetVarUndeclared(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		PresetVars:  PresetVars{"nonexistent": "value"},
	}

	_, err := spec.Apply(wf)
	if err == nil {
		t.Fatal("expected error for undeclared preset var")
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — prompt pack
// ---------------------------------------------------------------------------

func TestApplyPromptPack(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		PromptPack: PromptPack{
			"review_sys": "New system prompt.",
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Prompts["review_sys"].Body != "New system prompt." {
		t.Errorf("review_sys body = %q, want %q", applied.Prompts["review_sys"].Body, "New system prompt.")
	}

	// Original should not be mutated.
	if wf.Prompts["review_sys"].Body != "You are a code reviewer." {
		t.Error("original prompt was mutated")
	}

	// Unchanged prompt should be preserved.
	if applied.Prompts["review_usr"].Body != "Review this PR: {{vars.pr_title}}" {
		t.Error("review_usr prompt should be unchanged")
	}
}

func TestApplyPromptPackUnknownPrompt(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		PromptPack:  PromptPack{"nonexistent": "body"},
	}

	_, err := spec.Apply(wf)
	if err == nil {
		t.Fatal("expected error for unknown prompt in pack")
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — budget override
// ---------------------------------------------------------------------------

func TestApplyBudgetOverride(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		Budget: &BudgetOverride{
			MaxCostUSD: 5.0,
			MaxTokens:  50000,
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Budget.MaxCostUSD != 5.0 {
		t.Errorf("budget.max_cost_usd = %v, want 5.0", applied.Budget.MaxCostUSD)
	}
	if applied.Budget.MaxTokens != 50000 {
		t.Errorf("budget.max_tokens = %d, want 50000", applied.Budget.MaxTokens)
	}

	// Original budget should not be mutated.
	if wf.Budget.MaxTokens != 100000 {
		t.Error("original budget was mutated")
	}
}

func TestApplyBudgetPartialOverride(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		Budget: &BudgetOverride{
			MaxDuration: "30m",
			// MaxTokens and MaxCostUSD are zero → keep workflow defaults.
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Budget.MaxDuration != "30m" {
		t.Errorf("budget.max_duration = %q, want %q", applied.Budget.MaxDuration, "30m")
	}
	// Preserved from workflow.
	if applied.Budget.MaxTokens != 100000 {
		t.Errorf("budget.max_tokens = %d, want 100000 (preserved)", applied.Budget.MaxTokens)
	}
	if applied.Budget.MaxCostUSD != 10.0 {
		t.Errorf("budget.max_cost_usd = %v, want 10.0 (preserved)", applied.Budget.MaxCostUSD)
	}
}

func TestApplyBudgetOnWorkflowWithNoBudget(t *testing.T) {
	wf := sampleWorkflow()
	wf.Budget = nil

	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		Budget: &BudgetOverride{
			MaxCostUSD: 20.0,
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Budget == nil {
		t.Fatal("applied budget should not be nil")
	}
	if applied.Budget.MaxCostUSD != 20.0 {
		t.Errorf("budget.max_cost_usd = %v, want 20.0", applied.Budget.MaxCostUSD)
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — workflow name mismatch
// ---------------------------------------------------------------------------

func TestApplyWorkflowNameMismatch(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "wrong_name"},
	}

	_, err := spec.Apply(wf)
	if err == nil {
		t.Fatal("expected error for workflow name mismatch")
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — no-op (no presets)
// ---------------------------------------------------------------------------

func TestApplyNoOp(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "test",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Should get back an equivalent workflow.
	if applied.Name != wf.Name {
		t.Errorf("name = %q, want %q", applied.Name, wf.Name)
	}
	if applied.Entry != wf.Entry {
		t.Errorf("entry = %q, want %q", applied.Entry, wf.Entry)
	}
}

// ---------------------------------------------------------------------------
// Test: Apply — combined presets
// ---------------------------------------------------------------------------

func TestApplyCombined(t *testing.T) {
	wf := sampleWorkflow()
	spec := &RecipeSpec{
		Name:        "full",
		WorkflowRef: WorkflowRef{Name: "pr_refine"},
		PresetVars: PresetVars{
			"model_name": "gpt-4o",
			"pr_title":   "Fix auth bug",
		},
		PromptPack: PromptPack{
			"review_sys": "Be strict.",
		},
		Budget: &BudgetOverride{
			MaxCostUSD:    3.0,
			MaxIterations: 5,
		},
		EvaluationPolicy: EvaluationPolicy{
			PrimaryMetric: "verdict",
			SuccessValue:  "approved",
		},
	}

	applied, err := spec.Apply(wf)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Vars["model_name"].Default != "gpt-4o" {
		t.Error("model_name preset not applied")
	}
	if applied.Vars["pr_title"].Default != "Fix auth bug" {
		t.Error("pr_title preset not applied")
	}
	if applied.Prompts["review_sys"].Body != "Be strict." {
		t.Error("prompt pack not applied")
	}
	if applied.Budget.MaxCostUSD != 3.0 {
		t.Error("budget cost override not applied")
	}
	if applied.Budget.MaxIterations != 5 {
		t.Error("budget iterations override not applied")
	}
	// Preserved from workflow.
	if applied.Budget.MaxTokens != 100000 {
		t.Error("budget tokens should be preserved from workflow")
	}
}

// ---------------------------------------------------------------------------
// Test: EvaluationPolicy
// ---------------------------------------------------------------------------

func TestEvaluationPolicyRoundTrip(t *testing.T) {
	spec := &RecipeSpec{
		Name:        "eval_test",
		WorkflowRef: WorkflowRef{Name: "wf"},
		EvaluationPolicy: EvaluationPolicy{
			PrimaryMetric:    "approved",
			SuccessValue:     "true",
			SecondaryMetrics: []string{"total_cost_usd", "total_duration"},
		},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	loaded, err := Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.EvaluationPolicy.PrimaryMetric != "approved" {
		t.Errorf("primary_metric = %q, want %q", loaded.EvaluationPolicy.PrimaryMetric, "approved")
	}
	if loaded.EvaluationPolicy.SuccessValue != "true" {
		t.Errorf("success_value = %q, want %q", loaded.EvaluationPolicy.SuccessValue, "true")
	}
	if len(loaded.EvaluationPolicy.SecondaryMetrics) != 2 {
		t.Errorf("secondary_metrics len = %d, want 2", len(loaded.EvaluationPolicy.SecondaryMetrics))
	}
}
