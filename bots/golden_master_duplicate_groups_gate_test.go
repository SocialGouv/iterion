package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// oracleGateConverged returns the compiled `converged` expression of
// golden-master's oracle_gate. Read from the IR rather than grepped: a line
// scanner cannot tell a term of the conjunction from the same words in the
// comment above it.
func oracleGateConverged(t *testing.T) *expr.AST {
	t.Helper()
	wf := compileBot(t, "golden-master")
	if wf == nil {
		t.Fatal("golden-master does not compile")
	}
	gate, ok := wf.Nodes["oracle_gate"].(*ir.ComputeNode)
	if !ok {
		t.Fatal("oracle_gate is not a compute node")
	}
	for _, ex := range gate.Exprs {
		if ex.Key == "converged" {
			if ex.AST == nil {
				t.Fatalf("oracle_gate.converged did not compile: %s", ex.Raw)
			}
			return ex.AST
		}
	}
	t.Fatal("oracle_gate publishes no `converged`")
	return nil
}

// evalConverged evaluates that expression against one oracle_run output map.
func evalConverged(t *testing.T, ast *expr.AST, run map[string]any) bool {
	t.Helper()
	vars := map[string]any{"min_corpus": int64(12), "mutation_floor": int64(90)}
	ctx := &expr.Context{
		Vars: func(p []string) any {
			if len(p) != 1 {
				return nil
			}
			return vars[p[0]]
		},
		Outputs: func(p []string) any {
			if len(p) != 2 || p[0] != "oracle_run" {
				return nil
			}
			return run[p[1]]
		},
	}
	got, err := ast.EvalBool(ctx)
	if err != nil {
		t.Fatalf("evaluating oracle_gate.converged: %v", err)
	}
	return got
}

// greenOracleRun is every term of the conjunction at its passing value. Kept
// exhaustive on purpose: a term added to the gate without a value here reads
// as nil, the base case stops converging, and this test says so — which is
// the cheapest possible reminder that a new obligation also needs a fixture.
func greenOracleRun() map[string]any {
	return map[string]any{
		"stable":                    true,
		"noop_silent":               true,
		"revert_clean":              true,
		"collateral":                int64(0),
		"unstable_controls":         []any{},
		"uncontrolled":              []any{},
		"blind_lanes":               []any{},
		"missing_archetypes":        []any{},
		"corpus_distinct":           int64(20),
		"duplicate_groups_unproven": []any{},
		"runner_replayable":         true,
		"holdout_reused":            []any{},
		"score_pct":                 int64(100),
		"holdout_detected":          int64(4),
		"holdout_total":             int64(4),
	}
}

// TestGoldenMasterGateRefusesAnUnprovenDuplicateGroup pins the obligation to
// CONVERGENCE, which is the only place it decides anything.
//
// The harness computes `duplicate_groups_unproven` and appends a diagnostic,
// but a diagnostic reaches only `log_tail` -> `fail_log`, which is reporting.
// Without a term here every other conjunct can hold while a byte-identical
// group is undischarged, and the run converges green on references nobody can
// tell apart — the proof obligation would be decorative, which is the exact
// failure mode it was written to replace (a note the gate cannot read).
//
// The width floor does not cover it: duplicates were historically noticed only
// by making `corpus_distinct` smaller, so a corpus wide enough to clear the
// floor carries an undischarged pair straight through.
//
// Evaluated, not grepped for: a substring check passes on a term that is
// present but wired to the wrong operand.
func TestGoldenMasterGateRefusesAnUnprovenDuplicateGroup(t *testing.T) {
	ast := oracleGateConverged(t)

	if !evalConverged(t, ast, greenOracleRun()) {
		t.Fatalf("the all-green fixture does not converge — a term of %s has no value in greenOracleRun()", ast.Source())
	}

	red := greenOracleRun()
	red["duplicate_groups_unproven"] = []any{
		map[string]any{
			"ids":          []any{"012", "013"},
			"separated_by": "M-07",
			"why":          "the declared separator moves EVERY member together",
		},
	}
	if evalConverged(t, ast, red) {
		t.Fatalf("oracle_gate converges with an undischarged byte-identical group: the obligation reaches log_tail only, and log_tail is reporting — %s", ast.Source())
	}
}

// TestGoldenMasterWrapperRefusesAnUnprovenDuplicateGroup pins the same term on
// the STANDALONE entry point. `verify-oracle.sh` is what CI and humans run;
// its verdict is a second, hand-restated copy of the conjunction, so a term
// added to the graph gate alone leaves the entry point that is actually
// invoked weaker than the one that is not — the precedent the wrapper's own
// comment records for `holdout_reused`.
func TestGoldenMasterWrapperRefusesAnUnprovenDuplicateGroup(t *testing.T) {
	requireModernizeTools(t)
	ws := t.TempDir()
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "canon"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Green on every other term. The one under test is read from the
	// environment, so the two runs differ by exactly that field.
	const harness = `import json, os, sys
if os.environ.get("GM_MODE", "gate") == "selftest":
    sys.exit(0)
print(json.dumps({"mode": "gate", "stable": True, "noop_silent": True,
                  "revert_clean": True, "collateral": 0, "uncontrolled": [],
                  "blind_lanes": [], "missing_archetypes": [],
                  "runner_replayable": True, "holdout_reused": [],
                  "holdout_detected": 2, "holdout_total": 2,
                  "duplicate_groups_unproven": json.loads(os.environ.get("STUB_UNPROVEN", "[]"))}))
`
	for name, body := range map[string]string{
		"canon/test_rules.py": "print('ok')\n",
		"harness.py":          harness,
	} {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := emitRunner(t, ws)

	run := func(t *testing.T, unproven string) (int, string) {
		t.Helper()
		cmd := exec.Command("sh", runner)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(),
			"GM_REPORT_TMP="+filepath.Join(t.TempDir(), "report.json"),
			"GM_WORKSPACE="+ws, "STUB_UNPROVEN="+unproven)
		b, err := cmd.CombinedOutput()
		exit := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("wrapper failed to execute: %v\n%s", err, b)
			}
			exit = ee.ExitCode()
		}
		return exit, string(b)
	}

	if exit, out := run(t, "[]"); exit != 0 {
		t.Fatalf("the all-green report must exit 0: exit %d\n%s", exit, out)
	}

	unproven, err := json.Marshal([]map[string]any{{
		"ids": []string{"012", "013"}, "separated_by": "M-07",
		"why": "the declared separator is INVALID (apply.sh exited 1)",
	}})
	if err != nil {
		t.Fatal(err)
	}
	exit, out := run(t, string(unproven))
	if exit != 1 || !strings.Contains(out, "GATE RED") {
		t.Fatalf("the wrapper accepted an undischarged byte-identical group — the entry point CI runs is weaker than the graph gate: exit %d\n%s", exit, out)
	}
}
