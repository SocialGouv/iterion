package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type autoMemoryClient struct {
	mu    sync.Mutex
	n     int
	calls []api.CreateMessageRequest
	write bool
}

var autoMemoryDirRE = regexp.MustCompile(`Your persistent memory directory for this project is: ([^\n\\]+)`)

func (c *autoMemoryClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	c.n++
	var evs []api.StreamEvent
	if c.write && c.n == 1 {
		b, _ := json.Marshal(req)
		m := autoMemoryDirRE.FindStringSubmatch(string(b))
		if m == nil {
			return nil, fmt.Errorf("no memory dir in the prompt")
		}
		in, _ := json.Marshal(map[string]any{"path": filepath.Join(m[1], "MEMORY.md"), "content": "# Memory\n- Staging DB: psql 'postgres://app:__ITERION_SECRET_DB_PASS__@db:5432/app'\n"})
		evs = toolUseEvents("t1", "write_file", string(in), 50, 10)
	} else {
		evs = textEvents("done", 60, 5)
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestAnAutoMemoryWrittenWithAFileToolKeepsThePlaceholder(t *testing.T) {
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
	cl := run(true)
	if len(cl.calls) < 2 {
		t.Fatalf("scenario broken: %d calls", len(cl.calls))
	}
	found := 0
	for _, root := range []string{home, storeDir, work} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
			if e != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), key) {
				found++
				t.Logf("the value is in %s", p)
			}
			return nil
		})
	}
	if found > 0 {
		t.Errorf("%d persisted file(s) hold the materialised secret", found)
	}
	cl2 := run(false)
	first, _ := json.Marshal(cl2.calls[0])
	if i := strings.Index(string(first), key); i >= 0 {
		t.Errorf("the next run's first request carries the value: ...%s...", string(first)[max(0, i-160):min(len(first), i+60)])
	}
}
