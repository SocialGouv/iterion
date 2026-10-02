package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Claw's tools that quote their input back — a trigger its URL, a message,
// a structured payload — return the placeholder to the model: every tool's
// result is unmaterialised but the workspace readers'.
func TestAClawToolQuotingItsInputReturnsThePlaceholderWhateverItIs(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	cases := []struct {
		tool  string
		input map[string]any
	}{
		{"remote_trigger", map[string]any{"url": srv.URL + "/hook?key=__ITERION_SECRET_HOOK_KEY__", "method": "GET"}},
		{"send_user_message", map[string]any{"message": "The hook key is __ITERION_SECRET_HOOK_KEY__"}},
		{"structured_output", map[string]any{"payload": map[string]any{"token": "__ITERION_SECRET_HOOK_KEY__"}}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			gotKey = ""
			g := secretguard.New([]secretguard.Secret{{Name: "HOOK_KEY", Value: key}}, secretguard.DefaultConfig())
			tr := tool.NewRegistry()
			if err := tool.RegisterClawSimple(tr); err != nil {
				t.Fatal(err)
			}
			if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
				t.Fatal(err)
			}
			in, _ := json.Marshal(c.input)
			reg := NewRegistry()
			mock := newMockClient(toolUseEvents("t1", c.tool, string(in), 50, 10), textEvents("done", 60, 5))
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Notify the hook."}}, Schemas: map[string]*ir.Schema{}}
			exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
			node := &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "n"},
				LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
				Tools:     []string{c.tool},
			}
			if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			calls := mock.getCalls()
			if len(calls) < 2 {
				t.Fatalf("scenario broken: %d model call(s)", len(calls))
			}
			if c.tool == "remote_trigger" && gotKey != key {
				t.Fatalf("scenario broken: server got key %q", gotKey)
			}
			second, _ := json.Marshal(calls[1].Messages)
			if strings.Contains(string(second), key) {
				i := strings.Index(string(second), key)
				t.Errorf("the model's next prompt carries the secret value via %s: ...%s...", c.tool, string(second)[max(0, i-120):min(len(second), i+60)])
			}
		})
	}
}

// A question to the operator is kept, not run: the run pauses on it, the
// studio shows it, an auto-answer model may read it. It stays in placeholder
// form, as claude_code's interception of ask_user sees it.
func TestAClawQuestionToTheOperatorKeepsThePlaceholder(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	g := secretguard.New([]secretguard.Secret{{Name: "HOOK_KEY", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tool.RegisterAskUser(tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"question": "Is the hook key __ITERION_SECRET_HOOK_KEY__ still valid, or should I rotate it?"})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "ask_user", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Check the hook."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "n"},
		LLMFields:         ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	var ni *ErrNeedsInteraction
	if !errors.As(err, &ni) {
		t.Fatalf("scenario broken: %T %v", err, err)
	}
	q := fmt.Sprint(ni.Questions)
	conv := string(ni.Conversation)
	if strings.Contains(q, key) {
		t.Errorf("the paused question carries the secret value: %s", q)
	}
	if strings.Contains(conv, key) {
		t.Errorf("the checkpointed conversation carries the secret value")
	}
	if !strings.Contains(q, "__ITERION_SECRET_HOOK_KEY__") {
		t.Errorf("the question no longer quotes the placeholder: %s", q)
	}
}

// A board issue is kept by iterion's store: it stays in placeholder form.
func TestAClawBoardIssueKeepsThePlaceholder(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	g := secretguard.New([]secretguard.Secret{{Name: "HOOK_KEY", Value: key}}, secretguard.DefaultConfig())
	boardDir := t.TempDir()
	ns, err := native.NewStore(boardDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBoardTools(tr, &tool.BoardConfig{Store: ns, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatal(err)
	}
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	in, _ := json.Marshal(map[string]any{"title": "Rotate the hook key", "body": "The key __ITERION_SECRET_HOOK_KEY__ leaked in the logs; rotate it."})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "mcp_iterion_board_create_issue", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "File the rotation ticket."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "n"},
		LLMFields:    ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:        []string{"mcp.iterion_board.create_issue"},
		Capabilities: []string{boardops.CapBoardCreate},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("scenario broken: %d calls", len(calls))
	}
	second, _ := json.Marshal(calls[1].Messages)
	found := 0
	_ = filepath.WalkDir(boardDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), key) {
			found++
			t.Errorf("the board store file %s carries the secret value", strings.TrimPrefix(p, boardDir))
		}
		return nil
	})
	if found == 0 && !strings.Contains(string(second), "__ITERION_SECRET_HOOK_KEY__") {
		t.Fatalf("scenario broken: no issue body anywhere")
	}
	stored := false
	_ = filepath.WalkDir(boardDir, func(p string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			stored = stored || strings.Contains(string(b), "__ITERION_SECRET_HOOK_KEY__")
		}
		return nil
	})
	if !stored {
		t.Fatal("scenario broken: the board store holds no issue body")
	}
}

