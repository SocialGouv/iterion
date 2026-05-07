package detect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// isolateEnv unsets all env vars that influence detection so subtests start
// from a known empty state. Each test then re-sets only the vars it cares
// about via t.Setenv (which is automatically rolled back at test end).
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ITERION_BACKEND_PREFERENCE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"OPENAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
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

func TestDetect_BothClaudeOAuthAndAnthropic_PrefersClaudeCode(t *testing.T) {
	isolateEnv(t)
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
