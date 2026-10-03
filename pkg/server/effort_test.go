package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestEffortCapabilities_ClawOpus48 proves the endpoint returns the full
// Anthropic Opus 4.8 effort matrix AND the "ultracode" mode that only
// exists for that model. This is what makes it worth having a test:
// the ultracode tag is a hand-rolled amendment made by handleEffortCapabilities
// on top of the claw-registry data. If someone removed the append the
// studio's ultracode picker would silently disappear, and only a test
// that inspects the matrix would catch it.
func TestEffortCapabilities_ClawOpus48(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "claw", "claude-opus-4-8")
	if got.Source != "claw-registry" {
		t.Errorf("Source=%q, want %q", got.Source, "claw-registry")
	}
	if got.Default != "high" {
		t.Errorf("Default=%q, want %q", got.Default, "high")
	}
	// The floor levels must all be present. We assert on the set, not a
	// slice ordering, so a future registry re-order does not turn a
	// harmless change into a red test.
	assertEffortLevels(t, got.Supported,
		[]string{"low", "medium", "high", "xhigh", "max", "ultracode"}, // required
		nil, // forbidden
	)
}

// Opus 5 carries ultracode's orchestration half too: the endpoint offers the
// mode there, through the same predicate the compiler's C089 gate uses.
func TestEffortCapabilities_ClawOpus5(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "claw", "claude-opus-5")
	if got.Source != "claw-registry" {
		t.Errorf("Source=%q, want %q", got.Source, "claw-registry")
	}
	// Only the mode is asserted here: the level table (low … max, default)
	// for the Claude 5 family is the claw registry's to ship
	// (apikit.EffortCapabilities returns no levels for claude-opus-5 at the
	// pinned claw-code-go revision) — a gap of that registry, not of this
	// endpoint, tracked on claw-code-go.
	assertEffortLevels(t, got.Supported,
		[]string{"ultracode"}, // required
		nil,                   // forbidden
	)
}

// TestEffortCapabilities_ClaudeCodeMirrorsClaw proves the claude_code
// backend routes through the same claw-registry data as claw itself
// (they share apikit.EffortCapabilities). If a regression tied
// claude_code to a static fallback list the ultracode mode would vanish
// for the studio's default backend, and this test would catch it.
func TestEffortCapabilities_ClaudeCodeMirrorsClaw(t *testing.T) {
	_, hs := newTestServer(t)

	cc := getEffortCaps(t, hs.URL, "claude_code", "claude-opus-4-8")
	cw := getEffortCaps(t, hs.URL, "claw", "claude-opus-4-8")

	if cc.Source != cw.Source {
		t.Errorf("Source diverges: claude_code=%q claw=%q", cc.Source, cw.Source)
	}
	if cc.Default != cw.Default {
		t.Errorf("Default diverges: claude_code=%q claw=%q", cc.Default, cw.Default)
	}
	if !sameStringSet(cc.Supported, cw.Supported) {
		t.Errorf("Supported diverges:\nclaude_code=%v\nclaw       =%v", cc.Supported, cw.Supported)
	}
}

// TestEffortCapabilities_UltracodeOnlyOnOpus48 proves the ultracode
// append is conditioned on the model, not tacked onto every Anthropic
// entry. Opus 4.7 has the same floor levels but must NOT carry
// ultracode — the mid-conversation system-message plumbing that backs
// it only ships on 4.8.
func TestEffortCapabilities_UltracodeNotOnOpus47(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "claw", "claude-opus-4-7")
	assertEffortLevels(t, got.Supported,
		[]string{"low", "medium", "high", "xhigh", "max"}, // required
		[]string{"ultracode"},                             // forbidden
	)
}

// TestEffortCapabilities_ClawOpenAI proves the OpenAI matrix comes
// through unchanged: minimal/low/medium/high, medium default, and
// crucially NO ultracode (that mode does not exist for OpenAI models,
// so the handler's ResolveModelAlias check must return false).
func TestEffortCapabilities_ClawOpenAI(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "claw", "gpt-5.5")
	if got.Source != "claw-registry" {
		t.Errorf("Source=%q, want %q", got.Source, "claw-registry")
	}
	if got.Default != "medium" {
		t.Errorf("Default=%q, want %q", got.Default, "medium")
	}
	assertEffortLevels(t, got.Supported,
		[]string{"minimal", "low", "medium", "high"}, // required
		[]string{"ultracode", "xhigh", "max"},        // forbidden
	)
}

