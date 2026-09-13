package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const settledContractSource = `dsl: 2
schema payload:
  value: string
schema decision:
  again: bool
prompt sys:
  Test only.
compute planner:
  output: payload
  publish: plan
  expr:
    value: "'the plan'"
router fan:
  mode: fan_out_all
agent dead_a:
  model: "stub"
  system: sys
  output: payload
agent dead_b:
  model: "stub"
  system: sys
  output: payload
agent collect:
  model: "stub"
  system: sys
  output: payload
  publish: report
  await: best_effort
human gate:
  interaction: human
  output: decision
workflow wf:
  entry: planner
  worktree: none
  planner -> fan
  fan -> dead_a
  fan -> dead_b
  dead_a -> collect with { plan: "{{artifacts.plan}}" }
  dead_b -> collect
  collect -> gate
  gate -> collect as retry(3) when again
  gate -> done else
`

func TestSettledArtifactContractAfterForcedEdgeRemoval(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const id = "settled-contract-edit"
	compile := func(source string) *ir.Workflow {
		t.Helper()
		result := compileBot(t, source)
		if result.HasErrors() {
			t.Fatalf("compile: %+v", result.Diagnostics)
		}
		return result.Workflow
	}
	hash := func(source string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(source))) }
	original := compile(settledContractSource)
	executor := newStubExecutor()
	for _, node := range []string{"dead_a", "dead_b"} {
		executor.on(node, func(map[string]any) (map[string]any, error) {
			return nil, errors.New("branch failed before producing output")
		})
	}
	var inputs []map[string]any
	executor.on("collect", func(input map[string]any) (map[string]any, error) {
		inputs = append(inputs, deepCopyAnyMap(input))
		return map[string]any{"value": "published report"}, nil
	})
	launch := New(original, s, executor, WithWorkflowHash(hash(settledContractSource)), WithWorkflowSource(settledContractSource), WithExecutionContext(&store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce, RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}))
	if err := launch.Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial run: %v", err)
	}
	parked, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	carriedFloor := false
	for _, edge := range parked.Checkpoint.SettledIncoming["collect"] {
		carriedFloor = carriedFloor || edge.From == "dead_a"
	}
	if !carriedFloor {
		t.Fatal("the checkpoint did not carry the floor edge whose removal is under test")
	}
	old, err := s.LoadArtifact(ctx, id, "collect", 0)
	if err != nil {
		t.Fatal(err)
	}
	if old.Contract == nil || len(old.Contract.Dependencies) != 1 {
		t.Fatalf("first contract did not consume the floor artifact: %+v", old.Contract)
	}
	dep := old.Contract.Dependencies[0]
	if dep.LogicalRef != "plan" || dep.NodeID != "planner" || !dep.Required {
		t.Fatalf("floor dependency=%+v", dep)
	}
	if len(inputs) != 1 || inputs[0]["plan"] == nil {
		t.Fatalf("initial collector inputs=%v", inputs)
	}
	editedSource := strings.ReplaceAll(settledContractSource, "  fan -> dead_a\n", "")
	editedSource = strings.ReplaceAll(editedSource, "  dead_a -> collect with { plan: \"{{artifacts.plan}}\" }\n", "")
	editedSource = strings.ReplaceAll(editedSource, "agent dead_a:\n  model: \"stub\"\n  system: sys\n  output: payload\n", "")
	edited := compile(editedSource)
	// A source hash really changed; this must exercise force, not the legacy
	// empty-hash shortcut used by older graph-only resume fixtures.
	unforced := New(edited, s, executor, WithWorkflowHash(hash(editedSource)))
	if err := unforced.Resume(ctx, id, map[string]any{"again": true}); !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatalf("ordinary edited resume=%v, want source-change refusal", err)
	}
	forced := New(edited, s, executor, WithWorkflowHash(hash(editedSource)), WithWorkflowSource(editedSource), WithForceResume(true))
	if err := forced.Resume(ctx, id, map[string]any{"again": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("forced resume: %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("collector ran %d times, want 2", len(inputs))
	}
	if _, present := inputs[1]["plan"]; present {
		t.Fatalf("removed floor edge still supplied plan: %v", inputs[1])
	}
	current, err := s.LoadArtifact(ctx, id, "collect", 1)
	if err != nil {
		t.Fatal(err)
	}
	if current.Contract == nil || len(current.Contract.Dependencies) != 0 || current.Contract.ProducerRevision != hash(editedSource) {
		t.Fatalf("new contract retains removed dependency or old source: %+v", current.Contract)
	}
	oldAgain, err := s.LoadArtifact(ctx, id, "collect", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldAgain.Contract.Dependencies) != 1 || !oldAgain.Contract.Dependencies[0].Required {
		t.Fatal("historical consumed dependency was weakened")
	}
	// The accepted migration is durable: a fresh engine finishes without
	// requiring force again, validating the new authoritative contract.
	if err := New(edited, s, executor, WithWorkflowHash(hash(editedSource))).Resume(ctx, id, map[string]any{"again": false}); err != nil {
		t.Fatalf("ordinary resume after accepted edit: %v", err)
	}
}
