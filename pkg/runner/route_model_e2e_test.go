package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// openRouterPi answers like pi on OpenRouter: the reported model is
// OpenRouter's own slug. With reask set, its first answer omits a required
// field so the schema re-ask runs.
type openRouterPi struct {
	mu        sync.Mutex
	calls     int
	effective string
	reask     bool
}

func (b *openRouterPi) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	out := map[string]any{"verdict": true, "reason": "ok"}
	if b.reask && b.calls == 1 {
		delete(out, "reason")
	}
	return delegate.Result{BackendName: delegate.BackendPi, Tokens: 1000, EffectiveModel: b.effective, Output: out, Duration: time.Millisecond}, nil
}

type discardEmitter struct{}

func (discardEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	return &evt, nil
}

var openRouterCreds = secrets.Credentials{APIKeys: map[secrets.Provider]string{
	secrets.ProviderAnthropic: "sk-ant", secrets.ProviderOpenRouter: "sk-or",
}}

// From the executor that ran the chain, through the persisted
// delegate_finished, to the credential the meter charges: every call
// OpenRouter served — the schema re-ask included — is charged to OpenRouter,
// on the route the executor named. Red when the store hooks drop
// route_model, when the re-ask's delegate_finished carries none, and when
// the meter keys the route on the report instead of the served route.
func TestRouteModel_ExecutorToMeterChargesTheRouteThatServed(t *testing.T) {
	for _, reask := range []bool{false, true} {
		for _, tc := range []struct{ model, effective, key string }{
			{"openrouter/auto", "anthropic/claude-opus-4-8", "openrouter/anthropic/claude-opus-4-8"},
			{"openrouter/anthropic/claude-sonnet-4.5", "anthropic/claude-sonnet-4.5-20250929", "openrouter/anthropic/claude-sonnet-4.5"},
		} {
			usage := newMetricsEmitter(discardEmitter{}, metrics.New())
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendPi, &openRouterPi{effective: tc.effective, reask: reask})
			wf := &ir.Workflow{
				Prompts: map[string]*ir.Prompt{"ask": {Body: "judge"}},
				Schemas: map[string]*ir.Schema{"verdict_schema": {Name: "verdict_schema", Fields: []*ir.SchemaField{
					{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString},
				}}},
			}
			exec := model.NewClawExecutor(model.NewRegistry(), wf,
				model.WithBackendRegistry(reg),
				model.WithEventHooks(model.NewStoreEventHooks(context.Background(), usage, "run", iterlog.Nop(), nil, nil)),
				model.WithLogger(iterlog.Nop()), model.WithDefaultBackend(delegate.BackendPi))
			node := &ir.JudgeNode{
				BaseNode:     ir.BaseNode{ID: "judge"},
				LLMFields:    ir.LLMFields{Backend: delegate.BackendPi, Model: tc.model, UserPrompt: "ask"},
				SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
			}
			if _, err := exec.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{}); err != nil {
				t.Fatalf("execute: %v", err)
			}
			totals := usage.RouteTotals()
			if len(totals) == 0 {
				t.Fatalf("reask=%v %s: nothing metered", reask, tc.model)
			}
			for k, v := range totals {
				if slot := credentialSlotForRoute(openRouterCreds, k.backend, k.model); slot != string(secrets.ProviderOpenRouter) || k.model != tc.key {
					t.Errorf("reask=%v %s: %d tokens on %+v charged to %q, want route %q on openrouter", reask, tc.model, v.tokens(), k, slot, tc.key)
				}
			}
		}
	}
}

// pi fails both routes on a usage window; only the OpenRouter route burned
// tokens before failing.
type usageWindowPi struct{}

func (usageWindowPi) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	res := delegate.Result{BackendName: delegate.BackendPi, Duration: time.Millisecond, Output: map[string]any{}}
	if task.Model == "openrouter/anthropic/claude-sonnet-4.5" {
		res.Tokens = 4242
	}
	return res, &delegate.ErrRateLimited{Provider: "x", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)}
}

// A chain that ends on `action: skip` books what its failed routes burned on
// the last route that executed — here the OpenRouter fallback — never on the
// declared Anthropic route. Red when the skip outcome carries no route.
func TestRouteModel_ASkipBooksTheRouteThatBurned(t *testing.T) {
	usage := newMetricsEmitter(discardEmitter{}, metrics.New())
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendPi, usageWindowPi{})
	exec := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{}},
		model.WithBackendRegistry(reg),
		model.WithEventHooks(model.NewStoreEventHooks(context.Background(), usage, "run-skip", iterlog.Nop(), nil, nil)),
		model.WithLogger(iterlog.Nop()), model.WithDefaultBackend(delegate.BackendPi),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	node := &ir.AgentNode{}
	node.ID = "impl"
	node.Backend = delegate.BackendPi
	node.Model = "anthropic/claude-sonnet-4-5"
	node.UserPrompt = "ask"
	node.Fallbacks = []ir.Fallback{
		{Name: "or", Model: "openrouter/anthropic/claude-sonnet-4.5", On: []string{"usage_window"}},
		{Name: "give_up", Action: ir.FallbackActionSkip, On: []string{"usage_window"}},
	}
	if _, err := exec.Execute(model.WithRunID(context.Background(), "run-skip"), node, map[string]any{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var booked int64
	for k, v := range usage.RouteTotals() {
		booked += v.tokens()
		if slot := credentialSlotForRoute(openRouterCreds, k.backend, k.model); slot != string(secrets.ProviderOpenRouter) {
			t.Errorf("%d tokens on %+v charged to %q, want openrouter", v.tokens(), k, slot)
		}
	}
	if booked != 4242 {
		t.Errorf("booked %d tokens, want the 4242 the OpenRouter route burned", booked)
	}
}
