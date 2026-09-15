package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// A mutant's revert restores HEAD. So uncommitted changes are DESTROYED during
// the run, and every figure the gate then reports describes a tree that stopped
// existing partway through the measurement.
//
// The harness has always known this and has always said it, word for word:
//
//	WORKSPACE NOT COMMITTED (N path(s): ...). Mutant reverts restore HEAD, so
//	these changes are destroyed during the run and the verdict below describes
//	a tree that never existed. Commit, then gate.
//
// It said it in a `note(report, ...)` — a string, with no machine-readable
// field beside it — so no term of either gate could read it, and a rite
// converged on exactly that state. The harness condemns this shape elsewhere in
// its own source: "a notice string is where debts go to hide".
//
// Measured before wiring it, on the last four rites: two of the three that
// converged had a clean tree. The term refuses no legitimate run; it fires on
// the pathological case and on that alone.
//
// These checks drive the REAL conjunction — parsed out of main.bot and
// evaluated by the engine's own expression evaluator — rather than matching its
// text. That the term is carried by the STANDALONE gate too is not asserted
// here: TestGoldenMasterStandaloneRunnerCarriesTheGraphGatesTerms compares the
// two gates field by field, which is the repo's mechanism for that leg.
func TestGoldenMasterGateRefusesATreeThatWasNotTheTree(t *testing.T) {
	ast := convergedAST(t)

	if ok := evalConverged(t, ast, dirtyReport()); !ok {
		t.Fatalf("the conjunction refuses a rite whose tree was COMMITTED: " +
			"the term is refusing legitimate runs, not the pathological case")
	}

	if ok := evalConverged(t, ast, dirtyReport("src/main/java/Foo.java")); ok {
		t.Errorf("the conjunction ACCEPTS a rite that judged an UNCOMMITTED tree: " +
			"mutant reverts restore HEAD, so those changes were destroyed mid-run and every " +
			"figure above describes a tree that never existed. The harness says so in a notice " +
			"and nothing reads it. Add `length(outputs.oracle_run.workspace_dirty) == 0` to `converged`.")
	}
}

// The term must be what does the work. Removing that one clause has to hand the
// dirty report a green — otherwise the check above passes for some unrelated
// reason and would keep passing the day the term is deleted.
func TestGoldenMasterWorkspaceDirtyTermIsWhatRefusesTheDirtyTree(t *testing.T) {
	const term = " && length(outputs.oracle_run.workspace_dirty) == 0"
	src := convergedExpr(t, readHarnessFile(t, "golden-master/main.bot"))
	if !strings.Contains(src, term) {
		t.Fatalf("the workspace term is not in the conjunction, so this falsification has nothing to remove: %s", src)
	}
	without, err := expr.Parse(exprSrc(t, strings.Replace(src, term, "", 1)))
	if err != nil {
		t.Fatalf("parsing the conjunction without its workspace term: %v", err)
	}
	if ok := evalConverged(t, without, dirtyReport("src/main/java/Foo.java")); !ok {
		t.Errorf("with the workspace term removed, the uncommitted tree is STILL refused: " +
			"something else in the conjunction is doing the refusing, and the previous test would stay " +
			"green if the term were deleted. Find what refuses, and pin that instead.")
	}
}

// dirtyReport satisfies every other term of the conjunction — held-out pair
// included, at the 7/7 of a rite that really converged — so the only thing
// under test is the paths the caller passes.
func dirtyReport(paths ...string) map[string]any {
	r := convergedReport(7, 7)
	dirty := make([]any, 0, len(paths))
	for _, p := range paths {
		dirty = append(dirty, p)
	}
	r["workspace_dirty"] = dirty
	return r
}
