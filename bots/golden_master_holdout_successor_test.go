package bots

import (
	"os"
	"strings"
	"testing"
)

// The golden-master rite leaves the NEXT gate a held-out set to score, and the
// instruction that asks for it is one paragraph away from voiding the very term
// it protects. The harness's committed check is directory-wide —
// `git ls-files -- mutants/holdout` (oracle-harness.py:holdout_committed_in_tree)
// — and `seal_holdout` early-returns without moving ANYTHING once that is true
// and the gate has not opted in. So a successor committed before a harness run
// has relocated the rite's OWN set leaves that set in the tree: the sealed pile
// stays empty, `holdout_total` and `holdout_detected` are both 0, the "missing
// sealed set" bail cannot fire (the committed successor keeps the directory
// present), and the gate's `holdout_detected == holdout_total` term passes
// 0 == 0 — the vacuous green this whole bot exists to refuse.
//
// As first shipped, the instruction's own completion criterion fired on exactly
// that path: `holdout_awaiting_gate` is set for anything committed under
// `mutants/holdout/` with no opt-in, so the agent read "finished" at the moment
// it had emptied its strongest term.
//
// Same shape and same known limit as clean_tree_clause_test.go: a grep cannot
// tell a rule from a quotation of one, so this catches the likely regression —
// a reflow or a tightening that drops the ordering or collapses the conjunction
// back to a single field — not a semantic inversion. Reword on purpose and
// update the expectations in the same change.
const (
	holdoutSuccessorBot    = "golden-master/main.bot"
	holdoutSuccessorMarker = "LEAVE THE NEXT GATE A SET TO SCORE."
)

