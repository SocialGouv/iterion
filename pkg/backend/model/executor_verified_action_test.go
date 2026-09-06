package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The Verified Action ladder (ADR-044) keys success on the postcondition,
// not the recipe's exit code. These tests exercise every rung with real
// shell recipes/postconditions in a per-test temp workspace; the self-repair
// and agent-recovery rungs use a scripted mock LLM client.

func vaExecutor(t *testing.T, workDir string, opts ...ClawExecutorOption) *ClawExecutor {
	t.Helper()
	all := append([]ClawExecutorOption{WithWorkDir(workDir)}, opts...)
	return newTestClawExecutor(NewRegistry(), &ir.Workflow{}, all...)
}

func vaMeta(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	m, ok := out["_verified_action"].(map[string]any)
	if !ok {
		t.Fatalf("output missing _verified_action metadata: %v", out)
	}
	return m
}

// Rung 1 — idempotent skip: postcondition already met → recipe does NOT run.
func TestVerifiedAction_IdempotentSkip(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the marker the postcondition checks for.
	if err := os.WriteFile(filepath.Join(dir, "pre_existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "skip_node"},
		Command:       "touch should_not_run",
		Postcondition: "test -f pre_existing",
		Policy:        ir.PolicyRequired,
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "should_not_run")); statErr == nil {
		t.Fatal("recipe ran despite postcondition already met (idempotent skip failed)")
	}
	if m := vaMeta(t, out); m["rung"] != "idempotent_skip" || m["postcondition_met"] != true {
		t.Fatalf("rung = %v, want idempotent_skip met=true", m)
	}
}

// Rung 2 — recipe satisfies the postcondition (the ~95% path).
func TestVerifiedAction_RecipeOK(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "recipe_ok"},
		Command:       "touch marker",
		Postcondition: "test -f marker",
		Policy:        ir.PolicyRequired,
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if m := vaMeta(t, out); m["rung"] != "recipe" || m["postcondition_met"] != true {
		t.Fatalf("rung = %v, want recipe met=true", m)
	}
}

// A postcondition resolves `{{outputs.<node>.<field>}}` from the template
// snapshot the run attached, exactly as it resolves `{{input.*}}` (#797).
// Before, the literal braces reached `test -f`, so the oracle could never
// be met — a silent constant in the one command that decides success.
func TestVerifiedAction_PostconditionResolvesOutputsRef(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "pc_outputs"},
		Command:       "touch marker",
		Postcondition: "test -f {{outputs.prev.marker}}",
		Policy:        ir.PolicyRequired,
	}
	node.PostcondRefs = mustRefs(node.Postcondition)
	ctx := WithTemplateData(context.Background(), &TemplateData{
		Outputs: map[string]map[string]any{"prev": {"marker": "marker"}},
	})
	out, err := exec.Execute(ctx, node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v — the postcondition did not resolve {{outputs.prev.marker}}", err)
	}
	if m := vaMeta(t, out); m["rung"] != "recipe" || m["postcondition_met"] != true {
		t.Fatalf("rung = %v, want recipe met=true", m)
	}
}

// The postcondition is truth, NOT the exit code: a recipe that exits
// non-zero but achieves the goal is a success ("nothing to commit" lies).
func TestVerifiedAction_ExitCodeLies(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "exit_lies"},
		Command:       "touch marker; exit 7",
		Postcondition: "test -f marker",
		Policy:        ir.PolicyRequired,
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: recipe exited non-zero but goal held — want success, got %v", err)
	}
	if m := vaMeta(t, out); m["rung"] != "recipe" {
		t.Fatalf("rung = %v, want recipe", m)
	}
}

// Postcondition stdout (JSON) becomes the success output so authors can
// surface state (e.g. a commit sha).
func TestVerifiedAction_PostconditionJSONOutput(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "pc_json"},
		Command:       "touch marker",
		Postcondition: `test -f marker && printf '{"sha":"abc123"}'`,
		Policy:        ir.PolicyRequired,
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out["sha"] != "abc123" {
		t.Fatalf("sha = %v, want abc123 (postcondition JSON stdout not surfaced)", out["sha"])
	}
}

