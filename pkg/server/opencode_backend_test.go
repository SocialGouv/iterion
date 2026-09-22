package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestOpenCodeEffortCapabilities: the compiler says opencode carries a
// reasoning-effort dial (C177), so the studio's picker must not 400 on it —
// the two would otherwise contradict each other for a legal workflow.
func TestOpenCodeEffortCapabilities(t *testing.T) {
	_, hs := newTestServer(t)

	got := getEffortCaps(t, hs.URL, "opencode", "anthropic/claude-sonnet-4-6")
	if got.Source != "opencode-variant" {
		t.Errorf("Source=%q, want %q", got.Source, "opencode-variant")
	}
	// Only the levels iterion passes through verbatim. xhigh and ultracode
	// collapse onto high before argv, so offering them would promise a level
	// the backend silently substitutes.
	assertEffortLevels(t, got.Supported,
		[]string{"low", "medium", "high", "max"}, // required
		[]string{"xhigh", "ultracode"},           // forbidden
	)
	// opencode sends no --variant when none is set: the model's own default
	// applies and iterion has nothing to name.
	if got.Default != "" {
		t.Errorf("Default=%q, want empty — no documented default", got.Default)
	}
}

// TestOpenCodeIsALaunchOverrideBackend: a backend missing from the launch
// map is 400'd on every model_overrides entry naming it.
func TestOpenCodeIsALaunchOverrideBackend(t *testing.T) {
	if err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "a", Backend: "opencode", Model: "anthropic/claude-sonnet-4-6"},
	}); err != nil {
		t.Fatalf("opencode rejected as a launch override backend: %v", err)
	}
	// Control: a name that is not a backend still fails, so the assertion
	// above is not a test that cannot fail.
	if err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "a", Backend: "opencade", Model: "m"},
	}); err == nil {
		t.Fatal("a misspelled backend was accepted")
	}
}
