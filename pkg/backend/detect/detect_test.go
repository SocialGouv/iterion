package detect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv unsets all env vars that influence detection so subtests start
// from a known empty state. Each test then re-sets only the vars it cares
// about via t.Setenv (which is automatically rolled back at test end).
// Binary probes are also stubbed to "not found" so tests don't depend on
// whether `claude` / `codex` happen to be installed on the host (CI runners
// have neither; dev machines often have one or both).
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ITERION_BACKEND_PREFERENCE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL",
		// z.ai routes Anthropic through ANTHROPIC_BASE_URL + a ZAI_API_KEY /
		// ANTHROPIC_AUTH_TOKEN; without scrubbing these, a z.ai-configured
		// dev machine (a first-class supported provider) flips the anthropic
		// provider off and turns detection tests red.
		"ZAI_API_KEY",
		// OpenAI ChatGPT-forfait preference overrides are detection inputs.
		"ITERION_OPENAI_USE_OAUTH",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"HOME",
	} {
		t.Setenv(k, "")
	}
	// Use a fresh empty HOME so legacy ~/.claude / ~/.codex on the dev
	// machine don't leak into tests.
	t.Setenv("HOME", t.TempDir())
	stubBinary(t, &findClaudeBinary, "")
	stubBinary(t, &findCodexBinary, "")
	// The macOS Keychain probe shells out to /usr/bin/security and would
	// read the dev machine's real Claude Code login on darwin — stub it to
	// "absent" so detection is deterministic on every host. Tests that
	// exercise the keychain path re-stub it with a source label.
	stubSource(t, &claudeKeychainOAuthSource, "")
}

// stubBinary swaps a binary-probe var for the duration of the test.
// path == "" means "not installed". Restored on test cleanup.
func stubBinary(t *testing.T, target *func() (string, bool), path string) {
	t.Helper()
	prev := *target
	*target = func() (string, bool) {
		if path == "" {
			return "", false
		}
		return path, true
	}
	t.Cleanup(func() { *target = prev })
}

// stubSource swaps an OAuth-source-probe var (label == "" means "absent")
// for the duration of the test. Restored on test cleanup.
func stubSource(t *testing.T, target *func() string, label string) {
	t.Helper()
	prev := *target
	*target = func() string { return label }
	t.Cleanup(func() { *target = prev })
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writefile: %v", err)
	}
}

func TestPreferenceFromEnv_Default(t *testing.T) {
	isolateEnv(t)
	got := PreferenceFromEnv()
	if len(got) != 2 || got[0] != BackendClaudeCode || got[1] != BackendClaw {
		t.Fatalf("default preference = %v, want [claude_code claw]", got)
	}
}

func TestPreferenceFromEnv_CSV(t *testing.T) {
	isolateEnv(t)
	t.Setenv("ITERION_BACKEND_PREFERENCE", " claw, claude_code ,codex")
	got := PreferenceFromEnv()
	want := []string{"claw", "claude_code", "codex"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetect_NoCredentials(t *testing.T) {
	isolateEnv(t)
	r := Detect(context.Background())

	if r.ResolvedDefault != "" {
		t.Fatalf("ResolvedDefault = %q, want empty", r.ResolvedDefault)
	}
	for _, b := range r.Backends {
		if b.Available {
			t.Fatalf("backend %q reported available with no creds", b.Name)
		}
	}
}

func TestDetect_AnthropicAPIKey(t *testing.T) {
	isolateEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	r := Detect(context.Background())

	// claw must be available because anthropic provider is configured.
	clawSt := findBackend(t, r, BackendClaw)
	if !clawSt.Available {
		t.Fatalf("claw should be available with ANTHROPIC_API_KEY")
	}
	if clawSt.Auth != AuthAPIKey {
		t.Fatalf("claw auth = %q, want %q", clawSt.Auth, AuthAPIKey)
	}

	// Default preference is [claude_code, claw]; claude_code unavailable, so
	// resolved default should be claw.
	if r.ResolvedDefault != BackendClaw {
		t.Fatalf("ResolvedDefault = %q, want claw", r.ResolvedDefault)
	}

	// Anthropic provider must be available.
	provFound := false
	for _, p := range r.Providers {
		if p.Name == "anthropic" {
			provFound = true
			if !p.Available {
				t.Fatalf("anthropic provider should be available")
			}
			if p.Source != "ANTHROPIC_API_KEY" {
				t.Fatalf("anthropic source = %q", p.Source)
			}
		}
	}
	if !provFound {
		t.Fatalf("anthropic provider missing from report")
	}
}

func TestDetect_ClaudeCodeOAuth(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, "credentials.json"), `{"claudeAiOauth":{"accessToken":"x"}}`)

	r := Detect(context.Background())

	st := findBackend(t, r, BackendClaudeCode)
	if !st.Available {
		t.Fatalf("claude_code should be available with OAuth creds")
	}
	if st.Auth != AuthOAuth {
		t.Fatalf("claude_code auth = %q, want oauth", st.Auth)
	}
	if r.ResolvedDefault != BackendClaudeCode {
		t.Fatalf("ResolvedDefault = %q, want claude_code", r.ResolvedDefault)
	}
}

