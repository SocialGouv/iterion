package dryrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bot with an agent whose prompt reads a node that has not run and a
// field its input never carries, a tool that would touch the workspace, a
// tool whose command the shell refuses, a judge, a bounded loop with no
// exit at its cap, and a human gate.
const dryBot = `vars:
  goal: string
  attempts: int = 2

schema verdict:
  ok: bool
  note: string

prompt survey_user:
  Survey {{vars.goal}}; last note {{outputs.assess.note}}; hint {{input.missing}}.

agent survey:
  model: "claude-opus-4-7"
  user: survey_user
  output: verdict

schema ready_payload:
  revision: string

emit ping:
  event: "ready"
  with {
    revision: "{{outputs.survey.note}}"
  }

wait hold:
  event: "ready"
  timeout: "30s"
  output: ready_payload

wait orphan:
  event: "never"
  timeout: "30s"

tool probe:
  command: "echo {{outputs.assess.note}}"

tool build:
  command: "touch {{vars.goal}}.marker"

tool broken:
  command: "if [ -f x ]; then echo"

judge assess:
  model: "claude-opus-4-7"
  output: verdict

human sign_off:
  output: verdict
  interaction: human

workflow dry:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 60
  survey -> ping
  ping -> hold
  hold -> orphan
  orphan -> probe
  probe -> build
  build -> broken
  broken -> assess
  assess -> survey when not ok as retry(3)
  assess -> sign_off when ok
  sign_off -> done
`

