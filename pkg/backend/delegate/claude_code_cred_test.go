package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
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
		"MOONSHOT_API_KEY",
		"MOONSHOT_BASE_URL",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_OAUTH_TOKEN",
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
	if v, present := got["ANTHROPIC_API_KEY"]; !present || v != "" {
		t.Errorf("ANTHROPIC_API_KEY must be present-and-empty when the z.ai key wins precedence (an inherited value would ride along): present=%v val=%q", present, v)
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
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-TESTTOKEN","refreshToken":"r","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got := claudeForfaitEnv(dir, false)
	assertForfaitEnv(t, got, dir)
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-TESTTOKEN" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN: got %q, want the file's accessToken", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}

	// No readable file → the key is still written, empty, for the same
	// suppression reason: we have pointed the CLI at a per-run config dir, so
	// an inherited platform token must not quietly serve in its place.
	bare := claudeForfaitEnv(t.TempDir(), false)
	if v, present := bare["CLAUDE_CODE_OAUTH_TOKEN"]; !present || v != "" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN must be present-and-empty with no credentials file: present=%v val=%q", present, v)
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
	if v, present := got["ANTHROPIC_API_KEY"]; !present || v != "" {
		t.Errorf("ANTHROPIC_API_KEY must be present-and-empty when the hint forces z.ai (an inherited value would ride along): present=%v val=%q", present, v)
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

// TestAnthropicCredEnv_HintZAINoKeySuppressesAmbientAnthropic pins the
// forbidden fallback of issue #1390: an unsatisfiable `provider: zai`
// hint (no z.ai key resolvable, from ctx or process env) must clear
// EVERY Anthropic-flavoured credential the CLI would otherwise
// inherit — the two z.ai vars, ANTHROPIC_API_KEY (the pod's shared
// key), CLAUDE_CODE_OAUTH_TOKEN (the runner's forfait env channel),
// AND CLAUDE_CONFIG_DIR (the forfait FILE channel: the CLI reads
// $CLAUDE_CONFIG_DIR/.credentials.json when no env token is set) —
// so the node fails with "no credential" instead of silently routing
// to Anthropic-direct and 404'ing on the GLM id.
//
// Mutation: drop any one entry (e.g. remove "CLAUDE_CONFIG_DIR": ""
// from the returned map), and this reddens on the assertion that
// it must be present-and-empty (mergeCmdEnv's suppression signal).
//
// The test exercises BOTH the host path (sandboxed=false) and the
// sandbox path (sandboxed=true), because the sandbox_secret_files
// path bakes CLAUDE_CONFIG_DIR into the container's spec.Env — a
// `provider: zai` node without a z.ai key that inherits the
// container-baked value would silently authenticate as the forfait
// tenant on Anthropic-direct.
func TestAnthropicCredEnv_HintZAINoKeySuppressesAmbientAnthropic(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		name := "host"
		if sandboxed {
			name = "sandboxed"
		}
		t.Run(name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			// The forbidden alternatives in this test: EVERY
			// Anthropic-flavoured credential a parent or container-baked
			// env could carry that a naive fallback would let win —
			// including the alt-provider switches (Bedrock, Vertex,
			// Foundry) that claw-code-go detectProvider reads BEFORE
			// ANTHROPIC_API_KEY.
			t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-anthropic")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oat-ambient-forfait")
			t.Setenv("CLAUDE_CONFIG_DIR", "/iterion/claude-forfait")
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
			t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
			t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "1")
			got := anthropicCredEnvForCLI(context.Background(), "zai", sandboxed)
			// Every secret / token / switch is cleared to "": the
			// signal mergeCmdEnv reads as "actively suppress".
			for _, k := range []string{
				"ANTHROPIC_BASE_URL",
				"ANTHROPIC_AUTH_TOKEN",
				"ANTHROPIC_API_KEY",
				"CLAUDE_CODE_OAUTH_TOKEN",
				"CLAUDE_CODE_USE_BEDROCK",
				"CLAUDE_CODE_USE_VERTEX",
				"CLAUDE_CODE_USE_FOUNDRY",
			} {
				v, present := got[k]
				if !present || v != "" {
					t.Errorf("%s must be present-and-empty (suppression signal): present=%v val=%q", k, present, v)
				}
			}
			// CLAUDE_CONFIG_DIR is the exception: an empty value is
			// treated as absent by mergeCmdEnv, and the CLI then
			// defaults to $HOME/.claude — on a dev laptop with
			// `claude login`, that resolves a valid forfait and
			// re-opens the leak. The fix points at a poisoned
			// absolute path so the CLI's read fails deterministically.
			cfg, present := got["CLAUDE_CONFIG_DIR"]
			if !present {
				t.Errorf("CLAUDE_CONFIG_DIR must be present (set to a poisoned path, not cleared)")
			}
			if cfg == "" {
				t.Errorf("CLAUDE_CONFIG_DIR must NOT be empty — an empty value degrades to $HOME/.claude and re-opens the forfait leak on a dev laptop")
			}
			if !strings.HasPrefix(cfg, "/") {
				t.Errorf("CLAUDE_CONFIG_DIR must be an absolute path, got %q", cfg)
			}
			// The poisoned path must not exist on any sane host, so
			// the CLI's `.credentials.json` read at that dir fails.
			if _, err := os.Stat(cfg); err == nil {
				t.Errorf("CLAUDE_CONFIG_DIR poisoned path %q ACTUALLY EXISTS on this host — pick another sentinel", cfg)
			}
			// The suppression marker travels with the map — every
			// iterion-internal reader of CLAUDE_CONFIG_DIR keys on
			// it to skip treating the poisoned dir as an OAuth
			// forfait (R0a39d6).
			if got[ForfaitSuppressedEnvKey] != "1" {
				t.Errorf("%s must be %q on a suppressed forfait, got %q", ForfaitSuppressedEnvKey, "1", got[ForfaitSuppressedEnvKey])
			}
		})
	}
}

