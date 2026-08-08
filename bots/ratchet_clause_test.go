package bots

import (
	"os"
	"strings"
	"testing"
)

// The campaign contracts of whole-improve-loop and branch-improve-loop end
// with an advisory block the agent answers BEFORE it reports. Its third
// question — `Ratchet` — asks for the cheap deterministic check that would
// have caught the defect just fixed. See docs/improvement-ratchet.md.
//
// WHAT THIS TEST IS, AND IS NOT. It is an ANTI-DELETION guard: each guard
// sentence below was written to close a failure mode found in review, and
// this test fails when one is dropped or reworded away by a later edit. It is
// NOT a semantic proof — a grep cannot tell a rule from a quotation of that
// rule. Only review catches that; what this catches is the far likelier
// regression, someone tightening the prose and losing a guard with it.
//
// Two assertions do NOT depend on a phrasing, and they are the load-bearing
// ones. The clause may not name the remaining-work field outside the three
// sentences allowed to, and it may not name a history-rewriting command
// un-negated. Both were first written as enumerations — of the verbs that
// route, of the spellings that forbid — and both were defeated by the next
// rewording, so they now scan for the dangerous THING and exempt narrowly.
//
// Known limit: an exemption is per sentence, so a second, illegitimate clause
// riding inside an exempted sentence is not seen. Splitting finer would make
// every reflow a failure.
//
// If you deliberately reword a guard, update the expected literal here in
// the same change — the failure message names it and echoes the clause.

// continuationIndent is the indentation the advisory block's bullets wrap
// their continuation lines at. It is what bounds the clause: the next prompt
// section sits at a shallower indent.
const continuationIndent = "      "

// ratchetClause returns the `- Ratchet:` bullet of a bot's advisory block,
// lower-cased, stripped of markdown/typographic ornament, with every
// whitespace run collapsed to one space — so an assertion matches a sentence
// regardless of where the prompt's line wrapping falls or whether a term got
// backticked.
func ratchetClause(t *testing.T, botPath string) string {
	t.Helper()
	src, err := os.ReadFile(botPath)
	if err != nil {
		t.Fatalf("read %s: %v", botPath, err)
	}
	_, after, found := strings.Cut(string(src), "- Ratchet:")
	if !found {
		t.Fatalf("%s: no `- Ratchet:` clause in the advisory block — the "+
			"campaign no longer asks what check would have caught the defect "+
			"it just fixed (docs/improvement-ratchet.md)", botPath)
	}
	// The bullet ends where its continuation indent does. Cutting on the
	// first blank line would be wrong in the failure case that matters: drop
	// the blank line and the next one further down the file still "ends" the
	// clause, so it silently swallows the termination contract and every
	// assertion below starts matching text it never meant to inspect.
	lines := strings.Split(after, "\n")
	// lines[0] is the remainder of the marker's own line, which carries no
	// continuation indent of its own.
	body := []string{strings.TrimSpace(lines[0])}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		// A blank line does NOT end the clause: someone airing out a long
		// bullet into two paragraphs must not silently push the tail out of
		// scope, which would blind every assertion below to whatever the tail
		// says. Only the indent decides.
		if trimmed == "" {
			continue
		}
		// Only a shallower indent ends the clause. That covers both exits:
		// the block's sibling bullets sit one level in from the continuation
		// indent, and the next prompt section is shallower still. A nested
		// list INSIDE this bullet is deeper, so it stays in scope.
		if !strings.HasPrefix(line, continuationIndent) {
			break
		}
		body = append(body, trimmed)
	}
	clause := strings.Join(body, " ")
	if strings.Contains(clause, "TERMINATION CONTRACT") {
		t.Fatalf("%s: the `- Ratchet:` bullet ran into the termination "+
			"contract — its boundary is not where this test thinks it is, so "+
			"every assertion below is unreliable", botPath)
	}
	ornament := strings.NewReplacer(
		"`", "", "*", "", "“", `"`, "”", `"`, "’", "'",
	)
	return strings.Join(strings.Fields(ornament.Replace(strings.ToLower(clause))), " ")
}

