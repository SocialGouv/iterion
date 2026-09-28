package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// varExpandFn answers a handful of names from engine state (workDir,
// bundle, container workspace) rather than from any environment — and the
// launch-time fallback screen treats a var whose text consults one as
// UNDECIDED, because the screen has no engine state to read them from
// (ir.EngineSuppliedVarNames). The two lists are one fact: if varExpandFn
// answers a listed name from the process environment, or starts answering
// an unlisted name from engine state, the screen and dispatch diverge.
func TestVarExpandFnAnswersExactlyTheEngineSuppliedNames(t *testing.T) {
	e := &Engine{workDir: "/wd", repoRoot: "/repo"}
	fn := e.varExpandFn()
	for _, name := range ir.EngineSuppliedVarNames {
		t.Setenv(name, "/env-lie")
		if got := fn(name); got == "/env-lie" {
			t.Errorf("varExpandFn(%q) read the process environment, but the launch screen treats the name as engine-supplied", name)
		}
	}
	// The reverse direction: an unlisted name must fall through to the
	// process environment. (A NEW engine-supplied name added here must join
	// ir.EngineSuppliedVarNames in the same change — the twin comment on
	// varExpandFn says so; a function cannot enumerate its own specials.)
	t.Setenv("C1606_NOT_ENGINE_SUPPLIED", "from-env")
	if got := fn("C1606_NOT_ENGINE_SUPPLIED"); got != "from-env" {
		t.Errorf("varExpandFn answered an unlisted name from engine state (%q) — ir.EngineSuppliedVarNames must list it", got)
	}
}
