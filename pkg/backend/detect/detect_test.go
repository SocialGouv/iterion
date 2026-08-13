package detect

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
		// xAI Grok is a first-class claw provider (XAI_API_KEY); scrub so a
		// host with a real key does not flip claw Available unexpectedly.
		"XAI_API_KEY", "XAI_BASE_URL",
		// OpenAI ChatGPT-forfait preference overrides are detection inputs.
		"ITERION_OPENAI_USE_OAUTH",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"ITERION_PI_BIN", "PI_CODING_AGENT_DIR", "ITERION_PI_AGENT_DIR",
		"HOME",
	} {
		t.Setenv(k, "")
	}
	// Use a fresh empty HOME so pre-existing ~/.claude / ~/.codex on the dev
	// machine don't leak into tests.
	t.Setenv("HOME", t.TempDir())
	stubBinary(t, &findClaudeBinary, "")
	stubBinary(t, &findCodexBinary, "")
	// pi ships on some dev machines and not others; without this the pi
	// hints differ per host.
	stubBinary(t, &findPiBinary, "")
	// The macOS Keychain probe shells out to /usr/bin/security and would
	// read the dev machine's real Claude Code login on darwin — stub it to
	// "absent" so detection is deterministic on every host. Tests that
	// exercise the keychain path re-stub it with a source label.
	stubSource(t, &claudeKeychainOAuthSource, "")
	// Likewise prevent real `claude auth status` subprocess calls; tests that
	// need web-auth detection stub this explicitly.
	stubClaudeAuthStatus(t, false)
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