// unNegatedMentions returns, for each occurrence of cmd in clause that is NOT
// preceded by a negation, the text just before it — enough context for the
// failure message to show what the clause actually says. The lookback stops at
// the last ". " before the occurrence, so a negation from the previous
// sentence cannot launder the next one; the byte cap only bounds the message.
// It is generous on purpose: the natural way to spell the prohibition out is
// to enumerate ("never reach for any of these: git rebase, git reset --hard,
// …"), which pushes the negation far back, and a test that reds on a STRONGER
// guard is a test that gets weakened. The cost is that a run-on — a dangerous
// instruction spliced onto the guard sentence without ending it — reads as
// negated. A properly punctuated one does not.
func unNegatedMentions(clause, cmd string) []string {
	const window = 200
	var offenders []string
	for i, rest := 0, clause; ; {
		at := strings.Index(rest, cmd)
		if at < 0 {
			return offenders
		}
		start := i + at
		from := start - window
		if from < 0 {
			from = 0
		}
		before := clause[from:start]
		if cut := strings.LastIndex(before, ". "); cut >= 0 {
			before = before[cut+2:]
		}
		if !containsAny(before, []string{"never", "no ", "not ", "don't", "avoid"}) {
			offenders = append(offenders, before)
		}
		i, rest = start+len(cmd), rest[at+len(cmd):]
	}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// mustContain fails with the literal it was looking for, so a maintainer who
// reworded on purpose can see exactly what to update.
func mustContain(t *testing.T, clause, want, why string) {
	t.Helper()
	if !strings.Contains(clause, want) {
		// The clause is echoed because the other way to get here is a
		// mis-parse: if what follows is not the bullet you expected, fix the
		// boundary rather than weakening the expectation.
		t.Errorf("Ratchet clause no longer contains %q.\n  Why it is there: %s\n"+
			"  If the rewording is deliberate, update this expectation in the same change.\n"+
			"  Clause as parsed: %q",
			want, why, clause)
	}
}

func TestRatchetClauseKeepsItsConvergenceGuards(t *testing.T) {
	cases := []struct {
		bot string
		// remainingField is the termination contract's remaining-work note.
		// It is coupled to the done-flag ("empty when <flag>"), so a declined
		// ratchet parked there holds the flag false on every pass and the
		// loop burns max_passes without ever converging.
		remainingField string
		// carveBack keeps the exemption from swallowing a real finding: the
		// declined CHECK is not remaining work, but what it would have
		// guarded may well be.
		carveBack string
		// antiOscillation is the bot's own guard against the campaign
		// re-opening, on a later pass, work this question made it do.
		antiOscillation string
		whyAntiOsc      string
	}{
		{
			bot:             "whole-improve-loop/main.bot",
			remainingField:  "sites_remaining",
			carveBack:       "the exception to never land it is an axis that is about such checks",
			antiOscillation: "don't land the gate yet: that missing check is a genuine remaining site",
			whyAntiOsc: "without it the exception tells the agent to land a repo-wide gate it " +
				"cannot green in this pass, which reds the deterministic gate on untouched code",
		},
		{
			bot:            "branch-improve-loop/main.bot",
			remainingField: "issues_remaining",
			carveBack:      `still a "missing test" issue`,
			antiOscillation: `a check landed by an earlier pass of this run is already answered ` +
				`(its commit message names the issue it closes, and git log is the record): ` +
				`never re-open it as a "weak test" finding`,
			whyAntiOsc: "the campaign's own rubric counts missing or weak tests as a real " +
				"issue, so without this the next fresh pass re-opens the ratchet it just " +
				"landed and branch_clean never becomes true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.bot, func(t *testing.T) {
			clause := ratchetClause(t, tc.bot)

			mustContain(t, clause, "keep it out of "+tc.remainingField,
				"that field is coupled to the done-flag, so a deferred ratchet parked "+
					"there makes the campaign report unfinished work it has decided not to "+
					"do — every pass, until max_passes")
			mustContain(t, clause, tc.carveBack,
				"without the carve-back the exemption reads as a licence to drop a whole "+
					"class of real findings from the done-flag")
			mustContain(t, clause, tc.antiOscillation, tc.whyAntiOsc)
			// The closer is what makes the whole question advisory — the
			// property both manifests sell. Without it the bullet reads as a
			// mandate, and an advisory question that mandates work is the one
			// thing this clause must never become.
			mustContain(t, clause, "it never grows the pass, never blocks it",
				"this is the sentence that keeps the question advisory; the gate, not this "+
					"question, decides whether the pass converges")

			// Any MENTION of the field is suspect — enumerating the verbs
			// that route into it ("name it in", "report it in", …) is a game
			// the next rewording always wins. Only three sentences may name
			// it, and each is exempted by a literal asserted above, so the
			// blind spot cannot be widened without breaking an assertion.
			exempt := []string{
				"keep it out of " + tc.remainingField,
				tc.antiOscillation,
				tc.carveBack,
			}
			for _, sentence := range strings.Split(clause, ". ") {
				if !strings.Contains(sentence, tc.remainingField) {
					continue
				}
				// These sentences put something in the field ON PURPOSE — a
				// real remaining site, or a real finding — which is the
				// opposite of the defect scanned for here.
				if containsAny(sentence, exempt) {
					continue
				}
				t.Errorf("Ratchet clause names %s outside the sentences allowed to: %q\n"+
					"  A check the agent decided not to build is not outstanding work; "+
					"parking it there holds the done-flag false on every pass.",
					tc.remainingField, sentence)
			}
		})
	}
}

