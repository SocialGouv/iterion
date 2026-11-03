package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	pr := parseBotUnit("dep-update-guard/main.bot")
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

	// The claim and the shas still cannot separate "lost" from "already in the
	// head": a re-run on the bot's own alignment commit re-applies the same
	// edits, still claims applied, and leaves the head unmoved
	// (buildkit-operator#50). align_probe decides — read by git, in a node
	// wired straight after align — and its `carried` is read NEGATED, so a
	// reading absent from an older checkpoint holds instead of passing.
	if !strings.Contains(lost, "outputs.align_probe.pending") {
		t.Errorf("alignment_lost does not read the work align left in the workspace (align_probe.pending): %q", lost)
	}
	if !strings.Contains(lost, "!outputs.align_probe.carried") {
		t.Errorf("alignment_lost does not read align_probe.carried negated, so a claim the PR does not carry — or a missing reading — passes as clean: %q", lost)
	}
	if _, ok := cr.Workflow.Nodes["align_probe"].(*ir.ToolNode); !ok {
		t.Errorf("align_probe is %T, want a tool node: the tree is read by git, never reported by an agent", cr.Workflow.Nodes["align_probe"])
	}
	var fromAlign []string
	probeToGate := false
	probeInputs := map[string]string{}
	for _, e := range cr.Workflow.Edges {
		if e.From == "align" {
			fromAlign = append(fromAlign, e.To)
			if e.To == "align_probe" {
				for _, m := range e.With {
					probeInputs[m.Key] = strings.Trim(m.Raw, `"`)
				}
			}
		}
		if e.From == "align_probe" && e.To == "align_gate" {
			probeToGate = true
		}
	}
	if len(fromAlign) != 1 || fromAlign[0] != "align_probe" || !probeToGate {
		t.Errorf("align must lead to align_probe alone, and align_probe to align_gate (got align -> %v, align_probe -> align_gate: %v): the tree is read before any other node runs on it", fromAlign, probeToGate)
	}
	// carried is only as good as the claim it checks and the commit it reads.
	for key, want := range map[string]string{
		"head_sha": "{{outputs.prepare.head_sha}}",
		"claimed":  "{{outputs.align.files_changed}}",
	} {
		if probeInputs[key] != want {
			t.Errorf("align -> align_probe maps %s from %q, want %q", key, probeInputs[key], want)
		}
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
			// The hold names what align_probe found, in its own paragraph
			// AHEAD of align's prose: a blank line does not close a fence or
			// an HTML block that prose leaves open, so coming first is what
			// keeps the reason — and the file names it carries — out of it.
			summary := ""
			for _, m := range e.With {
				if m.Key == "align_summary" {
					summary = m.Raw
				}
			}
			if !strings.Contains(summary, "{{outputs.align_probe.reason}}\n\n{{outputs.align.breaking_summary}}") {
				t.Errorf("the hold's align_summary must open with align_probe.reason, then a blank line, then align's prose; got %q", summary)
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

	// The commit agent's nothing-to-commit summary is published on the clean
	// comment, ahead of "Nothing left to align.": it has to be true on the
	// pass whose head already carries an alignment (buildkit-operator#50).
	// And align_probe matches align's claim path by path: the align prompt
	// is where that contract is stated.
	for name, rule := range map[string]struct{ absent, present string }{
		"commit_system": {absent: "no alignment needed", present: "nothing to commit"},
		"align_system":  {present: "never a directory"},
	} {
		p, ok := cr.Workflow.Prompts[name]
		if !ok || p == nil {
			t.Fatalf("prompt %s is not declared", name)
		}
		if rule.absent != "" && strings.Contains(p.Body, rule.absent) {
			t.Errorf("prompt %s still has the agent write %q", name, rule.absent)
		}
		if !strings.Contains(p.Body, rule.present) {
			t.Errorf("prompt %s no longer states %q", name, rule.present)
		}
	}
}

// TestDepUpdateGuardLostAlignmentPredicate EVALUATES commit_check against the
// states it has to separate, instead of reading its source for the right
// substrings. Grepping the expression proves the fields are mentioned; only
// running it proves they combine into the right answer.
//
// SocialGouv/iterion#400 as it actually happened: `align` wrote the otel
// WithEndpointURL fix, the run died on a provider usage window before
// `commit`, the retry re-cloned the repo, and the alignment was gone. The
// checkpoint still replayed applied=true, the shas still showed an unmoved
// head — which is also exactly what a bump needing no alignment looks like.
// The gate read the second meaning, went green, and the bump merged without
// its fix. Whatever align_probe re-measured on the fresh clone, the fix's
// files are ones the bump never changed: the claim is not carried.
func TestDepUpdateGuardLostAlignmentPredicate(t *testing.T) {
	cr := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	check, ok := cr.Workflow.Nodes["commit_check"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("commit_check is %T, want a compute node", cr.Workflow.Nodes["commit_check"])
	}

	// probe is align_probe's output, or nil for a run whose checkpoint
	// predates the node and so carries no reading at all.
	eval := func(t *testing.T, key string, applied bool, probe map[string]any, commitSHA, headSHA string) bool {
		t.Helper()
		outputs := map[string]map[string]any{
			"align":       {"applied": applied},
			"align_probe": probe,
			"commit":      {"sha": commitSHA},
			"prepare":     {"head_sha": headSHA},
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

	reading := func(pending, carried bool) map[string]any {
		return map[string]any{"pending": pending, "carried": carried}
	}
	for _, tc := range []struct {
		name       string
		applied    bool
		probe      map[string]any
		commitSHA  string
		headSHA    string
		wantLost   bool
		wantCommit bool
	}{
		{
			// buildkit-operator#50 (2026-11-03): run 1 aligned and pushed
			// fb4a7353; its push fired `synchronize`, and run 2 read THAT
			// commit as its head. `align` re-applied the same edits (a no-op
			// on a tree that already carried them) and still claimed applied,
			// `commit` had nothing to commit, and the shas read "unmoved head"
			// — the lost-alignment shape, except nothing was lost: the head
			// already carries the alignment. The gate went red and the arming
			// from run 1 could never fire. align_probe tells the two apart:
			// nothing pending, and the PR changes every file align claims.
			name:    "align re-applied onto a head that already carries it: nothing lost",
			applied: true, probe: reading(false, true), commitSHA: "", headSHA: "fb4a7353761a7e809173badc573712d99eec0208",
			wantLost: false, wantCommit: false,
		},
		{
			// iterion#400. The one the gate has to start catching.
			name:    "align applied, the workspace held it, head unmoved: the alignment vanished",
			applied: true, probe: reading(true, false), commitSHA: "", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
		{
			// iterion#400 again, with the death landing after align_probe:
			// the resume re-executes the probe on a fresh clone where the fix
			// is gone, so nothing reads pending — but the fix's files are not
			// ones the bump changes, so the claim is not carried.
			name:    "the probe re-measured on a fresh clone after the edits were lost",
			applied: true, probe: reading(false, false), commitSHA: "", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
		{
			// align claims edits it reverted, reset away, or never wrote: the
			// workspace is clean and matches the head, and the head is the bare
			// bump. Read as "already in the head", this merged without the fix.
			name:    "align claims edits that exist nowhere: not pending, not carried",
			applied: true, probe: reading(false, false), commitSHA: "", headSHA: "086571ac3a6a62f62947ee0f53201b43210ae6cb",
			wantLost: true, wantCommit: false,
		},
		{
			// A re-run on the bot's own alignment commit whose align writes NEW
			// edits that never land: the claimed files are ones the PR already
			// changes, but the workspace still holds what was never pushed.
			name:    "a re-run on the bot's own alignment writes new edits that never land",
			applied: true, probe: reading(true, true), commitSHA: "", headSHA: "fb4a7353761a7e809173badc573712d99eec0208",
			wantLost: true, wantCommit: false,
		},
		{
			// A run resumed from a checkpoint older than align_probe has no
			// reading at all: it must hold, never pass for a carried claim.
			name:    "no align_probe reading at all: holds",
			applied: true, probe: nil, commitSHA: "", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
		{
			// buildkit-operator#18: undici, security-only release, no
			// consuming code. Same unmoved head, opposite meaning — this one
			// is genuinely green and must stay mergeable.
			name:    "align applied nothing, head unmoved: the bump needed no alignment",
			applied: false, probe: reading(false, false), commitSHA: "", headSHA: "086571ac3a6a62f62947ee0f53201b43210ae6cb",
			wantLost: false, wantCommit: false,
		},
		{
			// The probe alone opens no hold: a file align made without claiming
			// an alignment (a build dropping, say) is not an alignment lost.
			name:    "align claimed nothing over a workspace holding a stray file: no alignment to lose",
			applied: false, probe: reading(true, false), commitSHA: "", headSHA: "086571ac3a6a62f62947ee0f53201b43210ae6cb",
			wantLost: false, wantCommit: false,
		},
		{
			name:    "align applied and the head moved: committed",
			applied: true, probe: reading(true, false), commitSHA: "aaaa111122223333444455556666777788889999", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: false, wantCommit: true,
		},
		{
			// The agent answering with HEAD's sha rather than "" is the same
			// unmoved branch dressed as a commit. did_commit already compares
			// the two shas for this reason; alignment_lost must inherit it
			// rather than settle for a non-empty sha.
			name:    "align applied, agent echoes the unchanged head sha",
			applied: true, probe: reading(true, false), commitSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a", headSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
			wantLost: true, wantCommit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval(t, "alignment_lost", tc.applied, tc.probe, tc.commitSHA, tc.headSHA); got != tc.wantLost {
				t.Errorf("alignment_lost = %v, want %v", got, tc.wantLost)
			}
			if got := eval(t, "did_commit", tc.applied, tc.probe, tc.commitSHA, tc.headSHA); got != tc.wantCommit {
				t.Errorf("did_commit = %v, want %v", got, tc.wantCommit)
			}
		})
	}
}