// TestEffortCapabilities_GPT6SolLunaCarryNone proves the GPT-6 effort
// matrices come through with the split the provider documents: Sol and
// Luna accept reasoning_effort `none`, Astra does not. The studio picker
// reads this list verbatim, so a registry regression that dropped none
// would silently remove the level from the only models that honour it.
func TestEffortCapabilities_GPT6SolLunaCarryNone(t *testing.T) {
	_, hs := newTestServer(t)

	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		got := getEffortCaps(t, hs.URL, "claw", model)
		assertEffortLevels(t, got.Supported,
			[]string{"none"},      // required
			[]string{"ultracode"}, // forbidden
		)
	}
	got := getEffortCaps(t, hs.URL, "claw", "gpt-6-astra")
	assertEffortLevels(t, got.Supported,
		nil,              // required
		[]string{"none"}, // forbidden
	)
}

// TestCodexEffortFallbackGatesNone pins the static matrix emitted when the
// Codex CLI is unreachable: it mirrors the SDK's Effort constants every
// codex model accepts, and offers none ONLY for the models the claw
// registry (the catalogue of record) marks as none-carriers — promising it
// for every codex model would sell a level the CLI refuses at run time.
func TestCodexEffortFallbackGatesNone(t *testing.T) {
	assertEffortLevels(t, codexEffortFallback,
		[]string{"low", "medium", "high", "max"}, // required
		[]string{"none", "ultracode", "xhigh"},   // forbidden
	)
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna", "openai/gpt-6-sol"} {
		assertEffortLevels(t, codexEffortFallbackFor(model),
			[]string{"none", "low", "medium", "high", "max"}, // required
			nil, // forbidden
		)
	}
	for _, model := range []string{"gpt-6-astra", "gpt-5.5", "totally-unknown-model"} {
		assertEffortLevels(t, codexEffortFallbackFor(model),
			[]string{"low", "medium", "high", "max"}, // required
			[]string{"none"},                         // forbidden
		)
	}
}

// TestEffortCapabilities_Pi proves the pi backend returns its static
// model-independent matrix — the levels iterion can express, dropped
// down from pi's full off|minimal|low|medium|high|xhigh|max dial. This
// path never touches the model registry, so a regression that consulted
// apikit for a pi model would degrade the picker silently on pi runs.
func TestEffortCapabilities_Pi(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "pi", "any-model-name")
	if got.Source != "pi-thinking" {
		t.Errorf("Source=%q, want %q", got.Source, "pi-thinking")
	}
	if got.Default != "medium" {
		t.Errorf("Default=%q, want %q", got.Default, "medium")
	}
	assertEffortLevels(t, got.Supported,
		[]string{"none", "low", "medium", "high", "xhigh", "max"},
		nil,
	)

	// Model-independence: swapping the model must not change the shape.
	other := getEffortCaps(t, hs.URL, "pi", "some/other-model")
	if !sameStringSet(got.Supported, other.Supported) || got.Default != other.Default || got.Source != other.Source {
		t.Errorf("pi response is not model-independent:\nfirst =%+v\nsecond=%+v", got, other)
	}
}

