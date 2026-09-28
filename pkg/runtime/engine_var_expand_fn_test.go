package runtime

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// engineSuppliedVarFns is the table varExpandFn dispatches through — the
// one list of names answered from engine state — and
// ir.EngineSuppliedVarNames is the launch-time fallback screen's copy
// (a var whose text consults one reads UNDECIDED there, the screen having
// no engine state). The two are one fact, pinned in BOTH directions: a
// name added to the table without its ir twin, or listed in ir without a
// table entry, reddens this test.
func TestVarExpandFnEngineSuppliedNamesMatchTheScreenList(t *testing.T) {
	e := &Engine{workDir: "/wd", repoRoot: "/repo"}
	fn := e.varExpandFn()
	for name := range engineSuppliedVarFns {
		if !slices.Contains(ir.EngineSuppliedVarNames, name) {
			t.Errorf("varExpandFn answers %q from engine state, but ir.EngineSuppliedVarNames does not list it — the launch screen would read it decided-empty where dispatch reads engine state", name)
		}
	}
	for _, name := range ir.EngineSuppliedVarNames {
		if _, ok := engineSuppliedVarFns[name]; !ok {
			t.Errorf("ir.EngineSuppliedVarNames lists %q, but varExpandFn does not answer it — the screen reads it undecided where dispatch reads the process environment", name)
		}
		t.Setenv(name, "/env-lie")
		if got := fn(name); got == "/env-lie" {
			t.Errorf("varExpandFn(%q) read the process environment, but the launch screen treats the name as engine-supplied", name)
		}
	}
	// An unlisted name must fall through to the process environment.
	t.Setenv("C1606_NOT_ENGINE_SUPPLIED", "from-env")
	if got := fn("C1606_NOT_ENGINE_SUPPLIED"); got != "from-env" {
		t.Errorf("varExpandFn answered an unlisted name from engine state (%q) — it belongs in the table and in ir.EngineSuppliedVarNames", got)
	}
}
