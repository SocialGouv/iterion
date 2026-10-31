package model

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// facadeScriptedBackend plays the claude_code delegate under a GLM-only
// credential set: the primary (no hint — the z.ai key takes the wire by
// default precedence, or an explicit zai pin) is refused the way the real
// delegate refuses a facade substitution, and the glm route serves.
type facadeScriptedBackend struct {
	calls []string         // ProviderHint of each Execute call, in order
	fail  map[string]error // provider hint -> error to return (absent = success)
}

func (b *facadeScriptedBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls = append(b.calls, task.ProviderHint)
	if err, ok := b.fail[task.ProviderHint]; ok {
		return delegate.Result{}, err
	}
	return delegate.Result{
		BackendName:    delegate.BackendClaudeCode,
		EffectiveModel: task.Model,
		Duration:       time.Millisecond,
		Output:         map[string]any{"ok": true},
	}, nil
}

// The acceptance core of #2772: a GLM-only credential set, a node asking
// for a claude id on the default wire precedence, and the declared `glm`
// rescue route. The primary is not SERVED — the z.ai facade would accept
// the claude id and answer it with GLM under the claude label — so the
// delegate refuses it as `unavailable` before the spawn, the declared
// fallback fires, and the run's record names the route that actually
// served: provider zai, model glm-5.3.
func TestFacadeSubstitutionFiresTheDeclaredFallback(t *testing.T) {
	t.Parallel()
	// The refusal exactly as the claude_code delegate raises it
	// (facadeSubstitutionRefusal) — the chain sees only the typed error.
	substitution := &delegate.ErrModelUnavailable{
		Provider: "zai",
		Model:    "claude-opus-5-5",
		Detail:   "the z.ai facade does not serve it — it would answer with GLM under the requested label",
	}
	fake := &facadeScriptedBackend{fail: map[string]error{"": substitution}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, fake)

	var finished []DelegateInfo
	var fallbacks []ProviderFallbackInfo
	e := newFallbackExecutor(reg, EventHooks{
		OnDelegateFinished: func(_ string, info DelegateInfo) { finished = append(finished, info) },
		OnProviderFallback: func(_ string, info ProviderFallbackInfo) { fallbacks = append(fallbacks, info) },
	})

	node := &ir.AgentNode{}
	node.ID = "reviewer_claude"
	node.Backend = delegate.BackendClaudeCode
	node.Model = "claude-opus-5-5"
	node.Fallbacks = []ir.Fallback{{
		Name:     "glm",
		Model:    "glm-5.3",
		Provider: "zai",
		On:       []string{"usage_window", "auth", "unavailable"},
		Metered:  true,
	}}

	out, err := e.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("the declared glm route must serve: %v", err)
	}
	if got := fake.calls; len(got) != 2 || got[0] != "" || got[1] != "zai" {
		t.Errorf("calls = %v, want the refused primary (no hint) then the zai route", got)
	}
	if out["_fallback_used"] != true {
		t.Errorf("_fallback_used missing — a gate must read the route change: %v", out)
	}
	if out["_served_by"] != "glm" {
		t.Errorf("_served_by = %v, want glm — the route that actually served", out["_served_by"])
	}
	if out["_model"] != "glm-5.3" {
		t.Errorf("_model = %v, want glm-5.3 — the run table must name the model that served, not the claude label", out["_model"])
	}
	if out["_provider"] != "zai" {
		t.Errorf("_provider = %v, want zai — the selected route is provider AND model, published together", out["_provider"])
	}
	if len(fallbacks) != 1 {
		t.Fatalf("fallback events = %+v, want exactly one", fallbacks)
	}
	if fallbacks[0].Reason != string(delegate.FallbackUnavailable) {
		t.Errorf("fallback reason = %q, want %q — a facade substitution is a model the credential cannot reach",
			fallbacks[0].Reason, delegate.FallbackUnavailable)
	}
	if fallbacks[0].ToModel != "glm-5.3" || fallbacks[0].To != "zai" {
		t.Errorf("fallback event = %+v, want the glm/zai route named", fallbacks[0])
	}
	if len(finished) != 1 {
		t.Fatalf("delegate_finished = %+v, want the one serving delegation", finished)
	}
	if finished[0].EffectiveModel != "glm-5.3" || finished[0].Provider != "zai" {
		t.Errorf("finished = %+v, want provider zai + model glm-5.3 bound together", finished[0])
	}
}

// The serving provider is published on the PRIMARY route too: the selected
// route is a fact of the run record however the chain ended.
func TestServingProviderIsPublishedOnThePrimaryRoute(t *testing.T) {
	t.Parallel()
	fake := &facadeScriptedBackend{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, fake)
	e := newFallbackExecutor(reg, EventHooks{})

	node := &ir.AgentNode{}
	node.ID = "reviewer"
	node.Backend = delegate.BackendClaudeCode
	node.Model = "glm-5.3"
	node.Provider = "zai"

	out, err := e.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("the glm route serves: %v", err)
	}
	if out["_provider"] != "zai" {
		t.Errorf("_provider = %v, want zai on a clean primary", out["_provider"])
	}
	if _, degraded := out["_fallback_used"]; degraded {
		t.Errorf("_fallback_used must stay clear on the primary route: %v", out)
	}
	if out["_model"] != "glm-5.3" {
		t.Errorf("_model = %v, want glm-5.3", out["_model"])
	}
}

// The run RECORD (the run level of the routing ladder) names the serving
// provider beside the serving model: NodesServed and the delegate_finished
// event carry the route's provider, not only its model.
func TestServingProviderReachesTheRunRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-provider"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil, nil)
	hooks.OnDelegateFinished("reviewer_claude", DelegateInfo{
		BackendName:    delegate.BackendClaudeCode,
		DeclaredModel:  "claude-opus-5-5",
		EffectiveModel: "glm-5.3",
		Provider:       "zai",
		Duration:       time.Millisecond,
	})

	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	served, ok := run.NodesServed["reviewer_claude"]
	if !ok {
		t.Fatal("NodesServed missing the node")
	}
	if served.Provider != "zai" || served.Model != "glm-5.3" {
		t.Errorf("NodesServed = %+v, want provider zai beside model glm-5.3", served)
	}

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	finished := findEvent(t, evts, store.EventDelegateFinished)
	if finished.Data["provider"] != "zai" {
		t.Errorf("delegate_finished[provider] = %v, want zai", finished.Data["provider"])
	}
}
