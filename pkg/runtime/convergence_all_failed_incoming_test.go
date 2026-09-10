package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// #559 — when EVERY branch of a best_effort fan-out fails, the convergence
// node still executes (that is what best_effort promises) but arrives with
// none of its incoming `with` mappings.
//
// The mappings that matter here do not depend on the failed branches at all:
// they read a durable parent output or a var, which are still on the run
// state. A required join input such as `expected_count` therefore goes
// missing, and shell rendering — which deliberately keeps an unresolved
// reference as a literal — hands `{{input.expected_count}}` to the command.
func TestConvergence_allFailedBestEffortKeepsParentSourcedMappings(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitBestEffort)
	// Both branch→join edges carry a mapping sourced from the PARENT's
	// output, beside the one sourced from the branch itself.
	for _, e := range wf.Edges {
		if e.To != "finalize" {
			continue
		}
		e.With = append(e.With, &ir.DataMapping{
			Key:  "expected_count",
			Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "count"}}},
			Raw:  "{{outputs.entry.count}}",
		})
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "go", "count": 2}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		return nil, errors.New("fail A")
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		return nil, errors.New("fail B")
	})
	var finalizeInput map[string]any
	exec.on("finalize", func(input map[string]any) (map[string]any, error) {
		finalizeInput = input
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "run-559", nil); err != nil {
		t.Fatalf("run: %v (best_effort tolerates all failures)", err)
	}
	if finalizeInput == nil {
		t.Fatal("the convergence node never ran")
	}
	got, ok := finalizeInput["expected_count"]
	if !ok {
		t.Fatalf("input = %v — expected_count is missing, so a shell node would be handed the literal {{input.expected_count}}. Its mapping reads the PARENT's output, which every failed branch left untouched.", finalizeInput)
	}
	if got != 2 {
		t.Fatalf("expected_count = %v, want 2 (the parent's own output)", got)
	}
}

// The SAME symptom on a sibling site (grep-la-classe): `fan_out_each` whose
// `over` resolves to an EMPTY array also skips to the convergence after
// deleting the tracking entry (fan_out_each.go:147). No branch ran, so no
// source has output, so the same guard drops every incoming mapping —
// including the ones reading a durable parent output.
//
// Probe first, fix second: if this is red too, the fix has to cover both
// sites or it leaves the class alive.
func TestConvergence_emptyFanOutEachKeepsParentSourcedMappings(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)
	for _, e := range wf.Edges {
		if e.From == "handle" && e.To == "collect" {
			e.With = []*ir.DataMapping{{
				Key:  "expected_count",
				Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "count"}}},
				Raw:  "{{outputs.entry.count}}",
			}}
		}
	}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{}, "count": 0}, nil
	})
	var collectInput map[string]any
	exec.on("collect", func(input map[string]any) (map[string]any, error) {
		collectInput = input
		return map[string]any{}, nil
	})
	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "run-559-empty", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if collectInput == nil {
		t.Fatal("the convergence node never ran")
	}
	if _, ok := collectInput["expected_count"]; !ok {
		t.Fatalf("input = %v — same class as the all-failed case: an empty fan_out_each leaves the convergence without its parent-sourced mappings", collectInput)
	}
}
