package bots

import (
	"regexp"
	"strings"
	"testing"
)

// The golden-master gate is stated TWICE: once as the graph conjunction
// (`compute oracle_gate.converged`) and once as `RUNNER_VERDICT_PY`'s `ok`,
// the standalone `verify-oracle.sh` entry point that CI and humans run. The
// runner's own comment says it must never be weaker than the graph gate.
//
// Nothing checked that. C031 makes a graph term reading an undeclared report
// field a compile error, so leg one is guarded; the runner's `ok` is a Python
// string inside a tool script and is invisible to every compiler in this repo.
// That asymmetry is the structural reason the same defect happened twice — the
// `holdout_reused` term, then `duplicate_groups_unproven`, each landed in the
// graph conjunction while the entry point CI runs kept approving exactly the
// tree the graph refuses.
//
// A verdict that establishes something NEAR what it claims and reports the
// resemblance is the defect this bot exists to catch in other people's
// deliveries; carrying it in its own two gates was not tenable.
func TestGoldenMasterStandaloneRunnerCarriesTheGraphGatesTerms(t *testing.T) {
	bot := readHarnessFile(t, "golden-master/main.bot")
	graph := convergedExpr(t, bot)
	runner := runnerVerdictOK(t, bot)

	// The divergences that PRE-DATE this check, named so they stay visible
	// instead of being silently re-created. The first two compare a report
	// field against a graph `var`, which a runner reading only the report file
	// structurally cannot reach — closing them means emitting the thresholds
	// into the runner. The third is a plain report field and therefore the very
	// class of weakness this test exists to prevent; it is tracked here rather
	// than closed, because widening the standalone gate is its own change with
	// its own blast radius.
	knownGaps := map[string]string{
		"score_pct":         "compared against vars.mutation_floor, which the report file does not carry",
		"corpus_distinct":   "compared against vars.min_corpus, which the report file does not carry",
		"unstable_controls": "a plain report field — a real gap, named rather than closed here",
	}

	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`outputs\.oracle_run\.(\w+)`).FindAllStringSubmatch(graph, -1) {
		field := m[1]
		if seen[field] {
			continue
		}
		seen[field] = true

		carried := regexp.MustCompile(`r\.get\(\\"` + regexp.QuoteMeta(field) + `\\"\)`).MatchString(runner)
		why, known := knownGaps[field]
		switch {
		case carried && known:
			t.Errorf("the standalone runner now carries %s, but it is still listed as a known gap (%q): "+
				"delete the entry, or the allowlist starts covering divergences nobody decided to accept", field, why)
		case carried:
		case known:
			t.Logf("known divergence: the standalone runner does not read %s (%s)", field, why)
		default:
			t.Errorf("oracle_gate.converged refuses on outputs.oracle_run.%s but RUNNER_VERDICT_PY's `ok` never reads it: "+
				"`verify-oracle.sh` — the entry point CI and humans run — would approve exactly the tree the graph gate "+
				"refuses. Add `and not r.get(%q)` (or the comparison that fits) to `ok`, or, if the term genuinely cannot "+
				"reach the standalone runner, add it to knownGaps in this test with the reason.", field, field)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no outputs.oracle_run.* term found in the conjunction: it moved, fix this test")
	}
}

// convergedExpr returns the one line that IS the graph conjunction. Pinned on
// the expression, not on the `converged:` name — `schema gate_state` declares
// the field too, and matching that would compare the gate against a type.
func convergedExpr(t *testing.T, bot string) string {
	t.Helper()
	for _, l := range strings.Split(bot, "\n") {
		if strings.Contains(l, `converged: "outputs.oracle_run.`) {
			return l
		}
	}
	t.Fatal("the `converged:` conjunction was not found in main.bot — the gate was restructured, fix this test")
	return ""
}

// runnerVerdictOK returns the `ok=(...)` assignment of RUNNER_VERDICT_PY, and
// only that: `mode=(r.get("mode") or "gate")` sits above it and the notice read
// below, neither of which decides the verdict.
func runnerVerdictOK(t *testing.T, bot string) string {
	t.Helper()
	const start, end = `ok=(r.get(`, `n=r.get(\"notice\")`
	i := strings.Index(bot, start)
	j := strings.Index(bot, end)
	if i < 0 || j < 0 || j <= i {
		t.Fatal("RUNNER_VERDICT_PY's `ok=(...)` assignment was not found between its markers — the runner verdict was restructured, fix this test")
	}
	return bot[i:j]
}
