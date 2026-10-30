package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The observability.effective-model and observability.facade-routing rows of
// docs/e2e-coverage-matrix.md were filed `unit-only` on the reading that the
// durable record (delegate_started/delegate_finished, run.json nodes_served,
// model_drift / model_served_via_facade) was only ever asserted at the
// recorder — NewStoreEventHooks fed a hand-built DelegateInfo
// (pkg/backend/model/delegate_observability_test.go). But the hooks ride the
// executor's dispatchWithObservability on every agent dispatch, and the
// delegate backend is a registry seam: a fake backend stamps whatever
// EffectiveModel / SessionFingerprint a real CLI would parse off its stream,
// with no credential anywhere. These tests drive that path — real engine,
// real ClawExecutor, real store, scripted backend — so a regression at the
// recording site (the hooks wiring, the delegateInfoFromResult copy, the
// NodesServed persistence) fails HERE, through the engine, not only at the
// store helper.

// recordBackend is a delegate.Backend that answers each node's task with the
// result a facade-served (or drifted) CLI session would report.
type recordBackend struct {
	calls       int
	effective   map[string]string // nodeID -> EffectiveModel; default: the declared model
	fingerprint map[string]string // nodeID -> SessionFingerprint
}

func (b *recordBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls++
	effective := task.Model
	if m, ok := b.effective[task.NodeID]; ok {
		effective = m
	}
	return delegate.Result{
		Output:             map[string]any{"answer": "ok"},
		BackendName:        "record_fake",
		EffectiveModel:     effective,
		SessionFingerprint: b.fingerprint[task.NodeID],
		Tokens:             10,
	}, nil
}

const recordWorkflowSource = `schema out:
  answer: string
prompt p:
  Say ok.
agent triage:
  backend: record_fake
  model: "anthropic/claude-opus-5"
  system: p
  output: out
agent report:
  backend: record_fake
  model: "claude-opus-5"
  system: p
  output: out
workflow record:
  entry: triage
  worktree: none
  triage -> report
  report -> done
`

// runRecordWorkflow compiles the fixture, runs it through the real engine
// with the real ClawExecutor wired to the store hooks exactly as
// pkg/runview's executor builder wires them, and returns the finished run
// and its events.
func runRecordWorkflow(t *testing.T, runID string, backend *recordBackend) (*store.Run, []*store.Event) {
	t.Helper()
	pr := parsertest.Parse("record.bot", recordWorkflowSource)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hooks := model.NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil, nil)
	br := delegate.NewRegistry()
	br.Register("record_fake", backend)
	ex := model.NewClawExecutor(model.NewRegistry(), cr.Workflow,
		model.WithBackendRegistry(br),
		model.WithEventHooks(hooks),
		model.WithLogger(iterlog.Nop()),
		model.WithWorkDir(t.TempDir()))
	eng := New(cr.Workflow, st, ex, WithLogger(iterlog.Nop()))
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", run.Status)
	}
	events, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	return run, events
}

func recordEventsOfType(events []*store.Event, typ store.EventType) []*store.Event {
	var out []*store.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// The model that actually ran is recorded on delegate_started /
// delegate_finished, on run.json nodes_served, and as one model_drift event
// when it differs from the declared model: — produced here by the engine
// path, the delegate stream half scripted away.
func TestEffectiveModelRecordThroughEngine(t *testing.T) {
	backend := &recordBackend{
		effective:   map[string]string{"triage": "glm-4.6"},
		fingerprint: map[string]string{},
	}
	run, events := runRecordWorkflow(t, "run-record-model", backend)
	if backend.calls != 2 {
		t.Fatalf("backend calls = %d, want 2", backend.calls)
	}

	started := recordEventsOfType(events, store.EventDelegateStarted)
	if len(started) != 2 {
		t.Fatalf("delegate_started events = %d, want 2 (one per node)", len(started))
	}
	if started[0].Data["declared_model"] != "anthropic/claude-opus-5" {
		t.Errorf("delegate_started declared_model = %v", started[0].Data["declared_model"])
	}
	if _, ok := started[0].Data["effective_model"]; ok {
		t.Error("delegate_started must not carry effective_model — the provider has not spoken yet")
	}

	finished := recordEventsOfType(events, store.EventDelegateFinished)
	if len(finished) != 2 {
		t.Fatalf("delegate_finished events = %d, want 2", len(finished))
	}
	if finished[0].Data["effective_model"] != "glm-4.6" {
		t.Errorf("delegate_finished effective_model = %v, want glm-4.6", finished[0].Data["effective_model"])
	}

	drifts := recordEventsOfType(events, store.EventModelDrift)
	if len(drifts) != 1 {
		t.Fatalf("model_drift events = %d, want exactly 1 (triage rewrote its model): %v", len(drifts), drifts)
	}
	if drifts[0].NodeID != "triage" || drifts[0].Data["effective_model"] != "glm-4.6" || drifts[0].Data["declared_model"] != "anthropic/claude-opus-5" {
		t.Errorf("model_drift = node %q data %v", drifts[0].NodeID, drifts[0].Data)
	}

	served := run.NodesServed["triage"]
	if served.Backend != "record_fake" || served.Model != "glm-4.6" || served.DeclaredModel != "anthropic/claude-opus-5" {
		t.Errorf("NodesServed[triage] = %+v — the effective model was captured then dropped", served)
	}
	if got := run.NodesServed["report"].Model; got != "claude-opus-5" {
		t.Errorf("NodesServed[report].Model = %q, want the declared id served as-is", got)
	}
}

// A facade answers the requested id with the model it aliases it to, so
// declared and effective AGREE and model_drift stays silent: the session
// fingerprint is the only evidence. One model_served_via_facade event per
// node and facade, and the fingerprint on run.json — again through the
// engine, the facade itself scripted down to the stamp its delegate leaves.
func TestFacadeRoutingRecordThroughEngine(t *testing.T) {
	backend := &recordBackend{
		effective: map[string]string{},
		fingerprint: map[string]string{
			"triage": "facade:https://facade.invalid/anthropic",
			"report": "anthropic-oauth",
		},
	}
	run, events := runRecordWorkflow(t, "run-record-facade", backend)
	if backend.calls != 2 {
		t.Fatalf("backend calls = %d, want 2", backend.calls)
	}

	if drifts := recordEventsOfType(events, store.EventModelDrift); len(drifts) != 0 {
		t.Errorf("model_drift must stay silent when the ids agree: %v", drifts)
	}
	facade := recordEventsOfType(events, store.EventModelServedViaFacade)
	if len(facade) != 1 {
		t.Fatalf("model_served_via_facade events = %d, want exactly 1 (the facade node only)", len(facade))
	}
	if facade[0].NodeID != "triage" || facade[0].Data["fingerprint"] != "facade:https://facade.invalid/anthropic" {
		t.Errorf("model_served_via_facade = node %q data %v", facade[0].NodeID, facade[0].Data)
	}

	if got := run.NodesServed["triage"].Fingerprint; got != "facade:https://facade.invalid/anthropic" {
		t.Errorf("NodesServed[triage].Fingerprint = %q — the facade route was captured then dropped", got)
	}
	if got := run.NodesServed["report"].Fingerprint; got != "anthropic-oauth" {
		t.Errorf("NodesServed[report].Fingerprint = %q", got)
	}
}

// compile-time guard: recordBackend must stay a delegate.Backend.
var _ delegate.Backend = (*recordBackend)(nil)
