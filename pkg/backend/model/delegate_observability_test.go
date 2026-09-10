package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestDelegateModelReachesStore is the #474 regression: EffectiveModel is
// captured on delegate.Result then used for a log-only drift warning, and
// until this test existed it never reached events.jsonl or run.json. A
// CLI-backend run has no llm_request.model escape hatch, so these two
// surfaces ARE the record of what actually ran.
func TestDelegateModelReachesStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-model"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var logBuf bytes.Buffer
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelInfo, &logBuf), nil)

	info := DelegateInfo{
		BackendName:     "claude_code",
		DeclaredModel:   "anthropic/claude-opus-5",
		EffectiveModel:  "glm-4.6",
		ContextWindow:   200_000,
		MaxOutputTokens: 8192,
		PeakInputTokens: 120_000,
		Duration:        1500 * time.Millisecond,
		Tokens:          42,
		CostUSD:         0.12,
	}
	hooks.OnDelegateStarted("campaign", DelegateInfo{
		BackendName:   info.BackendName,
		DeclaredModel: info.DeclaredModel,
	})
	hooks.OnDelegateFinished("campaign", info)

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	started := findEvent(t, evts, store.EventDelegateStarted)
	if started.Data["backend"] != "claude_code" {
		t.Errorf("delegate_started backend = %v", started.Data["backend"])
	}
	if started.Data["declared_model"] != "anthropic/claude-opus-5" {
		t.Errorf("delegate_started declared_model = %v", started.Data["declared_model"])
	}
	if _, ok := started.Data["effective_model"]; ok {
		t.Error("delegate_started must not carry effective_model — the provider has not spoken yet")
	}

	finished := findEvent(t, evts, store.EventDelegateFinished)
	for key, want := range map[string]any{
		"backend":           "claude_code",
		"declared_model":    "anthropic/claude-opus-5",
		"effective_model":   "glm-4.6",
		"context_window":    200_000,
		"max_output_tokens": 8192,
		"context_used":      120_000,
		"cost_usd":          0.12,
	} {
		if got := finished.Data[key]; !eventValueEqual(got, want) {
			t.Errorf("delegate_finished[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
		}
	}

	drift := findEvent(t, evts, store.EventModelDrift)
	if drift.NodeID != "campaign" {
		t.Errorf("model_drift node = %q", drift.NodeID)
	}
	if drift.Data["declared_model"] != "anthropic/claude-opus-5" || drift.Data["effective_model"] != "glm-4.6" {
		t.Errorf("model_drift data = %v", drift.Data)
	}

	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	served, ok := run.NodesServed["campaign"]
	if !ok {
		t.Fatal("run.json NodesServed missing campaign — the model was captured then dropped")
	}
	if served.Backend != "claude_code" || served.Model != "glm-4.6" || served.DeclaredModel != "anthropic/claude-opus-5" {
		t.Errorf("NodesServed[campaign] = %+v", served)
	}
	if served.ContextWindow != 200_000 || served.MaxOutputTokens != 8192 {
		t.Errorf("window/tokens dropped: %+v", served)
	}
}

// TestDelegateErrorDoesNotBlankRecordedModel is the follow-on to #474:
// onDelegateError used to call recordServed unconditionally, so a
// failed attempt with empty EffectiveModel last-write-wins-blanked the
// model an earlier success had stored — the fact a failed run.json must
// still keep. An error that DOES report a model still overwrites.
func TestDelegateErrorDoesNotBlankRecordedModel(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-error-blank"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelError, nil), nil)
	hooks.OnDelegateFinished("campaign", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "anthropic/claude-opus-5",
		EffectiveModel: "glm-4.6",
	})

	hooks.OnDelegateError("campaign", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "anthropic/claude-opus-5",
		EffectiveModel: "",
		Error:          errors.New("delegate failed"),
	})

	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after empty-model error: %v", err)
	}
	served, ok := run.NodesServed["campaign"]
	if !ok {
		t.Fatal("NodesServed missing campaign after model-less error")
	}
	if served.Model != "glm-4.6" {
		t.Errorf("model-less error blanked NodesServed.Model: %+v", served)
	}

	hooks.OnDelegateError("campaign", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "anthropic/claude-opus-5",
		EffectiveModel: "other",
		Error:          errors.New("delegate failed with a reported model"),
	})

	run, err = st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after reported-model error: %v", err)
	}
	served = run.NodesServed["campaign"]
	if served.Model != "other" {
		t.Errorf("error with EffectiveModel should last-write-win, got %+v", served)
	}
}