// holdoutSuccessorClause returns the instruction, lower-cased, stripped of
// ornament and whitespace-collapsed, so an assertion matches wherever the
// prompt's line wrapping falls. The scan runs to the end of the prompt block
// (column 0) or to the next template reference, which is where the paragraph
// hands over to the failure log.
func holdoutSuccessorClause(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(holdoutSuccessorBot)
	if err != nil {
		t.Fatalf("read %s: %v", holdoutSuccessorBot, err)
	}
	_, after, found := strings.Cut(string(src), holdoutSuccessorMarker)
	if !found {
		t.Fatalf("%s: no %q clause — the rite no longer leaves the next gate a "+
			"set to score, so every later held-out figure is 0/0",
			holdoutSuccessorBot, holdoutSuccessorMarker)
	}
	var body []string
	for _, line := range strings.Split(after, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			break // column 0: the prompt block itself ended
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{{") {
			break // the paragraph is over; the failure log follows
		}
		if trimmed == "" {
			continue
		}
		body = append(body, trimmed)
	}
	ornament := strings.NewReplacer("`", "", "*", "", "’", "'", "—", "-")
	return strings.Join(strings.Fields(ornament.Replace(strings.ToLower(strings.Join(body, " ")))), " ")
}

func TestGoldenMasterHoldoutSuccessorIsOrderedAndNonVacuous(t *testing.T) {
	clause := holdoutSuccessorClause(t)

	// 1. THE ORDER. Drawing the successor is safe only after a harness run has
	//    moved the rite's own set out of the tree, and the agent has no way to
	//    infer that: the seal declines in silence and selfcheck withholds the
	//    score, so the wrong order shows up days later as a 0/0.
	for _, want := range []string{"only after", "gm_mode=selfcheck"} {
		if !strings.Contains(clause, want) {
			t.Errorf("the successor instruction does not name %q, so it no longer "+
				"orders the commit after the seal.\n"+
				"  Why it is there: seal_holdout declines DIRECTORY-WIDE once "+
				"anything under mutants/holdout/ is committed, so a successor "+
				"committed first leaves the rite's own set unsealed and unscored.\n"+
				"  Clause as parsed: %q", want, clause)
		}
	}

	// 2. THE STOP-CONDITION, as a conjunction. `holdout_awaiting_gate` alone is
	//    true for the degenerate case it is meant to prevent — a set committed
	//    under mutants/holdout/ that nothing ever sealed. `holdout_total` is
	//    published under selfcheck (the score is not) and discriminates
	//    exactly: non-zero means the rite's own set reached the sealed pile.
	finished := clauseSentence(clause, "finished on this point")
	if finished == "" {
		t.Fatalf("the successor instruction no longer states when the agent is "+
			"finished on this point.\n  Clause as parsed: %q", clause)
	}
	for _, want := range []string{"holdout_awaiting_gate", "holdout_total"} {
		if !strings.Contains(finished, want) {
			t.Errorf("the completion criterion does not name %q.\n"+
				"  Why both: holdout_awaiting_gate fires for anything committed "+
				"under mutants/holdout/ with no opt-in — including a rite whose "+
				"own set was never sealed — so alone it reads \"finished\" at the "+
				"moment the held-out term went vacuous. A non-zero holdout_total "+
				"in the same report is what says the rite's own set is in the "+
				"sealed pile.\n  Criterion as parsed: %q", want, finished)
		}
	}

	// 3. A FRESH draw. Both sets are now written in one sitting by one context,
	//    and identity is the apply/revert bytes plus the declared targets — so a
	//    near-duplicate is not caught here but at the later gate, as
	//    `holdout_reused`, which is the expensive late red this instruction
	//    exists to remove.
	if !strings.Contains(clause, "repeat") || !strings.Contains(clause, "targets") {
		t.Errorf("the successor instruction no longer requires the second set to "+
			"be distinct from the first (identity: the apply/revert scripts plus "+
			"the declared targets).\n"+
			"  Why it is there: one context drawing both sets in one sitting "+
			"produces near-duplicates, and a repeated fingerprint turns the LATER "+
			"gate red, days after the run that could have fixed it.\n"+
			"  Clause as parsed: %q", clause)
	}

	// 4. THE OPT-IN IS NOT THE RITE'S TO WRITE. This paragraph is the first
	//    place the agent is told the flag exists, and `seal_committed_opted_in`
	//    reads it from the config being judged at EVERY gate — including this
	//    run's own, which runs minutes later. A flag written "for the later
	//    gate" therefore makes THIS gate seal the committed set, score both
	//    sets in one run and empty the magazine again, after moving tracked
	//    files out from under git.
	if !strings.Contains(clause, "seal_committed") {
		t.Fatalf("the successor instruction no longer names the opt-in flag.\n"+
			"  Clause as parsed: %q", clause)
	}
	var warned bool
	for _, phrasing := range []string{"do not write that flag", "never write that flag",
		"do not set that flag", "never set that flag"} {
		if strings.Contains(clause, phrasing) {
			warned = true
			break
		}
	}
	if !warned {
		t.Errorf("the successor instruction names \"seal_committed\" without telling "+
			"the rite not to write it.\n"+
			"  Why it is there: the flag is read from the config at every gate, this "+
			"run's included, so one written in advance makes THIS gate seal and score "+
			"the committed set — both sets spent in one run, and tracked files moved "+
			"out from under git as uncommitted deletions.\n  Clause as parsed: %q", clause)
	}

	// 5. The motive, stated as the harness actually behaves. A spent set does
	//    not make the next gate refuse — it boots, replays the whole corpus and
	//    reports 0/0 with `holdout_spent_unreplaced`, which the conjunction
	//    passes. Naming the field keeps the paragraph checkable against the
	//    harness instead of against a plausible story.
	if !strings.Contains(clause, "holdout_spent_unreplaced") {
		t.Errorf("the successor instruction no longer names holdout_spent_unreplaced.\n"+
			"  Why it is there: the earlier wording said a later gate \"finds an "+
			"empty magazine and refuses\". It does not refuse — it pays for a boot "+
			"and a full replay, reports 0/0 with that field, and its "+
			"detected == total term passes. Green on emptiness is the real "+
			"outcome, and the stronger motive.\n  Clause as parsed: %q", clause)
	}
}

// clauseSentence returns the sentence of an already-normalised clause that
// carries the marker, or "" when nothing does. Sentences are cut on ". " so a
// field name carrying a dot (there is none here) would not split one.
func clauseSentence(clause, marker string) string {
	for _, s := range strings.Split(clause, ". ") {
		if strings.Contains(s, marker) {
			return s
		}
	}
	return ""
}
