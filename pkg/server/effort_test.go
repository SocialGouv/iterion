package server

import (
	"net/http"
	"testing"
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
func TestEffortCapabilities_UltracodeOnlyOnOpus48(t *testing.T) {
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
		[]string{"low", "medium", "high", "xhigh", "max"},
		nil,
	)

	// Model-independence: swapping the model must not change the shape.
	other := getEffortCaps(t, hs.URL, "pi", "some/other-model")
	if !sameStringSet(got.Supported, other.Supported) || got.Default != other.Default || got.Source != other.Source {
		t.Errorf("pi response is not model-independent:\nfirst =%+v\nsecond=%+v", got, other)
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
