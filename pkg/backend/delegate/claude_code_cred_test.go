package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// resetClaudeCredEnv scrubs every process-env var that participates in
// anthropicCredEnvForCLI's resolution so each test starts from a clean
// slate. Keeps the matrix focused on the ctx-creds + hint inputs.
func resetClaudeCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ZAI_API_KEY",
		"CLAUDE_CONFIG_DIR",
	} {
		t.Setenv(k, "")
	}
}

// ctxWithCreds wires a minimal sealed-credentials context for the
// helper. Either or both maps may be nil/empty.
func ctxWithCreds(t *testing.T, apiKeys map[secrets.Provider]string, oauthDirs map[string]string) context.Context {
	t.Helper()
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              apiKeys,
		OAuthCredentialFiles: oauthDirs,
	})
}

// --- default precedence (no hint) ----------------------------------

func TestAnthropicCredEnv_AutoZAIFromCtxWinsOverAnthropic(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderZAI:       "zai-test",
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "", false)
	if got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
		t.Fatalf("ANTHROPIC_BASE_URL: got %q, want %q", got["ANTHROPIC_BASE_URL"], secrets.ZAIDefaultBaseURL)
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, present := got["ANTHROPIC_API_KEY"]; present {
		t.Errorf("ANTHROPIC_API_KEY must NOT be set when z.ai key wins precedence")
	}
}

func TestAnthropicCredEnv_AutoAnthropicWhenNoZAI(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "", false)
	if got["ANTHROPIC_API_KEY"] != "sk-anthropic-test" {
		t.Errorf("ANTHROPIC_API_KEY: got %q, want sk-anthropic-test", got["ANTHROPIC_API_KEY"])
	}
}

func TestAnthropicCredEnv_AutoEnvFallbackZAI(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ZAI_API_KEY", "env-zai-test")
	got := anthropicCredEnvForCLI(context.Background(), "", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "env-zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want env-zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want default z.ai URL", got["ANTHROPIC_BASE_URL"])
	}
}

// --- hint: anthropic ------------------------------------------------

// TestAnthropicCredEnv_HintAnthropicSkipsZAIInCtx is THE motivating
// case for the provider feature: a node says "I need Anthropic's 1M
// context, route me there even though ZAI_API_KEY is set on the
// process and would otherwise win the precedence".
func TestAnthropicCredEnv_HintAnthropicSkipsZAIInCtx(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderZAI:       "zai-test",
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "anthropic", false)
	if got["ANTHROPIC_API_KEY"] != "sk-anthropic-test" {
		t.Fatalf("ANTHROPIC_API_KEY: got %q, want sk-anthropic-test (hint must force this even with z.ai key present)", got["ANTHROPIC_API_KEY"])
	}
	// And critically, z.ai routing must NOT be wired.
	if got["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want unset (hint anthropic must not route to z.ai)", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want unset", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestAnthropicCredEnv_HintAnthropicFallsToOAuthDir(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, nil, map[string]string{
		string(secrets.OAuthKindClaudeCode): "/tmp/iterion-oauth-claude",
	})
	got := anthropicCredEnvForCLI(ctx, "anthropic", false)
	assertForfaitEnv(t, got, "/tmp/iterion-oauth-claude")
}

func TestAnthropicCredEnv_AutoForfaitSuppressesInheritedKey(t *testing.T) {
	resetClaudeCredEnv(t)
	// A shared, possibly-dead ANTHROPIC_API_KEY in the runner's pod env must
	// NOT shadow a resolved per-run forfait: the returned map suppresses it
	// (""), and mergeCmdEnv (claudesdk) turns that into an absent var so the
	// CLI uses the CLAUDE_CONFIG_DIR OAuth token.
	t.Setenv("ANTHROPIC_API_KEY", "sk-dead-shared")
	ctx := ctxWithCreds(t, nil, map[string]string{
		string(secrets.OAuthKindClaudeCode): "/tmp/iterion-oauth-claude",
	})
	got := anthropicCredEnvForCLI(ctx, "", false)
	assertForfaitEnv(t, got, "/tmp/iterion-oauth-claude")
}