// TestProviderFingerprint_SuppressedForfaitDoesNotReadAsOAuth pins the
// reader-side half of R0a39d6: the poisoned CLAUDE_CONFIG_DIR sentinel
// is NOT rendered as `anthropic-oauth` — a label that would persist
// into node output, cross-provider fork guards and usagecap Readings.
// The suppression marker on the env map switches the label to
// `anthropic-suppressed` so the sentinel never disguises itself as a
// real forfait.
//
// Mutation: drop the `isForfaitSuppressed(env)` guard from
// providerFingerprint's CLAUDE_CONFIG_DIR branch → the fingerprint
// reads `anthropic-oauth` on a poisoned dir → red.
func TestProviderFingerprint_SuppressedForfaitDoesNotReadAsOAuth(t *testing.T) {
	env := map[string]string{
		"CLAUDE_CONFIG_DIR":     suppressedForfaitDir,
		ForfaitSuppressedEnvKey: "1",
	}
	got := providerFingerprint(env)
	if got == "anthropic-oauth" {
		t.Fatalf("suppressed forfait must NOT render as anthropic-oauth (would persist into node output / fork guard); got %q", got)
	}
	if got != "anthropic-suppressed" {
		t.Errorf("suppressed forfait fingerprint = %q, want anthropic-suppressed", got)
	}
	// Sibling assertion: an UNSUPPRESSED CLAUDE_CONFIG_DIR keeps its
	// OAuth label. If we accidentally break the normal path this
	// reddens too.
	normal := providerFingerprint(map[string]string{"CLAUDE_CONFIG_DIR": "/home/dev/.claude"})
	if normal != "anthropic-oauth" {
		t.Errorf("a normal CLAUDE_CONFIG_DIR must still read as anthropic-oauth, got %q", normal)
	}
}

