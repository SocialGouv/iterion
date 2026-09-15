package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestConvergenceReinvocationUsesCurrentOutputs(t *testing.T) {
	for _, shape := range []string{"all-failed", "all-partial", "all-tail", "each-failed", "each-partial", "each-empty"} {
		t.Run(shape, func(t *testing.T) {
			each := shape == "each-failed" || shape == "each-partial" || shape == "each-empty"
			wf := fanOutWorkflow(ir.AwaitBestEffort)
			collector, producer := "finalize", "agent_a"
			if each {
				wf = fanOutEachWorkflow(false, ir.AwaitBestEffort, 0)
				collector, producer = "collect", "handle"
			}
			if shape == "all-tail" {
				producer = "tail"
				wf.Nodes[producer] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: producer}}
				for _, edge := range wf.Edges {
					if edge.From == "agent_a" && edge.To == collector {
						edge.To = producer
					}
				}
				wf.Edges = append(wf.Edges, &ir.Edge{From: producer, To: collector})
			}
			for _, edge := range wf.Edges {
				if edge.To == collector {
					edge.With = []*ir.DataMapping{settledRef("review", producer, "review"), settledRef("parent", "entry", "summary")}
				}
				if edge.From == collector {
					edge.To = "gate"
					edge.With = []*ir.DataMapping{settledRef("direct_review", producer, "review")}
				}
			}
			wf.Nodes["gate"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}}
			wf.Edges = append(wf.Edges, &ir.Edge{From: "gate", To: "entry", LoopName: "retry", Condition: "again"}, &ir.Edge{From: "gate", To: "pause", IsElse: true})
			body := map[string]bool{}
			for id := range wf.Nodes {
				body[id] = true
			}
			wf.Loops["retry"] = &ir.Loop{Name: "retry", MaxIterations: 3, Entries: map[string]bool{"entry": true}, Body: body}
			wf.Nodes["pause"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "pause"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
			wf.Nodes["after_resume"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "after_resume"}}
			wf.Edges = append(wf.Edges, &ir.Edge{From: "pause", To: "after_resume", With: []*ir.DataMapping{settledRef("review", producer, "review")}}, &ir.Edge{From: "after_resume", To: "done"})
			wf.Nodes[producer].(*ir.AgentNode).Publish = "archive"
			wf.Edges[len(wf.Edges)-2].With = append(wf.Edges[len(wf.Edges)-2].With, &ir.DataMapping{
				Key: "archive", Raw: "{{artifacts.archive}}", Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"archive"}}},
			})
			exec := newStubExecutor()
			pass := 0
			exec.on("entry", func(map[string]any) (map[string]any, error) {
				items := []any{item("A"), item("B")}
				if shape == "each-empty" && pass == 1 {
					items = nil
				}
				return map[string]any{"summary": "parent stays visible", "items": items}, nil
			})
			exec.on(producer, func(input map[string]any) (map[string]any, error) {
				if pass == 0 {
					return map[string]any{"review": "first pass"}, nil
				}
				if shape == "each-partial" && input["id"] == "B" {
					return map[string]any{"review": "second pass"}, nil
				}
				return nil, errors.New("current attempt failed")
			})
			if !each {
				exec.on("agent_b", func(map[string]any) (map[string]any, error) {
					if shape == "all-partial" {
						return map[string]any{"ok": true}, nil
					}
					return nil, errors.New("B failed")
				})
			}
			if shape == "all-tail" {
				exec.on("agent_a", func(map[string]any) (map[string]any, error) {
					if pass == 0 {
						return map[string]any{"ok": true}, nil
					}
					return nil, errors.New("head fails before reaching tail")
				})
			}
			var visits, downstream []map[string]any
			exec.on(collector, func(input map[string]any) (map[string]any, error) {
				visits = append(visits, deepCopyAnyMap(input))
				return map[string]any{}, nil
			})
			exec.on("gate", func(input map[string]any) (map[string]any, error) {
				downstream = append(downstream, deepCopyAnyMap(input))
				pass++
				return map[string]any{"again": pass < 2}, nil
			})
			s := tmpStore(t)
			ctx := context.Background()
			id := "fresh-" + shape
			if err := New(wf, s, exec).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("run = %v, want pause", err)
			}
			if len(visits) != 2 || len(downstream) != 2 {
				t.Fatalf("visits=%v downstream=%v", visits, downstream)
			}
			if visits[0]["review"] != "first pass" {
				t.Fatalf("first visit=%v", visits[0])
			}
			var want any
			if shape == "each-partial" {
				want = "second pass"
			}
			if got, exists := visits[1]["review"]; !exists || got != want {
				t.Errorf("second collector review=%v (present=%v), want %v", got, exists, want)
			}
			if got := downstream[1]["direct_review"]; got != want {
				t.Errorf("downstream direct reference=%v, want %v", got, want)
			}
			if got := visits[1]["parent"]; got != "parent stays visible" {
				t.Errorf("parent output lost: %v", got)
			}
			var resumed map[string]any
			exec.on("after_resume", func(input map[string]any) (map[string]any, error) {
				resumed = input
				return map[string]any{}, nil
			})
			if err := New(wf, s, exec).Resume(ctx, id, map[string]any{"approved": true}); err != nil {
				t.Fatal(err)
			}
			if got, exists := resumed["review"]; !exists || got != want {
				t.Errorf("after fresh-engine resume review=%v (present=%v), want %v", got, exists, want)
			}
			wantArchive := "first pass"
			if shape == "each-partial" {
				wantArchive = "second pass"
			}
			archive, _ := resumed["archive"].(map[string]any)
			if archive["review"] != wantArchive {
				t.Errorf("last published artifact after resume=%v, want %s", resumed["archive"], wantArchive)
			}
		})
	}
}

