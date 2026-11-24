package ir

import (
	"os"
	"strings"
	"testing"
)

const subbotSrc = `
schema verdict:
  validated: bool

tool plan:
  command: "true"
  output: verdict

subbot run_ticket:
  source: "child.bot"
  with { issue: "{{outputs.plan.validated}}" }
  output: verdict

workflow w:
  entry: plan

  plan -> run_ticket
  run_ticket -> done when validated
  run_ticket -> fail
`

// TestCompileSubbot checks a subbot node compiles into a SubbotNode carrying
// its source, with-mappings and output schema.
func TestCompileSubbot(t *testing.T) {
	w := mustCompile(t, subbotSrc)
	n, ok := w.Nodes["run_ticket"].(*SubbotNode)
	if !ok {
		t.Fatalf("run_ticket is not a SubbotNode: %T", w.Nodes["run_ticket"])
	}
	if n.NodeKind() != NodeSubbot {
		t.Fatalf("NodeKind = %v, want subbot", n.NodeKind())
	}
	if n.Source != "child.bot" || n.OutputSchema != "verdict" {
		t.Fatalf("source/output = %q/%q", n.Source, n.OutputSchema)
	}
	if got := NodeOutputSchema(n); got != "verdict" {
		t.Fatalf("NodeOutputSchema = %q, want verdict", got)
	}
	if len(n.With) != 1 || n.With[0].Key != "issue" {
		t.Fatalf("with mappings = %v", n.With)
	}
}

// TestSubbotNoSource asserts C119 fires for a subbot without a source.
func TestSubbotNoSource(t *testing.T) {
	src := `
schema empty:
  ok: bool

subbot run_ticket:
  output: empty

tool a:
  command: "true"
  output: empty

workflow w:
  entry: a
  a -> run_ticket
  run_ticket -> done
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagSubbotNoSource) {
		t.Fatal("expected C119 (DiagSubbotNoSource)")
	}
}

const subbotLandingSrc = `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  isolated: true
  auto_merge: true
  merge_into: "main"
  merge_strategy: merge
`

// TestCompileSubbotLandingIntent checks the landing declaration compiles
// into the node: the arming flag, the target and the strategy the host
// runners wire into the child engine.
func TestCompileSubbotLandingIntent(t *testing.T) {
	w := mustCompile(t, subbotLandingSrc)
	n, ok := w.Nodes["lot"].(*SubbotNode)
	if !ok {
		t.Fatalf("lot is not a SubbotNode: %T", w.Nodes["lot"])
	}
	if !n.AutoMerge || n.MergeInto != "main" || n.MergeStrategy != "merge" {
		t.Fatalf("landing intent = (%t, %q, %q), want (true, main, merge)", n.AutoMerge, n.MergeInto, n.MergeStrategy)
	}
	if !n.Isolated {
		t.Fatalf("isolated lost at compile")
	}
}

// TestSubbotLandingNeedsIsolation asserts C329: an auto_merge on a child
// running in the parent's workspace arms a landing that never fires —
// the child has no finalization of its own — so the intent is refused
// where it is written, not discovered at the terminal.
func TestSubbotLandingNeedsIsolation(t *testing.T) {
	src := `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  auto_merge: true
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagSubbotLandingWithoutIsolated) {
		t.Fatal("expected C329 (DiagSubbotLandingWithoutIsolated)")
	}
}

// TestSubbotLandingFieldsNeedAutoMerge asserts C330: merge_into /
// merge_strategy without the arming flag would be dropped in silence.
func TestSubbotLandingFieldsNeedAutoMerge(t *testing.T) {
	src := `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  isolated: true
  merge_into: "main"
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagSubbotLandingWithoutAutoMerge) {
		t.Fatal("expected C330 (DiagSubbotLandingWithoutAutoMerge)")
	}
}

// TestSubbotLandingStrategyEnum asserts the strategy's closed vocabulary,
// and that the code it rides still admits this carrier: the refusal is a
// C004 member, so the author greps the code and lands on the catalogue's
// fix line and the reference row — both must name `merge_strategy` and
// `merge_into`, or the remedy describes a check that does not know the
// refusals exist.
func TestSubbotLandingStrategyEnum(t *testing.T) {
	src := `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  isolated: true
  auto_merge: true
  merge_strategy: rebase
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagBadTemplateRef) {
		t.Fatal("expected the merge_strategy enum refusal")
	}
	hint := HintFor(DiagBadTemplateRef)
	for _, carrier := range []string{"merge_strategy", "merge_into"} {
		if !strings.Contains(hint, carrier) {
			t.Fatalf("the C004 fix line does not admit the subbot %s carrier: %s", carrier, hint)
		}
	}
	doc, err := os.ReadFile("../../../docs/references/diagnostics.md")
	if err != nil {
		t.Fatal(err)
	}
	var c004 string
	for line := range strings.SplitSeq(string(doc), "\n") {
		if strings.HasPrefix(line, "| **C004**") {
			c004 = line
			break
		}
	}
	if c004 == "" {
		t.Fatal("docs/references/diagnostics.md has no C004 row")
	}
	for _, carrier := range []string{"merge_strategy", "merge_into"} {
		if !strings.Contains(c004, carrier) {
			t.Fatalf("the C004 reference row does not admit the subbot %s carrier: %s", carrier, c004)
		}
	}
}

// TestSubbotLandingMergeIntoVocabulary asserts merge_into's closed
// vocabulary, read the way every intent reader folds it (case-insensitive,
// whitespace-trimmed): the opt-out, the default, or a branch name git
// accepts. A target git would misread — a padded branch name, a stray word
// — compiles today and dies at the child's finalize (merge_status=failed,
// the router's bounded retries, then the alert); refused where written
// instead, naming what was written, with the written value kept verbatim.
func TestSubbotLandingMergeIntoVocabulary(t *testing.T) {
	refused := `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  isolated: true
  auto_merge: true
  merge_into: "main "
`
	diags := compileFile(t, refused).Diagnostics
	if !hasDiag(diags, DiagBadTemplateRef) {
		t.Fatal("expected the merge_into vocabulary refusal for a target git would misread")
	}
	found := false
	for _, d := range diags {
		if d.Code == DiagBadTemplateRef && strings.Contains(d.Error(), "merge_into") && strings.Contains(d.Error(), `"main "`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal does not name the field and the value as written: %v", diags)
	}

	// The folded opt-out and the folded default compile — the reading the
	// runtime and the router give the same field — and the written value
	// survives verbatim (never silently rewritten).
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"folded opt-out", " None "},
		{"folded default", "Current"},
		{"plain branch", "main"},
	} {
		src := `
tool a:
  command: "true"

workflow w:
  entry: a

  a -> lot
  lot -> done

subbot lot:
  source: "child.bot"
  isolated: true
  auto_merge: true
  merge_into: "` + tc.value + `"
`
		res := compileFile(t, src)
		if hasDiag(res.Diagnostics, DiagBadTemplateRef) {
			t.Fatalf("%s (%q): compile refused a value every intent reader folds", tc.name, tc.value)
		}
		n, ok := res.Workflow.Nodes["lot"].(*SubbotNode)
		if !ok {
			t.Fatalf("%s: lot is not a SubbotNode: %T", tc.name, res.Workflow.Nodes["lot"])
		}
		if n.MergeInto != tc.value {
			t.Fatalf("%s: merge_into = %q, want the written value %q verbatim", tc.name, n.MergeInto, tc.value)
		}
	}
}
