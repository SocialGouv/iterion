package runview

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The launch-surface witnesses of the run-level codemode override (#3345):
// the detached forwarding (the subprocess's ONLY carrier of the launch's
// choice) and the launch warnings (the runtime twins of C348 and C347).

// TestDetachedForwardingCarriesTheLaunchsCodemode — a detached subprocess
// re-resolves the knob from scratch, so anything appendCodemodeArgs does not
// pass is replaced by the workflow's own value: for `off` that would run a
// capability the operator asked away.
func TestDetachedForwardingCarriesTheLaunchsCodemode(t *testing.T) {
	t.Parallel()
	args := appendCodemodeArgs(nil, "off", &ir.CodemodeBounds{
		ToolCalls: 256,
		Wall:      90 * time.Second,
		Memory:    1 << 30,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--codemode off", "--codemode-tool-calls 256", "--codemode-wall 1m30s", "--codemode-memory 1073741824"} {
		if !strings.Contains(joined, want) {
			t.Errorf("detached args %q miss %q — the subprocess would not know", joined, want)
		}
	}
	// Fields the launch did not set forward nothing: the authored value
	// must keep standing, not a zero that Clamp would read as "nobody chose"
	// (which happens to be right here, but the CPU line would lie about
	// having been chosen).
	for _, banned := range []string{"--codemode-cpu", "--codemode-script-bytes", "--codemode-result-bytes", "--codemode-tool-result-bytes"} {
		if strings.Contains(joined, banned) {
			t.Errorf("detached args %q carry %s — the launch did not choose it", joined, banned)
		}
	}

	// A launch that expressed no codemode forwards nothing at all.
	if args := appendCodemodeArgs(nil, "", nil); len(args) != 0 {
		t.Errorf("an empty launch forwarded %v, want nothing — the subprocess must inherit the DSL", args)
	}
}

// TestLaunchWarningsNameTheCompileTimeTwins — the launch can produce both
// shapes the compiler warns about (C348 bounds beside off, C347 above the
// ceiling). The runtime still enforces the truth, so the launch warns rather
// than refuses, and each warning names its compile-time twin so a reader can
// follow the thread.
func TestLaunchWarningsNameTheCompileTimeTwins(t *testing.T) {
	t.Parallel()
	warnings := codemodeLaunchWarnings("off", &ir.CodemodeBounds{ToolCalls: 256})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "C348") {
		t.Errorf("bounds beside a run-level off warned %+v, want exactly one warning naming C348", warnings)
	}
	warnings = codemodeLaunchWarnings("off", nil)
	if len(warnings) != 0 {
		t.Errorf("an off with no bounds warned %+v, want silence", warnings)
	}
	warnings = codemodeLaunchWarnings("on", &ir.CodemodeBounds{ToolCalls: 100000})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "C347") {
		t.Errorf("bounds above the ceiling warned %+v, want exactly one warning naming C347", warnings)
	}
	warnings = codemodeLaunchWarnings("on", &ir.CodemodeBounds{ToolCalls: 256})
	if len(warnings) != 0 {
		t.Errorf("in-bounds bounds warned %+v, want silence", warnings)
	}
}
