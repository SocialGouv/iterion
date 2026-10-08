package worktreepool

import "testing"

// The runtime-ignored set derives from the canonical noise list
// (pkg/treenoise): a drifted devbox.lock and a hard-killed tool script's
// scratch are the engine's own writes, recognised like the mirror is.
// The protected unknowns stay protected — an ignored .env may belong to
// the operator.
func TestIsRuntimeIgnoredPathCoversTheCanonicalNoise(t *testing.T) {
	for path, want := range map[string]bool{
		".claude":               true,
		".claude/skills/x.md":   true,
		"devbox.lock":           true,
		".iterion-script-a1.sh": true,
		".gomodcache/x":         true,
		".devbox/y":             true,
		".tmp-run/z":            true,
		".env":                  false,
		"main.go":               false,
		"vendor/x/y.go":         false,
	} {
		if got := isRuntimeIgnoredPath(path); got != want {
			t.Errorf("isRuntimeIgnoredPath(%q) = %v, want %v", path, got, want)
		}
	}
}
