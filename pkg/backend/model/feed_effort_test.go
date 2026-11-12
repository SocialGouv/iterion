package model

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The DomiFa route: the default runner carries no OPENAI_COMPATIBLE_*
// table, and the platform's declared feed `pliage` states the efforts the
// gateway accepts for scaleway/deepseek-v4.1-flash.
const feedEffortWire = "scaleway/deepseek-v4.1-flash"

// unsetGatewayTables removes the runner's env tables from the process —
// absent, not empty: the default runner has none.
func unsetGatewayTables(t *testing.T) {
	t.Helper()
	for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv, compatgw.FeedTableEnv, compatgw.ResolvedEnvName} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// feedEffortCtx plants the run table a platform declaration folds to, on
// the run's context, through the runner's own bundle-to-context adapter.
// scaleway/strict accepts only high and max, so a lowered effort maps.
func feedEffortCtx(t *testing.T) context.Context {
	t.Helper()
	now := time.Now()
	table := pricefeed.Fold(pricefeed.FoldConfig{Now: now}, pricefeed.ScopeState{Config: &pricefeed.ConfigRecord{
		UpdatedAt: now,
		Feeds: []pricefeed.FeedConfig{{
			Name: "pliage", Type: pricefeed.TypeDeclared, Enabled: true,
			Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"entries": `{
				"scaleway/deepseek-v4.1-flash":{"context_window":128000,"max_output_tokens":16384,"input_usd_per_mtok":0.3,"output_usd_per_mtok":1.2,"reasoning_efforts":["low","high","xhigh","max"]},
				"scaleway/strict":{"reasoning_efforts":["high","max"]}}`},
		}},
	}})
	if table == nil {
		t.Fatal("the declaration folded to nothing")
	}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	ctx := compatgw.OverlayFeedTable(context.Background(), raw)
	if compatgw.FeedTableJSON(func(name string) string { return envGetenv(ctx, name) }) == nil {
		t.Fatal("the table did not plant")
	}
	return ctx
}

// W1 — the DomiFa regression: a gateway node asking `medium` of a route
// whose run table accepts low/high/xhigh/max sends `high`, and the
// adjustment event names the feed that said so. The runner's env table,
// when one is set with a CONFLICTING list, does not decide: the run table
// is read first.
func TestFeedEffortsMapTheGatewayDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"no env table", ""},
		{"a conflicting env table", `{"scaleway/deepseek-v4.1-flash":["low","max"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:8081/v1")
			t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
			t.Setenv("OPENAI_COMPATIBLE_API_KEY", "test-key")
			unsetGatewayTables(t)
			if tc.env != "" {
				t.Setenv(compatgw.ReasoningEffortsEnv, tc.env)
			}
			ctx := feedEffortCtx(t)
			mock := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"ok"}`, 10, 10))
			reg := NewRegistry()
			reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
			var logs bytes.Buffer
			emitter := &captureEmitter{}
			exec := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.New(iterlog.LevelDebug, &logs)), WithEventHooks(NewStoreEventHooks(ctx, emitter, "effort", iterlog.Nop(), nil, nil)))
			node := verdictJudge("claw")
			node.Model = modelroute.OpenAICompatible + "/" + feedEffortWire
			node.ReasoningEffort = "medium"
			if _, err := exec.Execute(ctx, node, nil); err != nil {
				t.Fatal(err)
			}
			calls := mock.getCalls()
			if len(calls) != 1 || calls[0].ReasoningEffort != "high" {
				t.Fatalf("the gateway was sent %+v, want reasoning_effort high", calls)
			}
			var adjusted []store.Event
			for _, event := range emitter.events {
				if event.Type == store.EventReasoningEffortAdjusted {
					adjusted = append(adjusted, event)
				}
			}
			if len(adjusted) != 1 {
				t.Fatalf("reasoning_effort_adjusted events = %d, want 1", len(adjusted))
			}
			data := adjusted[0].Data
			if data["requested"] != "medium" || data["applied"] != "high" || data["source"] != "feed:pliage" {
				t.Errorf("the adjustment event = %v, want medium -> high from feed:pliage", data)
			}
			if reason, _ := data["reason"].(string); !strings.Contains(reason, feedEffortWire) || !strings.Contains(reason, "feed:pliage") {
				t.Errorf("the adjustment reason %q names neither the wire id nor the source", reason)
			}
			if !strings.Contains(logs.String(), "feed:pliage") {
				t.Errorf("the warning line does not name the source: %s", logs.String())
			}
		})
	}
}

