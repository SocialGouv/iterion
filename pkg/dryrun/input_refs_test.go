package dryrun

import (
	"context"
	"strings"
	"testing"
)

// pm is reached by two edges: the `when ok` one maps feedback, the
// `when not ok` one maps nothing. On the false pass the prompt's
// {{input.feedback}} resolves to nothing — mapped by ANOTHER incoming edge,
// so empty on this path only.
const mappedElsewhereBot = `schema verdict:
  ok: bool
  note: string

agent survey:
  model: "claude-opus-4-7"
  output: verdict

agent pm:
  model: "claude-opus-4-7"
  user: pu
  output: verdict

prompt pu:
  Feedback so far: {{input.feedback}}

workflow im:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> pm when ok with { feedback: "{{outputs.survey.note}}" }
  survey -> pm when not ok
  pm -> done
`

// A node reached through an edge that maps nothing, where ANOTHER incoming
// edge maps the field: the finding is the path-only kind — a warning, worded
// with the facts (which edge maps, which maps none) — and reads neither as
// failing nor as clean's opposite: --strict does not fail on it (#1455).
func TestAnInputMappedOnAnotherPathIsItsOwnKind(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, mappedElsewhereBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := findingsOf(r, KindUnmappedOnPath)
	if len(found) != 1 {
		t.Fatalf("the path-only case is not its own kind: %+v", r.Findings)
	}
	f := found[0]
	if f.Node != "pm" {
		t.Fatalf("the finding landed on %q, want pm: %+v", f.Node, f)
	}
	for _, want := range []string{"{{input.feedback}}", "mapped by `survey -> pm (when ok)`", "`survey -> pm (when not ok)` maps none"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the finding does not say %q: %s", want, f.Detail)
		}
	}
	if r.Failing() {
		t.Fatalf("a field mapped on another path is not a defect: %s", r.Render())
	}
	if !r.Clean() {
		t.Fatalf("the path-only kind warns, it does not hold the bot: %s", r.Render())
	}
}

// The never-mapped case — no incoming edge of the node maps the field —
// stays the unresolved_ref it always was: a --strict failure, worded with
// the fact that says so (empty on every path).
func TestAnInputMappedByNoEdgeStaysTheStrictKind(t *testing.T) {
	src := strings.Replace(mappedElsewhereBot, "  survey -> pm when ok with { feedback: \"{{outputs.survey.note}}\" }\n", "  survey -> pm when ok\n", 1)
	r, err := Run(context.Background(), compileBot(t, src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	unresolved := findingsOf(r, KindUnresolvedRef)
	var inputFindings []Finding
	for _, f := range unresolved {
		if strings.Contains(f.Detail, "{{input.feedback}}") {
			inputFindings = append(inputFindings, f)
		}
	}
	if len(inputFindings) == 0 {
		t.Fatalf("the never-mapped input read is not named: %+v", r.Findings)
	}
	for _, f := range inputFindings {
		if !strings.Contains(f.Detail, "no incoming edge of this node maps") || !strings.Contains(f.Detail, "empty on every path") {
			t.Errorf("the finding does not say the field is never mapped: %s", f.Detail)
		}
	}
	if findingsOf(r, KindUnmappedOnPath) != nil {
		t.Fatalf("a never-mapped field read as a path-only case: %+v", r.Findings)
	}
	if !r.Failing() {
		t.Fatalf("a field no incoming edge maps is empty on every run — a defect: %s", r.Render())
	}
}

// An entry node reads the run-level payload the compiler and the dry run
// cannot know: the finding keeps the generic wording, never claims an
// incoming edge said anything.
func TestAnEntryInputReadKeepsTheGenericWording(t *testing.T) {
	src := `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  user: pu
  output: verdict

prompt pu:
  Goal: {{input.goal}}

workflow en:
  worktree: none
  sandbox: none
  entry: survey
  survey -> done
`
	r, err := Run(context.Background(), compileBot(t, src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := findingsOf(r, KindUnresolvedRef)
	if len(found) == 0 {
		t.Fatalf("the entry's input read was not named: %+v", r.Findings)
	}
	for _, f := range found {
		if strings.Contains(f.Detail, "{{input.goal}}") && strings.Contains(f.Detail, "incoming") {
			t.Errorf("the entry's finding claims facts about incoming edges it has none of: %s", f.Detail)
		}
	}
}

// The clean verdict must not deny what the report lists: with a
// path-only input read standing, the sentence names the exception.
func TestTheCleanVerdictNamesThePathOnlyException(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, mappedElsewhereBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean() {
		t.Fatalf("the path-only kind warns, it does not hold the bot: %s", r.Render())
	}
	if out := r.Render(); !strings.Contains(out, "nothing left as written but what another path supplies") {
		t.Fatalf("the clean verdict denies the finding it lists:\n%s", out)
	}
}

// The golden-master shape: the loop HEAD is the entry, its only incoming
// edge is the back-edge and it maps the field — the field is legitimately
// absent before the first crossing. The wording must say the mapping edge
// and the timing, never an empty "maps none".
func TestALoopHeadEntryInputReadsAsItsOwnWording(t *testing.T) {
	src := `schema verdict:
  ok: bool
  note: string

agent survey:
  model: "claude-opus-4-7"
  output: verdict

prompt pu:
  Log: {{input.fail_log}}

agent looped:
  model: "claude-opus-4-7"
  user: pu
  output: verdict

workflow lh:
  worktree: none
  sandbox: none
  entry: looped
  budget:
    max_iterations: 20
  looped -> survey when ok
  survey -> looped as again(5) with { fail_log: "{{outputs.survey.note}}" }
  looped -> done when not ok
`
	r, err := Run(context.Background(), compileBot(t, src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := findingsOf(r, KindUnmappedOnPath)
	if len(found) == 0 {
		t.Fatalf("the loop head's first-pass read is not named: %+v", r.Findings)
	}
	for _, f := range found {
		if !strings.Contains(f.Detail, "mapped by `survey -> looped`") || !strings.Contains(f.Detail, "a loop's first pass") {
			t.Errorf("the finding does not say the mapping edge and the timing: %s", f.Detail)
		}
		if strings.Contains(f.Detail, "maps none") {
			t.Errorf("the finding claims an edge maps none when every edge maps it: %s", f.Detail)
		}
	}
}
