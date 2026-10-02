package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/store"
)

type askMock struct {
	mu      sync.Mutex
	scripts [][]api.StreamEvent
	calls   []api.CreateMessageRequest
}

func (m *askMock) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if len(m.scripts) == 0 {
		return nil, fmt.Errorf("no more scripts")
	}
	s := m.scripts[0]
	m.scripts = m.scripts[1:]
	ch := make(chan api.StreamEvent, len(s))
	for _, ev := range s {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func askToolUse(id, name, in string) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: id, Name: name}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: in}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
}

const askBot = `secrets:
  hook_key: "${RVA_HOOK_KEY}"

agent ask:
  backend: claw
  model: "anthropic/claude-sonnet-4-6"
  interaction: human
  user: "Check the hook key {{secrets.hook_key}}."

workflow w:
  worktree: none
  sandbox: none
  entry: ask
  ask -> done
`

// A claw agent asks the operator a question naming a secret by its
// placeholder: the question the run pauses on — the event, the interaction,
// the checkpoint — keeps the placeholder.
func TestAClawQuestionReachesTheStoreInPlaceholderForm(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	wf := compileBotText(t, askBot)
	ws := t.TempDir()
	guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
	in, _ := json.Marshal(map[string]any{"question": "Is the hook key __ITERION_SECRET_hook_key__ still valid?"})
	mock := &askMock{scripts: [][]api.StreamEvent{askToolUse("t1", "ask_user", string(in))}}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	tr := tool.NewRegistry()
	if err := tool.RegisterAskUser(tr, nil); err != nil {
		t.Fatal(err)
	}
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	hooks := model.NewStoreEventHooks(context.Background(), s, "ask-run", nil, guard)
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
	exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard), model.WithToolRegistry(tr),
		model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw))
	t.Cleanup(func() { _ = exec.Close() })
	err = New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "ask-run", nil)
	found := 0
	_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), key) {
			found++
			i := strings.Index(string(b), key)
			t.Errorf("the store file %s carries the value: ...%s...", strings.TrimPrefix(p, storeDir), string(b)[max(0, i-80):min(len(b), i+40)])
		}
		return nil
	})
	if err == nil {
		t.Fatal("scenario broken: the run did not pause on the question")
	}
}

// A question quoting a value the model read (bash, a file) is kept whole by
// the interaction and the checkpoint, which the run needs; the event log, an
// observational sink, has it scrubbed.
func TestAValueQuotedInAQuestionIsScrubbedInTheEventLog(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	wf := compileBotText(t, askBot)
	ws := t.TempDir()
	guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
	in, _ := json.Marshal(map[string]any{"question": "I read hk-live-9f8e7d6c5b4a3210-FAKE in config.yml: rotate it?"})
	mock := &askMock{scripts: [][]api.StreamEvent{askToolUse("t1", "ask_user", string(in))}}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	tr := tool.NewRegistry()
	if err := tool.RegisterAskUser(tr, nil); err != nil {
		t.Fatal(err)
	}
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	hooks := model.NewStoreEventHooks(context.Background(), s, "ask-run2", nil, guard)
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
	exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard), model.WithToolRegistry(tr),
		model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw))
	t.Cleanup(func() { _ = exec.Close() })
	err = New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "ask-run2", nil)
	if err == nil {
		t.Fatal("scenario broken: the run did not pause on the question")
	}
	events := 0
	_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() || filepath.Base(p) != "events.jsonl" {
			return nil
		}
		events++
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "human_input_requested") {
			t.Fatalf("scenario broken: no human_input_requested event in %s", p)
		}
		if strings.Contains(string(b), key) {
			t.Errorf("the event log %s carries the value the question quotes", strings.TrimPrefix(p, storeDir))
		}
		return nil
	})
	if events == 0 {
		t.Fatal("scenario broken: no event log")
	}
}
