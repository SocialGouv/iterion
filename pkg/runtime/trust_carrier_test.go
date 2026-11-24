package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The carrier bench (ADR-162 U1): the floors read ONE value per run, resolved
// by the engine and pushed to the executor beside the workspace
// (SetWorkspaceTrust), never re-derived. Each row drives a real Engine over a
// real store through one entry point a launching surface uses, and reads what
// reached the executor.

// trustProbeExecutor records every trust the engine pushes through the
// workspaceTrustSetter seam — the seam model.ClawExecutor receives it through.
// The engine pushes from its own goroutine; the mutex keeps the observer
// race-free all the same.
type trustProbeExecutor struct {
	*stubExecutor
	mu     sync.Mutex
	pushes []store.RunTrust
}

func (p *trustProbeExecutor) SetWorkspaceTrust(t store.RunTrust) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, t)
}

// received is every value pushed; the test fails when nothing was, because a
// row that never reached the seam proves nothing about the value.
func (p *trustProbeExecutor) received(t *testing.T) []store.RunTrust {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pushes) == 0 {
		t.Fatal("the engine pushed no workspace trust at all — the seam is not wired, so this row proves nothing")
	}
	return append([]store.RunTrust(nil), p.pushes...)
}

// carrierWorkflow is a tool node, a human gate and done, under a per-run
// sandbox: an untrusted run is admitted only inside one (#1773), and no LLM
// node means the credential admission has nothing to judge.
func carrierWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "trust_carrier",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"review": &ir.HumanNode{BaseNode: ir.BaseNode{ID: "review"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "review"}, {From: "review", To: "done"}},
		Schemas:  map[string]*ir.Schema{},
		Prompts:  map[string]*ir.Prompt{},
		Vars:     map[string]*ir.Var{},
		Loops:    map[string]*ir.Loop{},
		Worktree: "none",
		Sandbox:  &ir.SandboxSpec{Mode: "auto"},
	}
}

func carrierEngine(t *testing.T, s store.RunStore, probe *trustProbeExecutor, opts ...EngineOption) *Engine {
	t.Helper()
	return New(carrierWorkflow(), s, probe, append([]EngineOption{
		WithWorkDir(t.TempDir()),
		WithSandboxDrivers(workingHostRegistry(t)),
		WithLogger(iterlog.Nop()),
	}, opts...)...)
}

// seedRunDoc writes the document a pickup, a resume surface or a sub-bot's
// parent is built from: the publisher wrote the marker at admission.
func seedRunDoc(t *testing.T, s store.RunStore, runID string, trust store.RunTrust) {
	t.Helper()
	ctx := context.Background()
	r, err := s.CreateRun(ctx, runID, "trust_carrier", nil)
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	r.Trust = trust
	if !trust.Trusted() {
		r.RepoSHAExpected = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun(%s): %v", runID, err)
	}
}

// runToTheGate runs the engine until the human gate parks it.
func runToTheGate(t *testing.T, eng *Engine, runID string) {
	t.Helper()
	if err := eng.Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run(%s) = %v, want it parked at the human gate", runID, err)
	}
}

func assertEveryPush(t *testing.T, got []store.RunTrust, want store.RunTrust, why string) {
	t.Helper()
	for _, g := range got {
		if g != want {
			t.Fatalf("the executor received trust %q (pushes %q), want %q — %s", g, got, want, why)
		}
	}
}

