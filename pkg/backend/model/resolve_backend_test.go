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
		"RESCUE_PROVIDER",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	// Point PATH at an empty dir so the detector can't discover the host's
	// real `claude`/`codex` binaries. Without this, the claude_code probe's
	// `claude auth status` fallback would spawn the real CLI during these
	// unit tests — non-deterministic across hosts and, worse, the CLI writes
	// `.claude.json.backup` files into the test cwd. Tests that need a binary
	// present install a fake one on PATH explicitly (see fakeExecOnPath).
	t.Setenv("PATH", t.TempDir())
}

// fakeExecOnPath drops a no-op executable named `name` into a fresh dir and
// prepends that dir to PATH, so the detector's binary probe (exec.LookPath)
// finds it without invoking the host's real CLI. PATH is restored on cleanup
// via t.Setenv.
func fakeExecOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	// claude_code requires a discoverable `claude` binary AND an OAuth
	// credential. The creds file above supplies the credential; install a
	// fake binary so the file probe wins without spawning the real CLI.
	fakeExecOnPath(t, "claude")
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

// nodeWithBackendProvider returns an AgentNode carrying both literal
// backend and provider strings so resolveBackend / resolveProvider
// tests can exercise their respective env-expansion paths.
func nodeWithBackendProvider(backend, provider string) *ir.AgentNode {
	n := &ir.AgentNode{}
	n.LLMFields.Backend = backend
	n.LLMFields.Provider = provider
	return n
}

func TestResolveBackend_EnvVarExpansion(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("MY_BACKEND", "claude_code")
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(nodeWithBackendProvider("${MY_BACKEND}", ""))
	if got != "claude_code" {
		t.Fatalf("got %q, want claude_code (env-expanded)", got)
	}
}

func TestResolveBackend_EnvVarDefaultExpansion(t *testing.T) {
	resetEnvForResolve(t)
	// MY_BACKEND unset → default form wins.
	e := newExecutorForResolveTest("")
	got := e.resolveBackendName(nodeWithBackendProvider("${MY_BACKEND:-codex}", ""))
	if got != "codex" {
		t.Fatalf("got %q, want codex (env default)", got)
	}
}

func TestResolveProvider_NodeExplicit(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")
	got := e.resolveProvider(nodeWithBackendProvider("claude_code", "anthropic"))
	if got != "anthropic" {
		t.Fatalf("got %q, want anthropic", got)
	}
}

func TestResolveProvider_EnvVarExpansion(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("RESCUE_PROVIDER", "anthropic")
	e := newExecutorForResolveTest("")
	got := e.resolveProvider(nodeWithBackendProvider("claude_code", "${RESCUE_PROVIDER:-zai}"))
	if got != "anthropic" {
		t.Fatalf("got %q, want anthropic (env-expanded)", got)
	}
}

func TestResolveProvider_EnvVarDefaultExpansion(t *testing.T) {
	resetEnvForResolve(t)
	// RESCUE_PROVIDER unset → default "zai" wins.
	e := newExecutorForResolveTest("")
	got := e.resolveProvider(nodeWithBackendProvider("claude_code", "${RESCUE_PROVIDER:-zai}"))
	if got != "zai" {
		t.Fatalf("got %q, want zai (env default)", got)
	}
}

func TestResolveProvider_AutoNormalizesToEmpty(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")
	got := e.resolveProvider(nodeWithBackendProvider("claude_code", "auto"))
	if got != "" {
		t.Fatalf("got %q, want '' (auto → blank lets cred resolver fall to its default precedence)", got)
	}
}

func TestResolveProvider_EmptyWhenUnset(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")
	got := e.resolveProvider(&ir.AgentNode{})
	if got != "" {
		t.Fatalf("got %q, want '' for unset Provider", got)
	}
}

func equalProviderSteps(a, b []providerStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolveProviderChain(t *testing.T) {
	resetEnvForResolve(t)
	e := newExecutorForResolveTest("")

	cases := []struct {
		name     string
		provider string
		setEnv   map[string]string
		want     []providerStep
	}{
		{"unset", "", nil, []providerStep{{}}},
		{"single", "anthropic", nil, []providerStep{{Provider: "anthropic"}}},
		{"auto normalises to blank", "auto", nil, []providerStep{{}}},
		{"chain", "anthropic,zai,openai", nil, []providerStep{{Provider: "anthropic"}, {Provider: "zai"}, {Provider: "openai"}}},
		{"chain with whitespace", "anthropic, zai , openai", nil, []providerStep{{Provider: "anthropic"}, {Provider: "zai"}, {Provider: "openai"}}},
		{"trailing comma dropped", "anthropic,", nil, []providerStep{{Provider: "anthropic"}}},
		{"leading comma dropped", ",anthropic", nil, []providerStep{{Provider: "anthropic"}}},
		{"consecutive duplicates collapsed", "zai,zai,anthropic", nil, []providerStep{{Provider: "zai"}, {Provider: "anthropic"}}},
		{"explicit auto kept as chain element", "auto,anthropic", nil, []providerStep{{}, {Provider: "anthropic"}}},
		// Per-element model overrides (`provider:model`): the headline feature.
		{"per-element model swap", "zai:glm-5.2,anthropic:claude-opus-4-8", nil, []providerStep{{Provider: "zai", Model: "glm-5.2"}, {Provider: "anthropic", Model: "claude-opus-4-8"}}},
		{"mixed model and inherit", "zai:glm-5.2,anthropic", nil, []providerStep{{Provider: "zai", Model: "glm-5.2"}, {Provider: "anthropic"}}},
		{"model with whitespace", "zai : glm-5.2", nil, []providerStep{{Provider: "zai", Model: "glm-5.2"}}},
		{"model id with colon split on first", "zai:foo:bar", nil, []providerStep{{Provider: "zai", Model: "foo:bar"}}},
		// Env expansion runs on the whole field BEFORE splitting, so an
		// env default may itself carry the rest of the chain.
		{"rescue head expands then chains", "${RESCUE_PROVIDER:-zai},anthropic", nil, []providerStep{{Provider: "zai"}, {Provider: "anthropic"}}},
		{"rescue head overridden", "${RESCUE_PROVIDER:-zai},anthropic", map[string]string{"RESCUE_PROVIDER": "openai"}, []providerStep{{Provider: "openai"}, {Provider: "anthropic"}}},
		{"env supplies whole chain", "${PROVIDERS:-anthropic,zai}", nil, []providerStep{{Provider: "anthropic"}, {Provider: "zai"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.setEnv {
				t.Setenv(k, v)
			}
			got := e.resolveProviderChain(nodeWithBackendProvider("claude_code", tc.provider))
			if !equalProviderSteps(got, tc.want) {
				t.Fatalf("resolveProviderChain(%q) = %v, want %v", tc.provider, got, tc.want)
			}
			// resolveProvider must equal the head provider of the chain (back-compat).
			if head := e.resolveProvider(nodeWithBackendProvider("claude_code", tc.provider)); head != tc.want[0].Provider {
				t.Errorf("resolveProvider(%q) = %q, want head %q", tc.provider, head, tc.want[0].Provider)
			}
		})
	}
}