// assertForfaitEnv pins the forfait env contract: the OAuth config dir is
// wired and every Anthropic-flavoured credential that could shadow it is
// explicitly emptied (suppression signal consumed by mergeCmdEnv).
func assertForfaitEnv(t *testing.T, got map[string]string, wantDir string) {
	t.Helper()
	if got["CLAUDE_CONFIG_DIR"] != wantDir {
		t.Errorf("CLAUDE_CONFIG_DIR: got %q, want %q", got["CLAUDE_CONFIG_DIR"], wantDir)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		v, present := got[k]
		if !present || v != "" {
			t.Errorf("%s must be present and empty (suppression): present=%v val=%q", k, present, v)
		}
	}
}

// When the materialised dir holds a real credentials.json, the resolver also
// exports CLAUDE_CODE_OAUTH_TOKEN — the first-precedence headless auth path
// that bypasses a cloud runner's env shadowing the credentials FILE. A dir
// without a readable file degrades to the file path (no token key).
func TestClaudeForfaitEnv_ExportsOAuthTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-TESTTOKEN","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got := claudeForfaitEnv(dir, false)
	assertForfaitEnv(t, got, dir)
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-TESTTOKEN" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN: got %q, want the file's accessToken", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}

	// No file → no token key, file path preserved.
	bare := claudeForfaitEnv(t.TempDir(), false)
	if _, present := bare["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN must be absent when no credentials file is present: %v", bare)
	}
}

func TestAnthropicCredEnv_HintAnthropicClearsStaleZAIEnv(t *testing.T) {
	resetClaudeCredEnv(t)
	// Simulate a stale parent-shell env where z.ai vars are already set
	// — the hint must actively unset them so the CLI subprocess inherits
	// only what we want.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leftover-zai")
	got := anthropicCredEnvForCLI(context.Background(), "anthropic", false)
	if got["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want '' (must clear stale value)", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want '' (must clear stale value)", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// --- hint: zai ------------------------------------------------------

func TestAnthropicCredEnv_HintZAIForcesEvenWithAnthropicCtx(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
		secrets.ProviderZAI:       "zai-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "zai", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want zai-test (hint zai pins z.ai routing)", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, present := got["ANTHROPIC_API_KEY"]; present {
		t.Errorf("ANTHROPIC_API_KEY must NOT be set when hint forces z.ai")
	}
}

// TestAnthropicCredEnv_HintZAIFallsToEnvKey ensures the hint also
// works when only the process env carries ZAI_API_KEY (the common
// desktop case).
func TestAnthropicCredEnv_HintZAIFallsToEnvKey(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ZAI_API_KEY", "env-zai-test")
	got := anthropicCredEnvForCLI(context.Background(), "zai", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "env-zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want env-zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// --- provider fingerprint (cross-provider session fork guard) -------

func TestProviderFingerprint_FacadeBaseURL(t *testing.T) {
	got := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "redacted",
	})
	want := "facade:https://api.z.ai/api/anthropic"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProviderFingerprint_AnthropicDirect(t *testing.T) {
	got := providerFingerprint(map[string]string{"ANTHROPIC_API_KEY": "sk-..."})
	if got != "anthropic-direct" {
		t.Errorf("got %q, want anthropic-direct", got)
	}
}

func TestProviderFingerprint_AnthropicOAuth(t *testing.T) {
	got := providerFingerprint(map[string]string{"CLAUDE_CONFIG_DIR": "/some/dir"})
	if got != "anthropic-oauth" {
		t.Errorf("got %q, want anthropic-oauth", got)
	}
}

func TestProviderFingerprint_ClearedEnvIsAnthropic(t *testing.T) {
	// The providerHint==anthropic path actively clears BASE_URL +
	// AUTH_TOKEN so a stale z.ai value can't leak in. The fingerprint
	// must reflect "Anthropic-direct (env)" in that shape so a session
	// produced under it doesn't trigger a false cross-provider drop on
	// a follow-up node that lands in the same shape.
	got := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "",
		"ANTHROPIC_AUTH_TOKEN": "",
	})
	if got != "anthropic-env" {
		t.Errorf("got %q, want anthropic-env", got)
	}
}

func TestProviderFingerprint_DifferentFacadesDiffer(t *testing.T) {
	// Two facades on different gateways must not collide — the parent
	// session signature won't validate on the other gateway either.
	a := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"})
	b := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://other.proxy/anthropic"})
	if a == b {
		t.Errorf("facades on different hosts collided: both %q", a)
	}
}

func TestProviderFingerprint_DirectVsFacadeDiffer(t *testing.T) {
	direct := providerFingerprint(map[string]string{"ANTHROPIC_API_KEY": "sk-..."})
	facade := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "redacted",
	})
	if direct == facade {
		t.Errorf("anthropic-direct vs z.ai facade fingerprints collided: %q", direct)
	}
}

