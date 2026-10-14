package bots

import (
	"strings"
	"testing"
)

// TestDeclaredSuitesReadAlikeByEveryReader: four nodes in three bots read the
// repository's suites — modernize's plan_read before a lot is picked and its
// lot_verify at every gate, the campaign's finalize on the final tree, the
// assessment's contract_lint when the contract is drafted. They cannot share
// text, so one corpus runs through the four real scripts: the three that
// execute agree both ways, and the lint never accepts what they refuse.
func TestDeclaredSuitesReadAlikeByEveryReader(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	requireModernizeTools(t)
	planRead := toolScript(t, "modernize/main.bot", "plan_read")
	lotVerify := toolScript(t, "modernize/main.bot", "lot_verify")

	for name, tc := range map[string]struct {
		top      string
		readable bool
	}{
		"absent":                     {"", true},
		"a string":                   {"suites: \"true\"\n", true},
		"a list":                     {"suites:\n  - \"true\"\n  - \"true\"\n", true},
		"a blank item, then one":     {"suites:\n  - \"   \"\n  - \"true\"\n", true},
		"a trailing carriage return": {"suites:\n  - \"true\\r\"\n", true},
		"a one-line block":           {"suites: |\n  true\n", true},
		"a folded block":             {"suites: >\n  true\n  true\n", true},
		"a mapping":                  {"suites:\n  unit: \"true\"\n", false},
		"null":                       {"suites: ~\n", false},
		"a number in a list":         {"suites:\n  - 3\n", false},
		"an empty list":              {"suites: []\n", false},
		"an empty string in a list":  {"suites:\n  - \"\"\n", false},
		"a unit separator only":      {"suites:\n  - \"\\x1f\"\n", false},
		"a multi-line block":         {"suites: |\n  true\n  true\n", false},
		"a NUL in the second item":   {"suites:\n  - \"true\"\n  - \"tr\\0ue\"\n", false},
		"declared twice":             {"suites: \"true\"\nsuites: \"false\"\n", false},
		"declared twice, alike":      {"suites: \"true\"\nsuites: \"true\"\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			plan := suitesPlan("", tc.top, "true", "todo")

			pr := modernizePlanRead(t, planRead, plan, "", 0)
			planReadReads := !pr.Refused

			ws, base, _ := suitesWorkspace(t, plan)
			lv, _, exit := modernizeLotVerifyRaw(t, lotVerify, ws, "L1", base, "true", nil)
			lotVerifyReads := exit == 0 && !lv.Unreadable

			handoff, _ := campaignFinalize(t, suitesPlan("", tc.top, "true", "done"), nil)
			suitesLine := handoffLine(handoff, "suites on the final tree")
			finalizeReads := suitesLine != "" && !strings.Contains(suitesLine, "NOT RUN")

			contract := strings.Replace(aGoodContract, "gate_timeout_s: 3600\n", "gate_timeout_s: 3600\n\n"+tc.top, 1)
			if tc.top != "" && contract == aGoodContract {
				t.Fatal("the corpus entry did not apply to the lint's contract")
			}
			lint := lintContract(t, contract, goodOutcomes)
			lintAccepts := assessmentBool(t, lint, "ok")

			if planReadReads != tc.readable || lotVerifyReads != tc.readable || finalizeReads != tc.readable {
				t.Fatalf("want readable=%v; plan_read=%v (%s) lot_verify=%v (%s) finalize=%v (%s)",
					tc.readable, planReadReads, pr.Notice, lotVerifyReads, lv.LogTail, finalizeReads, suitesLine)
			}
			if lintAccepts && !tc.readable {
				t.Fatal("contract_lint accepted suites the execution bot refuses: the assessment would sign off a programme that cannot start")
			}
			if !lintAccepts && tc.readable {
				t.Fatalf("contract_lint refused suites every reader accepts: %s", assessmentString(t, lint, "reason"))
			}
		})
	}
}
