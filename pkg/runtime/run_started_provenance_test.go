package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A run whose IR was compiled by one build and executed by another had
// nothing anywhere saying so. Measured 2026-09-05: server pods recreated
// on `:edge` at v3.111.0 while the runners stayed at v3.106.1; five
// launches died in under 75 s each and the operator had to compare the
// healthz of two deployments to work out why.
//
// run_started is where the executing side introduces itself: which build
// is running the workflow, which revision of the source it is, and — only
// when they differ — which build compiled it.
func TestRunStartedPayload_CarriesTheExecutionProvenance(t *testing.T) {
	wf := &ir.Workflow{
		Loops: map[string]*ir.Loop{"review_loop": {MaxIterations: 50}},
	}
	run := &store.Run{
		WorkflowHash:   "e7e2f6e6",
		IterionVersion: "v3.111.0+2ecc75b1aaaa",
	}

	data := runStartedPayload(wf, run)
	if got, _ := data["engine_version"].(string); got != appinfo.Version {
		t.Errorf("engine_version = %q, want %q", got, appinfo.Version)
	}
	if got, _ := data["workflow_hash"].(string); got != "e7e2f6e6" {
		t.Errorf("workflow_hash = %q, want the launch revision", got)
	}
	if got, _ := data["launched_by_version"].(string); got != "v3.111.0+2ecc75b1aaaa" {
		t.Errorf("launched_by_version = %q, want the compiling build", got)
	}
	// The loop bounds the studio's run-level indicator reads must survive
	// the payload gaining fields.
	loops, ok := data["loops"].(map[string]any)
	if !ok || loops["review_loop"] != 50 {
		t.Errorf("loops = %v, want review_loop: 50 — the run-level loop indicator reads this", data["loops"])
	}

	// Same build on both sides (every local run): the field is ABSENT
	// rather than present-and-equal, so a reader who sees it knows it
	// means something.
	same := runStartedPayload(wf, &store.Run{IterionVersion: appinfo.FullVersion()})
	if _, present := same["launched_by_version"]; present {
		t.Error("launched_by_version is present when the launcher and the engine are the same build")
	}
	// A run doc that carries neither still yields a usable payload.
	if bare := runStartedPayload(nil, nil); bare["engine_version"] == nil {
		t.Error("a payload with no workflow and no run doc still has to name the engine")
	}
}

// The end-to-end half: the payload reaches the persisted event, so the
// provenance is readable from the run and not only from a unit test.
func TestRunStarted_StampsProvenanceOnTheTimeline(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "provenance",
		Entry: "done",
		Nodes: map[string]ir.Node{
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	s := tmpStore(t)
	ctx := context.Background()
	if err := New(wf, s, newStubExecutor()).Run(ctx, "run-provenance", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, err := s.LoadEvents(ctx, "run-provenance")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, e := range events {
		if e.Type != store.EventRunStarted {
			continue
		}
		if got, _ := e.Data["engine_version"].(string); got != appinfo.Version {
			t.Fatalf("run_started.engine_version = %q, want %q", got, appinfo.Version)
		}
		return
	}
	t.Fatal("no run_started event")
}
