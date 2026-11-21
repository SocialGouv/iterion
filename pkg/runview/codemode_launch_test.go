package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
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
		ToolCalls: 256, ConcurrentToolCalls: 3,
		Wall:   90 * time.Second,
		Memory: 1 << 30,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--codemode off", "--codemode-tool-calls 256", "--codemode-concurrent-tool-calls 3", "--codemode-wall 1m30s", "--codemode-memory 1073741824"} {
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

	if args := appendCodemodeArgs(nil, "", &ir.CodemodeBounds{}); len(args) != 0 {
		t.Errorf("zero bounds forwarded %v, want inheritance", args)
	}

	// A launch that expressed no codemode forwards nothing at all.
	if args := appendCodemodeArgs(nil, "", nil); len(args) != 0 {
		t.Errorf("an empty launch forwarded %v, want nothing — the subprocess must inherit the DSL", args)
	}
}

// TestLaunchWarningsNameTheCompileTimeTwins — the launch can produce both
// shapes the compiler warns about (C348 bounds beside an off, C347 above the
// ceiling). The warnings judge the EFFECTIVE mode — the launch's choice
// where it made one (normalized: "OFF" and " off " warn like "off"), else
// the authored ladder over the whole workflow — because bounds set for a
// workflow where no program can ever run are inert whichever rung decided
// the off, and a ceiling warning about bounds no child will ever carry is
// noise. The runtime still enforces the truth, so the launch warns rather
// than refuses, and each warning names its compile-time twin so a reader
// can follow the thread.
func TestLaunchWarningsNameTheCompileTimeTwins(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"off", "OFF", " off "} {
		warnings := codemodeLaunchWarnings(nil, spelling, &ir.CodemodeBounds{ConcurrentToolCalls: 3})
		if len(warnings) != 1 || !strings.Contains(warnings[0], "C348") {
			t.Errorf("bounds beside a run-level off (%q) warned %+v, want exactly one warning naming C348", spelling, warnings)
		}
	}
	warnings := codemodeLaunchWarnings(nil, "off", nil)
	if len(warnings) != 0 {
		t.Errorf("an off with no bounds warned %+v, want silence", warnings)
	}
	warnings = codemodeLaunchWarnings(nil, "on", &ir.CodemodeBounds{ToolCalls: 100000})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "C347") {
		t.Errorf("bounds above the ceiling warned %+v, want exactly one warning naming C347", warnings)
	}
	warnings = codemodeLaunchWarnings(nil, "on", &ir.CodemodeBounds{ToolCalls: 256})
	if len(warnings) != 0 {
		t.Errorf("in-bounds bounds warned %+v, want silence", warnings)
	}
}

// TestTheLaunchWarningResolvesTheAuthoredMode — an empty launch mode
// inherits the authored ladder, and the warnings must judge what will
// ACTUALLY run: bounds set beside a bot whose author turned codemode off
// everywhere are inert (found by the adversarial round — the first draft
// keyed the warning on the launch's own spelling alone), and above-ceiling
// bounds on such a bot do not get the ceiling warning, because no child will
// ever carry them.
func TestTheLaunchWarningResolvesTheAuthoredMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "authored-off.bot")
	src := `
agent a:
  model: "m"
  codemode: off

workflow w:
  entry: a
  a -> done
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, err := CompileWorkflow(path, "", store.RunTrustDefault)
	if err != nil {
		t.Fatalf("CompileWorkflow: %v", err)
	}
	warnings := codemodeLaunchWarnings(wf, "", &ir.CodemodeBounds{ToolCalls: 4096})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "C348") {
		t.Errorf("launch bounds beside an authored off warned %+v, want exactly one warning naming C348", warnings)
	}
	warnings = codemodeLaunchWarnings(wf, "", &ir.CodemodeBounds{ToolCalls: 1 << 40})
	if len(warnings) != 1 || strings.Contains(warnings[0], "C347") {
		t.Errorf("above-ceiling bounds on an authored-off bot warned %+v, want only the inert warning", warnings)
	}
	// A launch ON beats the authored off: the bounds are live again.
	warnings = codemodeLaunchWarnings(wf, "on", &ir.CodemodeBounds{ToolCalls: 4096})
	if len(warnings) != 0 {
		t.Errorf("a launch on over an authored off warned %+v, want silence — the bounds are live", warnings)
	}
}