// TestEffortCapabilities_CompilerParity proves the picker serves every
// backend the compiler credits with a reasoning-effort dial
// (ir.EffortDialBackends, the set C177 screens against): a 400 on one of
// them would contradict the compiler for a legal workflow — the bug grok
// shipped with. kimi is the control: it has no dial on either side, so it
// must keep 400ing (both sides agree).
func TestEffortCapabilities_CompilerParity(t *testing.T) {
	_, hs := newTestServer(t)
	// codex is in the dial set and its arm queries the CLI when the cache
	// is cold; seed an empty live list so the test never spawns a process.
	seedCodexModelCache(t, nil)

	for _, backend := range ir.EffortDialBackends() {
		got := getEffortCaps(t, hs.URL, backend, "claude-opus-4-8")
		if len(got.Supported) == 0 {
			t.Errorf("backend %q: Supported is empty — the picker must offer at least one level for a dialled backend", backend)
		}
	}

	resp, err := http.Get(hs.URL + "/api/effort-capabilities?backend=kimi&model=kimi-for-coding")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("kimi: status=%d, want 400 — kimi has no dial on either side; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
}

// TestEffortCapabilities_UnknownBackend proves the endpoint rejects an
// unknown backend name with 400 rather than falling back to a default.
// The studio picker relies on the 400 to hide the field when the
// (backend, model) pair is not supported.
func TestEffortCapabilities_UnknownBackend(t *testing.T) {
	_, hs := newTestServer(t)

	resp, err := http.Get(hs.URL + "/api/effort-capabilities?backend=noSuchBackend&model=claude-opus-4-8")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
}

// TestEffortCapabilities_MissingParams proves both required query
// parameters are enforced. The endpoint historically 500'd when model
// was omitted; a 400 is the guard that keeps this regression closed.
func TestEffortCapabilities_MissingParams(t *testing.T) {
	_, hs := newTestServer(t)

	for _, tc := range []struct {
		name, url string
	}{
		{"missing backend", hs.URL + "/api/effort-capabilities?model=claude-opus-4-8"},
		{"missing model", hs.URL + "/api/effort-capabilities?backend=claw"},
		{"both missing", hs.URL + "/api/effort-capabilities"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", resp.StatusCode, mustReadBody(t, resp))
			}
		})
	}
}

// TestEffortCapabilities_UnknownModel proves the endpoint returns a
// clean shape (no supported levels, no default, source still claw)
// rather than a 5xx when the model is not in the registry. The studio
// treats an empty Supported list as "hide the reasoning_effort field",
// so the field's emptiness is load-bearing.
func TestEffortCapabilities_UnknownModel(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "claw", "totally-made-up-model-2999")

	if got.Source != "claw-registry" {
		t.Errorf("Source=%q, want %q", got.Source, "claw-registry")
	}
	if len(got.Supported) != 0 {
		t.Errorf("Supported=%v, want empty for an unknown model", got.Supported)
	}
	if got.Default != "" {
		t.Errorf("Default=%q, want empty for an unknown model", got.Default)
	}
}

// TestResolveEffort_LiteralPassthrough proves a non-env literal comes
// back unchanged. This is the studio canvas's read path for a plain
// "reasoning_effort: high" DSL field.
func TestResolveEffort_LiteralPassthrough(t *testing.T) {
	_, hs := newTestServer(t)

	got := getResolveEffort(t, hs.URL, "high")
	if got.Resolved != "high" {
		t.Errorf("Resolved=%q, want %q", got.Resolved, "high")
	}
}

// TestResolveEffort_EnvSubstitutionExpands proves ${VAR:-default}
// forms are expanded server-side when VAR is unset, using the fallback.
// The literal "${_ITERION_EFFORT_TEST_UNSET:-max}" must resolve to
// "max" — the whole point of the endpoint.
func TestResolveEffort_EnvSubstitutionExpands(t *testing.T) {
	_, hs := newTestServer(t)

	// Guarantee the fallback path by explicitly clearing the var. The
	// underscore prefix keeps it distinct from any real host env var.
	t.Setenv("_ITERION_EFFORT_TEST_UNSET", "")

	got := getResolveEffort(t, hs.URL, "${_ITERION_EFFORT_TEST_UNSET:-max}")
	if got.Resolved != "max" {
		t.Errorf("Resolved=%q, want %q (env fallback should expand)", got.Resolved, "max")
	}
}

// TestResolveEffort_EnvSubstitutionUsesSetValue proves the endpoint
// reads the process env, not just the fallback. A regression that hard-
// coded the fallback path would fail this — the set value must win.
func TestResolveEffort_EnvSubstitutionUsesSetValue(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_EFFORT_TEST_SET", "xhigh")

	got := getResolveEffort(t, hs.URL, "${_ITERION_EFFORT_TEST_SET:-low}")
	if got.Resolved != "xhigh" {
		t.Errorf("Resolved=%q, want %q (env value should win over fallback)", got.Resolved, "xhigh")
	}
}

// TestResolveEffort_EnvExpandsToNone proves an env expansion to `none`
// survives resolution instead of being erased to "" — the regression
// from issue #1837, where ResolveEffortLiteral rejected a level the
// DSL did not yet enumerate and the studio canvas showed no effort at
// all for a node that explicitly asked for none.
func TestResolveEffort_EnvExpandsToNone(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_EFFORT_TEST_NONE", "none")

	got := getResolveEffort(t, hs.URL, "${_ITERION_EFFORT_TEST_NONE:-low}")
	if got.Resolved != "none" {
		t.Errorf("Resolved=%q, want %q (a resolved none must not be erased)", got.Resolved, "none")
	}
}

