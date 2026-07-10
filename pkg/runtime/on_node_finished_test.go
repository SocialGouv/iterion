package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestOnNodeFinished_ReceivesRunIDAndRawOutput is the contract MVP3b's
// watch auto-stamp relies on: the hook fires with the run ID and the
// node's raw (unsanitized) output, so the wiring layer can read a
// dispatch node's dispatched_ids and subscribe the run.
func TestOnNodeFinished_ReceivesRunIDAndRawOutput(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "on_finished_test",
		Entry: "dispatch",
		Nodes: map[string]ir.Node{
			"dispatch": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dispatch"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "dispatch", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("dispatch", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"dispatched_ids": []any{"native:abc", "native:def"},
		}, nil
	})

	type capture struct {
		runID  string
		nodeID string
		output map[string]any
	}
	var (
		mu  sync.Mutex
		got []capture
	)

	s := tmpStore(t)
	eng := New(wf, s, exec, WithOnNodeFinished(func(runID, nodeID string, output map[string]any) {
		mu.Lock()
		got = append(got, capture{runID: runID, nodeID: nodeID, output: output})
		mu.Unlock()
	}))

	if err := eng.Run(context.Background(), "run-onfin", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var dispatch *capture
	for i := range got {
		if got[i].runID != "run-onfin" {
			t.Errorf("callback runID = %q, want run-onfin (node %s)", got[i].runID, got[i].nodeID)
		}
		if got[i].nodeID == "dispatch" {
			dispatch = &got[i]
		}
	}
	if dispatch == nil {
		t.Fatalf("onNodeFinished never fired for the dispatch node")
	}
	raw, ok := dispatch.output["dispatched_ids"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("raw dispatched_ids not delivered to hook: %#v", dispatch.output)
	}
}