// stubClaudeAuthStatus swaps claudeAuthStatusFn for the duration of the test.
func stubClaudeAuthStatus(t *testing.T, loggedIn bool) {
	t.Helper()
	prev := claudeAuthStatusFn
	claudeAuthStatusFn = func(_ context.Context, _ string) bool { return loggedIn }
	t.Cleanup(func() { claudeAuthStatusFn = prev })
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

func TestDetect_XAIProvider(t *testing.T) {
	isolateEnv(t)

	// Absent key → xai listed but unavailable.
	r := Detect(context.Background())
	xai := findProvider(t, r, "xai")
	if xai.Available {
		t.Fatalf("xai Available=true with no XAI_API_KEY")
	}
	if xai.SuggestedModel != "xai/grok-3" {
		t.Fatalf("xai suggested = %q, want xai/grok-3", xai.SuggestedModel)
	}

	// Present key → available, source name only (no secret leak), and
	// claw flips to available so auto-resolve can land on it.
	t.Setenv("XAI_API_KEY", "xai-test-secret")
	r = Detect(context.Background())
	xai = findProvider(t, r, "xai")
	if !xai.Available {
		t.Fatal("xai Available=false with XAI_API_KEY set")
	}
	if xai.Source != "XAI_API_KEY" {
		t.Fatalf("xai Source = %q, want XAI_API_KEY", xai.Source)
	}
	claw := findBackend(t, r, BackendClaw)
	if !claw.Available {
		t.Fatal("claw should be available when XAI_API_KEY is set")
	}
	if r.ResolvedDefault != BackendClaw {
		t.Fatalf("ResolvedDefault = %q, want claw", r.ResolvedDefault)
	}
	// Never leak the key value into the report fields.
	if strings.Contains(xai.Source, "xai-test-secret") {
		t.Fatal("detect report leaks XAI_API_KEY value")
	}
	for _, src := range claw.Sources {
		if strings.Contains(src, "xai-test-secret") {
			t.Fatal("claw sources leak XAI_API_KEY value")
		}
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

func TestDetect_ClaudeCodeWebAuth(t *testing.T) {
	// Binary present, no credentials file, but `claude auth status` reports
	// loggedIn=true. Covers the newer claude.ai OAuth (Max/Pro) that stores
	// tokens in the OS keychain rather than ~/.claude/.credentials.json.
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	stubClaudeAuthStatus(t, true)

	r := Detect(context.Background())

	st := findBackend(t, r, BackendClaudeCode)
	if !st.Available {
		t.Fatalf("claude_code should be available via auth status fallback")
	}
	if st.Auth != AuthOAuth {
		t.Fatalf("claude_code auth = %q, want %q", st.Auth, AuthOAuth)
	}
	if r.ResolvedDefault != BackendClaudeCode {
		t.Fatalf("ResolvedDefault = %q, want claude_code", r.ResolvedDefault)
	}
}

func TestDetect_ClaudeCodeWebAuth_NotLoggedIn(t *testing.T) {
	// Binary present, no credentials file, auth status says not logged in.
	// claude_code must remain unavailable.
	isolateEnv(t)
	stubBinary(t, &findClaudeBinary, "/fake/claude")
	stubClaudeAuthStatus(t, false)

	r := Detect(context.Background())

	st := findBackend(t, r, BackendClaudeCode)
	if st.Available {
		t.Fatalf("claude_code should NOT be available when not logged in")
	}
	if len(st.Hints) == 0 {
		t.Fatalf("expected hints when claude_code unavailable, got none")
	}
}

// `claude auth status` reports loggedIn=true even when the only credential is
// an ANTHROPIC_API_KEY (authMethod=api_key). That path must NOT count as OAuth
// forfait, or any API-key host would wrongly auto-resolve to claude_code over
// claw. Only authMethod=claude.ai qualifies.
func TestClaudeAuthStatusIsForfait(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"forfait claude.ai", `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`, true},
		{"api key not forfait", `{"loggedIn":true,"authMethod":"api_key","apiKeySource":"ANTHROPIC_API_KEY"}`, false},
		{"logged out", `{"loggedIn":false,"authMethod":"none"}`, false},
		{"empty authMethod", `{"loggedIn":true}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeAuthStatusIsForfait([]byte(tc.out)); got != tc.want {
				t.Fatalf("claudeAuthStatusIsForfait(%s) = %v, want %v", tc.out, got, tc.want)
			}
		})
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

func findProvider(t *testing.T, r Report, name string) ProviderStatus {
	t.Helper()
	for _, p := range r.Providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q missing from report", name)
	return ProviderStatus{}
}

// pi is reported so the studio can show it and ITERION_BACKEND_PREFERENCE=pi
// can resolve — auto-selection filters on what Detect reports, so without an
// entry the variable was inert. It must still never be auto-SELECTED.
func TestDetectPi(t *testing.T) {
	find := func(r Report, name string) BackendStatus {
		t.Helper()
		for _, b := range r.Backends {
			if b.Name == name {
				return b
			}
		}
		t.Fatalf("backend %q absent from the report — the studio cannot show it", name)
		return BackendStatus{}
	}

	t.Run("absent binary", func(t *testing.T) {
		isolateEnv(t)
		st := find(Detect(context.Background()), BackendPi)
		if st.Available {
			t.Error("available with no pi binary")
		}
	})

	t.Run("binary but no credential", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		st := find(Detect(context.Background()), BackendPi)
		if st.Available {
			t.Errorf("available with no credential (sources %v) — every call would fail", st.Sources)
		}
		if len(st.Hints) == 0 {
			t.Error("no hint explaining what is missing")
		}
	})

	// A fresh pi install writes `{}`. Treating existence as a login would
	// advertise a backend that fails on its first call.
	t.Run("empty pi credential store is not a login", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PI_CODING_AGENT_DIR", dir)
		if find(Detect(context.Background()), BackendPi).Available {
			t.Error("an empty auth.json was read as a login")
		}
	})

	// The entry's own `type` decides the reported kind: pi's store holds both
	// shapes, and the studio renders this as an auth badge.
	for name, tc := range map[string]struct {
		store string
		want  string
	}{
		"an api-key login is reported as api_key": {`{"zai":{"type":"api_key","key":"k"}}`, AuthAPIKey},
		"an oauth login is reported as oauth":     {`{"anthropic":{"type":"oauth","access":"a"}}`, AuthOAuth},
		"oauth wins when both are present":        {`{"zai":{"type":"api_key"},"anthropic":{"type":"oauth"}}`, AuthOAuth},
	} {
		t.Run("pi's own login: "+name, func(t *testing.T) {
			isolateEnv(t)
			stubBinary(t, &findPiBinary, "/fake/pi")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(tc.store), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PI_CODING_AGENT_DIR", dir)
			st := find(Detect(context.Background()), BackendPi)
			if !st.Available || st.Auth != tc.want {
				t.Errorf("status = %+v, want available with auth=%s", st, tc.want)
			}
		})
	}

	// The provider keys pi reads are the ones iterion already probes for claw.
	t.Run("a provider key is enough", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		t.Setenv("OPENAI_API_KEY", "sk-test")
		if !find(Detect(context.Background()), BackendPi).Available {
			t.Error("not available despite OPENAI_API_KEY — pi reads that variable")
		}
	})

	// iterion bridges this into pi's OAuth-only openai-codex provider, so it
	// is a genuine credential for pi even though pi cannot read it itself.
	t.Run("a ChatGPT-Codex login counts", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"),
			[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","account_id":"acct-1"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)
		st := find(Detect(context.Background()), BackendPi)
		if !st.Available {
			t.Errorf("status = %+v, want available — iterion bridges this credential", st)
		}
	})

	// detect must not be more optimistic than the bridge that consumes the
	// credential: a chatgpt-mode blob without the tokens the bridge requires
	// made detect report available while piCodexSeed stepped aside, and the
	// node died with "No API key found for openai-codex".
	t.Run("a chatgpt blob the bridge would refuse does not count", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"),
			[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)
		if find(Detect(context.Background()), BackendPi).Available {
			t.Error("available on a blob with no account_id — the bridge refuses it, so the node fails")
		}
	})

	t.Run("an api-key Codex login does not count", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"),
			[]byte(`{"auth_mode":"apikey"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)
		if find(Detect(context.Background()), BackendPi).Available {
			t.Error("an api-key Codex login was counted; the bridge requires chatgpt mode")
		}
	})

	// Reporting pi must not change which backend an existing workflow gets.
	t.Run("never auto-selected", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		t.Setenv("OPENAI_API_KEY", "sk-test")
		r := Detect(context.Background())
		if slices.Contains(r.PreferenceOrder, BackendPi) {
			t.Errorf("PreferenceOrder = %v, must not contain pi", r.PreferenceOrder)
		}
		if r.ResolvedDefault == BackendPi {
			t.Error("pi was auto-selected — every existing empty-backend workflow would change")
		}
	})
}

