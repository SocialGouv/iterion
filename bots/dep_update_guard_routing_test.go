package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestDepUpdateGuardVerdictRouting asserts the WIRING, not the strings.
//
// Every user-facing claim this bot makes — the comment badge, the "what Vetty
// did" line, the required check's description — is selected by one value: the
// verdict stamped on the edge into post_feedback. The string tables are well
// covered; the edges that choose between them were not, and a mapping deleted
// there fails no test while making every message wrong in production.
//
// So this pins the two things a string test cannot see: that the choice is
// made from a DETERMINISTIC observation (two shas the run owns) rather than
// from the commit agent's own account of its work, and that each branch stamps
// the verdict matching what it observed.
func TestDepUpdateGuardVerdictRouting(t *testing.T) {
	src, err := os.ReadFile("dep-update-guard/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	pr := parser.Parse("dep-update-guard/main.bot", string(src))
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}

	// The decision is a compute node reading shas — not `outputs.commit.committed`,
	// which is the agent grading its own homework.
	check, ok := cr.Workflow.Nodes["commit_check"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("commit_check is %T, want a compute node deciding from the shas",
			cr.Workflow.Nodes["commit_check"])
	}
	var decides string
	for _, e := range check.Exprs {
		if e.Key == "did_commit" {
			decides = e.Raw
		}
	}
	if decides == "" {
		t.Fatal("commit_check does not compute did_commit")
	}
	for _, want := range []string{"outputs.commit.sha", "outputs.prepare.head_sha"} {
		if !strings.Contains(decides, want) {
			t.Errorf("did_commit does not read %s — it must be observed, not reported: %q", want, decides)
		}
	}
	if strings.Contains(decides, "outputs.commit.committed") {
		t.Errorf("did_commit trusts the commit agent's self-report: %q", decides)
	}

	// Both branches exist, each stamping the verdict that matches what it saw.
	var sawCommitted, sawClean bool
	for _, e := range cr.Workflow.Edges {
		if e.From != "commit_check" || e.To != "post_feedback" {
			continue
		}
		verdict := ""
		for _, m := range e.With {
			if m.Key == "verdict" {
				verdict = strings.Trim(m.Raw, `"`)
			}
		}
		switch {
		case e.Condition == "did_commit" && !e.Negated:
			sawCommitted = true
			if verdict != "committed" {
				t.Errorf("the did_commit branch stamps verdict %q, want \"committed\"", verdict)
			}
		case e.Condition == "did_commit" && e.Negated:
			sawClean = true
			if verdict != "clean" {
				t.Errorf("the not-did_commit branch stamps verdict %q, want \"clean\"", verdict)
			}
		}
	}
	if !sawCommitted {
		t.Error("no edge routes a real alignment to the committed verdict")
	}
	if !sawClean {
		t.Error("no edge routes a no-op alignment to the clean verdict — every run would claim a commit")
	}
}
