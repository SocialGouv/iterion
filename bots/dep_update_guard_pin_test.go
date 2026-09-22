package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// The gate Vetty posts is a statement about a REVISION. The publish endpoint
// resolves the pull request's head at the moment it writes the status, so
// without a pin the two can differ: the bot audits A, a push lands B, and A's
// verdict certifies B — which, with a required check and auto-merge armed, is
// how an unaudited commit reaches the default branch.
//
// `audited_sha` is that pin, and the server REFUSES a stale one. But an ABSENT
// pin does not fail: it posts unpinned (so a repo on an older bundle keeps its
// gate instead of deadlocking). That is exactly what makes a forgotten edge
// silent — the run stays green and the certificate goes back to following the
// head. Hence this test: every edge into the publishing node must carry it, not
// just one. (`TestCatalogInputReadsAreMappedByAnIncomingEdge` only proves SOME
// edge maps a read field, which is the weaker property.)
//
// Mutation that must redden it: drop `audited_sha` from any one of the six
// `-> post_feedback` edges in bots/dep-update-guard/main.bot.
func TestDepUpdateGuardEveryFeedbackEdgePinsTheAuditedSHA(t *testing.T) {
	const (
		bot      = "dep-update-guard/main.bot"
		node     = "post_feedback"
		pinField = "audited_sha"
	)

	u := unit.LoadDir(bot)
	if u.Merged == nil {
		t.Fatalf("%s: the bundle did not parse: %v", bot, u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("%s: the bundle did not compile: %v", bot, cr.Diagnostics)
	}

	var seen int
	for _, e := range cr.Workflow.Edges {
		if e.To != node {
			continue
		}
		seen++
		pin := ""
		for _, m := range e.With {
			if m.Key == pinField {
				pin = strings.TrimSpace(m.Raw)
				break
			}
		}
		if pin == "" {
			t.Errorf("edge %s -> %s (%s) does not fill %q: its verdict would post UNPINNED and the gate would certify whatever head the forge reports at publish time — add %s: \"{{outputs.commit.sha}}\" on the edge that committed, \"{{outputs.prepare.head_sha}}\" on the ones that did not",
				e.From, e.To, edgeGuard(e), pinField, pinField)
			continue
		}
		// A pin that is not a reference is a constant, and a constant sha is a
		// lie that matches nothing — the gate would then never post at all.
		if !strings.Contains(pin, "{{") {
			t.Errorf("edge %s -> %s (%s) pins %q to the literal %q: a pin must RESOLVE to the revision this run read",
				e.From, e.To, edgeGuard(e), pinField, pin)
			continue
		}
		// Presence is the weaker half. The value has to be the sha true on THIS
		// path, and the likeliest slip is not an omission — it is pasting the
		// sibling's value, which five of the six edges carry. On the one edge
		// that committed, the pre-commit head is never the post-commit head, so
		// that paste is a permanent gate refusal on Vetty's primary path.
		wantRef := "outputs.prepare.head_sha"
		why := "this path pushed nothing, so the verdict covers the head the run read"
		if e.Condition == "did_commit" && !e.Negated {
			wantRef = "outputs.commit.sha"
			why = "this is the one path where the alignment moved the branch"
		}
		if !strings.Contains(pin, wantRef) {
			t.Errorf("edge %s -> %s (%s) pins %q to %s, want a reference to %s — %s",
				e.From, e.To, edgeGuard(e), pinField, pin, wantRef, why)
		}
	}

	// The did_commit edge is the one the value assertion above exists for; a
	// rename that made it unreachable would leave every remaining edge checked
	// against the lenient branch, and this test would stay green saying nothing.
	var sawCommitEdge bool
	for _, e := range cr.Workflow.Edges {
		if e.To == node && e.Condition == "did_commit" && !e.Negated {
			sawCommitEdge = true
		}
	}
	if !sawCommitEdge {
		t.Errorf("no `when did_commit` edge reaches %q — the guard above then only ever checks the lenient branch", node)
	}

	// A walk that silently found nothing would pass every assertion above.
	if seen == 0 {
		t.Fatalf("no edge reaches %q — the node was renamed or the walk broke, and this guard has been proving nothing", node)
	}
}

// edgeGuard renders an edge's predicate for an error message: which branch a
// reader has to go fix.
func edgeGuard(e *ir.Edge) string {
	switch {
	case e.IsElse:
		return "else"
	case e.ExpressionSrc != "":
		return "when " + e.ExpressionSrc
	case e.Condition != "" && e.Negated:
		return "when not " + e.Condition
	case e.Condition != "":
		return "when " + e.Condition
	default:
		return "unconditional"
	}
}
