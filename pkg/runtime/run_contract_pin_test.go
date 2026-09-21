package runtime

import (
	"context"
	"encoding/json"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// pinWorkflow builds the same one-node workflow with or without a bound
// contract, so two passes of one run differ by the contract alone.
func pinWorkflow(name string, contract *ir.PublicContract) *ir.Workflow {
	wf := &ir.Workflow{
		Name:  name,
		Entry: "step",
		Nodes: map[string]ir.Node{
			"step": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "step", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Contract: contract,
	}
	if contract != nil {
		wf.Contracts = map[string]*ir.PublicContract{contract.Name: contract}
	}
	return wf
}

// TestRunResolveDocPinsTheExecutingProgramsContract: the run doc's pinned
// public contract mirrors the program the engine EXECUTES, pass after pass —
// stamped at launch, re-stamped when a pass carries a different contract,
// CLEARED when a pass's program dropped it. A parent re-attaching to a
// finished subbot child must project from the contract of the pass that ran,
// never from a stale one left by an earlier pass (#1280). Without the clear,
// a resume whose program dropped the contract leaves the old pin live and
// the re-attach projects a shape the current pass does not produce.
func TestRunResolveDocPinsTheExecutingProgramsContract(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	exec := newStubExecutor()

	pinned := func(name string) *ir.PublicContract {
		return &ir.PublicContract{Name: name, Outputs: []*ir.PublicPort{
			{Name: "url", Type: "string", FromNode: "step", FromField: "url"},
		}}
	}

	e1 := New(pinWorkflow("pin", pinned("kid")), s, exec, WithWorkflowHash("h1"), WithLogger(iterlog.Nop()))
	run, err := e1.runResolveDoc(ctx, "run-pin", nil)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(run.PublicContract) == 0 {
		t.Fatal("the contracted pass pinned no contract on the run doc")
	}
	var pc ir.PublicContract
	if err := json.Unmarshal(run.PublicContract, &pc); err != nil {
		t.Fatalf("decode the pinned contract: %v", err)
	}
	if pc.Name != "kid" {
		t.Fatalf("pinned contract = %q, want kid", pc.Name)
	}

	// A resume whose program DROPPED the contract clears the pin: the doc
	// must never advertise a contract the executing program does not keep.
	e2 := New(pinWorkflow("pin", nil), s, exec, WithWorkflowHash("h2"), WithLogger(iterlog.Nop()))
	run2, err := e2.runResolveDoc(ctx, "run-pin", nil)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(run2.PublicContract) != 0 {
		t.Fatalf("the contractless pass left a stale pin (%d bytes) — a re-attach would project a shape the pass does not produce", len(run2.PublicContract))
	}

	// And a pass that brings a DIFFERENT contract re-pins it.
	e3 := New(pinWorkflow("pin", pinned("kid2")), s, exec, WithWorkflowHash("h3"), WithLogger(iterlog.Nop()))
	run3, err := e3.runResolveDoc(ctx, "run-pin", nil)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	var pc3 ir.PublicContract
	if err := json.Unmarshal(run3.PublicContract, &pc3); err != nil {
		t.Fatalf("decode the re-pinned contract: %v", err)
	}
	if pc3.Name != "kid2" {
		t.Fatalf("re-pinned contract = %q, want kid2", pc3.Name)
	}
}