// The session's todo list is written to disk: it stays in placeholder form.
func TestAClawTodoFileKeepsThePlaceholder(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	todos := t.TempDir()
	t.Setenv("CLAW_TODOS_DIR", todos)
	g := secretguard.New([]secretguard.Secret{{Name: "HOOK_KEY", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tool.RegisterClawTodo(tr); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"action": "write", "todos": []map[string]any{{"id": "1", "content": "Rotate __ITERION_SECRET_HOOK_KEY__", "status": "pending", "priority": "high"}}})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "todo_write", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Plan the rotation."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "n"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:     []string{"todo_write"},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	n := 0
	_ = filepath.WalkDir(todos, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return nil
		}
		n++
		b, _ := os.ReadFile(p)
		st, _ := os.Stat(p)
		if strings.Contains(string(b), key) {
			t.Errorf("todo file %s (mode %v) carries the secret value", filepath.Base(p), st.Mode())
		}
		return nil
	})
	if n == 0 {
		t.Fatal("scenario broken: no todo file written")
	}
}

func runClawEchoTool(t *testing.T, register func(*tool.Registry, func(context.Context, json.RawMessage) (string, error)) error, nodeTool, modelName string, mcp []string) string {
	t.Helper()
	const key = "AIzaSyD-echo-9f8e7d6c5b4a3210fake"
	g := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	var ranWith string
	echo := func(_ context.Context, in json.RawMessage) (string, error) {
		ranWith = string(in)
		return "ran with " + string(in), nil
	}
	if err := register(tr, echo); err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", modelName, `{"q":"__ITERION_SECRET_MAPS_KEY__"}`, 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Look it up."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n"},
		LLMFields:        ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:            []string{nodeTool},
		ActiveMCPServers: mcp,
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 || !strings.Contains(ranWith, key) {
		t.Fatalf("scenario broken: %d model call(s), the tool ran with %q", len(calls), ranWith)
	}
	second, _ := json.Marshal(calls[1].Messages)
	return string(second)
}

func echoBuiltin(name string) func(*tool.Registry, func(context.Context, json.RawMessage) (string, error)) error {
	return func(tr *tool.Registry, exec func(context.Context, json.RawMessage) (string, error)) error {
		return tr.RegisterBuiltin(name, name, json.RawMessage(`{"type":"object"}`), exec)
	}
}

// Every claw tool but the workspace readers returns the placeholder of what
// it quotes back: a search its query, a write its path, a notebook edit its
// source, an MCP tool whatever it reports.
func TestEveryClawToolButTheWorkspaceReadersReturnsThePlaceholder(t *testing.T) {
	const key = "AIzaSyD-echo-9f8e7d6c5b4a3210fake"
	for _, c := range []struct {
		name, nodeTool, modelName string
		register                  func(*tool.Registry, func(context.Context, json.RawMessage) (string, error)) error
		mcp                       []string
	}{
		{"web_search", "web_search", "web_search", echoBuiltin("web_search"), nil},
		{"write_file", "write_file", "write_file", echoBuiltin("write_file"), nil},
		{"notebook_edit", "notebook_edit", "notebook_edit", echoBuiltin("notebook_edit"), nil},
		{"mcp", "mcp.srv.echo", "mcp_srv_echo", func(tr *tool.Registry, exec func(context.Context, json.RawMessage) (string, error)) error {
			return tr.RegisterMCP("srv", "echo", "echo", json.RawMessage(`{"type":"object"}`), exec)
		}, []string{"srv"}},
	} {
		second := runClawEchoTool(t, c.register, c.nodeTool, c.modelName, c.mcp)
		if strings.Contains(second, key) {
			t.Errorf("%s: the model's next prompt carries the value the tool ran with: %s", c.name, second)
		} else if !strings.Contains(second, "__ITERION_SECRET_MAPS_KEY__") {
			t.Errorf("%s: scenario broken, the result quotes nothing: %s", c.name, second)
		}
	}
}