// TestResolveEffort_InvalidExpansionYieldsEmpty proves that a literal
// whose expansion is not a valid effort value comes back as "". The
// studio then falls back to displaying the raw literal. A regression
// that returned the raw expansion would let a garbage value flow into
// a runtime that later rejects it 500 nodes deep.
func TestResolveEffort_InvalidExpansionYieldsEmpty(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_EFFORT_TEST_UNSET2", "")

	got := getResolveEffort(t, hs.URL, "${_ITERION_EFFORT_TEST_UNSET2:-not-a-real-effort}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (invalid expansion)", got.Resolved)
	}
}

// TestResolveEffort_EmptyLiteral proves the endpoint accepts an empty
// query param without erroring, and returns "" — the studio uses the
// empty response to decide "no explicit effort declared".
func TestResolveEffort_EmptyLiteral(t *testing.T) {
	_, hs := newTestServer(t)

	got := getResolveEffort(t, hs.URL, "")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty for empty literal", got.Resolved)
	}
}

// TestResolveModel_EnvSubstitutionExpands proves ${VAR:-default}
// forms are expanded server-side when VAR is unset, using the fallback.
func TestResolveModel_EnvSubstitutionExpands(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_MODEL_TEST_UNSET", "")

	got := getResolveModel(t, hs.URL, "${_ITERION_MODEL_TEST_UNSET:-openai-codex/gpt-5.6-sol}")
	if got.Resolved != "openai-codex/gpt-5.6-sol" {
		t.Errorf("Resolved=%q, want %q (env fallback should expand)", got.Resolved, "openai-codex/gpt-5.6-sol")
	}
}

// TestResolveModel_EnvSubstitutionUsesSetValue proves the endpoint
// reads the process env, not just the fallback — the case that lets
// the studio canvas tell sol from terra from luna.
func TestResolveModel_EnvSubstitutionUsesSetValue(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_MODEL_TEST_SET", "openai-codex/gpt-5.6-terra")

	got := getResolveModel(t, hs.URL, "${_ITERION_MODEL_TEST_SET:-openai-codex/gpt-5.6-sol}")
	if got.Resolved != "openai-codex/gpt-5.6-terra" {
		t.Errorf("Resolved=%q, want %q (env value should win over fallback)", got.Resolved, "openai-codex/gpt-5.6-terra")
	}
}

// TestResolveModel_InvalidExpansionYieldsEmpty proves a literal whose
// expansion is not a model spec comes back as "". The studio then
// falls back to displaying the authored default / compact env-ref
// rather than leaking process env.
func TestResolveModel_InvalidExpansionYieldsEmpty(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("_ITERION_MODEL_TEST_HOME", "/home/nobody")

	got := getResolveModel(t, hs.URL, "${_ITERION_MODEL_TEST_HOME}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (path is not a model spec)", got.Resolved)
	}
}

func TestResolveModel_EmptyLiteral(t *testing.T) {
	_, hs := newTestServer(t)

	got := getResolveModel(t, hs.URL, "")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty for empty literal", got.Resolved)
	}
}

func TestResolveModel_APIKeyEnvIsNotAnOracle(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345")
	got := getResolveModel(t, hs.URL, "${ANTHROPIC_API_KEY}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (API key env must not leak)", got.Resolved)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	got = getResolveModel(t, hs.URL, "${GITHUB_TOKEN}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (GITHUB_TOKEN must not leak)", got.Resolved)
	}

	t.Setenv("SOME_OTHER_VAR", "gpt-5.6-sol")
	got = getResolveModel(t, hs.URL, "${SOME_OTHER_VAR}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (name gate, not shape)", got.Resolved)
	}
}

func TestResolveModel_CredentialNamedModelVarIsNotAnOracle(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("LITELLM_MODEL_API_KEY", "gpt-5.6-sol")
	got := getResolveModel(t, hs.URL, "${LITELLM_MODEL_API_KEY}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (MODEL+KEY is a credential)", got.Resolved)
	}

	t.Setenv("OPENROUTER_MODEL_KEY", "gpt-5.6-sol")
	got = getResolveModel(t, hs.URL, "${OPENROUTER_MODEL_KEY}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (MODEL+KEY is a credential)", got.Resolved)
	}
}

