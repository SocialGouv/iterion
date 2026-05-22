package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResumeFromFailed_ToolNodeInputMapping is a regression test for
// ticket e7e17a2f: a tool node downstream of an agent observes the
// agent's output via an edge `with {...}` mapping on first run; on
// resume from a failed_resumable, the same mapping must re-apply so
// the tool node sees the resolved input map instead of literal
// `{{input.X}}` templates reaching its command string.
// TestResumeFromFailed_ToolNodeVarMapping mirrors feature_dev's
// commit_changes scenario: the tool's input is wired from a workflow
// `vars` value via an edge `with` mapping. On resume, vars are
// re-resolved (cp.Vars is intentionally discarded — see comment in
// resumeFromFailure) so the var must still feed the edge mapping.
func TestResumeFromFailed_ToolNodeVarMapping(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "resume_var_mapping",
		Entry: "upstream",
		Nodes: map[string]ir.Node{
			"upstream": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "upstream"}},
			"downstream": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "downstream"},
				Command:  "echo {{input.workspace_dir}}",
				CommandRefs: []*ir.Ref{
					{Raw: "{{input.workspace_dir}}", Kind: ir.RefInput, Path: []string{"workspace_dir"}},
				},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{
				From: "upstream",
				To:   "downstream",
				With: []*ir.DataMapping{
					{
						Key: "workspace_dir",
						Raw: "{{vars.workspace_dir}}",
						Refs: []*ir.Ref{
							{Raw: "{{vars.workspace_dir}}", Kind: ir.RefVars, Path: []string{"workspace_dir"}},
						},
					},
				},
			},
			{From: "downstream", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"workspace_dir": {Name: "workspace_dir", Type: ir.VarString, HasDefault: true, Default: "/var/data/wf"},
		},
		Loops: map[string]*ir.Loop{},
	}

	failOnce := true
	observedInputs := []map[string]interface{}{}
	exec := newStubExecutor()
	exec.on("upstream", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("downstream", func(input map[string]interface{}) (map[string]interface{}, error) {
		observedInputs = append(observedInputs, mapCopy(input))
		if failOnce {
			failOnce = false
			return nil, fmt.Errorf("simulated tool failure")
		}
		return map[string]interface{}{"ok": true}, nil
	})

	st := tmpStore(t)
	eng := New(wf, st, exec)

	runID := "run-resume-var-mapping"
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("expected first run to fail")
	}
	r, _ := st.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status=%q, want failed_resumable", r.Status)
	}

	if err := eng.Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(observedInputs) != 2 {
		t.Fatalf("downstream executed %d times, want 2", len(observedInputs))
	}
	for i, in := range observedInputs {
		got, ok := in["workspace_dir"]
		if !ok {
			t.Errorf("attempt %d: input missing 'workspace_dir' key; full input=%v", i+1, in)
			continue
		}
		if got != "/var/data/wf" {
			t.Errorf("attempt %d: input[workspace_dir]=%v, want /var/data/wf", i+1, got)
		}
	}
}

func mapCopy(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestResumeFromFailed_ToolNodeInputMapping(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "resume_input_mapping",
		Entry: "upstream",
		Nodes: map[string]ir.Node{
			"upstream": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "upstream"}},
			"downstream": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "downstream"},
				Command:  "echo {{input.workspace}}",
				CommandRefs: []*ir.Ref{
					{Raw: "{{input.workspace}}", Kind: ir.RefInput, Path: []string{"workspace"}},
				},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{
				From: "upstream",
				To:   "downstream",
				With: []*ir.DataMapping{
					{
						Key: "workspace",
						Raw: "{{outputs.upstream.workspace_path}}",
						Refs: []*ir.Ref{
							{Raw: "{{outputs.upstream.workspace_path}}", Kind: ir.RefOutputs, Path: []string{"upstream", "workspace_path"}},
						},
					},
				},
			},
			{From: "downstream", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	failOnce := true
	observedInputs := []map[string]interface{}{}
	exec := newStubExecutor()
	exec.on("upstream", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"workspace_path": "/tmp/wf-test"}, nil
	})
	exec.on("downstream", func(input map[string]interface{}) (map[string]interface{}, error) {
		observedInputs = append(observedInputs, copyMap(input))
		if failOnce {
			failOnce = false
			return nil, fmt.Errorf("simulated tool failure")
		}
		return map[string]interface{}{"ok": true}, nil
	})

	st := tmpStore(t)
	eng := New(wf, st, exec)

	runID := "run-resume-input-mapping"
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("expected first run to fail")
	}
	r, _ := st.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status=%q, want failed_resumable", r.Status)
	}

	if err := eng.Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(observedInputs) != 2 {
		t.Fatalf("downstream executed %d times, want 2", len(observedInputs))
	}
	for i, in := range observedInputs {
		got, ok := in["workspace"]
		if !ok {
			t.Errorf("attempt %d: input missing 'workspace' key; full input=%v", i+1, in)
			continue
		}
		if got != "/tmp/wf-test" {
			t.Errorf("attempt %d: input[workspace]=%v, want /tmp/wf-test", i+1, got)
		}
	}
}

