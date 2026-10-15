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

// remapRepoRoot is the other engine reading the launch-vars edge prune has to
// agree with: a value it REWRITES is not the value the run's own edges read,
// so the pre-flight must refuse to decide on it (ir.ValueHoldsAbsolutePath).
//
// Pinned as the guarantee rather than as a list of shapes: a shape added to
// one walk and not the other reddens here, whatever either enumeration looks
// like. The ir side is deliberately broader — it refuses every absolute path,
// not only the repo root, having neither path to compare — so only this
// direction is a defect.
func TestRemapRepoRootRewritesNothingTheEdgePruneDecidesOn(t *testing.T) {
	const repoRoot, projectDir = "/srv/repo", "/wt/run-1"
	for _, v := range []any{
		repoRoot,
		repoRoot + "/",
		"/srv/repo/./",
		"rel/x",
		"",
		true,
		int64(3),
		float64(1.5),
		nil,
		[]any{"rel/x", repoRoot},
		[]any{"rel/x", "rel/y"},
		[]any{[]any{repoRoot}},
		[]any{map[string]any{"dir": repoRoot}},
		map[string]any{"dir": repoRoot, "n": float64(1)},
		map[string]any{"dir": "rel/x"},
		map[string]any{"nested": []any{repoRoot}},
		// Shapes neither walk follows: the launch payload's decoding and
		// CoerceVarValue produce none of them, and both readings leave them
		// alone — so the prune may decide on them.
		[]string{repoRoot},
		map[string]string{"dir": repoRoot},
	} {
		_, rewritten := remapRepoRoot(v, repoRoot, projectDir)
		if rewritten && !ir.ValueHoldsAbsolutePath(v) {
			t.Errorf("remapRepoRoot rewrites %#v but the edge prune decides on it — the pre-flight would read a value the run does not", v)
		}
		if !rewritten && reflectDeepHoldsRoot(v, repoRoot) {
			t.Errorf("remapRepoRoot left %#v alone though it carries the repo root — this test's corpus no longer describes the walk", v)
		}
	}
	// The corpus must exercise both verdicts, or it proves nothing.
	var yes, no int
	for _, v := range []any{repoRoot, "rel/x"} {
		if _, rewritten := remapRepoRoot(v, repoRoot, projectDir); rewritten {
			yes++
		} else {
			no++
		}
	}
	if yes == 0 || no == 0 {
		t.Fatalf("corpus rewrote %d and left %d: it must do both", yes, no)
	}
}

// reflectDeepHoldsRoot reports whether v carries repoRoot in a shape
// remapRepoRoot walks — written independently of both readings, so a shape
// dropped from either one shows up as a disagreement rather than as silence.
func reflectDeepHoldsRoot(v any, repoRoot string) bool {
	switch val := v.(type) {
	case string:
		return samePath(val, repoRoot)
	case []any:
		for _, e := range val {
			if reflectDeepHoldsRoot(e, repoRoot) {
				return true
			}
		}
	case map[string]any:
		for _, e := range val {
			if reflectDeepHoldsRoot(e, repoRoot) {
				return true
			}
		}
	}
	return false
}