func TestDelegateModelNoDriftWhenSameModel(t *testing.T) {
	cases := []struct {
		name, runID, declared, effective string
	}{
		{"provider prefix", "run-same-prefix", "anthropic/claude-opus-5", "claude-opus-5"},
		{"snapshot suffix", "run-same-snapshot", "openai/gpt-5.5", "gpt-5.5-2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			runID := tc.runID
			if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelError, nil), nil)
			hooks.OnDelegateFinished("n", DelegateInfo{
				BackendName:    "pi",
				DeclaredModel:  tc.declared,
				EffectiveModel: tc.effective,
			})
			evts, err := st.LoadEvents(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEvents: %v", err)
			}
			for _, e := range evts {
				if e.Type == store.EventModelDrift {
					t.Fatalf("%s must not emit model_drift: %+v", tc.name, e.Data)
				}
			}
		})
	}
}

func TestDelegateModelDriftDeduped(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-dedupe"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelError, nil), nil)
	info := DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "anthropic/claude-opus-5",
		EffectiveModel: "glm-4.6",
	}
	for i := 0; i < 3; i++ {
		hooks.OnDelegateFinished("campaign", info)
	}
	// A different rewrite on the same node is a new signal.
	hooks.OnDelegateFinished("campaign", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "anthropic/claude-opus-5",
		EffectiveModel: "kimi-k2",
	})

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var drifts []*store.Event
	for _, e := range evts {
		if e.Type == store.EventModelDrift {
			drifts = append(drifts, e)
		}
	}
	if len(drifts) != 2 {
		t.Fatalf("got %d model_drift events, want 2 (first rewrite once + distinct rewrite)", len(drifts))
	}
	if drifts[0].Data["effective_model"] != "glm-4.6" {
		t.Errorf("first drift effective = %v", drifts[0].Data["effective_model"])
	}
	if drifts[1].Data["effective_model"] != "kimi-k2" {
		t.Errorf("second drift effective = %v", drifts[1].Data["effective_model"])
	}
}

func TestDelegateInfoFromResult_carriesEffectiveModel(t *testing.T) {
	got := delegateInfoFromResult("claude_code", delegate.Result{
		EffectiveModel:     "glm-4.6",
		ContextWindow:      200_000,
		MaxOutputTokens:    8192,
		PeakInputTokens:    99,
		Tokens:             10,
		SessionFingerprint: "facade:https://api.z.ai/api/anthropic",
	})
	if got.EffectiveModel != "glm-4.6" {
		t.Errorf("EffectiveModel = %q", got.EffectiveModel)
	}
	if got.ContextWindow != 200_000 || got.MaxOutputTokens != 8192 || got.PeakInputTokens != 99 {
		t.Errorf("window fields dropped: %+v", got)
	}
	// The seam every facade surface hangs off: without this copy the
	// event and NodesServed.Fingerprint are permanently empty in
	// production, and a hand-built DelegateInfo in a test hides it.
	if got.Fingerprint != "facade:https://api.z.ai/api/anthropic" {
		t.Errorf("Fingerprint = %q — result.SessionFingerprint not carried", got.Fingerprint)
	}
}

