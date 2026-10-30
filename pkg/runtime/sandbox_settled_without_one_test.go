package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
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

// A DECLARED `sandbox: auto` the host cannot honour settles on the HOST path
// too, and both halves travel together: the executor is told nil — which is
// what puts every tool node on the host branch, with the host's drain bound,
// its report, its `_drain_cut` mark and ITERION_PROC_WAIT_DELAY as the knob
// — and the run's own stream carries `sandbox_skipped` saying why (#1425).
//
// It is the composition the tests around it leave open:
// TestResolveAndStartSandbox_AutoDegradesToHostWithVisibleEvent judges the
// resolver alone, and the sibling above a run that DECLARED nothing. This is
// the one a reader keys on, and sorting the host path from the driver path by
// the value written in the `.bot` gets it backwards: a run whose `.bot` says
// `sandbox: auto` reads exactly like `none` wherever no runtime answers.
//
// Mutations seen red: drop the SetSandbox(nil) call on the settle branch (the
// executor never hears the answer); hand the noop driver's activeSandbox back
// from the auto degrade instead of nil, nil (the executor gets a handle, so
// its nodes take the sandboxed branch); drop the emitEvent on the degrade
// branch (nothing on the stream says the run was not isolated).
func TestADeclaredAutoTheHostCannotHonourSettlesOnTheHostPath(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	st := tmpStore(t)
	exec := &sandboxSettleRecorder{stubExecutor: newStubExecutor()}
	wf := &ir.Workflow{
		Name:    "auto_declared",
		Nodes:   map[string]ir.Node{},
		Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
	}
	e := New(wf, st, exec,
		WithWorkDir(t.TempDir()),
		// With a default image, spec resolution gets past "nothing to run"
		// and reaches driver selection — where this host's answer is "no
		// container runtime", the production obstacle.
		WithSandboxDefaultImage("example.invalid/iterion-sandbox:test"),
		WithSandboxDrivers(driverlessHostRegistry()),
	)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-degraded", "auto_declared", nil); err != nil {
		t.Fatal(err)
	}
	// A repo root with no .devcontainer/devcontainer.json, so the degrade is
	// the driver's and not the resolver's.
	cleanup, err := e.startSandbox(ctx, "run-degraded", t.TempDir(), "", map[string]any{})
	if err != nil {
		t.Fatalf("a declared auto on a driverless host must DEGRADE, not refuse: %v", err)
	}
	t.Cleanup(cleanup)

	if exec.calls != 1 || exec.last != nil {
		t.Fatalf("the executor was told %d time(s), last handle %v — a degraded auto settles it on nil, which is the host branch for every tool node", exec.calls, exec.last)
	}
	evs, err := st.LoadEvents(ctx, "run-degraded")
	if err != nil {
		t.Fatal(err)
	}
	var skipped, started int
	for _, ev := range evs {
		switch ev.Type {
		case store.EventSandboxSkipped:
			skipped++
			if mode, _ := ev.Data["mode"].(string); mode != string(sandbox.ModeAuto) {
				t.Errorf("sandbox_skipped mode = %q, want %q", mode, sandbox.ModeAuto)
			}
		case store.EventSandboxStarted:
			started++
		}
	}
	if skipped != 1 || started != 0 {
		t.Fatalf("events: sandbox_skipped=%d sandbox_started=%d, want 1/0 — the event is the only honest key to which path ran", skipped, started)
	}
}
