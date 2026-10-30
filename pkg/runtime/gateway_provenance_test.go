package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2786 — the gateway provenance must reach the node output an OPERATOR
// reads: the node_finished event persisted in the store, not the value the
// backend returns (gateway_routing_test.go's TestClawBackend_TheCatalogRecordRidesTheResult
// reads the return value and stayed green while the live run lost the key —
// the output's reserved `_` namespace is re-projected from the Result
// downstream, and `_gateway_spec` rode the map alone). Both generation
// strategies are walked: plain text and tools+schema, the one the live run
// failed on. Red when the re-projection drops the gateway stamp.

const gwPlainBot = `agent gw:
  backend: claw
  model: "openai_compatible/gw/m"
  user: "Answer with 42."

workflow w:
  worktree: none
  sandbox: none
  entry: gw
  gw -> done
`

const gwToolsSchemaBot = `schema verdict:
  answer: string

agent gw:
  backend: claw
  model: "openai_compatible/gw/m"
  user: "Answer."
  output: verdict
  tools: [todo_write]

workflow w:
  worktree: none
  sandbox: none
  entry: gw
  gw -> done
`

func TestAGatewayNodesProvenanceReachesThePersistedOutput(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"gw/m":{"context_window":123456,"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`)
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)

	for _, c := range []struct {
		name    string
		bot     string
		scripts [][]api.StreamEvent
	}{
		{"plain_text", gwPlainBot, [][]api.StreamEvent{
			routerTextEvents("42"),
		}},
		{"tools_and_schema", gwToolsSchemaBot, [][]api.StreamEvent{
			askToolUse("t1", "todo_write", `{"todos":[]}`),
			routerTextEvents(`{"answer":"blue"}`),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wf := compileBotText(t, c.bot)
			ws := t.TempDir()
			reg := model.NewRegistry()
			mock := &askMock{scripts: c.scripts}
			reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
			tr := tool.NewRegistry()
			_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
			storeDir := t.TempDir()
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
			exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithToolRegistry(tr),
				model.WithBackendRegistry(br), model.WithDefaultBackend(delegate.BackendClaw))
			t.Cleanup(func() { _ = exec.Close() })
			if err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "gw-run", nil); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			lines := eventLinesOfType(t, storeDir, "node_finished")
			if len(lines) == 0 {
				t.Fatal("scenario broken: no node_finished")
			}
			var ev struct {
				NodeID string         `json:"node_id"`
				Data   map[string]any `json:"data"`
			}
			found := false
			for _, line := range lines {
				if json.Unmarshal([]byte(line), &ev) != nil || ev.NodeID != "gw" {
					continue
				}
				found = true
				out, _ := ev.Data["output"].(map[string]any)
				spec, _ := json.Marshal(out["_gateway_spec"])
				if !strings.Contains(string(spec), `"source":"operator"`) {
					t.Errorf("the persisted node output carries no _gateway_spec provenance: %s", line)
				}
			}
			if !found {
				t.Fatal("scenario broken: gw never finished")
			}
		})
	}
}
