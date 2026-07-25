package delegate

import (
	"bytes"
	"context"
	"os/exec"
	"sync/atomic"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// stubSandboxRun is a minimal sandbox.Run so tests can mark a Task as
// sandboxed without a real container.
type stubSandboxRun struct{}

func (stubSandboxRun) Driver() string { return "stub" }
func (stubSandboxRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, cmd[0], cmd[1:]...)
}
func (stubSandboxRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (stubSandboxRun) Cleanup(context.Context) error { return nil }

func TestAskUserSandboxHTTPServer_NilWithoutEndpointOrToken(t *testing.T) {
	if srv := askUserSandboxHTTPServer(Task{}); srv != nil {
		t.Fatal("no endpoint/token → nil")
	}
	if srv := askUserSandboxHTTPServer(Task{AskUserHTTPEndpoint: "http://h:1/api/v1/mcp/ask-user"}); srv != nil {
		t.Fatal("endpoint without token → nil")
	}
	if srv := askUserSandboxHTTPServer(Task{AskUserRunToken: "tok"}); srv != nil {
		t.Fatal("token without endpoint → nil")
	}
}

func TestAskUserSandboxHTTPServer_Config(t *testing.T) {
	srv := askUserSandboxHTTPServer(Task{
		AskUserHTTPEndpoint: "http://host.docker.internal:39000/api/v1/mcp/ask-user",
		AskUserRunToken:     "tok-123",
	})
	if srv == nil {
		t.Fatal("expected a server config")
	}
	if srv.URL != "http://host.docker.internal:39000/api/v1/mcp/ask-user" {
		t.Fatalf("URL = %q", srv.URL)
	}
	if srv.Headers["X-Iterion-Run"] != "tok-123" {
		t.Fatalf("X-Iterion-Run header = %q, want the run token", srv.Headers["X-Iterion-Run"])
	}
	if !srv.AlwaysLoad {
		t.Fatal("AlwaysLoad must be set so the tools surface past claude-code's tool-search deferral")
	}
}

// A sandboxed interactive task WITH the per-run endpoint gets the ask_user
// wiring (HTTP MCP server + interception hook), exactly like the stdio
// path — this is the ADR-082 Phase 3 fallback.
func TestWireAskUserHook_SandboxedWithEndpointWiresHTTP(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		InteractionEnabled:  true,
		Sandbox:             stubSandboxRun{},
		AskUserHTTPEndpoint: "http://host.docker.internal:39000/api/v1/mcp/ask-user",
		AskUserRunToken:     "tok",
		AllowedTools:        []string{"Bash"}, // restricted → extras must grow
	}
	var extras []string
	var pending atomic.Value
	opts := b.wireAskUserHook(task, nil, &extras, &pending, func() {})
	if len(opts) != 2 {
		t.Fatalf("expected 2 options (MCP server + PreToolUse hook), got %d", len(opts))
	}
	if len(extras) != 1 || extras[0] != askUserMCPToolName {
		t.Fatalf("extras = %v, want [%s]", extras, askUserMCPToolName)
	}
}

// Async pair rides the same HTTP transport when the executor bound
// PostAsyncQuestion (interaction: async) — no sandbox carve-out left.
func TestWireAskUserHook_SandboxedAsyncWiresHooks(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		InteractionEnabled:  true,
		Sandbox:             stubSandboxRun{},
		AskUserHTTPEndpoint: "http://host.docker.internal:39000/api/v1/mcp/ask-user",
		AskUserRunToken:     "tok",
		AllowedTools:        []string{"Bash"},
		PostAsyncQuestion:   func(AsyncQuestion) (string, error) { return "q1", nil },
	}
	var extras []string
	var pending atomic.Value
	opts := b.wireAskUserHook(task, nil, &extras, &pending, func() {})
	// MCP server + ask_user hook + ask_user_async hook + await_answers hook.
	if len(opts) != 4 {
		t.Fatalf("expected 4 options, got %d", len(opts))
	}
	want := map[string]bool{askUserMCPToolName: false, askUserAsyncMCPToolName: false, awaitAnswersMCPToolName: false}
	for _, e := range extras {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for fqn, seen := range want {
		if !seen {
			t.Errorf("expected allow-list FQN %q, missing (extras=%v)", fqn, extras)
		}
	}
}

// A sandboxed task WITHOUT the per-run endpoint must not wire anything —
// the degrade is loud (warning) but structural: no MCP server, no hooks,
// no extras.
func TestWireAskUserHook_SandboxedWithoutEndpointDisabled(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logBuf)}
	task := Task{
		InteractionEnabled: true,
		Sandbox:            stubSandboxRun{},
		AllowedTools:       []string{"Bash"},
		PostAsyncQuestion:  func(AsyncQuestion) (string, error) { return "q1", nil },
	}
	var extras []string
	var pending atomic.Value
	opts := b.wireAskUserHook(task, nil, &extras, &pending, func() {})
	if len(opts) != 0 {
		t.Fatalf("expected no options without an endpoint, got %d", len(opts))
	}
	if len(extras) != 0 {
		t.Fatalf("expected no extras, got %v", extras)
	}
	if logBuf.Len() == 0 {
		t.Fatal("the sandboxed no-endpoint degrade must be LOUD (a warning), not silent")
	}
}

func TestWireAskUserHook_InteractionDisabledNoop(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		Sandbox:             stubSandboxRun{},
		AskUserHTTPEndpoint: "http://h:1/api/v1/mcp/ask-user",
		AskUserRunToken:     "tok",
	}
	var extras []string
	var pending atomic.Value
	if opts := b.wireAskUserHook(task, nil, &extras, &pending, func() {}); len(opts) != 0 {
		t.Fatalf("interaction disabled → no wiring, got %d options", len(opts))
	}
}
