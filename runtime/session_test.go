package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/ir"
)

// TestSessionInherit verifies that _session_id flows through the output/input
// pipeline and is available to downstream nodes via edge data mappings.
//
// Workflow:
//
//	producer (agent) -> consumer (agent, session: inherit) -> done
//
// producer returns _session_id in its output; the edge maps it to consumer
// via the with clause. consumer checks that _session_id is present in its input.
func TestSessionInherit(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "session_inherit_test",
		Entry: "producer",
		Nodes: map[string]ir.Node{
			"producer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "producer"}},
			"consumer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "consumer"}, Session: ir.SessionInherit},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{
				From: "producer",
				To:   "consumer",
				With: []*ir.DataMapping{
					{
						Key:  "_session_id",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"producer", "_session_id"}, Raw: "{{outputs.producer._session_id}}"}},
						Raw:  "{{outputs.producer._session_id}}",
					},
					{
						Key:  "data",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"producer", "result"}, Raw: "{{outputs.producer.result}}"}},
						Raw:  "{{outputs.producer.result}}",
					},
				},
			},
			{From: "consumer", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var capturedSessionID string

	exec := newStubExecutor()
	exec.on("producer", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Simulate a delegation backend returning a session ID.
		return map[string]interface{}{
			"result":      "planned",
			"_session_id": "sess-abc-123",
		}, nil
	})
	exec.on("consumer", func(input map[string]interface{}) (map[string]interface{}, error) {
		// Capture the session ID received via input.
		if sid, ok := input["_session_id"].(string); ok {
			capturedSessionID = sid
		}
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-session-001", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedSessionID != "sess-abc-123" {
		t.Errorf("expected _session_id to be %q, got %q", "sess-abc-123", capturedSessionID)
	}
}

// TestSessionInheritThroughBranches verifies that _session_id flows correctly
// through parallel branches (fan_out -> join) and is accessible to downstream
// nodes after the join.
//
// Workflow:
//
//	fanout (router) -> [branch_a, branch_b] -> joiner -> consumer -> done
//
// Each branch returns a distinct _session_id. After the join, consumer
// receives branch_a's session ID via the with clause.
func TestSessionInheritThroughBranches(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "session_branch_test",
		Entry: "fanout",
		Nodes: map[string]ir.Node{
			"fanout":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fanout"}, RouterMode: ir.RouterFanOutAll},
			"branch_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_a"}, LLMFields: ir.LLMFields{Readonly: true}},
			"branch_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_b"}, LLMFields: ir.LLMFields{Readonly: true}},
			"consumer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "consumer"}, Session: ir.SessionInherit, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "fanout", To: "branch_a"},
			{From: "fanout", To: "branch_b"},
			{
				From: "branch_a",
				To:   "consumer",
				With: []*ir.DataMapping{
					{
						Key:  "_session_id",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"branch_a", "_session_id"}, Raw: "{{outputs.branch_a._session_id}}"}},
						Raw:  "{{outputs.branch_a._session_id}}",
					},
					{
						Key:  "review",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"branch_a", "review"}, Raw: "{{outputs.branch_a.review}}"}},
						Raw:  "{{outputs.branch_a.review}}",
					},
				},
			},
			{From: "branch_b", To: "consumer"},
			{From: "consumer", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var capturedSessionID string

	exec := newStubExecutor()
	exec.on("branch_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"review":      "looks good",
			"_session_id": "sess-branch-a",
		}, nil
	})
	exec.on("branch_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"review":      "needs work",
			"_session_id": "sess-branch-b",
		}, nil
	})
	exec.on("consumer", func(input map[string]interface{}) (map[string]interface{}, error) {
		if sid, ok := input["_session_id"].(string); ok {
			capturedSessionID = sid
		}
		return map[string]interface{}{"fixed": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-session-002", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedSessionID != "sess-branch-a" {
		t.Errorf("expected _session_id from branch_a %q, got %q", "sess-branch-a", capturedSessionID)
	}
}

// TestSessionFork verifies that session: fork propagates _session_id the same
// way as inherit. Two fork nodes share the same parent session ID.
//
// Workflow:
//
//	producer (agent, fresh) -> fork_a (agent, session: fork) -> done
//	                        -> fork_b (agent, session: fork) -> done
func TestSessionFork(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "session_fork_test",
		Entry: "producer",
		Nodes: map[string]ir.Node{
			"producer": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "producer"}},
			"splitter": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "splitter"}, RouterMode: ir.RouterFanOutAll},
			"fork_a":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "fork_a"}, LLMFields: ir.LLMFields{Readonly: true}, Session: ir.SessionFork},
			"fork_b":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "fork_b"}, LLMFields: ir.LLMFields{Readonly: true}, Session: ir.SessionFork},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
		},
		Edges: []*ir.Edge{
			{From: "producer", To: "splitter"},
			{
				From: "splitter",
				To:   "fork_a",
				With: []*ir.DataMapping{
					{
						Key:  "_session_id",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"producer", "_session_id"}, Raw: "{{outputs.producer._session_id}}"}},
						Raw:  "{{outputs.producer._session_id}}",
					},
				},
			},
			{
				From: "splitter",
				To:   "fork_b",
				With: []*ir.DataMapping{
					{
						Key:  "_session_id",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"producer", "_session_id"}, Raw: "{{outputs.producer._session_id}}"}},
						Raw:  "{{outputs.producer._session_id}}",
					},
				},
			},
			{From: "fork_a", To: "done"},
			{From: "fork_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var capturedA, capturedB string

	exec := newStubExecutor()
	exec.on("producer", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"result":      "work done",
			"_session_id": "sess-parent-000",
		}, nil
	})
	exec.on("fork_a", func(input map[string]interface{}) (map[string]interface{}, error) {
		if sid, ok := input["_session_id"].(string); ok {
			capturedA = sid
		}
		return map[string]interface{}{"commit_name": "feat: add session fork"}, nil
	})
	exec.on("fork_b", func(input map[string]interface{}) (map[string]interface{}, error) {
		if sid, ok := input["_session_id"].(string); ok {
			capturedB = sid
		}
		return map[string]interface{}{"summary": "implemented fork feature"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-session-fork-001", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedA != "sess-parent-000" {
		t.Errorf("fork_a: expected _session_id %q, got %q", "sess-parent-000", capturedA)
	}
	if capturedB != "sess-parent-000" {
		t.Errorf("fork_b: expected _session_id %q, got %q", "sess-parent-000", capturedB)
	}
}