// The one point covers every effort writer: the delegation seam, the
// human/interaction stand-in and the cut-turn reduction all map by the
// run table when the runner carries no env table.
func TestFeedEffortsReachEveryEffortWriter(t *testing.T) {
	r3GatewayEnv(t)
	unsetGatewayTables(t)
	ctx := feedEffortCtx(t)
	spec := modelroute.OpenAICompatible + "/" + feedEffortWire

	t.Run("delegation", func(t *testing.T) {
		e := newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithLogger(iterlog.Nop()))
		b := &r3EffortBackend{}
		if _, err := e.executeDelegate(ctx, b, delegate.Task{NodeID: "n", Model: spec, ReasoningEffort: "medium"}); err != nil || b.effort != "high" {
			t.Fatalf("delegation sent %q (err %v), want high", b.effort, err)
		}
	})
	t.Run("interaction", func(t *testing.T) {
		mock := newMockClient(toolUseEvents("ok", "structured_output", `{"answer":"yes"}`, 10, 10))
		reg := NewRegistry()
		reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
		wf := &ir.Workflow{Schemas: map[string]*ir.Schema{"ans": {Name: "ans", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}}, Prompts: map[string]*ir.Prompt{}}
		e := newTestClawExecutor(reg, wf, WithLogger(iterlog.Nop()))
		node := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "approve"}}
		node.Interaction = ir.InteractionLLM
		node.Model = spec
		node.OutputSchema = "ans"
		_, err := e.executeHumanLLM(ctx, node, map[string]any{"_reasoning_effort": "medium"}, nil)
		calls := mock.getCalls()
		if err != nil || len(calls) != 1 || calls[0].ReasoningEffort != "high" {
			t.Fatalf("the interaction stand-in sent %+v (err %v), want high", calls, err)
		}
	})
	t.Run("cut-turn", func(t *testing.T) {
		opts := GenerationOptions{Model: modelroute.OpenAICompatible + "/scaleway/strict", ProviderOptions: map[string]any{"reasoning_effort": "max"}}
		lowered, to, ok := effortAfterCut(ctx, opts)
		if !ok || to != "high" || lowered.ProviderOptions["reasoning_effort"] != "high" {
			t.Fatalf("the cut-turn reduction lowered to %q (ok %v), want high — the lowest level the run table accepts", to, ok)
		}
	})
}

// The sandboxed crossing renders the merged effort table under the name it
// already crosses: an older sandbox image, reading only that env table by
// the node's exact wire spelling, maps by the run table the host mapped by.
func TestForwardableProviderEnv_CrossesTheFeedEfforts(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	unsetGatewayTables(t)
	ctx := feedEffortCtx(t)
	const wire = "Scaleway/DeepSeek-v4.1-flash"
	env, _, err := forwardableProviderEnv(ctx, modelroute.OpenAICompatible+"/"+wire, "")
	if err != nil {
		t.Fatal(err)
	}
	crossed, ok := env[compatgw.ReasoningEffortsEnv]
	if !ok {
		t.Fatal("the effort table did not cross")
	}
	got := compatgw.ReasoningEfforts(wire, func(name string) string {
		if name == compatgw.ReasoningEffortsEnv {
			return crossed
		}
		return ""
	}, func(string, ...any) {})
	if want := []string{"low", "high", "xhigh", "max"}; !slices.Equal(got, want) {
		t.Fatalf("an older sandbox reads %v for %q from the crossing %q, want the run table's %v", got, wire, crossed, want)
	}
	// Nothing to say crosses as the explicit empty.
	env, _, err = forwardableProviderEnv(context.Background(), modelroute.OpenAICompatible+"/"+wire, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := env[compatgw.ReasoningEffortsEnv]; !ok || v != "" {
		t.Fatalf("the effort table must cross as the explicit empty: %q %v", v, ok)
	}
}
