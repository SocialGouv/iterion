package runtime

import (
	"context"
	"encoding/json"
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

const asyncAskBot = `secrets:
  hook_key: "${ASYNC_HOOK_KEY}"

agent ask:
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  interaction: async
  user: "Check the hook key {{secrets.hook_key}}."

workflow w:
  worktree: none
  sandbox: none
  entry: ask
  ask -> done
`

func asyncTextEvents(s string) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: s}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
}

// An async question (ask_user_async) that quotes a value the agent read, and
// one that names it by its placeholder: the human_input_requested{async}
// event in events.jsonl, and the interaction the studio shows.
func TestAnAsyncQuestionIsScrubbedInTheEventLog(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	for _, c := range []struct{ name, question string }{
		{"quoted", "I read hk-live-9f8e7d6c5b4a3210-FAKE in config.yml: rotate it?"},
		{"placeholder", "Is the hook key __ITERION_SECRET_hook_key__ still valid?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			wf := compileBotText(t, asyncAskBot)
			ws := t.TempDir()
			guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
			in, _ := json.Marshal(map[string]any{"question": c.question})
			mock := &askMock{scripts: [][]api.StreamEvent{askToolUse("t1", delegate.AskUserAsyncToolName, string(in)), asyncTextEvents("posted; carrying on")}}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			tr := tool.NewRegistry()
			if err := tool.RegisterAskUser(tr, nil); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterAsyncAsk(tr); err != nil {
				t.Fatal(err)
			}
			_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
			storeDir := t.TempDir()
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			hooks := model.NewStoreEventHooks(context.Background(), s, "async-run", nil, guard, nil)
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
			exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard), model.WithToolRegistry(tr),
				model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw),
				model.WithExecutorAsyncAsk(&model.StoreAsyncAskBinder{Store: s}))
			t.Cleanup(func() { _ = exec.Close() })
			err = New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "async-run", nil)
			if err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			// The event log is an observational sink; the interaction keeps
			// the question whole — the operator answers it. A question that
			// names the placeholder is never given the value: nothing in the
			// store holds it.
			seen := 0
			_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
				if e != nil || d.IsDir() {
					return nil
				}
				b, _ := os.ReadFile(p)
				if c.name == "placeholder" && strings.Contains(string(b), key) {
					t.Errorf("%s holds the value of a question that named its placeholder", strings.TrimPrefix(p, storeDir))
				}
				if filepath.Base(p) != "events.jsonl" {
					return nil
				}
				for _, line := range strings.Split(string(b), "\n") {
					if strings.Contains(line, "human_input_requested") {
						seen++
						if strings.Contains(line, key) {
							t.Errorf("the async question's event carries the value: %s", line)
						}
					}
				}
				return nil
			})
			if seen == 0 {
				t.Fatal("scenario broken: no async human_input_requested event")
			}
		})
	}
}