// --- shouldDropSessionFork (cross-provider fork guard) -----------

func TestShouldDropSessionFork_NotForking(t *testing.T) {
	// Bare resume (no fork) is always same-process continuation — no
	// drop, regardless of fingerprint state.
	for _, fp := range []string{"", "anthropic-direct", "facade:https://api.z.ai/api/anthropic"} {
		task := Task{SessionID: "s1", ForkSession: false, SessionFingerprint: fp}
		if drop, _ := shouldDropSessionFork(task, "anthropic-direct"); drop {
			t.Errorf("fp=%q: bare resume should not drop", fp)
		}
	}
}

func TestShouldDropSessionFork_EmptyParentLegacyDataDrops(t *testing.T) {
	// The actual production-observed scenario: detect_stack ran on an
	// older binary that did not stamp _session_fingerprint. The
	// downstream fork on a new (post-T2.3) binary would otherwise
	// proceed and 400 on the thinking-block signature. The conservative
	// drop is what the new policy enforces.
	task := Task{SessionID: "s1", ForkSession: true, SessionFingerprint: ""}
	drop, reason := shouldDropSessionFork(task, "anthropic-direct")
	if !drop {
		t.Fatal("expected drop on empty parent fingerprint")
	}
	if reason == "" {
		t.Error("expected a reason string")
	}
}

func TestShouldDropSessionFork_MatchingFingerprintKeepsFork(t *testing.T) {
	task := Task{SessionID: "s1", ForkSession: true, SessionFingerprint: "anthropic-direct"}
	if drop, _ := shouldDropSessionFork(task, "anthropic-direct"); drop {
		t.Error("matching fingerprints should NOT drop")
	}
}

func TestShouldDropSessionFork_MismatchDrops(t *testing.T) {
	task := Task{SessionID: "s1", ForkSession: true,
		SessionFingerprint: "facade:https://api.z.ai/api/anthropic"}
	if drop, _ := shouldDropSessionFork(task, "anthropic-direct"); !drop {
		t.Error("mismatch should drop")
	}
}

func TestShouldDropSessionFork_UnknownCurrentKeepsForkWithParentSet(t *testing.T) {
	// When the current provider is unresolved (env not wired) we
	// can't classify the request; keep the fork rather than drop
	// pre-emptively. If a mismatch exists it surfaces the same 400
	// either way — dropping wouldn't have helped.
	task := Task{SessionID: "s1", ForkSession: true, SessionFingerprint: "anthropic-direct"}
	if drop, _ := shouldDropSessionFork(task, ""); drop {
		t.Error("unknown current fingerprint should NOT trigger a drop when parent fingerprint is set")
	}
}

// Sandboxed forfait (ADR-082 Phase 3): the CLI runs inside a container
// where the host temp dir does not exist, so CLAUDE_CONFIG_DIR must be
// remapped to the in-sandbox seeded config dir — while the per-spawn
// CLAUDE_CODE_OAUTH_TOKEN is still read from the HOST file the runner's
// refresher keeps fresh.
func TestClaudeForfaitEnv_SandboxedRemapsConfigDir(t *testing.T) {
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-TESTTOKEN","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got := claudeForfaitEnv(dir, true)
	assertForfaitEnv(t, got, secrets.ClaudeCodeSandboxConfigDir)
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-TESTTOKEN" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN must still come from the HOST file: got %q", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// Host path unchanged when not sandboxed.
	host := claudeForfaitEnv(dir, false)
	assertForfaitEnv(t, host, dir)
}

// taskSandboxed must treat the noop passthrough as NOT sandboxed — its
// commands run on the host, where the host config dir is the right one.
func TestTaskSandboxed_NoopIsHost(t *testing.T) {
	if taskSandboxed(Task{}) {
		t.Error("nil sandbox must not be sandboxed")
	}
	if taskSandboxed(Task{Sandbox: noopLikeRun{}}) {
		t.Error("noop passthrough must not be sandboxed")
	}
	if !taskSandboxed(Task{Sandbox: k8sLikeRun{}}) {
		t.Error("a real driver must be sandboxed")
	}
}

type noopLikeRun struct{ sandbox.Run }

func (noopLikeRun) Driver() string { return "noop" }

type k8sLikeRun struct{ sandbox.Run }

func (k8sLikeRun) Driver() string { return "kubernetes" }
