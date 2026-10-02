package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

const secretInputKey = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"

func runClawToolWith(t *testing.T, tr *tool.Registry, toolName string, input map[string]any, workDir string) string {
	t.Helper()
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secretInputKey}}, secretguard.DefaultConfig())
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	in, _ := json.Marshal(input)
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", toolName, string(in), 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Do it."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(workDir), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "n"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
		Tools:     []string{toolName},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("scenario broken: %d model call(s)", len(calls))
	}
	second, _ := json.Marshal(calls[1].Messages)
	return string(second)
}

func aroundSecretInputKey(s string) string {
	i := strings.Index(s, secretInputKey)
	if i < 0 {
		return "(value absent)"
	}
	return s[max(0, i-160):min(len(s), i+len(secretInputKey)+40)]
}

func TestAClawGrepQuotingItsSecretPatternReturnsThePlaceholder(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
		t.Fatal(err)
	}
	second := runClawToolWith(t, tr, "grep", map[string]any{"pattern": "__ITERION_SECRET_GH_TOKEN__", "path": "."}, ws)
	t.Logf("model's next prompt around the value: %s", aroundSecretInputKey(second))
	if strings.Contains(second, secretInputKey) {
		t.Errorf("claw grep returned the materialised secret to the model")
	}
}

// A grep whose pattern carried a secret quotes it on no match or in its
// error: the reader's output goes back to placeholders for that call.
func TestAClawGrepErrorQuotingItsSecretPatternReturnsThePlaceholder(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x\n"), 0o644)
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
		t.Fatal(err)
	}
	second := runClawToolWith(t, tr, "grep", map[string]any{"pattern": "token=(__ITERION_SECRET_GH_TOKEN__", "path": "."}, ws)
	t.Logf("model's next prompt around the value: %s", aroundSecretInputKey(second))
	if strings.Contains(second, secretInputKey) {
		t.Errorf("claw grep error returned the materialised secret to the model")
	}
}

// An image fetched by a URL carrying a secret: its error quotes the URL.
func TestAClawImageURLCarryingASecretIsNotQuotedBack(t *testing.T) {
	tr := tool.NewRegistry()
	if err := tool.RegisterClawReadImage(tr); err != nil {
		t.Fatal(err)
	}
	second := runClawToolWith(t, tr, "read_image", map[string]any{"url": "https://127.0.0.1:1/render.png?apikey=__ITERION_SECRET_GH_TOKEN__"}, t.TempDir())
	t.Logf("model's next prompt around the value: %s", aroundSecretInputKey(second))
	if strings.Contains(second, secretInputKey) {
		t.Errorf("claw read_image returned the materialised URL to the model")
	}
}

// privacy_filter keeps what it redacts in the run's vault: its input stays
// in placeholder form.
func TestAClawPrivacyVaultKeepsThePlaceholder(t *testing.T) {
	store := t.TempDir()
	tr := tool.NewRegistry()
	cfg := &privacy.Config{StoreDir: store, Detector: detector.New(), RunIDFromCtx: func(context.Context) string { return "run-vault" }}
	if err := privacy.RegisterFilter(tr, cfg); err != nil {
		t.Fatal(err)
	}
	second := runClawToolWith(t, tr, "privacy_filter", map[string]any{"text": "Deploy with GITHUB_TOKEN=__ITERION_SECRET_GH_TOKEN__ then notify ops@example.com", "mode": "redact"}, t.TempDir())
	t.Logf("model's next prompt (privacy_filter result): %s", second[max(0, strings.Index(second, "redacted")-20):min(len(second), strings.Index(second, "redacted")+400)])
	vault := filepath.Join(store, "runs", "run-vault", "pii_vault.json")
	b, err := os.ReadFile(vault)
	if err != nil {
		t.Fatalf("no vault: %v", err)
	}
	st, _ := os.Stat(vault)
	t.Logf("vault %s (mode %v):\n%s", vault, st.Mode(), b)
	if strings.Contains(string(b), secretInputKey) {
		t.Errorf("the run store's pii_vault.json carries the materialised secret in clear")
	}
}

// A placeholder the call writes with JSON escapes is still one: execution
// materialises the decoded input, and the reader's output that quotes the
// value goes back to placeholders.
func TestAnEscapedPlaceholderCountsAsASecretTheCallNamed(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, raw string }{
		{"bash", `{"command":"echo \u005f_ITERION_SECRET_GH_TOKEN__"}`},
		{"grep", `{"pattern":"\u005f_ITERION_SECRET_GH_TOKEN__","path":"."}`},
	} {
		tr := tool.NewRegistry()
		if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
			t.Fatal(err)
		}
		if second := runClawRawJSON(t, tr, c.tool, c.raw, ws); strings.Contains(second, secretInputKey) {
			t.Errorf("%s %s: the materialised value reached the model: %s", c.tool, c.raw, aroundSecretInputKey(second))
		}
	}
}

func runClawRawJSON(t *testing.T, tr *tool.Registry, toolName, rawInput, workDir string) string {
	t.Helper()
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secretInputKey}}, secretguard.DefaultConfig())
	_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", toolName, rawInput, 50, 10), textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Do it."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(workDir), WithToolRegistry(tr), WithSecretGuard(g))
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"}, Tools: []string{toolName}}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("scenario broken: %d model call(s)", len(calls))
	}
	second, _ := json.Marshal(calls[1].Messages)
	return string(second)
}
