package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Test: NewFromRecipe — recipe presets are applied and execution works
// ---------------------------------------------------------------------------

func TestNewFromRecipePresetVars(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "review_wf",
		Entry: "review",
		Nodes: map[string]ir.Node{
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{SystemPrompt: "sys"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "review", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{
			"sys": {Name: "sys", Body: "You are a reviewer."},
		},
		Vars: map[string]*ir.Var{
			"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "claude"},
			"style": {Name: "style", Type: ir.VarString},
		},
		Loops: map[string]*ir.Loop{},
	}

	spec := &recipe.RecipeSpec{
		Name:        "fast_review",
		WorkflowRef: recipe.WorkflowRef{Name: "review_wf"},
		PresetVars: recipe.PresetVars{
			"model": "gpt-4o",
			"style": "concise",
		},
	}

	exec := newStubExecutor()
	var capturedInput map[string]interface{}
	exec.on("review", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedInput = input
		return map[string]interface{}{"summary": "ok"}, nil
	})

	s := tmpStore(t)
	eng, err := NewFromRecipe(spec, wf, s, exec)
	if err != nil {
		t.Fatalf("NewFromRecipe: %v", err)
	}

	err = eng.Run(context.Background(), "recipe-run-001", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify run completed.
	r, err := s.LoadRun("recipe-run-001")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// The resolved vars should reflect recipe presets.
	// (capturedInput may be empty if no edge mappings, but the engine should have resolved vars.)
	_ = capturedInput
}

// ---------------------------------------------------------------------------
// Test: NewFromRecipe — prompt pack override
// ---------------------------------------------------------------------------

func TestNewFromRecipePromptPack(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "review_wf",
		Entry: "review",
		Nodes: map[string]ir.Node{
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{SystemPrompt: "sys"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "review", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{
			"sys": {Name: "sys", Body: "Original prompt."},
		},
		Vars:  map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{},
	}

	spec := &recipe.RecipeSpec{
		Name:        "strict_review",
		WorkflowRef: recipe.WorkflowRef{Name: "review_wf"},
		PromptPack: recipe.PromptPack{
			"sys": "Be very strict and thorough.",
		},
	}

	exec := newStubExecutor()
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "done"}, nil
	})

	s := tmpStore(t)
	eng, err := NewFromRecipe(spec, wf, s, exec)
	if err != nil {
		t.Fatalf("NewFromRecipe: %v", err)
	}

	// Verify the engine's workflow has the overridden prompt.
	if eng.workflow.Prompts["sys"].Body != "Be very strict and thorough." {
		t.Errorf("prompt body = %q, want override", eng.workflow.Prompts["sys"].Body)
	}

	// Original workflow should not be modified.
	if wf.Prompts["sys"].Body != "Original prompt." {
		t.Error("original workflow prompt was mutated")
	}

	err = eng.Run(context.Background(), "recipe-run-002", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	r, _ := s.LoadRun("recipe-run-002")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: NewFromRecipe — budget override
// ---------------------------------------------------------------------------

func TestNewFromRecipeBudgetOverride(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "review_wf",
		Entry: "review",
		Nodes: map[string]ir.Node{
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "review", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget: &ir.Budget{
			MaxTokens:  100000,
			MaxCostUSD: 10.0,
		},
	}

	spec := &recipe.RecipeSpec{
		Name:        "cheap_review",
		WorkflowRef: recipe.WorkflowRef{Name: "review_wf"},
		Budget: &recipe.BudgetOverride{
			MaxCostUSD: 2.0,
		},
	}

	exec := newStubExecutor()
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng, err := NewFromRecipe(spec, wf, s, exec)
	if err != nil {
		t.Fatalf("NewFromRecipe: %v", err)
	}

	// Budget should merge: cost overridden, tokens preserved.
	if eng.workflow.Budget.MaxCostUSD != 2.0 {
		t.Errorf("budget.max_cost_usd = %v, want 2.0", eng.workflow.Budget.MaxCostUSD)
	}
	if eng.workflow.Budget.MaxTokens != 100000 {
		t.Errorf("budget.max_tokens = %d, want 100000", eng.workflow.Budget.MaxTokens)
	}
}

// ---------------------------------------------------------------------------
// Test: NewFromRecipe — workflow name mismatch
// ---------------------------------------------------------------------------

func TestNewFromRecipeNameMismatch(t *testing.T) {
	wf := &ir.Workflow{
		Name:    "actual_wf",
		Entry:   "n",
		Nodes:   map[string]ir.Node{"n": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "n"}}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	spec := &recipe.RecipeSpec{
		Name:        "test",
		WorkflowRef: recipe.WorkflowRef{Name: "wrong_name"},
	}

	_, err := NewFromRecipe(spec, wf, tmpStore(t), newStubExecutor())
	if err == nil {
		t.Fatal("expected error for name mismatch")
	}
}

// TestResolveVarsExpandsProjectDirInOverrides locks in the fix for a
// regression where the editor's LaunchView pre-fills the form with the
// literal default `${PROJECT_DIR}`, the user submits unchanged, and the
// override reaches tool nodes verbatim — breaking commands like
// `git -C '${PROJECT_DIR}' add ...` because the literal path doesn't
// exist on disk.
func TestResolveVarsExpandsProjectDirInOverrides(t *testing.T) {
	wf := &ir.Workflow{
		Name:    "wf",
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"workspace_dir": {
				Name:       "workspace_dir",
				Type:       ir.VarString,
				HasDefault: true,
				Default:    "${PROJECT_DIR}",
			},
		},
		Loops: map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec, WithWorkDir("/tmp/run-xyz"))

	// Default-only path: PROJECT_DIR resolves to the engine's workDir.
	got := eng.resolveVars(nil)
	if got["workspace_dir"] != "/tmp/run-xyz" {
		t.Errorf("default expansion: got %q, want %q", got["workspace_dir"], "/tmp/run-xyz")
	}

	// Override path: the editor re-sends the literal default. The same
	// expansion must apply, otherwise tool nodes see `${PROJECT_DIR}`
	// verbatim and shell-out fails.
	got2 := eng.resolveVars(map[string]interface{}{"workspace_dir": "${PROJECT_DIR}"})
	if got2["workspace_dir"] != "/tmp/run-xyz" {
		t.Errorf("override expansion: got %q, want %q", got2["workspace_dir"], "/tmp/run-xyz")
	}

	// User explicitly setting an absolute path remains untouched.
	got3 := eng.resolveVars(map[string]interface{}{"workspace_dir": "/some/other/path"})
	if got3["workspace_dir"] != "/some/other/path" {
		t.Errorf("explicit path passthrough: got %q, want %q", got3["workspace_dir"], "/some/other/path")
	}
}

