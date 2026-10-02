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

// The auto-memory note written with the value is persisted in placeholder
// form, and the next run's prompt carries it — not dropped.
func TestAnAutoMemoryNoteIsPersistedInPlaceholderForm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	const key = "dbpass-Q7w9-Zx81-FAKE-horse-battery"
	g := secretguard.New([]secretguard.Secret{{Name: "DB_PASS", Value: key}}, secretguard.DefaultConfig())
	work, storeDir := t.TempDir(), t.TempDir()
	run := func(write bool) *autoMemoryClient {
		tr := tool.NewRegistry()
		if err := tool.RegisterClawBuiltins(tr, work); err != nil {
			t.Fatal(err)
		}
		_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
		cl := &autoMemoryClient{write: write}
		reg := NewRegistry()
		reg.Register("anthropic", func(string) (api.APIClient, error) { return cl, nil })
		wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Record what you learned."}}, Schemas: map[string]*ir.Schema{}}
		exec := newTestClawExecutor(reg, wf, WithWorkDir(work), WithStoreDir(storeDir), WithAutoMemoryOverride("on"), WithBotID("bot1"), WithToolRegistry(tr), WithSecretGuard(g))
		node := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "n"},
			LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
			Tools:     []string{"write_file"},
		}
		if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
			t.Fatalf("scenario broken: %v", err)
		}
		return cl
	}
	run(true)
	persisted := 0
	for _, root := range []string{home, storeDir} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
			if e != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "Staging DB: psql 'postgres://app:__ITERION_SECRET_DB_PASS__@db:5432/app'") {
				persisted++
			}
			return nil
		})
	}
	cl2 := run(false)
	first, _ := json.Marshal(cl2.calls[0])
	seenNext := strings.Contains(string(first), "Staging DB: psql 'postgres://app:__ITERION_SECRET_DB_PASS__@db:5432/app'")
	if persisted == 0 || !seenNext {
		t.Errorf("the note was not persisted in placeholder form (persisted=%d, next run sees it=%v)", persisted, seenNext)
	}
}