// A workspace reader's output is left as is when its call carried no secret:
// an agent editing a line that holds a secret must see the value the file
// holds.
func TestAClawWorkspaceReaderKeepsTheValueTheFileHolds(t *testing.T) {
	const key = "AIzaSyD-echo-9f8e7d6c5b4a3210fake"
	for _, name := range []string{"read_file", "bash", "repl", "grep", "workspace_grep", "glob", "file_edit", "lsp", "read_image", "screenshot", "computer_use", "diagnostic_shell"} {
		t.Run(name, func(t *testing.T) {
			g := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: key}}, secretguard.DefaultConfig())
			tr := tool.NewRegistry()
			if err := tr.RegisterBuiltin(name, name, json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) {
				return "config.yml:3: maps_key: " + key, nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
				t.Fatal(err)
			}
			reg := NewRegistry()
			mock := newMockClient(toolUseEvents("t1", name, `{"path":"config.yml"}`, 50, 10), textEvents("done", 60, 5))
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Read the config."}}, Schemas: map[string]*ir.Schema{}}
			exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
			node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"}, Tools: []string{name}}
			if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			calls := mock.getCalls()
			if len(calls) < 2 {
				t.Fatalf("scenario broken: %d model call(s)", len(calls))
			}
			if second, _ := json.Marshal(calls[1].Messages); !strings.Contains(string(second), key) {
				t.Fatalf("%s: the value the file holds was hidden from the agent: %s", name, second)
			}
		})
	}
}

// An MCP tool whose name holds a reader's name is still an MCP tool: what it
// reports goes back to placeholders, its call carrying no secret.
func TestAnMCPToolNamedLikeAReaderReturnsThePlaceholder(t *testing.T) {
	const key = "AIzaSyD-echo-9f8e7d6c5b4a3210fake"
	g := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tr.RegisterMCP("fs", "read_file", "read a file", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) {
		return "maps_key: " + key, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "mcp_fs_read_file", `{"path":"config.yml"}`, 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Read it."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools: []string{"mcp.fs.read_file"}, ActiveMCPServers: []string{"fs"}}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("scenario broken: %d model call(s)", len(calls))
	}
	second, _ := json.Marshal(calls[1].Messages)
	if strings.Contains(string(second), key) {
		t.Fatalf("mcp_fs_read_file: its report went back to the model with the value: %s", second)
	}
}

// A board issue filed under the claude_code FQN spelling the claw loop
// bridges (lookupGenerationTool) is kept in placeholder form too.
func TestAClawBoardIssueFiledUnderItsFQNKeepsThePlaceholder(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	g := secretguard.New([]secretguard.Secret{{Name: "HOOK_KEY", Value: key}}, secretguard.DefaultConfig())
	boardDir := t.TempDir()
	ns, err := native.NewStore(boardDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBoardTools(tr, &tool.BoardConfig{Store: ns, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatal(err)
	}
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	in, _ := json.Marshal(map[string]any{"title": "Rotate the hook key", "body": "The key __ITERION_SECRET_HOOK_KEY__ leaked in the logs; rotate it."})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "mcp__iterion_board__create_issue", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "File the rotation ticket."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "n"},
		LLMFields:    ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:        []string{"mcp.iterion_board.create_issue"},
		Capabilities: []string{boardops.CapBoardCreate},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	stored, leaked := false, false
	_ = filepath.WalkDir(boardDir, func(p string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			stored = stored || strings.Contains(string(b), "__ITERION_SECRET_HOOK_KEY__")
			leaked = leaked || strings.Contains(string(b), key)
		}
		return nil
	})
	if leaked {
		t.Errorf("the board store holds the value: the FQN spelling was materialised")
	}
	if !stored && !leaked {
		t.Fatal("scenario broken: no issue body in the board store")
	}
}