// eventValueEqual compares event JSON values. events.jsonl round-trips
// integers as float64, so 8192 (int) and 8192.0 (float64) are the same fact.
func eventValueEqual(got, want any) bool {
	if got == want {
		return true
	}
	gf, gok := asFloat(got)
	wf, wok := asFloat(want)
	return gok && wok && gf == wf
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func findEvent(t *testing.T, evts []*store.Event, typ store.EventType) *store.Event {
	t.Helper()
	for i := range evts {
		if evts[i].Type == typ {
			return evts[i]
		}
	}
	t.Fatalf("no %s event in %d events", typ, len(evts))
	return nil
}

// A claude_code node whose tenant holds a z.ai key is routed through the
// Anthropic-shaped facade by default; the facade answers the requested
// claude id with the model it aliases it to, so declared and effective ids
// agree and no drift fires. The session fingerprint is the only evidence:
// it must reach the run record and raise one event per node and facade.
func TestDelegateFacadeRoutingReachesStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-facade"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var logBuf bytes.Buffer
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelInfo, &logBuf), nil)

	facade := DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "claude-opus-5",
		EffectiveModel: "claude-opus-5",
		Fingerprint:    "facade:https://api.z.ai/api/anthropic",
	}
	hooks.OnDelegateFinished("triage", facade)
	hooks.OnDelegateFinished("triage", facade) // a retry on the same route: no second event
	hooks.OnDelegateFinished("report", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "claude-opus-5",
		EffectiveModel: "claude-opus-5",
		Fingerprint:    "anthropic-oauth",
	})

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var facadeEvents []*store.Event
	for _, e := range evts {
		if e.Type == store.EventModelServedViaFacade {
			facadeEvents = append(facadeEvents, e)
		}
		if e.Type == store.EventModelDrift {
			t.Errorf("model_drift must stay silent when the ids agree: %v", e.Data)
		}
	}
	if len(facadeEvents) != 1 {
		t.Fatalf("got %d model_served_via_facade events, want exactly 1 (per node and facade): %+v", len(facadeEvents), facadeEvents)
	}
	ev := facadeEvents[0]
	if ev.NodeID != "triage" || ev.Data["fingerprint"] != "facade:https://api.z.ai/api/anthropic" || ev.Data["declared_model"] != "claude-opus-5" || ev.Data["backend"] != "claude_code" {
		t.Errorf("model_served_via_facade = node %q data %v", ev.NodeID, ev.Data)
	}

	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := run.NodesServed["triage"].Fingerprint; got != "facade:https://api.z.ai/api/anthropic" {
		t.Errorf("NodesServed[triage].Fingerprint = %q — the facade route was captured then dropped", got)
	}
	if got := run.NodesServed["report"].Fingerprint; got != "anthropic-oauth" {
		t.Errorf("NodesServed[report].Fingerprint = %q", got)
	}
}

// A delegation that FAILED must not claim it was served. claude_code
// returns a fully populated Result — session fingerprint and effective
// model both stamped — alongside the error on a rendered failure and on
// every hard CLI error subtype (auth, quota, max_budget), so the error
// path sees exactly the shape the success path does. Only the outcome
// differs, and the event's name is a claim about the outcome. The
// attempted route must still reach the run record.
func TestDelegateFacadeRoutingSilentOnFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-facade-failed"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var logBuf bytes.Buffer
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelInfo, &logBuf), nil)

	hooks.OnDelegateError("triage", DelegateInfo{
		BackendName:    "claude_code",
		DeclaredModel:  "claude-opus-5",
		EffectiveModel: "claude-opus-5",
		Fingerprint:    "facade:https://api.z.ai/api/anthropic",
		Error:          errors.New("delegate: claude-code error: subtype=error_during_execution"),
	})

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, e := range evts {
		if e.Type == store.EventModelServedViaFacade {
			t.Fatalf("a failed delegation emitted model_served_via_facade — the event asserts the node WAS served: %v", e.Data)
		}
	}

	// …but the attempted route is not lost: it stays on the run record,
	// beside the effective model a failed attempt already keeps (#474).
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := run.NodesServed["triage"].Fingerprint; got != "facade:https://api.z.ai/api/anthropic" {
		t.Errorf("NodesServed[triage].Fingerprint = %q — the attempted facade route must survive the failure", got)
	}
}