func TestTrustCarrier_TheEngineHandsTheExecutorTheRunsTrust(t *testing.T) {
	t.Run("a launch that states the trust carries it", func(t *testing.T) {
		probe := &trustProbeExecutor{stubExecutor: newStubExecutor()}
		eng := carrierEngine(t, tmpStore(t), probe, WithTrust(store.RunTrustFork, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"))
		runToTheGate(t, eng, "run-trust-launch")
		assertEveryPush(t, probe.received(t), store.RunTrustFork, "the cloud runner's launch states it, and every task of the run would read as the tenant's own")
	})

	// The control: the same fixture and seam, the opposite verdict. Without
	// it, a probe that received a constant would pass every other row.
	t.Run("an operator's own run stays trusted", func(t *testing.T) {
		probe := &trustProbeExecutor{stubExecutor: newStubExecutor()}
		eng := carrierEngine(t, tmpStore(t), probe)
		runToTheGate(t, eng, "run-trust-operator")
		assertEveryPush(t, probe.received(t), store.RunTrustDefault, "an ordinary run must keep the trusted zero value")
	})

	// The pickup shape: `iterion run --run-id`, the dispatcher, the studio's
	// in-process path build their engine from the document and replay no
	// WithTrust. The document is the trust's durable truth.
	t.Run("an engine built from a fork document adopts its trust", func(t *testing.T) {
		s := tmpStore(t)
		seedRunDoc(t, s, "run-trust-pickup", store.RunTrustFork)
		probe := &trustProbeExecutor{stubExecutor: newStubExecutor()}
		runToTheGate(t, carrierEngine(t, s, probe), "run-trust-pickup")
		assertEveryPush(t, probe.received(t), store.RunTrustFork, "a picked-up fork run would execute with the trusted zero value")
	})

	// A sub-bot child: its engine names the parent and nothing else. The
	// store stamps the parent's provenance on the child's document at
	// creation (CreateChildRun), and the child engine adopts it.
	for _, row := range []struct {
		name   string
		parent store.RunTrust
	}{
		{"a child of a trusted parent stays trusted", store.RunTrustDefault},
		{"a child of a fork parent carries the fork's trust", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := tmpStore(t)
			seedRunDoc(t, s, "run-trust-parent", row.parent)
			probe := &trustProbeExecutor{stubExecutor: newStubExecutor()}
			runToTheGate(t, carrierEngine(t, s, probe, WithParentRunID("run-trust-parent")), "run-trust-child")
			assertEveryPush(t, probe.received(t), row.parent, "a sub-bot runs in its parent's workspace")
			child, err := s.LoadRun(context.Background(), "run-trust-child")
			if err != nil {
				t.Fatal(err)
			}
			if child.Trust != row.parent {
				t.Fatalf("child document Trust = %q, want the parent's %q", child.Trust, row.parent)
			}
		})
	}

	// The resume shape: `iterion resume` builds a NEW engine from the
	// document and replays no WithTrust; the resumed fork run is still a fork
	// run when its next node executes.
	for _, row := range []struct {
		name  string
		trust store.RunTrust
	}{
		{"a resumed trusted run stays trusted", store.RunTrustDefault},
		{"a resumed fork run is still a fork run", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := tmpStore(t)
			first := &trustProbeExecutor{stubExecutor: newStubExecutor()}
			runToTheGate(t, carrierEngine(t, s, first, WithTrust(row.trust, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")), "run-trust-resume")
			probe := &trustProbeExecutor{stubExecutor: newStubExecutor()}
			if err := carrierEngine(t, s, probe).Resume(context.Background(), "run-trust-resume", map[string]any{"approved": true}); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			assertEveryPush(t, probe.received(t), row.trust, "the resumed nodes must read the run's own trust")
		})
	}
}

// Adoption reads the document only downward: an engine TOLD its workspace is
// untrusted keeps that verdict whatever the document says — the document is
// data, the option the launching surface's statement.
func TestAdoptRunTrust_NeverLoosens(t *testing.T) {
	e := &Engine{trust: store.RunTrustFork}
	e.adoptRunTrust(&store.Run{Trust: store.RunTrustDefault})
	if e.trust != store.RunTrustFork {
		t.Fatalf("engine trust = %q after adopting a trusted document, want it unchanged at %q", e.trust, store.RunTrustFork)
	}
	e2 := &Engine{}
	e2.adoptRunTrust(nil)
	if !e2.trust.Trusted() {
		t.Fatalf("engine trust = %q after adopting nil, want the trusted zero value", e2.trust)
	}
}
