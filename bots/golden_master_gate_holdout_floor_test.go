package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// `holdout_detected == holdout_total` is the gate's strongest clause and it is
// satisfied by `0 == 0`. A rite whose held-out set is empty therefore converges
// on a term that measured nothing — the "no-op confirmed as a success" this bot
// exists to refuse in other people's deliveries.
//
// The harness already distinguishes three ways to reach an empty set and writes
// a field for each (`holdout_spent_unreplaced`, `holdout_awaiting_gate`,
// `holdout_sealed_uncommitted`). None is declared in `schema oracle_output`, so
// C031 makes a graph term over any of them impossible to write: they were
// computed and unwired, which is how thirteen spent cycles each landed
// reporting `held-out detected 0/0`.
//
// These checks drive the REAL conjunction — parsed out of main.bot and
// evaluated by the engine's own expression evaluator — rather than matching its
// text. A bench that greps for a spelling certifies the spelling: it stays green
// against any rewrite that keeps the words and loses the meaning, and goes red
// against a rename that changes nothing.
func TestGoldenMasterGateRefusesAVacuousHeldOutTerm(t *testing.T) {
	ast := convergedAST(t)

	// Every other term satisfied, so the only thing under test is the held-out
	// pair. Figures taken from a rite that really converged (7 drawn, 7 seen).
	if ok := evalConverged(t, ast, convergedReport(7, 7)); !ok {
		t.Fatalf("the conjunction refuses a rite that converged for real (held-out 7/7): " +
			"the floor is refusing legitimate runs, not vacuous ones")
	}

	if ok := evalConverged(t, ast, convergedReport(0, 0)); ok {
		t.Errorf("the conjunction ACCEPTS a rite whose held-out set is EMPTY (0/0): " +
			"`holdout_detected == holdout_total` is true by having nothing to compare, so the gate's " +
			"strongest term certifies a measurement that never happened. Add " +
			"`outputs.oracle_run.holdout_total > 0` to `converged`.")
	}
}

// The floor must be what does the work. Removing that one term has to hand the
// vacuous report a green — otherwise the check above passes for some unrelated
// reason and would keep passing the day the floor is deleted.
func TestGoldenMasterHeldOutFloorIsWhatRefusesTheVacuousReport(t *testing.T) {
	const floor = " && outputs.oracle_run.holdout_total > 0"
	src := convergedExpr(t, readHarnessFile(t, "golden-master/main.bot"))
	if !strings.Contains(src, floor) {
		t.Fatalf("the floor term is not in the conjunction, so this falsification has nothing to remove: %s", src)
	}
	without, err := expr.Parse(exprSrc(t, strings.Replace(src, floor, "", 1)))
	if err != nil {
		t.Fatalf("parsing the conjunction without its floor: %v", err)
	}
	if ok := evalConverged(t, without, convergedReport(0, 0)); !ok {
		t.Errorf("with the floor removed, the vacuous 0/0 report is STILL refused: " +
			"something else in the conjunction is doing the refusing, and the previous test would stay " +
			"green if the floor were deleted. Find what refuses, and pin that instead.")
	}
}

// convergedReport is a report that satisfies every term of the conjunction but
// the held-out pair, which the caller sets.
func convergedReport(detected, total int64) map[string]any {
	return map[string]any{
		"stable":                    true,
		"noop_silent":               true,
		"revert_clean":              true,
		"collateral":                int64(0),
		"unstable_controls":         []any{},
		"uncontrolled":              []any{},
		"blind_lanes":               []any{},
		"missing_archetypes":        []any{},
		"duplicate_groups_unproven": []any{},
		"corpus_distinct":           int64(42),
		"runner_replayable":         true,
		"holdout_reused":            []any{},
		"score_pct":                 int64(100),
		"holdout_detected":          detected,
		"holdout_total":             total,
	}
}

// convergedAST parses the conjunction main.bot actually ships.
func convergedAST(t *testing.T) *expr.AST {
	t.Helper()
	ast, err := expr.Parse(exprSrc(t, convergedExpr(t, readHarnessFile(t, "golden-master/main.bot"))))
	if err != nil {
		t.Fatalf("the shipped `converged:` conjunction does not parse: %v", err)
	}
	return ast
}

// exprSrc strips the `converged:` key and the surrounding quotes off the line,
// leaving the expression source the engine compiles.
func exprSrc(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, `"`)
	j := strings.LastIndex(line, `"`)
	if i < 0 || j <= i {
		t.Fatalf("the conjunction line carries no quoted expression: %q", line)
	}
	return line[i+1 : j]
}

// evalConverged resolves `outputs.oracle_run.<field>` against the report and
// `vars.<name>` against the bot's declared defaults — the two namespaces the
// conjunction reads. An unknown path returns nil, which the evaluator treats as
// absent, so a term reading a field this fixture forgot fails loudly here rather
// than passing on a default.
func evalConverged(t *testing.T, ast *expr.AST, report map[string]any) bool {
	t.Helper()
	vars := map[string]any{"mutation_floor": int64(90), "min_corpus": int64(25)}
	ok, err := ast.EvalBool(&expr.Context{
		Outputs: func(path []string) any {
			if len(path) != 2 || path[0] != "oracle_run" {
				return nil
			}
			return report[path[1]]
		},
		Vars: func(path []string) any {
			if len(path) != 1 {
				return nil
			}
			return vars[path[0]]
		},
	})
	if err != nil {
		t.Fatalf("evaluating the conjunction: %v", err)
	}
	return ok
}
