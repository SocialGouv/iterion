package ir

import (
	"os"
	"testing"
)

// The overlay is consulted before the process env on every expansion
// form, and its absence keeps the historical env-only behaviour —
// falsified both ways so the settings surface cannot silently detach
// from the ~30 call sites that rely on this one choke point.
func TestExpandEnvWithDefault_OverlayPrecedence(t *testing.T) {
	t.Setenv("ITERION_OVERLAY_T1", "from-env")
	defer SetEnvOverlay(nil)

	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_OVERLAY_T1" {
			return "from-setting", true
		}
		return "", false
	})
	for form, want := range map[string]string{
		"${ITERION_OVERLAY_T1}":                             "from-setting",
		"${ITERION_OVERLAY_T1:-fallback}":                   "from-setting",
		"$ITERION_OVERLAY_T1":                               "from-setting",
		"${ITERION_OVERLAY_MISS:-fallback}":                 "fallback",
		"${ITERION_OVERLAY_NEST:-${ITERION_OVERLAY_T1:-x}}": "from-setting",
	} {
		if got := ExpandEnvWithDefault(form); got != want {
			t.Errorf("%s = %q, want %q (setting must beat env)", form, got, want)
		}
	}

	// An overlay hit with an empty value counts as unset: env then wins.
	SetEnvOverlay(func(string) (string, bool) { return "", true })
	if got := ExpandEnvWithDefault("${ITERION_OVERLAY_T1:-fb}"); got != "from-env" {
		t.Errorf("empty overlay value = %q, want the env value (a setting cannot pin empty)", got)
	}

	SetEnvOverlay(nil)
	if got := ExpandEnvWithDefault("${ITERION_OVERLAY_T1:-fb}"); got != "from-env" {
		t.Errorf("nil overlay = %q, want byte-identical env-only behaviour", got)
	}
	os.Unsetenv("ITERION_OVERLAY_T1")
	if got := ExpandEnvWithDefault("${ITERION_OVERLAY_T1:-fb}"); got != "fb" {
		t.Errorf("unset everywhere = %q, want the .bot default", got)
	}
}
