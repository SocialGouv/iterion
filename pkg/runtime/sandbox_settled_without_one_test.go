package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// sandboxSettleRecorder records every SetSandbox call, including the nil one:
// the question is not only WHICH handle the executor received but WHETHER it
// was told at all.
type sandboxSettleRecorder struct {
	*stubExecutor
	calls int
	last  sandbox.Run
}

func (s *sandboxSettleRecorder) SetSandbox(run sandbox.Run) { s.calls++; s.last = run }

// A run that settles WITHOUT a sandbox has to say so.
//
// The engine used to call SetSandbox only when a sandbox started, which left
// the executor unable to tell "this run has no sandbox" from "the sandbox has
// not started yet". A guard that must fail closed while the question is open —
// the MCP launcher-start policy — then stayed closed for the whole run, and
// an ordinary unsandboxed run would silently lose its project MCP servers.
func TestTheEngineSaysSoWhenARunSettlesWithoutASandbox(t *testing.T) {
	st := tmpStore(t)
	exec := &sandboxSettleRecorder{stubExecutor: newStubExecutor()}
	wf := &ir.Workflow{Name: "plain", Nodes: map[string]ir.Node{}}

	e := New(wf, st, exec, WithWorkDir(t.TempDir()))
	cleanup, err := e.startSandbox(context.Background(), "run-1", "", "", map[string]any{})
	if err != nil {
		t.Fatalf("startSandbox: %v", err)
	}
	t.Cleanup(cleanup)

	if exec.calls != 1 {
		t.Fatalf("the executor must be told the sandbox question is settled exactly once, got %d calls", exec.calls)
	}
	if exec.last != nil {
		t.Errorf("a run with no sandbox settles on nil, got %v", exec.last)
	}
}