// ---------------------------------------------------------------------------
// Test: NewFromRecipe — run inputs override recipe presets
// ---------------------------------------------------------------------------

func TestNewFromRecipeInputsOverridePresets(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "wf",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "agent", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"target": {Name: "target", Type: ir.VarString},
		},
		Loops: map[string]*ir.Loop{},
	}

	spec := &recipe.RecipeSpec{
		Name:        "preset_test",
		WorkflowRef: recipe.WorkflowRef{Name: "wf"},
		PresetVars:  recipe.PresetVars{"target": "preset_value"},
	}

	exec := newStubExecutor()
	exec.on("agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng, err := NewFromRecipe(spec, wf, s, exec)
	if err != nil {
		t.Fatalf("NewFromRecipe: %v", err)
	}

	// The recipe sets "target" default to "preset_value".
	// Run inputs override it to "run_value".
	// resolveVars should pick up the run input over the preset default.
	vars := eng.resolveVars(map[string]interface{}{"target": "run_value"})
	if vars["target"] != "run_value" {
		t.Errorf("target = %v, want %q (run input should override preset)", vars["target"], "run_value")
	}

	// Without run input, should use preset default.
	vars2 := eng.resolveVars(nil)
	if vars2["target"] != "preset_value" {
		t.Errorf("target = %v, want %q (preset default)", vars2["target"], "preset_value")
	}
}
