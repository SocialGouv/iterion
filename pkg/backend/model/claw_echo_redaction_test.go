package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A claw agent fetches a URL carrying a declared secret (an API key in the
// query): the tool runs with the value, and its result — which quotes the URL
// back — reaches the model in placeholder form, as claude_code's PostToolUse
// hook does for WebFetch.
func TestAClawToolEchoingItsInputReturnsThePlaceholder(t *testing.T) {
	const key = "AIzaSyD-echo-9f8e7d6c5b4a3210fake"
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()
	g := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: key}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"url": srv.URL + "/geocode?key=__ITERION_SECRET_MAPS_KEY__"})
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "web_fetch", string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Geocode the office address."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "geo"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:     []string{"web_fetch"},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 || gotKey != key {
		t.Fatalf("scenario broken: %d model call(s), the server got key %q", len(calls), gotKey)
	}
	second, _ := json.Marshal(calls[1].Messages)
	if strings.Contains(string(second), key) {
		t.Fatalf("the model's next prompt carries the value the tool ran with: %s", second)
	}
	if !strings.Contains(string(second), "key=__ITERION_SECRET_MAPS_KEY__") {
		t.Fatalf("the tool result no longer quotes the URL: %s", second)
	}
}
