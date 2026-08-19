package bots

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every campaign contract ends its commit-in-stride rule with a clean-tree
// clause: the deterministic gate verifies the WORKING TREE, so a pass that
// stops mid-unit must not leave half-done edits behind to red it as noise.
//
// The clause tells the agent to DISCARD those edits, and that instruction is
// only as safe as the verb it names. Two ways to get it wrong, both already
// observed in this file's history:
//
//   - Name no verb at all ("discard the in-progress edits"). `git stash` then
//     reads as a perfectly reasonable choice — and a run worktree shares
//     `refs/stash` with the operator's repo, so a run that dies between the
//     stash and its pop strands their tree with a ref they never made.
//   - Leave `git clean` un-negated. Several campaign bots run in the
//     operator's live checkout, where a blanket clean deletes THEIR untracked
//     files, not the run's.
//
// This is an ANTI-REGRESSION guard in the shape of ratchet_clause_test.go's,
// with the same known limit: a grep cannot tell a rule from a quotation of
// one. It catches the likely regression — a reflow or a tightening that drops
// the verb — not a semantic inversion. Reword on purpose and update the
// expectation in the same change.
var cleanTreeCarriers = []string{
	"app-dev/main.bot",
	"branch-improve-loop/main.bot",
	"e2e-coverage/main.bot",
	"feature-dev/main.bot",
	"feature-gap-fill/main.bot",
	"instrument/main.bot",
	"secured-renovacy/main.bot",
	"test-coverage/main.bot",
	"whole-improve-loop/main.bot",
}

const cleanTreeMarker = "END EVERY PASS ON A CLEAN TREE:"

// numberedBullet ends the clause at the contract's next rule, for the
// carriers whose clean-tree paragraph is not followed by a blank line.
var numberedBullet = regexp.MustCompile(`^\d+\. `)

// cleanTreeClause returns the clause, lower-cased, stripped of backticks and
// whitespace-collapsed, so an assertion matches regardless of where the
// prompt's line wrapping falls. The scan runs to the contract's next
// numbered rule (or the prompt block's own end, at column 0) and THROUGH
// blank lines: the `git clean` check below asserts ABSENCE, so an early
// stop would let an offending mention hide in text the assertion never
// inspected — under-capture is only safe for the presence assertions.
func cleanTreeClause(t *testing.T, botPath string) string {
	t.Helper()
	src, err := os.ReadFile(botPath)
	if err != nil {
		t.Fatalf("read %s: %v", botPath, err)
	}
	_, after, found := strings.Cut(string(src), cleanTreeMarker)
	if !found {
		t.Fatalf("%s: no %q clause — the campaign no longer tells the agent "+
			"to end a pass on a tree the deterministic gate can read",
			botPath, cleanTreeMarker)
	}
	body := []string{strings.TrimSpace(strings.Split(after, "\n")[0])}
	for _, line := range strings.Split(after, "\n")[1:] {
		if line != "" && !strings.HasPrefix(line, " ") {
			break // column 0: the prompt block itself ended
		}
		trimmed := strings.TrimSpace(line)
		if numberedBullet.MatchString(trimmed) {
			break
		}
		if trimmed == "" {
			continue
		}
		body = append(body, trimmed)
	}
	clause := strings.Join(body, " ")
	ornament := strings.NewReplacer("`", "", "*", "", "’", "'")
	return strings.Join(strings.Fields(ornament.Replace(strings.ToLower(clause))), " ")
}

func TestCleanTreeClauseNamesASafeDiscardVerb(t *testing.T) {
	for _, bot := range cleanTreeCarriers {
		t.Run(bot, func(t *testing.T) {
			clause := cleanTreeClause(t, bot)

			// A named verb, in its FULL form. Without one the agent picks its
			// own, and the obvious pick writes to a ref shared with the
			// operator's repo. The bare `git restore -- <paths>` is not
			// enough either: it restores the worktree FROM the index, which
			// the contract's own pre-commit `git add -A` habit turns into a
			// no-op, and it never removes newly created files.
			if !strings.Contains(clause, "git restore --staged --worktree") {
				t.Errorf("clean-tree clause does not name the full restore form "+
					"(git restore --staged --worktree).\n"+
					"  Why it is there: an unnamed \"discard\" leaves `git stash` "+
					"as a reasonable reading (a run worktree shares refs/stash "+
					"with the operator's repo), and a bare `git restore` no-ops "+
					"on staged edits.\n  Clause as parsed: %q", clause)
			}

			// A blanket clean, un-negated, is worse than the stash it replaced:
			// on an in-place run it deletes the operator's own untracked files.
			if offenders := unNegatedMentions(clause, "git clean"); len(offenders) > 0 {
				t.Errorf("clean-tree clause mentions `git clean` un-negated %d time(s).\n"+
					"  Why it is guarded: the workspace may be the operator's own "+
					"checkout, where a blanket clean deletes THEIR untracked files.\n"+
					"  Context before each mention: %q", len(offenders), offenders)
			}
		})
	}
}
