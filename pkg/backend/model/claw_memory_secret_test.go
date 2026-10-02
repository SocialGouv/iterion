package model

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A claw memory note is kept across runs and autoloaded into the next run's
// system prompt: its input stays in placeholder form.

func TestAClawMemoryNoteKeepsThePlaceholder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	const key = "dbpass-Q7w9-Zx81-FAKE-horse-battery"
	g := secretguard.New([]secretguard.Secret{{Name: "DB_PASS", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	in, _ := json.Marshal(map[string]any{"path": "deploy.md", "content": "# Deploy\nConnect with psql 'postgres://app:__ITERION_SECRET_DB_PASS__@db:5432/app'."})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "memory_write", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Record how to deploy."}}, Schemas: map[string]*ir.Schema{}}
	work := t.TempDir()
	exec := newTestClawExecutor(reg, wf, WithWorkDir(work), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "n"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Memory:    &ir.Memory{Enabled: true, Scope: "notes", Read: true, Write: true, Autoload: []string{"deploy.md"}},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	found := 0
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), key) {
			found++
		}
		return nil
	})
	if found > 0 {
		t.Errorf("the knowledge store holds the materialised secret in %d file(s)", found)
	}
	// Second run: the note autoloads into the system prompt.
	mock2 := newMockClient(textEvents("ok", 60, 5))
	reg2 := NewRegistry()
	reg2.Register("anthropic", func(string) (api.APIClient, error) { return mock2, nil })
	exec2 := newTestClawExecutor(reg2, wf, WithWorkDir(work), WithToolRegistry(tr), WithSecretGuard(g))
	if _, err := exec2.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	calls := mock2.getCalls()
	if len(calls) == 0 {
		t.Fatal("no call")
	}
	first, _ := json.Marshal(calls[0])
	if i := strings.Index(string(first), key); i >= 0 {
		t.Errorf("the next run's first request carries the value: ...%s...", string(first)[max(0, i-120):min(len(first), i+60)])
	}
}
