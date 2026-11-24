package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestUsageV2CollectorCopiesRoutesAndSelectsOwnedComponents(t *testing.T) {
	c := &ExecutionUsageCollector{}
	e := NewClawExecutor(NewRegistry(), verdictWorkflow())
	ctx := withExecutionUsageBackend(WithExecutionUsageCollector(context.Background(), c), "claude_code")
	backend := persistentRouteBackend(func(_ context.Context, task delegate.Task) (delegate.Result, error) {
		return delegate.Result{Tokens: 2, CostUSD: 1, EffectiveModel: "reported-" + task.Model, Output: map[string]any{"_tokens": 999, "_cost_usd": 999, "_model": "forged", "source": "generic_reported", "ordinal": 999}}, nil
	})
	if _, err := e.executeDelegate(ctx, backend, delegate.Task{Model: "A"}); err != nil {
		t.Fatal(err)
	}
	before, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	before.Components[0].Route.RequestedModel = "caller-mutated"
	before.OtherCostUSD = 999
	if _, err = e.executeDelegate(ctx, backend, delegate.Task{Model: "B"}); err != nil {
		t.Fatal(err)
	}
	delta, err := c.Since(before)
	if err != nil || len(delta.Components) != 1 || delta.Components[0].Ordinal != 2 || delta.Components[0].Route.RequestedModel != "B" || delta.OtherCostUSD != 1 {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
	full, err := c.Snapshot()
	if err != nil || full.Components[0].Route.RequestedModel != "A" || full.Components[0].Route.ReportedModel != "reported-A" || full.Tokens != 4 {
		t.Fatalf("snapshot=%+v err=%v", full, err)
	}
	other := &ExecutionUsageCollector{}
	foreign, _ := other.Snapshot()
	if _, err = c.Since(foreign); err == nil {
		t.Fatal("foreign collector checkpoint accepted")
	}
	if _, err = c.Since(ExecutionUsage{}); err == nil {
		t.Fatal("caller-invented checkpoint accepted")
	}
	empty, err := c.Since(full)
	if err != nil || len(empty.Components) != 0 || len(empty.Sessions) != 0 || empty.Tokens != 0 || empty.OtherCostUSD != 0 {
		t.Fatalf("no new calls must yield empty lot: %+v %v", empty, err)
	}
}
func TestUsageV2CumulativeWithoutIdentityDoesNotBecomeAdditive(t *testing.T) {
	c := &ExecutionUsageCollector{}
	c.observe("claude_code", delegate.Task{}, delegate.Result{CostUSD: 12, CostIsSessionTotal: true})
	if _, err := c.Snapshot(); err == nil {
		t.Fatal("missing session silently became additive")
	}
}
func TestUsageV2ExtractionCapturesOneEnteredSettlement(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "fixture")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	for _, mode := range []string{"no-schema", "pre-cancel", "success", "unreported"} {
		t.Run(mode, func(t *testing.T) {
			c := &ExecutionUsageCollector{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := NewClawExecutor(NewRegistry(), verdictWorkflow())
			spec := e.detectorSuggestedModel()
			if spec == "" {
				t.Fatal("fixture route missing")
			}
			client := &funcClient{fn: func(_ int, _ api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if mode == "unreported" {
					cancel()
					return nil, context.Canceled
				}
				return toolUseEvents("x", "structured_output", `{"verdict":true,"reason":"ok","call_id":"forged","_model":"forged"}`, 15, 5), nil
			}}
			e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
			schema := e.schemas["verdict_schema"]
			wire, _ := SchemaToJSON(schema)
			if mode == "no-schema" {
				wire = nil
			}
			if mode == "pre-cancel" {
				cancel()
			}
			_, spent, _, _ := e.extractStructuredCorrection(WithExecutionUsageCollector(ctx, c), "n", &delegate.Task{Model: "node/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished"}}, delegate.Result{}, schema, delegate.BackendClaudeCode)
			u, err := c.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "no-schema" || mode == "pre-cancel" {
				if len(client.calls) != 0 || len(u.Components) != 0 {
					t.Fatalf("phantom extraction calls=%d components=%+v", len(client.calls), u.Components)
				}
				return
			}
			if len(client.calls) != 1 || len(u.Components) != 1 {
				t.Fatalf("one entered settlement: calls=%d components=%+v", len(client.calls), u.Components)
			}
			component := u.Components[0]
			if component.Source != store.UsageStructuredExtraction || component.Ordinal != 1 || component.CallID == "" || component.CallID == "forged" || component.Route.RequestedModel != spec || component.Route.ReportedModel != "" || component.Route.SessionFingerprint != "" || component.Tokens != spent.Tokens {
				t.Fatalf("extraction provenance %+v", component)
			}
			if mode == "success" && (component.TokenSplit.Input != 15 || component.TokenSplit.Output != 5 || u.Tokens != 20) {
				t.Fatal(component)
			}
			if mode == "unreported" && (component.Tokens != 0 || component.UnreportedCalls != 1 || !errors.Is(ctx.Err(), context.Canceled)) {
				t.Fatalf("entered unknown disappeared: %+v", component)
			}
			component.TokenSplit.Input = 999
			again, _ := c.Snapshot()
			if again.Components[0].TokenSplit.Input == 999 {
				t.Fatal("split aliases collector")
			}
		})
	}
}
