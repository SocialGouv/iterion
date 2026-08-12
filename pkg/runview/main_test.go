package runview

import (
	"os"
	"testing"
)

// TestMain opts the whole package out of the product default `sandbox: auto`.
//
// Launch is a product entry point, so a bot with no `sandbox:` block resolves
// to `auto` and demands a container runtime. Ten test files here launch runs,
// and LaunchSpec carries no per-call override, so annotating them one at a time
// is how the next one gets missed — which is exactly what happened on the first
// pass of this fix: silencing one test surfaced two more behind it.
//
// Left implicit, these tests pass on a developer's Docker-equipped host and
// fail wherever there is none, including inside a container — which is where
// iterion's own bots run, and why four dependency PRs were held on 2026-08-12
// over `build/tests not green` that no bump had caused.
//
// Nothing here asserts how the sandbox default resolves; the sandbox-related
// tests in this package cover container reaping and the k8s reconcile no-op.
// A test that ever does assert it must set the variable itself rather than
// inherit this.
func TestMain(m *testing.M) {
	if err := os.Setenv("ITERION_SANDBOX_DEFAULT", "none"); err != nil {
		panic("runview tests: cannot opt out of sandbox: auto: " + err.Error())
	}
	os.Exit(m.Run())
}
