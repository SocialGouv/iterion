package bots

import (
	"os"
	"strings"
	"testing"
)

// Every campaign contract carries a status-capture clause, because the
// campaign agent runs the repo's checks in its OWN shell — outside the
// deterministic verify script the gate re-runs, and outside anything the
// engine can inspect. A pipeline exits with its LAST command's status, so
// `<check> 2>&1 | tail -10` reports `tail`, and a check that never ran reads
// as one that passed.
//
// Measured on PR #770, run 01a07283 (issue #779): the campaign ran a
// TypeScript check whose compiler was not installed, devbox returned 127, and
// the pass recorded EXIT=0. `pipefail` is not POSIX and these commands run
// under `sh`, so "remember to set pipefail" is not the rule — capturing the
// status before filtering is.
//
// An ANTI-REGRESSION guard in the shape of clean_tree_clause_test.go's, with
// the same known limit: a grep cannot tell a rule from a quotation of one. It
// catches the likely regression — a reflow or a rewrite that drops the rule —
// not a semantic inversion. Reword on purpose and update the expectation in
// the same change.
var statusCaptureCarriers = cleanTreeCarriers

const statusCaptureMarker = "CAPTURE THE STATUS, THEN FILTER"

func TestCampaignContractsCaptureStatusBeforeFiltering(t *testing.T) {
	for _, bot := range statusCaptureCarriers {
		t.Run(bot, func(t *testing.T) {
			raw, err := os.ReadFile(bot)
			if err != nil {
				t.Fatalf("read %s: %v", bot, err)
			}
			src := string(raw)
			if !strings.Contains(src, statusCaptureMarker) {
				t.Fatalf("%s: no %q clause — the campaign runs the repo's checks in its own shell, "+
					"where a piped gate reports the filter and a failed check reads as success (#779)",
					bot, statusCaptureMarker)
			}
			// The clause is only as good as what it names: the mechanism
			// (the pipeline's status is the LAST command's), the reason the
			// obvious cure is unavailable (pipefail is not POSIX here), and
			// the honest third outcome (a missing tool is not a pass).
			for _, want := range []string{"pipefail", "unavailable"} {
				if !strings.Contains(strings.ToLower(src), want) {
					t.Errorf("%s: the status-capture clause does not mention %q — "+
						"without it the agent reaches for `set -o pipefail` (absent from dash) "+
						"or reports a missing tool as a pass", bot, want)
				}
			}
		})
	}
}