func TestResolveModel_NestedEnvIsNotAnOracle(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("X_MODEL", "ANTHROPIC_API_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345")
	got := getResolveModel(t, hs.URL, "${${X_MODEL}}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (nested env form)", got.Resolved)
	}

	got = getResolveModel(t, hs.URL, "${$X_MODEL}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (nested $X form)", got.Resolved)
	}
}

func TestResolveModel_HyphenatedSecretIsNotAnOracle(t *testing.T) {
	_, hs := newTestServer(t)

	t.Setenv("GITHUB_TOKEN", "xoxb-1234-abcd-efghijkl")
	got := getResolveModel(t, hs.URL, "${GITHUB_TOKEN}")
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty (hyphenated secret must not leak)", got.Resolved)
	}
}

func TestResolveModel_LiteralTooLongYieldsEmpty(t *testing.T) {
	_, hs := newTestServer(t)

	got := getResolveModel(t, hs.URL, strings.Repeat("a", maxResolveLiteralBytes+1))
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty for literal over %d bytes", got.Resolved, maxResolveLiteralBytes)
	}
}

func TestResolveEffort_LiteralTooLongYieldsEmpty(t *testing.T) {
	_, hs := newTestServer(t)

	got := getResolveEffort(t, hs.URL, strings.Repeat("a", maxResolveLiteralBytes+1))
	if got.Resolved != "" {
		t.Errorf("Resolved=%q, want empty for literal over %d bytes", got.Resolved, maxResolveLiteralBytes)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func getEffortCaps(t *testing.T, base, backend, model string) effortCapabilitiesResponse {
	t.Helper()
	q := "backend=" + backend + "&model=" + model
	resp, err := http.Get(base + "/api/effort-capabilities?" + q)
	if err != nil {
		t.Fatalf("GET effort-capabilities: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out effortCapabilitiesResponse
	decodeJSONResp(t, resp, &out)
	return out
}

func getResolveEffort(t *testing.T, base, literal string) resolveEffortResponse {
	t.Helper()
	// Build the URL by hand — url.QueryEscape would hide bugs in how
	// the handler parses the literal, which is exactly what we want to
	// exercise.
	req, err := http.NewRequest(http.MethodGet, base+"/api/resolve-effort", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	q := req.URL.Query()
	q.Set("literal", literal)
	req.URL.RawQuery = q.Encode()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET resolve-effort: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out resolveEffortResponse
	decodeJSONResp(t, resp, &out)
	return out
}

func getResolveModel(t *testing.T, base, literal string) resolveModelResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/resolve-model", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	q := req.URL.Query()
	q.Set("literal", literal)
	req.URL.RawQuery = q.Encode()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET resolve-model: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out resolveModelResponse
	decodeJSONResp(t, resp, &out)
	return out
}

// assertEffortLevels checks required is a subset of got and forbidden
// disjoint from got. Order is not asserted (a re-order in the registry
// is not a contract break).
func assertEffortLevels(t *testing.T, got, required, forbidden []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, s := range got {
		set[s] = true
	}
	for _, r := range required {
		if !set[r] {
			t.Errorf("missing required level %q in Supported=%v", r, got)
		}
	}
	for _, f := range forbidden {
		if set[f] {
			t.Errorf("forbidden level %q present in Supported=%v", f, got)
		}
	}
}

// sameStringSet reports whether a and b contain the same strings,
// ignoring order.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// seedCodexModelCache installs a fake Codex model/list response so
// codexCapabilities exercises its live-list path without spawning the CLI.
func seedCodexModelCache(t *testing.T, models []codexsdk.ModelInfo) {
	t.Helper()
	codexCacheMu.Lock()
	codexCache = &codexCacheEntry{models: models, fetchedAt: time.Now()}
	codexCacheMu.Unlock()
	t.Cleanup(func() {
		codexCacheMu.Lock()
		codexCache = nil
		codexCacheMu.Unlock()
	})
}

// TestCodexCapabilities_LiveListOffersNoneForCarriers reproduces the
// live-list vs fallback divergence: codex 0.156.1's model/list reports
// low/medium/high/max even for gpt-6-sol, yet the runtime accepts none
// for Sol/Luna (and 400s it for Astra). The endpoint must agree with the
// runtime truth on BOTH paths — a picker that offers none only when the
// CLI is down is lying whenever the CLI is up.
func TestCodexCapabilities_LiveListOffersNoneForCarriers(t *testing.T) {
	cliList := []codexsdk.ModelInfo{
		{
			ID:    "gpt-6-sol",
			Model: "gpt-6-sol",
			// What codex 0.156.1 actually reports — no none.
			SupportedReasoningEfforts: []codexsdk.ReasoningEffortOption{
				{Value: "low"}, {Value: "medium"}, {Value: "high"}, {Value: "max"},
			},
			DefaultReasoningEffort: "medium",
		},
		{
			ID:    "gpt-6-astra",
			Model: "gpt-6-astra",
			SupportedReasoningEfforts: []codexsdk.ReasoningEffortOption{
				{Value: "low"}, {Value: "medium"}, {Value: "high"}, {Value: "max"},
			},
			DefaultReasoningEffort: "medium",
		},
	}
	seedCodexModelCache(t, cliList)

	sol, err := codexCapabilities(context.Background(), "gpt-6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if sol.Source != "codex-cli" {
		t.Fatalf("Source=%q, want codex-cli (live path)", sol.Source)
	}
	assertEffortLevels(t, sol.Supported,
		[]string{"none", "low", "medium", "high", "max"}, // required
		nil, // forbidden
	)

	astra, err := codexCapabilities(context.Background(), "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	assertEffortLevels(t, astra.Supported,
		[]string{"low", "medium", "high", "max"}, // required
		[]string{"none"},                         // forbidden: the runtime 400s it
	)
}

// A claw node names its model as a routing spec; the endpoint answers for
// it what it answers for the bare id — the levels the runtime clamps the
// node's effort to. Red when the spec itself is looked up in the registry,
// which knows no "anthropic/…" id and answers nothing.
func TestEffortCapabilities_ClawSpecReadsLikeItsBareID(t *testing.T) {
	_, hs := newTestServer(t)
	bare := getEffortCaps(t, hs.URL, "claw", "claude-opus-5-5")
	if len(bare.Supported) == 0 {
		t.Fatal("fixture: the registry no longer knows claude-opus-5-5's effort levels")
	}
	spec := getEffortCaps(t, hs.URL, "claw", "anthropic/claude-opus-5-5")
	if strings.Join(spec.Supported, ",") != strings.Join(bare.Supported, ",") || spec.Default != bare.Default {
		t.Errorf("anthropic/claude-opus-5-5 = %v (default %q), want %v (default %q)", spec.Supported, spec.Default, bare.Supported, bare.Default)
	}
}

// claude_code reads a node's spec like claw does: on its capability id.
func TestEffortCapabilities_ClaudeCodeSpecReadsLikeItsBareID(t *testing.T) {
	_, hs := newTestServer(t)
	bare := getEffortCaps(t, hs.URL, "claude_code", "claude-opus-5-5")
	spec := getEffortCaps(t, hs.URL, "claude_code", "anthropic/claude-opus-5-5")
	if len(bare.Supported) == 0 || strings.Join(spec.Supported, ",") != strings.Join(bare.Supported, ",") {
		t.Errorf("anthropic/claude-opus-5-5 = %v, want %v", spec.Supported, bare.Supported)
	}
}

// ultracode is offered on the model as written — the compiler's C089
// predicate — so an env-substituted spec keeps it, and a gateway alias does
// not gain it, even spelled like a Claude model.
func TestEffortCapabilities_UltracodeFollowsTheCompiler(t *testing.T) {
	_, hs := newTestServer(t)
	has := func(levels []string) bool {
		for _, l := range levels {
			if l == "ultracode" {
				return true
			}
		}
		return false
	}
	if got := getEffortCaps(t, hs.URL, "claw", "${X_MODEL:-anthropic/claude-opus-4-8}"); !has(got.Supported) {
		t.Errorf("env-substituted Opus 4.8 spec lost ultracode: %v", got.Supported)
	}
	for _, m := range []string{"openai_compatible/gpt-oss-120b", "openai_compatible/claude-opus-5-5"} {
		if got := getEffortCaps(t, hs.URL, "claw", m); has(got.Supported) {
			t.Errorf("gateway model %s was offered ultracode: %v", m, got.Supported)
		}
	}
}
