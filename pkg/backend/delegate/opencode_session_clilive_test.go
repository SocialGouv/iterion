//go:build clilive

// Real-CLI proof of opencode session resume and fork — gated by the `clilive`
// build tag because it needs the `opencode` CLI on PATH. It costs nothing and
// leaves the host alone: the model is a scripted OpenAI-compatible server on
// 127.0.0.1, every opencode root (config, database, caches) lives under the
// test's own directories, and the authority is what the CLI SENDS that server
// — a resumed turn's request carries the earlier turn's messages, which no
// fresh session could. Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILiveOpenCode -v ./pkg/backend/delegate/
package delegate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/git"
)

func TestCLILiveOpenCodeSessionResume(t *testing.T) {
	cli, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode CLI not on PATH")
	}
	model := newScriptedOpenAIModel(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []struct{ env, dir string }{
		{"HOME", "home"}, {"XDG_CONFIG_HOME", "config"}, {"XDG_DATA_HOME", "data"},
		{"XDG_CACHE_HOME", "cache"}, {"XDG_STATE_HOME", "state"},
	} {
		dir := filepath.Join(tmp, root.dir)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		t.Setenv(root.env, dir)
	}
	// The operator's own opencode settings must not reach the run.
	for _, name := range []string{"OPENCODE_DB", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_PERMISSION"} {
		t.Setenv(name, "")
	}
	t.Setenv("OPENCODE_DISABLE_MODELS_FETCH", "1")
	t.Setenv("OPENCODE_DISABLE_AUTOUPDATE", "1")
	mustWriteFile(t, filepath.Join(tmp, "config", "opencode", "opencode.json"), `{
  "provider": {
    "scripted": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Scripted",
      "options": {"baseURL": "`+model.URL+`", "apiKey": "scripted"},
      "models": {"m1": {"name": "m1", "tool_call": true}}
    }
  },
  "small_model": "scripted/m1",
  "share": "disabled",
  "autoupdate": false
}
`)
	repo := filepath.Join(tmp, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", git.NoAutoMaintenance(args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	b := NewOpenCodeBackend(testLogger(), cli)
	turn := func(t *testing.T, prompt, sessionID string, fork bool) (Result, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return b.Execute(ctx, Task{
			NodeID:      "n1",
			WorkDir:     repo,
			BaseDir:     repo,
			UserPrompt:  prompt,
			Model:       "scripted/m1",
			SessionID:   sessionID,
			ForkSession: fork,
		})
	}

	const first, second, third = "remember the codeword ZEBRA-42", "what is the codeword", "fork turn PELICAN-7"
	fresh, err := turn(t, first, "", false)
	if err != nil {
		t.Fatalf("fresh turn: %v (stderr %q)", err, fresh.Stderr)
	}
	if fresh.SessionID == "" {
		t.Fatal("the fresh turn reported no session id")
	}

	resumed, err := turn(t, second, fresh.SessionID, false)
	if err != nil {
		t.Fatalf("resume of %q: %v (stderr %q)", fresh.SessionID, err, resumed.Stderr)
	}
	if resumed.SessionID != fresh.SessionID {
		t.Fatalf("resume reported session %q, want the resumed %q", resumed.SessionID, fresh.SessionID)
	}
	model.requireAgentTurns(t, second, first)

	forked, err := turn(t, third, fresh.SessionID, true)
	if err != nil {
		t.Fatalf("fork of %q: %v (stderr %q)", fresh.SessionID, err, forked.Stderr)
	}
	if forked.SessionID == "" || forked.SessionID == fresh.SessionID {
		t.Fatalf("fork reported session %q, want a new id distinct from %q", forked.SessionID, fresh.SessionID)
	}
	model.requireAgentTurns(t, third, first, second)

	gone, err := turn(t, "x", "ses_doesnotexist", false)
	if err == nil {
		t.Fatalf("a resume of an unknown session succeeded with %v", gone.Output)
	}
	if !strings.Contains(gone.Stderr, "Session not found") {
		t.Fatalf("Stderr = %q, want opencode's own reason", gone.Stderr)
	}
}

// scriptedOpenAIModel is an OpenAI-compatible chat-completions endpoint that
// answers every request with a short streamed text and records what it was
// sent. Requests arrive from the CLI's own goroutines, so the record is
// mutex-guarded.
type scriptedOpenAIModel struct {
	URL string

	mu       sync.Mutex
	requests []scriptedOpenAIRequest
}

type scriptedOpenAIRequest struct {
	agentTurn bool   // the request offered tools: an agent turn, not a title
	messages  string // the request's messages, as the JSON the CLI sent
}

func newScriptedOpenAIModel(t *testing.T) *scriptedOpenAIModel {
	t.Helper()
	m := &scriptedOpenAIModel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages json.RawMessage   `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, scriptedOpenAIRequest{agentTurn: len(req.Tools) > 0, messages: string(req.Messages)})
		m.mu.Unlock()

		reply := "scripted answer"
		if len(req.Tools) == 0 {
			reply = "scripted title"
		}
		var body strings.Builder
		writeScriptedChunk(&body, map[string]any{"role": "assistant", "content": reply}, nil)
		writeScriptedChunk(&body, map[string]any{}, "stop")
		body.WriteString("data: [DONE]\n\n")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		_, _ = io.WriteString(w, body.String())
	}))
	t.Cleanup(srv.Close)
	m.URL = srv.URL + "/v1"
	return m
}

func writeScriptedChunk(body *strings.Builder, delta map[string]any, finish any) {
	chunk := map[string]any{
		"id":      "chatcmpl-scripted",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "m1",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if finish != nil {
		chunk["usage"] = map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	}
	raw, _ := json.Marshal(chunk)
	body.WriteString("data: " + string(raw) + "\n\n")
}

// requireAgentTurns asserts that the CLI sent at least one agent turn
// carrying prompt, and that every such turn also carried each of the earlier
// prompts — the conversation the session had to rehydrate. Matched by
// content, never by arrival order.
func (m *scriptedOpenAIModel) requireAgentTurns(t *testing.T, prompt string, earlier ...string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := 0
	for _, req := range m.requests {
		if !req.agentTurn || !strings.Contains(req.messages, prompt) {
			continue
		}
		seen++
		for _, want := range earlier {
			if !strings.Contains(req.messages, want) {
				t.Errorf("the agent turn for %q was sent without the earlier %q — the session was not rehydrated: %s", prompt, want, req.messages)
			}
		}
	}
	if seen == 0 {
		t.Fatalf("the CLI sent no agent turn carrying %q", prompt)
	}
}