// TestSessionFilesRoot_SuppressedForfaitReturnsNoRoot pins the reader
// R0a39d6 fixed, at its final shape: a forfait-suppressed node (provider
// zai, no z.ai key) names NO session root at all — its CLI runs against
// the poisoned sentinel and dies on "no credential" before any
// transcript exists, and the ambient default would point pack/unpack/
// HasSession at the OPERATOR'S OWN config dir for a session the run
// never wrote. Every caller degrades on "" (ErrNotExist / false).
//
// Mutations seen red: fall through to the ambient default (the old
// behaviour) → red on the empty assertion; return "" for the NORMAL
// forfait too → red on the normal-path assertion.
func TestSessionFilesRoot_SuppressedForfaitReturnsNoRoot(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		resetClaudeCredEnv(t)
		_ = t.TempDir() // the ambient home the fall-through WOULD pick
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		task := Task{ProviderHint: "zai"}
		if sandboxed {
			task.Sandbox = stubSandboxRun{}
		}
		if got := SessionFilesRoot(context.Background(), task, BackendClaudeCode); got != "" {
			t.Fatalf("sandboxed=%v: SessionFilesRoot = %q for a suppressed node, want no root at all — no reader may pick a session root the suppressed CLI never writes to", sandboxed, got)
		}
	}

	// The forbidden alternative: "" for everyone. A NON-suppressed
	// node still resolves a root (its own CLAUDE_CONFIG_DIR when the
	// resolution carries one, the ambient/home fallback otherwise).
	resetClaudeCredEnv(t)
	task := Task{ProviderHint: "anthropic"}
	got := SessionFilesRoot(context.Background(), task, BackendClaudeCode)
	if got == "" {
		t.Fatalf("SessionFilesRoot = %q for a non-suppressed node, want the ambient/home root", got)
	}
	if got == suppressedForfaitDir || strings.Contains(got, "nonexistent") {
		t.Fatalf("normal forfait resolved to the suppression sentinel: %q", got)
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

// A base URL is operator input, so it can embed the credential it
// authenticates with. The fingerprint is persisted on run.json, the
// node output map and events.jsonl and read by any run-reader, and the
// secret guard cannot mask a token that never entered the secret
// plumbing — so the credential-bearing URL components must not survive
// into the label. It stays an equality key for session reuse all the
// same: stable per URL, and distinct across URLs.
func TestProviderFingerprint_FacadeBaseURLCarriesNoCredential(t *testing.T) {
	const secret = "sk-live-abcdef123456"
	cases := []struct {
		name, base string
	}{
		{"userinfo", "https://" + secret + "@api.z.ai/api/anthropic"},
		{"userinfo with password", "https://user:" + secret + "@api.z.ai/api/anthropic"},
		{"query", "https://api.z.ai/api/anthropic?api_key=" + secret},
		{"fragment", "https://api.z.ai/api/anthropic#" + secret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": tc.base})
			if strings.Contains(got, secret) {
				t.Fatalf("fingerprint leaks the credential: %q", got)
			}
			if !strings.HasPrefix(got, "facade:") {
				t.Errorf("got %q — the facade: prefix is what usage_cap.forSource keys on", got)
			}
			if !strings.Contains(got, "api.z.ai/api/anthropic") {
				t.Errorf("got %q — scheme+host+path is the readable half, it must survive", got)
			}
			// Stable: a second call on the same URL must render the
			// same label, or shouldDropSessionFork discards the parent
			// session on every single call.
			if again := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": tc.base}); again != got {
				t.Errorf("unstable: %q then %q", got, again)
			}
			// Non-colliding: the sanitized form must not collapse onto
			// the bare URL, or a session built on one is resumed on the
			// other and its signed thinking blocks 400.
			bare := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"})
			if got == bare {
				t.Errorf("collided with the credential-free URL: %q", got)
			}
		})
	}

	// Two URLs differing ONLY in the stripped part stay distinct.
	a := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic?api_key=one"})
	b := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic?api_key=two"})
	if a == b {
		t.Errorf("two distinct facades collapsed to one label: %q", a)
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
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-TESTTOKEN","refreshToken":"r","expiresAt":4102444800000,"scopes":["user:inference"]}}`
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

// The usage-source stamp: every reading leaving a session names the
// provider routing it ran on, so the runner's meter charges the refusal
// to the credential that was actually spent.
func TestStampUsageSource(t *testing.T) {
	var got usagecap.Reading
	hook := stampUsageSource(func(r usagecap.Reading) error { got = r; return nil }, "anthropic-direct")
	if err := hook(usagecap.Reading{Window: usagecap.WindowFrequency}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if got.Source != "anthropic-direct" {
		t.Fatalf("Source = %q, want the session fingerprint", got.Source)
	}
	// A reading that already names its source keeps it.
	if err := hook(usagecap.Reading{Source: "facade:x"}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if got.Source != "facade:x" {
		t.Fatalf("Source = %q, want the reading's own label preserved", got.Source)
	}
	if stampUsageSource(nil, "x") != nil {
		t.Fatal("no observer must stay no observer")
	}
}

// An EXPIRED credentials file must not export CLAUDE_CODE_OAUTH_TOKEN. That
// variable is the first-precedence headless auth path — the CLI reads it BEFORE
// the credentials file — so exporting a dead token would shadow the very file
// the CLI (or the runner's refresh worker) can still renew from. Dropping the
// key degrades to the file path, which is this resolver's documented fallback.
func TestClaudeForfaitEnv_SkipsExpiredOAuthToken(t *testing.T) {
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-STALE","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got := claudeForfaitEnv(dir, false)
	// PRESENT AND EMPTY, not absent. This variable is the CLI's
	// first-precedence auth path, so an absent key lets the pod's own ambient
	// CLAUDE_CODE_OAUTH_TOKEN — the PLATFORM forfait on a prod runner —
	// outrank the per-run CLAUDE_CONFIG_DIR we just pointed at, and a tenant
	// whose blob went stale would silently bill the platform's account. The
	// three ANTHROPIC_* siblings are cleared for exactly this reason.
	v, present := got["CLAUDE_CODE_OAUTH_TOKEN"]
	if !present || v != "" {
		t.Errorf("expired token must CLEAR the inherited one: present=%v val=%q", present, v)
	}
	// The file path itself still travels: the CLI reads and refreshes it.
	if got["CLAUDE_CONFIG_DIR"] != dir {
		t.Errorf("CLAUDE_CONFIG_DIR: got %q, want %q", got["CLAUDE_CONFIG_DIR"], dir)
	}
}

// TestSuppressedForfaitDir_IsNoPathASandboxSeedsOrMounts pins the
// suppression sentinel against the only claude config paths a sandbox
// backend ever materialises: the config dir seeded inside the container
// and the read-only credential mount it is seeded from. The sentinel
// reaches a sandboxed CLI as exec-time env only (sandbox.ExecOpts carries
// Env, WorkDir and the stdio, no mount), so the one way it could ever
// read as a real forfait inside a container is by COLLIDING with a path
// the sandbox seeds — this is what a sentinel equal to one of them turns
// red.
func TestSuppressedForfaitDir_IsNoPathASandboxSeedsOrMounts(t *testing.T) {
	resetClaudeCredEnv(t)
	if !filepath.IsAbs(suppressedForfaitDir) {
		t.Fatalf("suppressedForfaitDir = %q, want an absolute path (a relative one resolves against the CLI's cwd)", suppressedForfaitDir)
	}
	for _, seeded := range []string{
		secrets.ClaudeCodeSandboxConfigDir,
		filepath.Dir(secrets.ClaudeCodeOAuthSandboxMountPath),
		secrets.SecretFilesMountDir,
	} {
		if suppressedForfaitDir == seeded ||
			strings.HasPrefix(suppressedForfaitDir, seeded+"/") ||
			strings.HasPrefix(seeded, suppressedForfaitDir+"/") {
			t.Errorf("suppressedForfaitDir %q overlaps the sandbox-seeded path %q — a sandboxed `provider: zai` node without a z.ai key would read the real forfait", suppressedForfaitDir, seeded)
		}
	}
	env := anthropicCredEnvForCLI(context.Background(), "zai", true)
	if got := env["CLAUDE_CONFIG_DIR"]; got != suppressedForfaitDir {
		t.Fatalf("sandboxed zai/no-key CLAUDE_CONFIG_DIR = %q, want the sentinel %q", got, suppressedForfaitDir)
	}
}
