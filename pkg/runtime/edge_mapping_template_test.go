package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestEdgeWithMappingInterpolatesContextRefsIntoToolInput mirrors the
// generation_request_id mappings used by Town's humanoid image tool.  A
// reference embedded in a larger string must be interpolated (not treated as
// a typed passthrough), and a template containing multiple context refs must
// resolve every ref on each loop visit.
func TestEdgeWithMappingInterpolatesContextRefsIntoToolInput(t *testing.T) {
	mapping := func(key, raw string) *ir.DataMapping {
		t.Helper()
		refs, err := ir.ParseRefs(raw)
		if err != nil {
			t.Fatalf("parse mapping %q: %v", raw, err)
		}
		return &ir.DataMapping{Key: key, Raw: raw, Refs: refs}
	}

	wf := &ir.Workflow{
		Name:  "edge_mapping_context_refs",
		Entry: "seed",
		Nodes: map[string]ir.Node{
			"seed": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "seed"}},
			"generate_family_images": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "generate_family_images"},
			},
			"review": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "review"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{
				From: "seed",
				To:   "generate_family_images",
				With: []*ir.DataMapping{
					mapping("generation_request_id", "{{run.id}}:initial"),
				},
			},
			{From: "generate_family_images", To: "review"},
			{From: "review", To: "done", Condition: "approved"},
			{
				From:      "review",
				To:        "generate_family_images",
				Condition: "approved",
				Negated:   true,
				LoopName:  "human_image_fix",
				With: []*ir.DataMapping{
					mapping("generation_request_id", "{{run.id}}:human:{{loop.human_image_fix.iteration}}"),
				},
			},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"human_image_fix": {Name: "human_image_fix", MaxIterations: 2},
		},
	}

	var requestIDs []string
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("generate_family_images", func(input map[string]any) (map[string]any, error) {
		requestID, _ := input["generation_request_id"].(string)
		requestIDs = append(requestIDs, requestID)
		return map[string]any{}, nil
	})
	exec.on("review", func(map[string]any) (map[string]any, error) {
		return map[string]any{"approved": len(requestIDs) >= 2}, nil
	})

	const runID = "run-town-human-images"
	eng := New(wf, tmpStore(t), exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{
		runID + ":initial",
		runID + ":human:1",
	}
	if len(requestIDs) != len(want) {
		t.Fatalf("tool calls=%d (%v), want %d (%v)", len(requestIDs), requestIDs, len(want), want)
	}
	for i := range want {
		if requestIDs[i] != want[i] {
			t.Errorf("tool call %d generation_request_id=%q, want %q", i+1, requestIDs[i], want[i])
		}
	}
}
