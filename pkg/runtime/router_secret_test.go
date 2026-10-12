package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/store"
)

// routerSecretBot: a reviewer quotes a value it read, a router picks from
// what it found — one route, or several.
const routerSecretBot = `secrets:
  hook_key: "${ROUTER_HOOK_KEY}"

prompt route_sys:
  Pick a lane from what the reviewer found: {{input.text}}

prompt work:
  Review config.yml.

agent seed:
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  user: work

agent lane_a:
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  user: work

agent lane_b:
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  user: work

%srouter pick:
  mode: llm
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  system: route_sys
%s
workflow w:
  worktree: none
  sandbox: none
  entry: seed
  seed -> pick with {
    text: "{{outputs.seed.text}}"
  }
  pick -> lane_a
  pick -> lane_b
%s`

func routerTextEvents(s string) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: s}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
}

// A router's reasoning quoting the value the reviewer read: its
// node_finished event is scrubbed like a node's output, one route selected or
// several.
func TestARoutersReasoningIsScrubbedInTheEventLog(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	t.Setenv("ROUTER_HOOK_KEY", key)
	reasoning := "The reviewer found the live key " + key + " hard-coded in config.yml, so rotate."
	for _, c := range []struct {
		name, join, multi, edges string
		route                    map[string]any
		lanes                    int
	}{
		{"single", "", "", "  lane_a -> done\n  lane_b -> done\n", map[string]any{"selected_route": "lane_a", "reasoning": reasoning}, 1},
		{"multi", "agent join:\n  backend: claw\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: work\n  await: wait_all\n\n", "  multi: true\n", "  lane_a -> join\n  lane_b -> join\n  join -> done\n", map[string]any{"selected_routes": []any{"lane_a", "lane_b"}, "reasoning": reasoning}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			wf := compileBotText(t, fmt.Sprintf(routerSecretBot, c.join, c.multi, c.edges))
			ws := t.TempDir()
			guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
			route, _ := json.Marshal(c.route)
			scripts := [][]api.StreamEvent{
				routerTextEvents("config.yml line 3 holds the live hook key " + key + " in clear."),
				askToolUse("r1", "structured_output", string(route)),
			}
			for range c.lanes {
				scripts = append(scripts, routerTextEvents("rotated"))
			}
			mock := &askMock{scripts: scripts}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			tr := tool.NewRegistry()
			_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
			storeDir := t.TempDir()
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			hooks := model.NewStoreEventHooks(context.Background(), s, "router-run", nil, guard, nil)
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
			exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard), model.WithToolRegistry(tr),
				model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw))
			t.Cleanup(func() { _ = exec.Close() })
			if err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "router-run", nil); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			seen := 0
			_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
				if e != nil || d.IsDir() || filepath.Base(p) != "events.jsonl" {
					return nil
				}
				b, _ := os.ReadFile(p)
				for _, line := range strings.Split(string(b), "\n") {
					if strings.Contains(line, `"node_finished"`) && strings.Contains(line, `"reasoning"`) {
						seen++
					}
					if strings.Contains(line, key) {
						t.Errorf("events.jsonl carries the value: %s", line)
					}
				}
				return nil
			})
			if seen == 0 {
				t.Fatal("scenario broken: no router node_finished with its reasoning")
			}
		})
	}
}