// A launched region owns its untaken routes too, but cannot invalidate an
// unselected LLM-multi target, its collector, the trunk after it, or a node
// reached only over a bounded back-edge. A branch-local collector applies
// the same rule to its private cursor, never to its parent's output map.
func TestConvergenceFreshnessRespectsInvocationScope(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "trunk"
		if nested {
			name = "branch"
		}
		t.Run(name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort}}, Edges: []*ir.Edge{
				{From: "router", To: "a"}, {From: "router", To: "b"}, {From: "router", To: "foreign"},
				{From: "a", To: "tail", Condition: "ok"}, {From: "a", To: "untaken", IsElse: true},
				{From: "tail", To: "join"}, {From: "untaken", To: "join"}, {From: "b", To: "join"}, {From: "foreign", To: "join"},
				{From: "tail", To: "before", LoopName: "local"}, {From: "join", To: "after"},
			}}
			ctx := context.Background()
			s := tmpStore(t)
			if _, err := s.CreateRun(ctx, "scope-"+name, "wf", nil); err != nil {
				t.Fatal(err)
			}
			eng := New(wf, s, newStubExecutor())
			parent := eng.newRunState("scope-"+name, nil)
			parent.ctx = ctx
			for _, id := range []string{"router", "a", "tail", "untaken", "b", "foreign", "join", "before", "after"} {
				parent.outputs[id] = map[string]any{"old": id}
			}
			parent.artifacts["archive"] = map[string]any{"review": "last published"}
			rs := parent
			var outer *branchResult
			if nested {
				outer = initBranchResult(parent, "outer", nil)
				outer.outputs = copyOutputs(parent.outputs)
				rs = newBranchRunState(parent, nil, outer)
			}
			results := []*branchResult{
				{branchID: "a", outputs: map[string]map[string]any{"a": {"partial": "discard failed branch"}}, err: errors.New("tail failed")},
				{branchID: "b", outputs: map[string]map[string]any{"b": {"fresh": true}}, selectedIncoming: map[string][]store.IncomingEdge{"join": {{From: "b", To: "join"}}}},
			}
			// No settled floor: this exercises the output view an invocation
			// REPLACES, which is independent of the evidence it leaves behind.
			if _, err := eng.processConvergence(rs, "join", results, []string{"a", "b"}, nil); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"a", "tail", "untaken"} {
				if value, exists := rs.outputs[id]; exists {
					t.Errorf("stale %s survived: %v", id, value)
				}
			}
			for _, id := range []string{"router", "foreign", "join", "before", "after"} {
				if rs.outputs[id]["old"] != id {
					t.Errorf("out-of-region output %s changed: %v", id, rs.outputs[id])
				}
			}
			if rs.outputs["b"]["fresh"] != true {
				t.Error("fresh successful branch output lost")
			}
			if rs.artifacts["archive"]["review"] != "last published" {
				t.Error("published artifact history lost")
			}
			if nested {
				if parent.outputs["a"]["old"] != "a" || parent.outputs["b"]["old"] != "b" {
					t.Error("nested convergence mutated its parent")
				}
				cp := branchCheckpointFromState(rs, outer, "join", false)
				restored := initBranchResult(parent, "outer", cp)
				if _, exists := restored.outputs["tail"]; exists {
					t.Error("stale nested output survived its branch cursor")
				}
				if restored.outputs["b"]["fresh"] != true {
					t.Error("fresh nested output lost from branch cursor")
				}
			}
		})
	}
}
