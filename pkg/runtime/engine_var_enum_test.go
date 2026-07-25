package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func enumTestWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "enum_gate_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"mode": {
				Name:       "mode",
				Type:       ir.VarString,
				EnumValues: []string{"autonomous", "interview"},
				HasDefault: true,
				Default:    "autonomous",
			},
			"free": {Name: "free", Type: ir.VarString},
		},
		Loops: map[string]*ir.Loop{},
	}
}

// TestValidateVarEnums exercises the launch gate directly: only
// provided values of enum-constrained string vars are checked, and a
// violation names the var, the value, and the allowed list.
func TestValidateVarEnums(t *testing.T) {
	eng := New(enumTestWorkflow(), tmpStore(t), newStubExecutor(), WithWorkDir(t.TempDir()))

	if err := eng.validateVarEnums(nil); err != nil {
		t.Errorf("nil inputs: unexpected error %v", err)
	}
	if err := eng.validateVarEnums(map[string]any{"mode": "interview"}); err != nil {
		t.Errorf("valid enum value: unexpected error %v", err)
	}
	// Unconstrained vars and undeclared keys pass untouched.
	if err := eng.validateVarEnums(map[string]any{"free": "anything", "undeclared": "x"}); err != nil {
		t.Errorf("unconstrained/undeclared: unexpected error %v", err)
	}

	err := eng.validateVarEnums(map[string]any{"mode": "yolo"})
	if err == nil {
		t.Fatal("expected an error for a value outside the enum set")
	}
	for _, want := range []string{`var "mode"`, `"yolo"`, `"autonomous"`, `"interview"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q (must name the var, the value, and the allowed list)", err.Error(), want)
		}
	}

	// A non-string value can never satisfy a string enum.
	if err := eng.validateVarEnums(map[string]any{"mode": 5}); err == nil {
		t.Error("expected an error for a non-string value on an enum var")
	}
}

// TestValidateVarEnumsExpandsEnv pins that the gate checks the value the
// run will actually see: a ${VAR} override is expanded with the same
// callback resolveVars uses before it is matched against the enum set.
func TestValidateVarEnumsExpandsEnv(t *testing.T) {
	eng := New(enumTestWorkflow(), tmpStore(t), newStubExecutor(), WithWorkDir(t.TempDir()))

	t.Setenv("ITERION_TEST_ENUM_MODE", "interview")
	if err := eng.validateVarEnums(map[string]any{"mode": "${ITERION_TEST_ENUM_MODE}"}); err != nil {
		t.Errorf("env-expanded valid value: unexpected error %v", err)
	}
	t.Setenv("ITERION_TEST_ENUM_MODE", "yolo")
	if err := eng.validateVarEnums(map[string]any{"mode": "${ITERION_TEST_ENUM_MODE}"}); err == nil {
		t.Error("expected an error once the env var expands to a non-enum value")
	}
}

// TestRunRejectsInvalidEnumVar verifies the gate at the Engine.Run seam —
// the single funnel every launch surface (CLI --var, HTTP launch,
// dispatcher bot_args, presets) delivers its inputs through: an invalid
// value fails the run before any node executes, a valid one runs to done.
func TestRunRejectsInvalidEnumVar(t *testing.T) {
	ctx := context.Background()

	s := tmpStore(t)
	eng := New(enumTestWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))
	err := eng.Run(ctx, "run-enum-bad", map[string]any{"mode": "yolo"})
	if err == nil {
		t.Fatal("expected Run to fail on an out-of-enum var value")
	}
	if !strings.Contains(err.Error(), "allowed values") {
		t.Errorf("Run error %q should mention the allowed values", err.Error())
	}
	r, lerr := s.LoadRun(ctx, "run-enum-bad")
	if lerr != nil {
		t.Fatalf("LoadRun: %v", lerr)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("run status = %s, want %s", r.Status, store.RunStatusFailed)
	}
	// No node ran: the gate fires before the exec loop.
	evs, _ := s.LoadEvents(ctx, "run-enum-bad")
	if hasEventType(evs, store.EventNodeStarted) {
		t.Error("no node should start when var validation fails")
	}

	// Control: the same workflow with a valid value runs to completion.
	s2 := tmpStore(t)
	eng2 := New(enumTestWorkflow(), s2, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng2.Run(ctx, "run-enum-ok", map[string]any{"mode": "interview"}); err != nil {
		t.Fatalf("valid enum value should run cleanly, got %v", err)
	}
	r2, _ := s2.LoadRun(ctx, "run-enum-ok")
	if r2.Status != store.RunStatusFinished {
		t.Errorf("run status = %s, want %s", r2.Status, store.RunStatusFinished)
	}
}
