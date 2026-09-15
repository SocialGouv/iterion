package bots

import (
	"os"
	"strings"
	"testing"
)

// The golden-master rite leaves the NEXT gate a held-out set to score, and the
// instruction that asks for it is one paragraph away from voiding the very term
// it protects.
//
// `seal_holdout` declines PER SET: it keeps the entries `git ls-files --
// mutants/holdout` reports and relocates every other one. So a committed
// successor waiting for its own gate does NOT keep the drawing run's set in the
// tree, and no ordering between the two acts is needed.
//
// What survives is a single-entry hazard, and it is the one this file pins: a
// set the rite commits IN STRIDE is tracked, so the seal leaves it behind —
// unsealed, readable by the hardening loop that must never see it, and scored
// as `holdout 0/0`, the vacuous green this whole bot exists to refuse.
//
// Same shape and same known limit as clean_tree_clause_test.go: a grep cannot
// tell a rule from a quotation of one, so this catches the likely regression —
// a reflow that drops the tracking rule, or a tightening that restores the
// superseded ordering doctrine — not a semantic inversion. Reword on purpose
// and update the expectations in the same change.
//
// The behaviour itself is proved against the harness, not here: the `MELANGE`
// case in `oracle-harness.py`'s `mk_holdout` selftest drives a tree holding a
// committed successor AND a fresh set, and asserts only the tracked one stays.
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

func TestGoldenMasterHoldoutSuccessorKeepsItsOwnSetUntracked(t *testing.T) {
	clause := holdoutSuccessorClause(t)

	// 1. THE GRANULARITY, stated. Told only "commit a set under
	//    mutants/holdout/", an agent has no reason to treat its own set
	//    differently from the successor — and committing both is the one act
	//    that still empties the term. Saying the seal declines PER SET is what
	//    makes the next sentence ("keep yours untracked") a rule instead of a
	//    superstition, and it is also what stops a later editor from restoring
	//    the superseded "commit only after the seal" ordering.
	for _, want := range []string{"per set", "in stride"} {
		if !strings.Contains(clause, want) {
			t.Errorf("the successor instruction does not name %q, so it no longer "+
				"tells the rite which set may be committed.\n"+
				"  Why it is there: seal_holdout keeps what git TRACKS and relocates "+
				"the rest. A successor is meant to be tracked; the rite's own set "+
				"must not be, or the seal leaves it in the tree, unsealed and "+
				"unscored, and the gate reports 0/0.\n"+
				"  Clause as parsed: %q", want, clause)
		}
	}

	// 2. THE STOP-CONDITION, and what it must NOT cost. The criterion is the
	//    commit itself: `holdout_awaiting_gate` is reported by the gate that
	//    runs anyway (oracle-harness.py sets it whenever a committed set has no
	//    opt-in), so sending the agent to run a selfcheck just to watch a field
	//    turn buys nothing and spends a full harness run.
	finished := clauseSentence(clause, "finished on this point")
	if finished == "" {
		t.Fatalf("the successor instruction no longer states when the agent is "+
			"finished on this point.\n  Clause as parsed: %q", clause)
	}
	if !strings.Contains(finished, "fingerprint") {
		t.Errorf("the completion criterion no longer keys on the successor being "+
			"committed with FRESH fingerprints.\n"+
			"  Why: that is the whole of it. A repeated fingerprint is what turns "+
			"the later gate red, and it is the only property of the act the rite "+
			"can still get wrong.\n  Criterion as parsed: %q", finished)
	}
	if !strings.Contains(clause, "do not run a selfcheck") {
		t.Errorf("the successor instruction no longer tells the rite NOT to run a "+
			"selfcheck to confirm this point.\n"+
			"  Why it is there: an earlier wording made the criterion a field read "+
			"from a selfcheck report, which cost a full harness run to observe "+
			"something the convergence gate reports on its own.\n"+
			"  Clause as parsed: %q", clause)
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

	// 5. THE ESCAPE, or the criterion above becomes a trap. ONE INHERITED state
	//    is unreachable by any act the rite is allowed to take — it may not
	//    write the opt-in, and must not delete a set it did not draw — so the
	//    paragraph has to say how it ENDS: by reporting it.
	//      - the judged config already carrying `"seal_committed": true`:
	//        `awaiting` is `committed_in_tree AND NOT opted_in`, so it never
	//        turns true, and every successor the rite commits is sealed and
	//        spent by this same run — an agent chasing the criterion re-commits
	//        a set each pass and watches the next seal strip it back out.
	//    The state that used to sit beside it — inheriting a tracked
	//    mutants/holdout/, which once blocked the seal for EVERYTHING — is gone
	//    with the per-set decline: an unclaimed successor is now a debt the run
	//    reports, not a wall it cannot pass.
	for _, want := range []string{"carries the opt-in", "work_remaining"} {
		if !strings.Contains(clause, want) {
			t.Errorf("the successor instruction does not name %q, so it no longer "+
				"gives the completion criterion an exit on a net whose config already "+
				"carries the opt-in.\n"+
				"  Why it is there: in that state `awaiting` never turns true, because "+
				"it is `committed_in_tree AND NOT opted_in` — every successor the rite "+
				"commits is sealed and spent by this same run. The criterion cannot be "+
				"met by any act the rite may take, and an agent with no stated exit "+
				"either loops or invents one (writing the opt-in, or deleting the "+
				"predecessor's set).\n  Clause as parsed: %q", want, clause)
		}
	}

	// 6. THE NOTICE THE SEALING RUN WILL EMIT. `holdout_sealed_uncommitted`
	//    fires on exactly the run this instruction now asks for, and its text
	//    says "a set a LATER run must score has one durable home — commit it
	//    under mutants/holdout/". Answered literally, the rite re-commits the
	//    set it just sealed: same fingerprint, published under mutants/audit/
	//    by this very run, so the gate that finally opts in refuses it as
	//    `holdout_reused`. The successor is the answer; say so where the
	//    instruction sends the agent to look.
	if !strings.Contains(clause, "holdout_sealed_uncommitted") {
		t.Errorf("the successor instruction no longer tells the rite how to read "+
			"holdout_sealed_uncommitted.\n"+
			"  Why it is there: that notice fires on the sealing run this paragraph "+
			"asks for, and its literal answer — re-committing the set just sealed — "+
			"is a repeated fingerprint the later gate refuses.\n"+
			"  Clause as parsed: %q", clause)
	}

	// 7. The motive, stated as the harness actually behaves. A spent set does
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
