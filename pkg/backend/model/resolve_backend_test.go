package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// resetEnvForResolve scrubs every env var that influences backend resolution
// and points HOME at a fresh tempdir so OAuth files on the dev box don't leak.
func resetEnvForResolve(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ITERION_DEFAULT_BACKEND", "ITERION_BACKEND_PREFERENCE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"OPENAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

// newExecutorForResolveTest returns a minimal executor wired to a fresh
// detector. We don't go through NewClawExecutor because that allocates
// session stores / registries we don't need.
func newExecutorForResolveTest(defaultBackend string) *ClawExecutor {
	return &ClawExecutor{
		defaultBackend: defaultBackend,
		detector:       detect.NewCachedDetector(0),
	}
}

func nodeWithBackend(backend string) *ir.AgentNode {
	n := &ir.AgentNode{}
	n.LLMFields.Backend = backend
	return n
}

func TestResolveBackend_NodeExplicit(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(nodeWithBackend("codex"))
	if got != "codex" {
		t.Fatalf("got %q, want codex", got)
	}
}

func TestResolveBackend_AutoStringTriggersDetect(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(nodeWithBackend("auto"))
	if got != detect.BackendClaw {
		t.Fatalf("got %q, want claw (auto + anthropic key)", got)
	}
}

func TestResolveBackend_WorkflowDefault(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("claude_code")
	got := e.resolveBackendName(&ir.AgentNode{})
	if got != "claude_code" {
		t.Fatalf("got %q, want claude_code", got)
	}
}

func TestResolveBackend_EnvOverridesDetect(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ITERION_DEFAULT_BACKEND", "codex")
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(&ir.AgentNode{})
	if got != "codex" {
		t.Fatalf("got %q, want codex (env override)", got)
	}
}

func TestResolveBackend_DetectAnthropicKey(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(&ir.AgentNode{})
	if got != detect.BackendClaw {
		t.Fatalf("got %q, want claw", got)
	}
}

func TestResolveBackend_DetectClaudeOAuth(t *testing.T) {
	resetEnvForResolve(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	credPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Also need a `claude` binary discoverable. We can't easily mock PATH
	// here without breaking other tests, so verify the detect report
	// agrees that claude_code is unavailable on this machine if the
	// binary is absent — and skip if so. CI / dev boxes usually have
	// `claude` installed; if not, we just confirm the fallback.
	report := detect.Detect(t.Context())
	var claudeAvail bool
	for _, b := range report.Backends {
		if b.Name == detect.BackendClaudeCode {
			claudeAvail = b.Available
		}
	}
	if !claudeAvail {
		t.Skip("claude binary not discoverable on this host; skipping OAuth-resolution check")
	}
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(&ir.AgentNode{})
	if got != detect.BackendClaudeCode {
		t.Fatalf("got %q, want claude_code", got)
	}
}

func TestResolveBackend_FallbackToClawWhenNothing(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(&ir.AgentNode{})
	if got != delegate.BackendClaw {
		t.Fatalf("got %q, want claw (last-resort)", got)
	}
}