func compileBot(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("dry.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("the fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("the fixture does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

func findingsOf(r *Report, kind Kind) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// Two passes run the whole graph without a model, a shell or the
// workspace: the human is answered, the tool's command is never run, the
// prompt's unresolved references and the shell's refusal are named, the
// true pass finishes through the gate and the false pass dies at the
// loop's cap — the death the exhaustion warning is about.
func TestADryRunMeetsTheGraphWithoutTheWorld(t *testing.T) {
	wf := compileBot(t, dryBot)
	work := t.TempDir()
	r, err := Run(context.Background(), wf, Options{WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(work, "x.marker")); statErr == nil {
		t.Fatal("the tool's command ran: the workspace carries its marker")
	}
	if len(r.Passes) != 2 || !r.Passes[0].Bias || r.Passes[1].Bias {
		t.Fatalf("passes came out as %+v", r.Passes)
	}
	yes, no := r.Passes[0], r.Passes[1]
	if yes.Status != "finished" || !contains(yes.Nodes, "sign_off") || !contains(yes.Nodes, "done") {
		t.Fatalf("the true pass did not finish through the human gate: %+v", yes)
	}
	if no.Status == "finished" || no.Failure == "" {
		t.Fatalf("the false pass did not die at the loop's cap: %+v", no)
	}
	loops := 0
	for _, e := range no.Edges {
		if e.From == "assess" && e.To == "survey" {
			loops++
		}
	}
	if loops != 3 {
		t.Fatalf("the bounded loop ran %d times on the false pass, want 3: %+v", loops, no.Edges)
	}
	unresolved := findingsOf(r, KindUnresolvedRef)
	var refs []string
	for _, f := range unresolved {
		refs = append(refs, f.Node+":"+f.Where+":"+f.Detail)
	}
	joined := strings.Join(refs, "\n")
	for _, want := range []string{"survey:user prompt:{{outputs.assess.note}}", "survey:user prompt:{{input.missing}}", "probe:command:{{outputs.assess.note}}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the unresolved reference %q was not named:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "vars.goal") {
		t.Errorf("a var without a default was not shaped for the launch:\n%s", joined)
	}
	// The waits were answered at once: the emitted event's payload reached
	// the first wait, the orphan wait went on with nothing — the run would
	// otherwise sit 30 seconds on it and fail.
	for _, id := range []string{"ping", "hold", "orphan", "probe"} {
		if !contains(yes.Nodes, id) {
			t.Errorf("the true pass did not reach %s: %v", id, yes.Nodes)
		}
	}
	syntax := findingsOf(r, KindShellSyntax)
	if len(syntax) != 1 || syntax[0].Node != "broken" || syntax[0].Where != "command" || !strings.Contains(syntax[0].Detail, "syntax error") {
		t.Fatalf("the shell's refusal was not named at the broken tool: %+v", syntax)
	}
	for _, f := range findingsOf(r, KindUnresolvedRef) {
		if f.Node == "build" {
			t.Fatalf("a resolved command was reported: %+v", f)
		}
	}
	if !contains(r.Shaped, "assess") || !contains(r.Shaped, "sign_off") || contains(r.Shaped, "build") {
		t.Fatalf("shaped came out as %v", r.Shaped)
	}
	if len(r.UnvisitedNodes) != 0 {
		t.Fatalf("both passes together left nodes unvisited: %v", r.UnvisitedNodes)
	}
	if r.Clean() {
		t.Fatal("a report with an unresolved reference and a shell refusal reads clean")
	}
	if out := r.Render(); !strings.Contains(out, "findings (") || !strings.Contains(out, "broken [shell_syntax]") {
		t.Fatalf("the rendering does not carry the findings:\n%s", out)
	}
}

// A fixture answers the node it names — the false pass no longer dies at
// the cap once the judge's fixture says ok — and the nodes without one are
// reported as shapes.
func TestFixturesAnswerTheNodesTheyName(t *testing.T) {
	wf := compileBot(t, dryBot)
	r, err := Run(context.Background(), wf, Options{
		WorkDir:  t.TempDir(),
		Fixtures: map[string]map[string]any{"assess": {"ok": true, "note": "fine"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passes[1].Status != "finished" {
		t.Fatalf("the judge's fixture did not steer the false pass to done: %+v", r.Passes[1])
	}
	missing := findingsOf(r, KindNoFixture)
	var nodes []string
	for _, f := range missing {
		nodes = append(nodes, f.Node)
	}
	if !contains(nodes, "survey") || !contains(nodes, "sign_off") || contains(nodes, "assess") {
		t.Fatalf("no_fixture named %v", nodes)
	}
	if contains(r.Shaped, "assess") {
		t.Fatalf("a node answered by a fixture is listed as a shape: %v", r.Shaped)
	}
}

// The shapes: an enum takes its first value then its last, a bool the
// bias, and a var without a default a shape of its type.
func TestShapesFollowTheBias(t *testing.T) {
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{
		{Name: "mode", Type: ir.FieldTypeString, EnumValues: []string{"fast", "slow"}},
		{Name: "ok", Type: ir.FieldTypeBool},
		{Name: "n", Type: ir.FieldTypeInt},
		{Name: "tags", Type: ir.FieldTypeStringArray},
	}}
	yes := Synthesize(schema, true)
	no := Synthesize(schema, false)
	if yes["mode"] != "fast" || no["mode"] != "slow" || yes["ok"] != true || no["ok"] != false || yes["n"] != int64(1) {
		t.Fatalf("shapes came out as %v / %v", yes, no)
	}
	if tags, ok := yes["tags"].([]any); !ok || len(tags) != 1 {
		t.Fatalf("a list shape is not one element: %v", yes["tags"])
	}
	if Synthesize(nil, true) == nil {
		t.Fatal("a node without a schema produces nil, not an empty output")
	}
	if v := VarValue(&ir.Var{Name: "goal", Type: ir.VarString}, true); v != "x" {
		t.Fatalf("a string var's shape is %v", v)
	}
	if v := VarValue(&ir.Var{Name: "m", Type: ir.VarString, EnumValues: []string{"a", "b"}}, false); v != "b" {
		t.Fatalf("an enum var's false shape is %v", v)
	}
}

// The shell checker parses, never runs: a valid text passes, a broken one
// is refused with the interpreter's message, an interpreter without a
// checker is said so.
func TestBashChecksSyntaxWithoutRunning(t *testing.T) {
	work := t.TempDir()
	marker := filepath.Join(work, "ran")
	if err := (Bash{}).Check("bash", "touch "+marker+"\necho done"); err != nil {
		t.Fatalf("a valid text was refused: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checker ran the text")
	}
	if err := (Bash{}).Check("bash", "if [ -f x ]; then echo"); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("a broken text passed: %v", err)
	}
	if err := (Bash{}).Check("py", "print("); err == nil || !strings.Contains(err.Error(), ErrNoChecker.Error()) {
		t.Fatalf("an interpreter without a checker was judged: %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
