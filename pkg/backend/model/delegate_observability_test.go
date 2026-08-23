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
		EffectiveModel:  "glm-4.6",
		ContextWindow:   200_000,
		MaxOutputTokens: 8192,
		PeakInputTokens: 99,
		Tokens:          10,
	})
	if got.EffectiveModel != "glm-4.6" {
		t.Errorf("EffectiveModel = %q", got.EffectiveModel)
	}
	if got.ContextWindow != 200_000 || got.MaxOutputTokens != 8192 || got.PeakInputTokens != 99 {
		t.Errorf("window fields dropped: %+v", got)
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
