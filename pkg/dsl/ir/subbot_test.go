package ir

import "testing"

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

// TestSubbotLandingStrategyEnum asserts the strategy's closed vocabulary.
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
}
