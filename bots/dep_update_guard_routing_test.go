package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
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

	// An unmoved head means "nothing to align" OR "the alignment vanished",
	// and the shas cannot tell them apart. Separating them needs align's own
	// claim, so the absence of that read is the whole defect — assert it is
	// there, and that it is combined with the sha observation rather than
	// replacing it.
	var lost string
	for _, e := range check.Exprs {
		if e.Key == "alignment_lost" {
			lost = e.Raw
		}
	}
	if lost == "" {
		t.Fatal("commit_check does not compute alignment_lost — a vanished alignment reads as a bump that needed none, and merges")
	}
	if !strings.Contains(lost, "outputs.align.applied") {
		t.Errorf("alignment_lost does not read align's own claim, so it cannot detect a contradiction: %q", lost)
	}
	if !strings.Contains(lost, "outputs.commit.sha") || !strings.Contains(lost, "outputs.prepare.head_sha") {
		t.Errorf("alignment_lost must be the CONTRADICTION between the claim and the shas, not the claim alone: %q", lost)
	}

	// The three branches exist, each stamping the verdict matching what it saw.
	// `clean` is the else: it is the only one of the three that asserts a
	// negative ("the bump needed nothing"), so it must be what is left when
	// neither positive observation held — never a condition of its own that
	// could match alongside them.
	var sawCommitted, sawClean, sawLost bool
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
		case e.Condition == "alignment_lost" && !e.Negated:
			sawLost = true
			if verdict != "hold_lost_alignment" {
				t.Errorf("the alignment_lost branch stamps verdict %q, want \"hold_lost_alignment\"", verdict)
			}
		case e.Condition == "did_commit" && !e.Negated:
			sawCommitted = true
			if verdict != "committed" {
				t.Errorf("the did_commit branch stamps verdict %q, want \"committed\"", verdict)
			}
		case e.IsElse:
			sawClean = true
			if verdict != "clean" {
				t.Errorf("the fallback branch stamps verdict %q, want \"clean\"", verdict)
			}
		}
	}
	if !sawCommitted {
		t.Error("no edge routes a real alignment to the committed verdict")
	}
	if !sawClean {
		t.Error("no edge routes a no-op alignment to the clean verdict — every run would claim a commit")
	}
	if !sawLost {
		t.Error("no edge routes a vanished alignment to hold_lost_alignment — it would be reported as a clean bump and merged")
	}
}

// TestDepUpdateGuardLostAlignmentPredicate EVALUATES commit_check against the
// four states it has to separate, instead of reading its source for the right
// substrings. Grepping the expression proves the fields are mentioned; only
// running it proves they combine into the right answer.
//
// The first case is SocialGouv/iterion#400 as it actually happened: `align`
// wrote the otel WithEndpointURL fix, the run died on a provider usage window
// before `commit`, the retry re-cloned the repo, and the alignment was gone.
// The checkpoint still replayed applied=true, the shas still showed an unmoved
// head — which is also exactly what a bump needing no alignment looks like.
// The gate read the second meaning, went green, and the bump merged without
// its fix.
func TestDepUpdateGuardLostAlignmentPredicate(t *testing.T) {
	src, err := os.ReadFile("dep-update-guard/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cr := ir.Compile(parser.Parse("dep-update-guard/main.bot", string(src)).File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	check, ok := cr.Workflow.Nodes["commit_check"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("commit_check is %T, want a compute node", cr.Workflow.Nodes["commit_check"])
	}

	eval := func(t *testing.T, key string, applied bool, commitSHA, headSHA string) bool {
		t.Helper()
		outputs := map[string]map[string]any{
			"align":   {"applied": applied},
			"commit":  {"sha": commitSHA},
			"prepare": {"head_sha": headSHA},
		}
		for _, e := range check.Exprs {
			if e.Key != key {
				continue
			}
			got, err := e.AST.EvalBool(&expr.Context{
				Outputs: func(path []string) any {
					if len(path) != 2 {
						return nil
					}
					return outputs[path[0]][path[1]]
				},
			})
			if err != nil {
				t.Fatalf("eval %s: %v", key, err)
			}
			return got
		}
		t.Fatalf("commit_check has no %s expression", key)
		return false
	}

	for _, tc := range []struct {
		name       string
		applied    bool
		commitSHA  string
		headSHA    string
		wantLost   bool
		wantCommit bool
	}{
		{
			// iterion#400. The one the gate has to start catching.
			name:    "align applied, head unmoved: the alignment vanished",
			applied: true, commitSHA: "", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
		{
			// buildkit-operator#18: undici, security-only release, no
			// consuming code. Same unmoved head, opposite meaning — this one
			// is genuinely green and must stay mergeable.
			name:    "align applied nothing, head unmoved: the bump needed no alignment",
			applied: false, commitSHA: "", headSHA: "086571ac3a6a62f62947ee0f53201b43210ae6cb",
			wantLost: false, wantCommit: false,
		},
		{
			name:    "align applied and the head moved: committed",
			applied: true, commitSHA: "aaaa111122223333444455556666777788889999", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: false, wantCommit: true,
		},
		{
			// The agent answering with HEAD's sha rather than "" is the same
			// unmoved branch dressed as a commit. did_commit already compares
			// the two shas for this reason; alignment_lost must inherit it
			// rather than settle for a non-empty sha.
			name:    "align applied, agent echoes the unchanged head sha",
			applied: true, commitSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval(t, "alignment_lost", tc.applied, tc.commitSHA, tc.headSHA); got != tc.wantLost {
				t.Errorf("alignment_lost = %v, want %v", got, tc.wantLost)
			}
			if got := eval(t, "did_commit", tc.applied, tc.commitSHA, tc.headSHA); got != tc.wantCommit {
				t.Errorf("did_commit = %v, want %v", got, tc.wantCommit)
			}
		})
	}
}