// bedrock and vertex are marked available on AWS_REGION / GOOGLE_CLOUD_PROJECT
// alone — variables an AWS host or CI runner sets by default — and pi reads
// neither AWS nor GCP application-default credentials. Counting them reported
// pi available where every call fails, and since auto-selection filters on this
// report, ITERION_BACKEND_PREFERENCE=pi would then resolve there.
func TestDetectPiIgnoresProvidersItCannotRead(t *testing.T) {
	find := func(r Report) BackendStatus {
		t.Helper()
		for _, b := range r.Backends {
			if b.Name == BackendPi {
				return b
			}
		}
		t.Fatal("pi absent from the report")
		return BackendStatus{}
	}

	for name, env := range map[string]string{
		"bedrock via AWS_REGION":          "AWS_REGION",
		"vertex via GOOGLE_CLOUD_PROJECT": "GOOGLE_CLOUD_PROJECT",
	} {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			stubBinary(t, &findPiBinary, "/fake/pi")
			t.Setenv(env, "somewhere")
			st := find(Detect(context.Background()))
			if st.Available {
				t.Errorf("pi available on %s (sources %v) — it cannot read that credential, "+
					"so every call fails and the preference variable resolves here", env, st.Sources)
			}
		})
	}

	// A key pi DOES read still counts.
	t.Run("a readable provider still counts", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("OPENAI_API_KEY", "sk-test")
		if !find(Detect(context.Background())).Available {
			t.Error("not available despite OPENAI_API_KEY")
		}
	})
}

// A typo'd or stale ITERION_PI_BIN must not make detection report pi as
// present: availability feeds auto-selection, so the run would resolve to a
// backend that dies at exec.
//
// Deliberately does NOT call isolateEnv — that stubs findPiBinary, and this
// test is about the real probe's own behaviour.
func TestFindPiBinaryValidatesTheOverride(t *testing.T) {
	t.Run("a nonexistent override is not reported as found", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		t.Setenv("ITERION_PI_BIN", missing)
		if path, ok := findPiBinary(); ok && path == missing {
			t.Error("a nonexistent ITERION_PI_BIN was reported as found; the run would die at exec")
		}
	})

	t.Run("a real override is found, trimmed", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "pi")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_PI_BIN", "  "+bin+"  ")
		path, ok := findPiBinary()
		if !ok || path != bin {
			t.Errorf("path=%q ok=%v, want the trimmed override — execution trims the same variable", path, ok)
		}
	})
}

// A probe must not be more optimistic than what will actually use the
// credential. Two ways detectPi was:
func TestDetectPiMatchesWhatTheRunWillRead(t *testing.T) {
	find := func(r Report) BackendStatus {
		t.Helper()
		for _, b := range r.Backends {
			if b.Name == BackendPi {
				return b
			}
		}
		t.Fatal("pi absent from the report")
		return BackendStatus{}
	}

	// piResolveEnv pins PI_CODING_AGENT_DIR from ITERION_PI_AGENT_DIR, so
	// probing only the former read a store the node never opens.
	t.Run("ITERION_PI_AGENT_DIR is the dir the run will use", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		pinned := t.TempDir()
		if err := os.WriteFile(filepath.Join(pinned, "auth.json"),
			[]byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_PI_AGENT_DIR", pinned)
		if st := find(Detect(context.Background())); !st.Available || st.Auth != AuthOAuth {
			t.Errorf("status = %+v, want the pinned store to count", st)
		}

		// And an empty pinned store must NOT be rescued by ~/.pi/agent.
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		empty := t.TempDir()
		if err := os.WriteFile(filepath.Join(empty, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_PI_AGENT_DIR", empty)
		if find(Detect(context.Background())).Available {
			t.Error("available from a store the node will not read")
		}
	})

	// On a ChatGPT-only host the openai provider is available with a FILE label,
	// not a variable name. Counting it listed the same credential twice and left
	// the row reading api_key, while implying pi's `openai` provider could use a
	// file only the openai-codex bridge can.
	t.Run("a ChatGPT-only host reports one oauth source", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findPiBinary, "/fake/pi")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"),
			[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","account_id":"acct"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)

		st := find(Detect(context.Background()))
		if !st.Available {
			t.Fatalf("status = %+v, want available", st)
		}
		if st.Auth != AuthOAuth {
			t.Errorf("auth = %q, want oauth — the only credential is a ChatGPT plan", st.Auth)
		}
		if len(st.Sources) != 1 {
			t.Errorf("sources = %v, want one — the same credential was listed twice", st.Sources)
		}
	})
}