func TestRatchetClauseKeepsItsBlastRadiusGuards(t *testing.T) {
	// A lint rule or a stricter build-time constraint added mid-campaign is
	// retroactive: the deterministic gate (which the verify-build skill
	// points at the repo's lint+test umbrella) then fails on files the pass
	// never touched, and the campaign spends its remaining passes cleaning up
	// after itself — on a fail_log truncated to 4000 chars, so it never even
	// sees the full damage.
	for _, bot := range []string{"whole-improve-loop/main.bot", "branch-improve-loop/main.bot"} {
		t.Run(bot, func(t *testing.T) {
			clause := ratchetClause(t, bot)
			mustContain(t, clause, "repo-wide gate",
				"the clause must name repo-wide checks as the thing that is NOT local")
			// The whole sentence, not the fragment: Willy's exception quotes
			// `never land it` back, so the bare fragment is satisfied by the
			// quotation even when the rule it defends has been deleted.
			mustContain(t, clause, "in your summary, never land it",
				"naming them is not enough — the clause must send them to the summary "+
					"and forbid landing them")
		})
	}
}

func TestRatchetClauseNeverRewritesBankedCommits(t *testing.T) {
	// The clause is answered in the "BEFORE YOU REPORT" block — by then the
	// commit that fixed the site is already landed, and possibly already
	// pushed. "Add it in the commit that fixes the site" therefore reads as
	// amend/rebase, which breaks the property these bots sell (git IS the
	// durable state) and makes push_back_tool duplicate or conflict, since it
	// never force-pushes.
	for _, bot := range []string{"whole-improve-loop/main.bot", "branch-improve-loop/main.bot"} {
		t.Run(bot, func(t *testing.T) {
			clause := ratchetClause(t, bot)

			mustContain(t, clause, "never amend or rebase a commit you already landed",
				"answered after the fix is committed, 'add it in the commit that fixes "+
					"it' is only executable as an amend or a rebase of banked — possibly "+
					"pushed — commits")
			mustContain(t, clause, "follow-up commit",
				"the agent needs a non-destructive way to land a late check")

			// What is forbidden is OFFERING a history-rewriting command, not
			// mentioning one: a maintainer who spells the prohibition out is
			// strengthening it. So each occurrence is judged on whether it is
			// negated. A fixed whitelist of negated forms was tried and only
			// accepted the two spellings it listed; a sentence-wide exemption
			// was tried and let the guard sentence carry its own
			// "unless … --amend" escape hatch. Neither survived review.
			for _, forbidden := range []string{
				"--amend", "git rebase", "--fixup", "--autosquash", "--squash",
				"git reset", "filter-branch", "push --force", "push -f",
			} {
				for _, window := range unNegatedMentions(clause, forbidden) {
					t.Errorf("Ratchet clause offers %q un-negated (…%q). The clause is "+
						"answered after the pass's commits are landed and possibly pushed; "+
						"handing the agent a history-rewriting command there strands work "+
						"the run had banked.", forbidden, window+forbidden)
				}
			}
		})
	}
}
