package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	anthropicprovider "github.com/SocialGouv/iterion/pkg/claw/pkg/api/providers/anthropic"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// effortRecord is the effort part of a node's output.
type effortRecord struct{ applied, source, requested, lowered any }

func effortRecordOf(out map[string]any) effortRecord {
	return effortRecord{out[OutputReasoningEffortApplied], out[OutputReasoningEffortSource], out[OutputReasoningEffortRequested], out[OutputReasoningEffortLowered]}
}

// TestTheRecordFollowsWhatTheClawRequestCarried reads the effort record of a
// claw node against what its model requests actually carried, in the four
// shapes where that differs from the level the executor handed the backend:
//
//   - claw sends a level the model accepts: `none` on a model whose matrix
//     starts at `low` goes out as `low` — the record says `low`, decided by
//     the backend, `none` asked for;
//   - a structured turn cut at max_tokens while reasoning is re-asked at a
//     lower effort, and the output is that answer — the record keeps the
//     level the call opened with and says it was lowered;
//   - a tool loop whose final turn is cut runs its recovery pass at a lower
//     effort, whose structured output is the node's — the same;
//   - the Anthropic Messages client sends no effort for a model outside its
//     matrix (a GLM id on that wire) — the record carries no applied level,
//     the backend as its source and the level asked for; the same client
//     with a Claude model sends it, and the record says so.
//
// Mutation that reddens it: drop the wireEffortTrail fold from the claw
// backend, or the WireReasoningEffort preference from recordReasoningEffort,
// or the client's own answer from wireReasoningEffort.
func TestTheRecordFollowsWhatTheClawRequestCarried(t *testing.T) {
	t.Run("the model's accepted set", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		mock := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"ok"}`, 10, 10))
		reg := NewRegistry()
		reg.Register("openai", func(string) (api.APIClient, error) { return mock, nil })
		e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()))
		node := verdictJudge("claw")
		node.Model = "openai/gpt-6-astra"
		node.ReasoningEffort = "none"
		out, err := e.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		calls := mock.getCalls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		wire := calls[0].ReasoningEffort
		if wire == "none" {
			t.Fatalf("premise: the model's matrix sent %q unchanged — this case proves nothing", wire)
		}
		if got, want := effortRecordOf(out), (effortRecord{wire, EffortSourceBackend, "none", nil}); got != want {
			t.Errorf("record = %+v, want %+v (the wire carried %q)", got, want, wire)
		}
	})

	t.Run("a cut structured turn re-asked lower", func(t *testing.T) {
		r3GatewayEnv(t)
		unsetGatewayTables(t)
		ctx := feedEffortCtx(t)
		mock := newMockClient(
			thinkingEvents("still weighing…", "max_tokens", 100, 8192),
			textEvents(`{"verdict":true,"reason":"ok"}`, 100, 20),
		)
		reg := NewRegistry()
		reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
		e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()))
		node := verdictJudge("claw")
		node.Model = modelroute.OpenAICompatible + "/" + feedEffortWire
		node.ReasoningEffort = "high"
		out, err := e.Execute(ctx, node, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		wires := wireEfforts(mock)
		if len(wires) != 2 || wires[0] != "high" || wires[1] != "low" {
			t.Fatalf("premise: the calls carried %v, want [high low]", wires)
		}
		if got, want := effortRecordOf(out), (effortRecord{"high", EffortSourceNode, nil, "low"}); got != want {
			t.Errorf("record = %+v, want %+v", got, want)
		}
	})

	t.Run("a tool loop's recovery pass lower", func(t *testing.T) {
		r3GatewayEnv(t)
		unsetGatewayTables(t)
		ctx := feedEffortCtx(t)
		mock := newMockClient(
			thinkingEvents("still weighing…", "max_tokens", 100, 8192),
			thinkingEvents("still weighing…", "max_tokens", 100, 8192),
			toolUseEvents("ok", "structured_output", `{"verdict":false,"reason":"decided at the lowered effort"}`, 100, 20),
		)
		reg := NewRegistry()
		reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
		tr := tool.NewRegistry()
		for _, name := range []string{"read_file", "todo_write"} {
			if err := tr.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
				t.Fatal(err)
			}
		}
		e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()), WithToolRegistry(tr), WithWorkDir(t.TempDir()))
		node := verdictJudge("claw")
		node.Model = modelroute.OpenAICompatible + "/" + feedEffortWire
		node.ReasoningEffort = "high"
		node.Tools = []string{"read_file"}
		out, err := e.Execute(ctx, node, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		wires := wireEfforts(mock)
		if len(wires) < 2 || wires[0] != "high" || wires[len(wires)-1] != "low" || out["verdict"] != false {
			t.Fatalf("premise: the calls carried %v and the verdict is %v, want high first, low last, the low call's verdict", wires, out["verdict"])
		}
		if got, want := effortRecordOf(out), (effortRecord{"high", EffortSourceNode, nil, "low"}); got != want {
			t.Errorf("record = %+v, want %+v", got, want)
		}
	})

	for _, tc := range []struct {
		model string
		want  effortRecord
	}{
		{"glm-5.3", effortRecord{nil, EffortSourceBackend, "high", nil}},
		{"claude-opus-5-5", effortRecord{"high", EffortSourceNode, nil, nil}},
	} {
		t.Run("the Anthropic Messages client with "+tc.model, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
			var mu sync.Mutex
			var bodies []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				mu.Lock()
				bodies = append(bodies, m)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, structuredOutputSSE)
			}))
			defer srv.Close()
			reg := NewRegistry()
			reg.Register("anthropic", func(m string) (api.APIClient, error) {
				return anthropicprovider.New().NewClient(api.ProviderConfig{APIKey: "k", Model: m, BaseURL: srv.URL})
			})
			e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()))
			node := verdictJudge("claw")
			node.Model = "anthropic/" + tc.model
			node.ReasoningEffort = "high"
			out, err := e.Execute(context.Background(), node, map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("requests = %d, want 1", len(bodies))
			}
			cfg, sent := bodies[0]["output_config"].(map[string]any)
			if sent != (tc.want.applied != nil) || (sent && cfg["effort"] != tc.want.applied) {
				t.Fatalf("premise: the request's output_config = %v, want effort %v", bodies[0]["output_config"], tc.want.applied)
			}
			if got := effortRecordOf(out); got != tc.want {
				t.Errorf("record = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// wireEfforts is the reasoning effort each call the mock received carried.
func wireEfforts(m *mockAPIClient) []string {
	var out []string
	for _, c := range m.getCalls() {
		out = append(out, c.ReasoningEffort)
	}
	return out
}

// structuredOutputSSE is one Anthropic Messages stream answering the
// structured_output tool with a verdict.
const structuredOutputSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"structured_output","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"verdict\":true,\"reason\":\"ok\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// TestClawAndClaudeCodeRecordTheSameEffortForAClaudeModel holds the two
// backends' records to one fact on the same Claude model: what the request
// carried. claw's is read off its real Anthropic client's request body;
// claude_code's is its report for the level the claude CLI was measured
// sending (delegate.claudeCodeWireEffort) — none for a model with no effort
// matrix, a clamp onto the accepted set otherwise.
func TestClawAndClaudeCodeRecordTheSameEffortForAClaudeModel(t *testing.T) {
	for _, tc := range []struct{ model, level string }{
		{"claude-haiku-4-5", "high"},
		{"claude-sonnet-4-6", "xhigh"},
		{"claude-opus-5-5", "high"},
		{"claude-opus-5-5", "max"},
	} {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, structuredOutputSSE)
			}))
			defer srv.Close()
			reg := NewRegistry()
			reg.Register("anthropic", func(m string) (api.APIClient, error) {
				return anthropicprovider.New().NewClient(api.ProviderConfig{APIKey: "k", Model: m, BaseURL: srv.URL})
			})
			e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()))
			node := verdictJudge("claw")
			node.Model = "anthropic/" + tc.model
			node.ReasoningEffort = tc.level
			out, err := e.Execute(context.Background(), node, map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			claw, _ := out[OutputReasoningEffortApplied].(string)

			dir := t.TempDir()
			cli := filepath.Join(dir, "claude")
			if err := os.WriteFile(cli, []byte(converseStandInCLI), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CATALOG_LOG", filepath.Join(dir, "catalog.log"))
			t.Setenv("TMPDIR", t.TempDir())
			b := &delegate.ClaudeCodeBackend{Logger: iterlog.Nop()}
			res, _ := b.Execute(context.Background(), delegate.Task{Command: cli, WorkDir: dir, UserPrompt: "x", Model: tc.model, ReasoningEffort: tc.level})
			if !res.WireReasoningEffortReported || res.WireReasoningEffort != claw {
				t.Errorf("claw recorded %q, claude_code reports %q (reported=%v)", claw, res.WireReasoningEffort, res.WireReasoningEffortReported)
			}
		})
	}
}