// Modern Claude Code (2.x) on macOS stores OAuth in the Keychain, not in a
// ~/.claude/.credentials.json file — so the file probe finds nothing even on
// a fully logged-in machine. The Keychain probe must flip claude_code to
// available so the resolver picks it and the studio enables Run.
func TestDetect_ClaudeCodeKeychainOAuth(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	// No credentials.json on disk; OAuth lives in the macOS Keychain.
	stubSource(t, &claudeKeychainOAuthSource, "macOS Keychain: Claude Code-credentials")

	r := Detect(context.Background())

	st := findBackend(t, r, BackendClaudeCode)
	if !st.Available {
		t.Fatalf("claude_code should be available via macOS Keychain OAuth")
	}
	if st.Auth != AuthOAuth {
		t.Fatalf("claude_code auth = %q, want oauth", st.Auth)
	}
	// The source must surface the Keychain origin (not a phantom file path).
	foundKeychain := false
	for _, s := range st.Sources {
		if strings.Contains(s, "Keychain") {
			foundKeychain = true
			break
		}
	}
	if !foundKeychain {
		t.Fatalf("claude_code sources = %v, want a Keychain source", st.Sources)
	}
	if r.ResolvedDefault != BackendClaudeCode {
		t.Fatalf("ResolvedDefault = %q, want claude_code (preferred over claw)", r.ResolvedDefault)
	}
}

// Regression guard for Linux/Windows: when neither a credentials file nor an
// OS-credential-store token is present, claude_code must stay unavailable and
// the resolver must not pick it. (isolateEnv stubs the Keychain probe to "".)
func TestDetect_ClaudeCodeNoOAuthAnywhere(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	// No credentials file, no Keychain token.

	r := Detect(context.Background())

	st := findBackend(t, r, BackendClaudeCode)
	if st.Available {
		t.Fatalf("claude_code should be unavailable with no OAuth source")
	}
	if r.ResolvedDefault != "" {
		t.Fatalf("ResolvedDefault = %q, want empty", r.ResolvedDefault)
	}
}

func TestDetect_BothClaudeOAuthAndAnthropic_PrefersClaudeCode(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, "credentials.json"), `{}`)

	r := Detect(context.Background())
	if r.ResolvedDefault != BackendClaudeCode {
		t.Fatalf("ResolvedDefault = %q, want claude_code (preferred over claw)", r.ResolvedDefault)
	}
}

func TestDetect_OverridePreferenceFavorsClaw(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, "credentials.json"), `{}`)
	t.Setenv("ITERION_BACKEND_PREFERENCE", "claw,claude_code")

	r := Detect(context.Background())
	if r.ResolvedDefault != BackendClaw {
		t.Fatalf("ResolvedDefault = %q, want claw (override)", r.ResolvedDefault)
	}
}

func TestDetect_CodexNotAutoSelected(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findCodexBinary, "/fake/codex")
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeFile(t, filepath.Join(dir, "auth.json"), `{}`)

	r := Detect(context.Background())

	codex := findBackend(t, r, BackendCodex)
	if !codex.Available {
		t.Fatalf("codex should be available")
	}
	// Default preference excludes codex → must NOT be auto-selected.
	if r.ResolvedDefault != "" {
		t.Fatalf("ResolvedDefault = %q, want empty (codex not auto)", r.ResolvedDefault)
	}
}

func TestDetect_CodexExplicitlyEnabled(t *testing.T) {
	isolateEnv(t)
	stubBinary(t, &findCodexBinary, "/fake/codex")
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeFile(t, filepath.Join(dir, "auth.json"), `{}`)
	t.Setenv("ITERION_BACKEND_PREFERENCE", "codex,claw")

	r := Detect(context.Background())
	if r.ResolvedDefault != BackendCodex {
		t.Fatalf("ResolvedDefault = %q, want codex (explicit opt-in)", r.ResolvedDefault)
	}
}

func TestResolve_EmptyWhenNoMatch(t *testing.T) {
	got := Resolve([]string{"claude_code", "claw"}, []BackendStatus{
		{Name: "codex", Available: true},
	})
	if got != "" {
		t.Fatalf("Resolve = %q, want empty", got)
	}
}

func TestSuggestedModel_OnlyForClaw(t *testing.T) {
	prov := []ProviderStatus{
		{Name: "anthropic", Available: true, SuggestedModel: "anthropic/claude-sonnet-4-6"},
		{Name: "openai", Available: true, SuggestedModel: "openai/gpt-5.4-mini"},
	}
	if m := SuggestedModel(BackendClaw, prov); m != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("claw suggested = %q", m)
	}
	if m := SuggestedModel(BackendClaudeCode, prov); m != "" {
		t.Fatalf("claude_code suggested should be empty, got %q", m)
	}
}

func TestZAISuggestedModel(t *testing.T) {
	isolateEnv(t)
	found := false
	for _, p := range detectProviders() {
		if p.Name != "zai" {
			continue
		}
		found = true
		if p.SuggestedModel != "anthropic/glm-5.2" {
			t.Fatalf("zai suggested model = %q, want anthropic/glm-5.2", p.SuggestedModel)
		}
	}
	if !found {
		t.Fatal("zai provider not present in detectProviders()")
	}
}

func TestCachedDetector_HonorsTTL(t *testing.T) {
	isolateEnv(t)
	cache := NewCachedDetector(0) // never expires
	r1 := cache.Get(context.Background())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	r2 := cache.Get(context.Background())
	if r1.ResolvedDefault != r2.ResolvedDefault {
		t.Fatalf("cache should not refresh: %q vs %q", r1.ResolvedDefault, r2.ResolvedDefault)
	}
	cache.Invalidate()
	r3 := cache.Get(context.Background())
	if r3.ResolvedDefault != BackendClaw {
		t.Fatalf("after invalidate, ResolvedDefault = %q, want claw", r3.ResolvedDefault)
	}
}

func findBackend(t *testing.T, r Report, name string) BackendStatus {
	t.Helper()
	for _, b := range r.Backends {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("backend %q missing from report", name)
	return BackendStatus{}
}