// Rung 3 — self-repair: recipe fails the postcondition, the LLM proposes a
// corrected command, the runtime re-runs it deterministically, and the
// postcondition then holds.
func TestVerifiedAction_SelfRepair(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	// Scripted LLM returns a corrected command that creates the marker.
	mock := newMockClient(toolUseEvents("t1", "structured_output",
		`{"corrected_command":"touch repaired_marker"}`, 50, 10))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir))

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "repair_node"},
		Command:       "echo recipe-noop", // runs, but does not create repaired_marker
		Goal:          "ensure repaired_marker exists",
		Postcondition: "test -f repaired_marker",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 2},
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "repaired_marker")); statErr != nil {
		t.Fatalf("corrected command did not run: %v", statErr)
	}
	if m := vaMeta(t, out); m["rung"] != "self_repair" || m["postcondition_met"] != true {
		t.Fatalf("rung = %v, want self_repair met=true", m)
	}
}

// Rung 4 — agent recovery (opt-in). The postcondition is satisfied on its
// third evaluation (rung1, rung2, post-agent), standing in for the agent
// achieving the goal with real tools; the mock backend just returns text.
func TestVerifiedAction_AgentRecovery(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	mock := newMockClient(textEvents("done", 10, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir))

	// counter postcondition: succeeds only on the 3rd check.
	pc := `c=$(cat ctr 2>/dev/null || echo 0); c=$((c+1)); printf '%s' "$c" > ctr; test "$c" -ge 3`
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "agent_node"},
		Command:       "echo recipe-noop",
		Goal:          "achieve the goal with tools",
		Postcondition: pc,
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxAgentAttempts: 1}, // no self-repair
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if m := vaMeta(t, out); m["rung"] != "agent_recovery" || m["postcondition_met"] != true {
		t.Fatalf("rung = %v, want agent_recovery met=true", m)
	}
}

// Rung 5 — policy: required fails (resumable) when the postcondition is
// never met.
func TestVerifiedAction_PolicyRequiredFails(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "req_fail"},
		Command:       "echo recipe-noop",
		Postcondition: "false",
		Policy:        ir.PolicyRequired,
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if err == nil {
		t.Fatal("Execute: want error when postcondition unmet under policy required")
	}
}

// Rung 5 — policy: best_effort warns + continues (no error), output flags
// the unmet postcondition.
func TestVerifiedAction_PolicyBestEffortContinues(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "best_effort"},
		Command:       "echo recipe-noop",
		Postcondition: "false",
		Policy:        ir.PolicyBestEffort,
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: best_effort must not error, got %v", err)
	}
	if m := vaMeta(t, out); m["postcondition_met"] != false {
		t.Fatalf("postcondition_met = %v, want false", m["postcondition_met"])
	}
}

// Rung 5 — policy: recover fails after the rungs are exhausted (the
// corrected command still does not satisfy the postcondition).
func TestVerifiedAction_RecoverExhaustedFails(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "structured_output",
		`{"corrected_command":"true"}`, 50, 10)) // no-op repair
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir))

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "recover_fail"},
		Command:       "echo recipe-noop",
		Goal:          "impossible",
		Postcondition: "false",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 1},
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if err == nil {
		t.Fatal("Execute: want error after recover rungs exhausted and postcondition still unmet")
	}
}

// Backward compatibility: a node WITHOUT a postcondition keeps exit-code =
// success semantics (the pre-ADR-044 behaviour).
func TestVerifiedAction_BackwardCompatNoPostcondition(t *testing.T) {
	dir := t.TempDir()
	exec := vaExecutor(t, dir)

	t.Run("exit non-zero errors", func(t *testing.T) {
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "bc_fail"}, Command: "exit 1"}
		if _, err := exec.Execute(context.Background(), node, map[string]any{}); err == nil {
			t.Fatal("want error: no postcondition → exit code is truth")
		}
	})
	t.Run("exit zero succeeds with no _verified_action key", func(t *testing.T) {
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "bc_ok"}, Command: "echo ok"}
		out, err := exec.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if _, present := out["_verified_action"]; present {
			t.Fatal("non-verified node must not stamp _verified_action")
		}
	})
}
