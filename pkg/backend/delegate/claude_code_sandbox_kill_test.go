package delegate

import (
	"strings"
	"testing"
)

// The wrapper must (a) record the PID before exec, (b) exec the ORIGINAL
// argv untouched so the stream-json stdin protocol and flags survive, and
// (c) stay POSIX-sh (no bashisms) since sandbox images run dash/busybox sh.
func TestWrapSandboxDelegateArgv(t *testing.T) {
	argv := []string{"claude", "--print", "--output-format", "stream-json"}
	got := wrapSandboxDelegateArgv("campaign-0-123", argv)

	if got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("wrapper must run through sh -c, got %v", got[:2])
	}
	script := got[2]
	if !strings.Contains(script, sandboxDelegatePIDFile("campaign-0-123")) {
		t.Errorf("script %q does not reference the pidfile", script)
	}
	if !strings.Contains(script, "echo $$ >") || !strings.Contains(script, `exec "$@"`) {
		t.Errorf("script %q must write $$ then exec \"$@\" (PID-preserving)", script)
	}
	// $0 placeholder then the untouched original argv.
	rest := got[4:]
	if len(rest) != len(argv) {
		t.Fatalf("original argv length changed: got %v", rest)
	}
	for i := range argv {
		if rest[i] != argv[i] {
			t.Errorf("argv[%d] = %q, want %q (must pass through untouched)", i, rest[i], argv[i])
		}
	}
}

func TestSandboxDelegateMark_SanitizesNodeID(t *testing.T) {
	m := sandboxDelegateMark(Task{NodeID: "P2_Campaign/α", Iteration: 3})
	if strings.ContainsAny(m, " /_αA") {
		t.Errorf("mark %q must be lowercase [a-z0-9-] only", m)
	}
	// "P2_Campaign/α" sanitizes rune-for-rune to "p2-campaign--" ('_', '/'
	// and the non-ASCII rune each map to '-'), then "-<iteration>-" joins.
	if !strings.HasPrefix(m, "p2-campaign---3-") {
		t.Errorf("mark %q must embed the sanitized node id + iteration", m)
	}
	// Distinct invocations must not collide (nanosecond suffix).
	if m2 := sandboxDelegateMark(Task{NodeID: "P2_Campaign/α", Iteration: 3}); m2 == m {
		t.Errorf("two marks for the same node collided: %q", m)
	}
}
