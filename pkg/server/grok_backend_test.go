package server

import "testing"

// TestGrokEffortCapabilities: the compiler says grok carries a
// reasoning-effort dial (C177 — grokMapEffort emits --reasoning-effort),
// so the studio's picker must not 400 on it — the two would otherwise
// contradict each other for a legal workflow. Same principle as
// TestOpenCodeEffortCapabilities.
func TestGrokEffortCapabilities(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "grok", "xai/grok-4.5")
	if got.Source != "grok-reasoning-effort" {
		t.Errorf("Source=%q, want %q", got.Source, "grok-reasoning-effort")
	}
	// Only the levels iterion passes through verbatim. ultracode collapses
	// onto high before argv, so offering it would promise a level the
	// backend silently substitutes.
	assertEffortLevels(t, got.Supported,
		[]string{"none", "low", "medium", "high", "xhigh", "max"}, // required
		[]string{"ultracode"}, // forbidden
	)
	// grok sends no --reasoning-effort when none is set: the CLI's own
	// default applies and iterion has nothing to name.
	if got.Default != "" {
		t.Errorf("Default=%q, want empty — no documented default", got.Default)
	}

	// Model-independence: the flag is the same for every grok model.
	other := getEffortCaps(t, hs.URL, "grok", "grok-3")
	if !sameStringSet(got.Supported, other.Supported) || got.Default != other.Default || got.Source != other.Source {
		t.Errorf("grok response is not model-independent:\nfirst =%+v\nsecond=%+v", got, other)
	}
}
