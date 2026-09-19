package dryrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
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
	if v := VarValue(&ir.Var{Name: "goal", Type: ir.VarString}, true, false); v != "x" {
		t.Fatalf("a string var's shape is %v", v)
	}
	if v := VarValue(&ir.Var{Name: "m", Type: ir.VarString, EnumValues: []string{"a", "b"}}, false, false); v != "b" {
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

// A router that fans out, two branches, a join.
const fanBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow fan:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split
  split -> b1
  split -> b2
  b1 -> join
  b2 -> join
  join -> done
`

// A bot with nothing to report — every reference resolves, every command
// parses — whose bounded loop has no exit at its cap: the false pass dies
// there, and only the pass status says so.
const dyingBot = `schema verdict:
  ok: bool

agent check:
  model: "claude-opus-4-7"
  output: verdict

judge assess:
  model: "claude-opus-4-7"
  output: verdict

workflow d:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 20
  check -> assess
  assess -> check when not ok as retry(2)
  assess -> done when ok
`

// A bot whose refusal is a fail node it declares.
const refusingBot = `schema verdict:
  ok: bool

agent check:
  model: "claude-opus-4-7"
  output: verdict

fail refused:
  code: REFUSED
  message: "the check said no"

workflow f:
  worktree: none
  sandbox: none
  entry: check
  check -> done when ok
  check -> refused when not ok
`

// A bot that reads a declared secret and a declared attachment in a prompt
// and in a command, and runs a script under the default interpreter.
const secretBot = `vars:
  goal: string

secrets:
  api_key: "${API_KEY}"

attachments:
  spec: file
    description: "the spec"

prompt u:
  Use {{secrets.api_key}} on {{attachments.spec}} ({{attachments.spec.url}}) for {{vars.goal}}.

agent a:
  model: "claude-opus-4-7"
  user: u

tool t:
  command: "head -c 1 {{attachments.spec}} >/dev/null; curl -sI {{attachments.spec.url}} >/dev/null; echo {{secrets.api_key}}"

tool s:
  script: "cat <<< bashism"

workflow sec:
  worktree: none
  sandbox: none
  entry: a
  a -> t
  t -> s
  s -> done
`

// The dry run leaves the operator's place alone: nothing is written where
// the process sits — the engine's mirrors land in the run's own temporary
// directory.
func TestADryRunLeavesTheOperatorsPlaceUntouched(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	if _, err := Run(context.Background(), compileBot(t, dryBot), Options{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the dry run wrote where the process sits: %v", names)
	}
}

// Clean reads the passes: a pass that dies is not clean even without a
// finding; a pass that ends at a fail node the bot declares is.
func TestCleanReadsThePasses(t *testing.T) {
	dying, err := Run(context.Background(), compileBot(t, dyingBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dying.Findings) != 0 {
		t.Fatalf("the dying bot has findings, the pass status is not isolated: %+v", dying.Findings)
	}
	if dying.Passes[1].Status == "finished" || dying.Passes[1].Deliberate || dying.Clean() {
		t.Fatalf("a pass that died at the loop's cap with nothing else to report reads as finished, deliberate or clean: %+v", dying.Passes[1])
	}
	refusing, err := Run(context.Background(), compileBot(t, refusingBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if refusing.Passes[0].Status != "finished" || refusing.Passes[1].Deliberate != true || refusing.Passes[1].Status == "finished" {
		t.Fatalf("the refusal was not read as deliberate: %+v", refusing.Passes)
	}
	if !refusing.Clean() {
		t.Fatalf("a bot whose false pass ends at its own fail node is not clean: %+v %+v", refusing.Passes, refusing.Findings)
	}
	if out := refusing.Render(); !strings.Contains(out, "a fail node the bot declares") {
		t.Fatalf("the rendering does not say the end was deliberate:\n%s", out)
	}
}

// The edges a fan-out takes are covered, though the engine emits no
// edge_selected for them.
func TestFanOutEdgesAreCovered(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, fanBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.UnvisitedEdges) != 0 || len(r.UnvisitedNodes) != 0 {
		t.Fatalf("a fan-out left edges or nodes unvisited: %v %v", r.UnvisitedEdges, r.UnvisitedNodes)
	}
	taken := map[Edge]bool{}
	for _, e := range r.Passes[0].Edges {
		taken[e] = true
	}
	if !taken[Edge{"split", "b1"}] || !taken[Edge{"split", "b2"}] {
		t.Fatalf("the fan-out's edges were not recorded: %v", r.Passes[0].Edges)
	}
}

// A declared secret and a declared attachment are not unresolved
// references: the prompt and the command render with a placeholder. A
// script without a language runs under sh, which the images ship as dash:
// a bashism is refused.
func TestDeclaredSecretsAndAttachmentsResolveAndShIsDash(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, secretBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if u := findingsOf(r, KindUnresolvedRef); len(u) != 0 {
		t.Fatalf("declared secrets or attachments were reported unresolved: %+v", u)
	}
	if _, err := exec.LookPath("dash"); err != nil {
		t.Skip("no dash on this host: sh text is unchecked here")
	}
	syntax := findingsOf(r, KindShellSyntax)
	if len(syntax) != 1 || syntax[0].Node != "s" || syntax[0].Where != "script" {
		t.Fatalf("the bashism in a default-interpreter script was not refused by dash: %+v (all: %+v)", syntax, r.Findings)
	}
}

// A fixture is held to the node's schema by the production validator, and
// the nodes it pins are named.
func TestFixturesAreHeldToTheSchema(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, dryBot), Options{
		Fixtures: map[string]map[string]any{"assess": {"ok": "not-a-bool", "note": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := findingsOf(r, KindFixture)
	if len(bad) == 0 || bad[0].Node != "assess" {
		t.Fatalf("an off-schema fixture passed unsaid: %+v", r.Findings)
	}
	if !contains(r.Pinned, "assess") || contains(r.Shaped, "assess") {
		t.Fatalf("pinned %v shaped %v", r.Pinned, r.Shaped)
	}
	if r.Clean() {
		t.Fatal("a report with an off-schema fixture reads clean")
	}
	if out := r.Render(); !strings.Contains(out, "pinned by fixtures: assess") {
		t.Fatalf("the rendering does not name the pinned node:\n%s", out)
	}
}

// A checker that does not answer in time says so — never a syntax verdict,
// never "no checker".
func TestAShellCheckTimeoutIsSaid(t *testing.T) {
	err := (Bash{Timeout: time.Nanosecond}).Check("bash", "echo slow")
	if !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("a timed-out check came out as %v", err)
	}
}

// A bot whose tool echoes a value that carries braces of its own.
const bracesBot = `schema verdict:
  ok: bool
  note: string

agent survey:
  model: "claude-opus-4-7"
  output: verdict

tool echoit:
  command: "echo {{outputs.survey.note}} ; echo done"

tool sc:
  language: python
  script: "x = {{input.missing}}"

workflow b:
  worktree: none
  sandbox: none
  entry: survey
  survey -> echoit
  echoit -> sc
  sc -> done
`

// A value that carries `{{…}}` is a value, not a reference: the renderer
// names what it left as written, the rendered text is never re-read. And a
// script's hole, rendered null, is named all the same.
func TestAValueCarryingBracesIsNotAnUnresolvedReference(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, bracesBot), Options{
		Fixtures: map[string]map[string]any{"survey": {"ok": true, "note": "please fill {{vars.goal}} in"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var script, phantom bool
	for _, f := range findingsOf(r, KindUnresolvedRef) {
		switch {
		case f.Node == "sc" && f.Where == "script" && strings.Contains(f.Detail, "{{input.missing}}"):
			script = true
		case f.Node == "echoit":
			phantom = true
		}
	}
	if phantom {
		t.Fatalf("a value carrying braces was read as an unresolved reference: %+v", r.Findings)
	}
	if !script {
		t.Fatalf("the script's null hole was not named: %+v", r.Findings)
	}
}

// A child whose judge decides, a refusal it declares, a celebration only a
// yes reaches.
const kidBot = `schema verdict:
  ok: bool

judge judge:
  model: "claude-opus-4-7"
  output: verdict

tool celebrate:
  command: "echo yes"

fail refused:
  code: REFUSED
  message: "no"

workflow kid:
  worktree: none
  sandbox: none
  entry: judge
  judge -> celebrate when ok
  judge -> refused when not ok
  celebrate -> done
`

// A parent that hands its work to one child.
const parentBot = `schema verdict:
  ok: bool

subbot kid:
  source: "kid.bot"
  output: verdict

workflow p:
  worktree: none
  sandbox: none
  entry: kid
  kid -> done
`

func withChild(t *testing.T, child string) Options {
	t.Helper()
	wf := compileBot(t, child)
	return Options{Path: "/nowhere/main.bot", Children: func(parent, source string) (string, *ir.Workflow, error) {
		if source != "kid.bot" {
			return "", nil, nil
		}
		return "/nowhere/kid.bot", wf, nil
	}}
}

// A child is read like the parent: its passes are on the report and a
// child that dies is not clean; a refusal it declares is; fixtures reach it
// under `node/child_node` and pin it; what no pass reached in it is named
// under its node; a fixture naming nothing is a finding, at either level.
func TestAChildIsReadLikeTheParent(t *testing.T) {
	parent := compileBot(t, parentBot)

	dying, err := Run(context.Background(), parent, withChild(t, dyingBot))
	if err != nil {
		t.Fatal(err)
	}
	if len(dying.Children) != 2 || dying.Children[0].Node != "kid" || !dying.Children[0].Bias || dying.Children[1].Bias {
		t.Fatalf("the child's passes are not on the report in node then bias order: %+v", dying.Children)
	}
	if dying.Children[1].Status == "finished" || dying.Children[1].Deliberate {
		t.Fatalf("the child's death at its loop's cap was not read: %+v", dying.Children[1])
	}
	if dying.Clean() {
		t.Fatalf("a parent whose child died reads clean: %+v", dying.Children)
	}
	if out := dying.Render(); !strings.Contains(out, "child kid (kid.bot) pass false") || !strings.Contains(out, "verdict: not clean") {
		t.Fatalf("the rendering does not carry the child's pass and the verdict:\n%s", out)
	}

	opts := withChild(t, kidBot)
	opts.Fixtures = map[string]map[string]any{"kid/judge": {"ok": false}, "kid/nope": {"x": 1}, "nope": {"x": 1}}
	refusing, err := Run(context.Background(), parent, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range refusing.Children {
		if !c.Deliberate || c.Status == "finished" {
			t.Fatalf("the child's declared refusal was not read as deliberate on pass %v: %+v", c.Bias, c)
		}
	}
	if !contains(refusing.Pinned, "kid/judge") {
		t.Fatalf("the child's fixture did not pin its judge: pinned %v", refusing.Pinned)
	}
	if !contains(refusing.UnvisitedNodes, "kid/celebrate") {
		t.Fatalf("what no child pass reached is not named under the child: %v", refusing.UnvisitedNodes)
	}
	var edge bool
	for _, e := range refusing.UnvisitedEdges {
		if e.From == "kid/judge" && e.To == "kid/celebrate" {
			edge = true
		}
	}
	if !edge {
		t.Fatalf("the child's edge no pass took is not named: %v", refusing.UnvisitedEdges)
	}
	unknown := map[string]bool{}
	for _, f := range findingsOf(refusing, KindFixture) {
		if strings.Contains(f.Detail, "names no node") {
			unknown[f.Node] = true
		}
	}
	if !unknown["nope"] || !unknown["kid/nope"] {
		t.Fatalf("a fixture naming nothing is silent at some level: %+v", refusing.Findings)
	}
	if refusing.Clean() {
		t.Fatal("two fixtures naming nothing and the report reads clean")
	}
	delete(opts.Fixtures, "nope")
	delete(opts.Fixtures, "kid/nope")
	pinnedOnly, err := Run(context.Background(), parent, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !pinnedOnly.Clean() {
		t.Fatalf("a child refusing as declared, pinned by a fixture, is not clean: %+v %+v", pinnedOnly.Children, pinnedOnly.Findings)
	}
	raw, err := json.Marshal(pinnedOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"clean":true`) || !strings.Contains(string(raw), `"children":[`) {
		t.Fatalf("the JSON carries neither the verdict nor the children:\n%s", raw)
	}
	if raw, _ = json.Marshal(dying); !strings.Contains(string(raw), `"clean":false`) {
		t.Fatalf("the JSON of a dirty report says clean:\n%s", raw)
	}
}

// A child that takes its time: a BOUNDED loop (the liveness monitor stops
// only an unbounded one that makes no progress) over a tool whose shell
// check the test makes slow — three hundred crossings at twenty
// milliseconds each, six seconds if nothing cuts it.
const slowKid = `schema verdict:
  ok: bool

agent check:
  model: "claude-opus-4-7"
  output: verdict

tool slow:
  command: "echo slow"

workflow spin:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 1000
  check -> slow
  slow -> check as spin(300)
`

// slowChecker is a shell checker that sleeps: the one way a dry run spends
// wall time, since nothing else in it waits.
type slowChecker struct{ d time.Duration }

func (s slowChecker) Check(string, string) error {
	time.Sleep(s.d)
	return nil
}

// A simulated child runs within what is left of the parent's pass — the
// caller's deadline reaches it through the node that hands it work — never
// on a fresh budget of its own.
func TestAChildRunsWithinWhatIsLeftOfThePass(t *testing.T) {
	parent := compileBot(t, parentBot)
	opts := withChild(t, slowKid)
	opts.Timeout = time.Hour
	opts.Shell = slowChecker{20 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := Run(ctx, parent, opts)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the dry run took %s: the child ran on a budget of its own", elapsed)
	}
	if len(r.Children) == 0 || r.Children[0].Status == "finished" || !strings.Contains(r.Children[0].Failure, "deadline") {
		t.Fatalf("the child was not cut by the parent's deadline: %+v", r.Children)
	}
}

// The reason a dry run gives for a reference kept as written comes from the
// program it holds: a declared attachment that resolves to nothing is said
// declared, an undeclared one undeclared.
func TestTheReasonComesFromTheProgram(t *testing.T) {
	x := NewExecutor(compileBot(t, secretBot), true, nil, nil)
	if why := x.whyUnresolved("attachments.spec.url"); !strings.Contains(why, "declared, but its \"url\"") {
		t.Fatalf("a declared attachment: %q", why)
	}
	if why := x.whyUnresolved("attachments.nope"); why != "no such attachment is declared" {
		t.Fatalf("an undeclared attachment: %q", why)
	}
	if why := x.whyUnresolved("loop.nope.iteration"); !strings.Contains(why, "no such loop") {
		t.Fatalf("an undeclared loop: %q", why)
	}
	if why := x.whyUnresolved("each.items.item"); !strings.Contains(why, "with:") {
		t.Fatalf("the foreach binding: %q", why)
	}
}

// A bot whose llm router selects several routes at once, converging at a
// node that waits for all of them.
const multiRouterBot = `schema verdict:
  ok: bool

prompt routing:
  Pick the fixes.

router pick:
  mode: llm
  model: "claude-opus-4-7"
  system: routing
  multi: true

agent fix_code:
  model: "claude-opus-4-7"
  output: verdict

agent fix_docs:
  model: "claude-opus-4-7"
  output: verdict

agent verify:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow m:
  worktree: none
  sandbox: none
  entry: pick
  pick -> fix_code
  pick -> fix_docs
  fix_code -> verify
  fix_docs -> verify
  verify -> done
`

// A multi-select llm router is answered on `selected_routes` — every
// candidate on the true pass, so each branch is covered — and the passes
// finish: a correct bot is clean, and `--strict` would let it through.
func TestAMultiSelectRouterIsAnsweredOnEveryRoute(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, multiRouterBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Passes {
		if p.Status != "finished" {
			t.Fatalf("pass %v ended %q (%s): %+v", p.Bias, p.Status, p.Failure, r.Findings)
		}
	}
	if !r.Clean() {
		t.Fatalf("a correct bot with a multi-select router is not clean: %+v", r.Findings)
	}
	if len(r.UnvisitedNodes) != 0 {
		t.Fatalf("the true pass did not cover every route: unvisited %v", r.UnvisitedNodes)
	}
}

// A pass that runs out of time is said so — the dry run's bound, not a death
// of the program — and the report is not clean, since what the pass would
// have met past that point is unknown.
func TestAPassThatRunsOutOfTimeIsSaidSo(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, dryBot), Options{Timeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Passes {
		if !p.TimedOut || p.Status == "finished" {
			t.Fatalf("pass %v under a nanosecond: %+v", p.Bias, p)
		}
	}
	if r.Clean() {
		t.Fatal("a report whose passes ran out of time reads clean")
	}
	// The pass line names the bound, and so does the verdict — each in its
	// own words, so neither can stand in for the other.
	if out := r.Render(); !strings.Contains(out, "the dry run's bound, not the program") || !strings.Contains(out, "before the run ended") || !strings.Contains(out, "--exec-timeout") {
		t.Fatalf("the rendering does not say the bound was hit, on the pass and in the verdict:\n%s", out)
	}
	whole, err := Run(context.Background(), compileBot(t, refusingBot), Options{Timeout: time.Minute})
	if err != nil || whole.timedOut() {
		t.Fatalf("a pass within its bound read as timed out: %v %+v", err, whole.Passes)
	}
}

// A field or a var typed `string[]` with an enum is shaped as a LIST of one
// enum value, never the bare value: a compute reading it as an array (the
// copilot bot's scope guard) does not die on the shape.
func TestAListOfEnumValuesIsShapedAsAList(t *testing.T) {
	enum := []string{"a", "b"}
	if v, ok := Value(ir.FieldTypeStringArray, enum, true, false).([]any); !ok || len(v) != 1 || v[0] != "a" {
		t.Fatalf("a string[] with an enum shaped as %#v", Value(ir.FieldTypeStringArray, enum, true, false))
	}
	if v, ok := Value(ir.FieldTypeStringArray, enum, false, false).([]any); !ok || len(v) != 1 || v[0] != "b" {
		t.Fatalf("the false pass shaped %#v", Value(ir.FieldTypeStringArray, enum, false, false))
	}
	if v := Value(ir.FieldTypeString, enum, true, false); v != "a" {
		t.Fatalf("a plain enum shaped as %#v", v)
	}
	if v, ok := VarValue(&ir.Var{Type: ir.VarStringArray, EnumValues: enum}, true, false).([]any); !ok || len(v) != 1 || v[0] != "a" {
		t.Fatalf("a string[] var with an enum shaped as %#v", VarValue(&ir.Var{Type: ir.VarStringArray, EnumValues: enum}, true, false))
	}
}

// A bot whose unbounded loop exits only on a verdict the dry run shapes:
// under a shape its outputs never change, so the liveness monitor stalls
// the loop on the false pass and the run falls through with no edge left
// — a ceiling the shapes imposed, not a death of the program.
const ceilingBot = `schema verdict:
  ok: bool

agent check:
  model: "claude-opus-4-7"
  output: verdict

judge assess:
  model: "claude-opus-4-7"
  output: verdict

workflow c:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 8
  check -> assess
  assess -> check when not ok as again(unbounded 100)
  assess -> done when ok
`

// A pass that runs to the bot's own ceiling — an exit riding a value the
// dry run shapes — is said so and is not a death: the report stays clean,
// where a bounded loop spent with no exit stays one.
func TestAPassAtTheBotsCeilingIsNotADeath(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, ceilingBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passes[0].Status != "finished" || !r.Passes[1].Ceiling || r.Passes[1].Status == "finished" {
		t.Fatalf("passes %+v", r.Passes)
	}
	if !r.Clean() {
		t.Fatalf("a pass at the bot's ceiling read as a death: %+v %+v", r.Passes, r.Findings)
	}
	if out := r.Render(); !strings.Contains(out, "ran to the bot's own ceiling") {
		t.Fatalf("the ceiling is not said:\n%s", out)
	}
	// The one reading of a ceiling: the bot's budget, or a stall followed
	// by the fall-through's death — never a death alone.
	for _, tc := range []struct {
		code     store.FailureCode
		declined string
		want     bool
	}{
		{store.FailureBudgetExceeded, "", true},
		{store.FailureNoOutgoingEdge, "liveness_stall", true},
		{store.FailureNoOutgoingEdge, "loop_budget_guard", true},
		{store.FailureLoopExhausted, "loop_out_of_fuel", true},
		{store.FailureLoopExhausted, "loop_cap", false},
		{store.FailureExecutionFailed, "loop_out_of_fuel", true},
		{store.FailureExecutionFailed, "loop_cap", false},
		{store.FailureNoOutgoingEdge, "", false},
		{store.FailureLoopExhausted, "", false},
		{store.FailureExecutionFailed, "", false},
		{store.FailureFailNode, "liveness_stall", false},
	} {
		if got := ceilingOf(tc.code, tc.declined); got != tc.want {
			t.Fatalf("ceilingOf(%s, %q) = %v, want %v", tc.code, tc.declined, got, tc.want)
		}
	}
}

// Two fan-out routers into the same branches, one reached, one never: the
// edges of the router no pass reached stay unvisited.
const twoRoutersBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split1:
  mode: fan_out_all

router split2:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow two:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split1 when ok
  survey -> split2 when not ok
  split1 -> b1
  split1 -> b2
  split2 -> b1
  split2 -> b2
  b1 -> join
  b2 -> join
  join -> done
`

// The edge a fan-out took is the one the engine names, never every edge
// into the branch's entry: a router no pass reached keeps its edges
// unvisited, and the router that ran has its edges covered.
func TestAFanOutEdgeIsTheOneTheEngineNames(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, twoRoutersBot), Options{
		Fixtures: map[string]map[string]any{"survey": {"ok": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	has := func(from, to string) bool {
		for _, e := range r.UnvisitedEdges {
			if e.From == from && e.To == to {
				return true
			}
		}
		return false
	}
	if !has("split2", "b1") || !has("split2", "b2") || !has("survey", "split2") {
		t.Fatalf("the router no pass reached was credited with its edges: unvisited %v", r.UnvisitedEdges)
	}
	if has("split1", "b1") || has("split1", "b2") {
		t.Fatalf("the router that ran was not credited with its edges: unvisited %v", r.UnvisitedEdges)
	}
	if !contains(r.UnvisitedNodes, "split2") {
		t.Fatalf("split2 was never reached and is not said unvisited: %v", r.UnvisitedNodes)
	}
}

// An unbounded loop the shapes stall, an exit taken, and a later node
// whose recorded output carries no field its edges read: the death is the
// program's, whatever the engine declined before it.
const staleBot = `schema verdict:
  ok: bool

agent check:
  model: "claude-opus-4-7"
  output: verdict

judge assess:
  model: "claude-opus-4-7"
  output: verdict

agent gate:
  model: "claude-opus-4-7"
  output: verdict

agent wrap:
  model: "claude-opus-4-7"
  output: verdict

workflow s:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 60
  check -> assess
  assess -> check when not ok as again(unbounded 100)
  assess -> gate
  gate -> done when ok
  gate -> wrap when not ok
  wrap -> done
`

// A decline the run moved past is not the reason of what it dies of later:
// the pass whose stalled loop fell through to its exit and then died at a
// node with no edge left is a death, and the report is not clean.
func TestADeathAfterADeclineTheRunMovedPastIsADeath(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, staleBot), Options{
		Fixtures: map[string]map[string]any{"gate": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Passes[1]
	crossings := 0
	for _, id := range p.Nodes {
		if id == "check" {
			crossings++
		}
	}
	if crossings < 2 {
		t.Fatalf("the false pass never crossed the loop, so nothing was declined before the death: %v", p.Nodes)
	}
	if p.Status == "finished" || !strings.Contains(p.Failure, "gate") {
		t.Fatalf("the false pass did not die at gate: %+v", p)
	}
	// The pass's own verdict, not the report's: the empty fixture is off
	// schema and is a finding by itself, so Clean() would be false whatever
	// the ceiling logic said.
	if p.Ceiling || !p.died() {
		t.Fatalf("a death after a decline the run moved past is read as a ceiling: %+v", p)
	}
}

// A fan-out whose branch holds a human gate: the dry run answers it like
// the trunk's, and the pass finishes.
const fanHumanBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

human ask:
  output: verdict
  interaction: human

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow fanh:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split
  split -> ask
  split -> b2
  ask -> join
  b2 -> join
  join -> done
`

// A human node inside a branch is answered by a shape as one on the trunk
// is: the pass does not pause there, and a correct bot is clean.
func TestAHumanInsideABranchIsAnswered(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, fanHumanBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Passes {
		if p.Status != "finished" || !contains(p.Nodes, "ask") {
			t.Fatalf("the branch's human was not answered: %+v", p)
		}
	}
	if !r.Clean() {
		t.Fatalf("a bot whose branch holds a human gate is not clean: %+v %+v", r.Passes, r.Findings)
	}
}

// A fan-out whose branch holds the canonical unbounded review loop, its
// exit riding a verdict the dry run shapes: under the shapes it stalls,
// and that is the ceiling — in a branch as on the trunk.
const branchCeilingBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

judge b2:
  model: "claude-opus-4-7"
  output: verdict

agent c1:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow bc:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 40
  survey -> split
  split -> b1
  split -> c1
  b1 -> b2
  b2 -> b1 when not ok as retry(unbounded 10)
  b2 -> join when ok
  c1 -> join
  join -> done
`

// A pass whose branch stalled on the shapes and had no edge left is at
// the bot's ceiling, exactly as the trunk's would be: the death carries
// its decline through the collector, and the report stays clean.
func TestACeilingInsideABranchIsACeiling(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, branchCeilingBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passes[0].Status != "finished" {
		t.Fatalf("the true pass did not finish: %+v", r.Passes[0])
	}
	p := r.Passes[1]
	if p.Status == "finished" || !p.Ceiling || !strings.Contains(p.Failure, "liveness_stall") {
		t.Fatalf("a branch's stall with no edge left is not read as a ceiling: %+v", p)
	}
	if !r.Clean() {
		t.Fatalf("a bot at its ceiling inside a branch is not clean: %+v %+v", r.Passes, r.Findings)
	}
}

// A fan-out whose branch ends at a declared fail node: the refusal is the
// program's decision, in a branch as on the trunk.
const branchRefusalBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent c1:
  model: "claude-opus-4-7"
  output: verdict

fail rejected:
  code: REJECTED
  message: "the branch said no"

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow br:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split
  split -> b1
  split -> c1
  b1 -> join when ok
  b1 -> rejected when not ok
  c1 -> join
  join -> done
`

// A refusal a branch reached is deliberate, whatever the other branches
// were doing when it was: the decision travels on the error, not on the
// order of the events.
func TestARefusalInsideABranchIsDeliberate(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, branchRefusalBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Passes[1]
	if p.Status == "finished" || !p.Deliberate {
		t.Fatalf("a branch's refusal is not read as deliberate: %+v", p)
	}
	if !r.Clean() {
		t.Fatalf("a bot that refused as declared inside a branch is not clean: %+v %+v", r.Passes, r.Findings)
	}
}

// A fan-out whose one branch stalls on the shapes (a ceiling) while the
// other spends a bounded loop with no exit (a death): every branch runs to
// its own end under the dry run, and the death is read as the death it is.
const branchMixedBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

judge b2:
  model: "claude-opus-4-7"
  output: verdict

agent c1:
  model: "claude-opus-4-7"
  output: verdict

judge c2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow bm:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 60
  survey -> split
  split -> b1
  split -> c1
  b1 -> b2
  b2 -> b1 when not ok as retry(unbounded 10)
  b2 -> join when ok
  c1 -> c2
  c2 -> c1 when not ok as fix(5)
  c2 -> join when ok
  join -> done
`

// The ceiling lands first (three crossings) and the death later (five): a
// run that cancelled the siblings of the first branch to end would never
// see the death.
func TestADeathInOneBranchBesideACeilingInAnotherIsADeath(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, branchMixedBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Passes[1]
	if p.Status == "finished" || p.Ceiling || !p.died() {
		t.Fatalf("a branch's death beside another's ceiling is not read as a death: %+v", p)
	}
	if r.Clean() {
		t.Fatalf("a bot whose branch dies is read as clean: %+v", r.Passes)
	}
}

// Both branches at a ceiling of the shapes — one stalled, one out of fuel:
// one ceiling, and the bot is clean.
func TestTwoCeilingsInTwoBranchesAreACeiling(t *testing.T) {
	src := strings.Replace(branchMixedBot, "  c2 -> c1 when not ok as fix(5)\n", "  c2 -> c1 when not ok as fix(unbounded 2)\n", 1)
	r, err := Run(context.Background(), compileBot(t, src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Passes[1]
	if p.Status == "finished" || !p.Ceiling {
		t.Fatalf("two ceilings are not read as one: %+v", p)
	}
	if !r.Clean() {
		t.Fatalf("a bot at its ceiling in both branches is not clean: %+v %+v", r.Passes, r.Findings)
	}
}

// A parent whose subbot sits inside a bounded loop: the child is crossed
// once, then three more times on the false pass.
const loopingParentBot = `schema verdict:
  ok: bool

subbot kid:
  source: "kid.bot"
  output: verdict

judge check:
  model: "claude-opus-4-7"
  output: verdict

workflow p:
  worktree: none
  sandbox: none
  entry: kid
  budget:
    max_iterations: 30
  kid -> check
  check -> kid when not ok as again(3)
  check -> done when ok
`

// A child inside a loop is simulated once per pass and counted on every
// crossing: the report carries one child pass per bias, saying how many
// times it was crossed, not one per crossing.
func TestAChildInsideALoopIsSimulatedOnceAndCounted(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, loopingParentBot), withChild(t, kidBot))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Children) != 2 {
		t.Fatalf("one child pass per bias expected, got %d: %+v", len(r.Children), r.Children)
	}
	crossings := map[bool]int{}
	for _, c := range r.Children {
		if c.Node != "kid" || len(c.Nodes) == 0 {
			t.Fatalf("the child pass is not the kid's: %+v", c)
		}
		crossings[c.Bias] = c.Crossings
	}
	if crossings[true] != 1 || crossings[false] != 4 {
		t.Fatalf("crossings not counted: %v", crossings)
	}
	if out := r.Render(); !strings.Contains(out, "crossed 4 times") {
		t.Fatalf("the crossings are not said:\n%s", out)
	}
}

// #1318 — a fan_out_each router whose `over:` reads a `json`-typed output
// field takes an array shape; today it took the default object shape and
// died "expected an array" on both passes. The router now fires once (one
// element on the true pass, empty on the false pass), and the whole
// downstream subgraph reads on the true pass.
const fanOutEachOverJSONBot = `schema plan_out:
  items: json

agent plan:
  model: "claude-opus-4-7"
  output: plan_out

agent work:
  model: "claude-opus-4-7"
  system: work_sys

prompt work_sys:
  Work on {{input.item}}

router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.items}}"

workflow probe:
  worktree: none
  sandbox: none
  entry: plan
  plan -> dispatch
  dispatch -> work
  work -> done
`

func TestAJSONOutputConsumedByFanOutEachIsShapedAsAnArray(t *testing.T) {
	wf := compileBot(t, fanOutEachOverJSONBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Passes) != 2 {
		t.Fatalf("two passes expected, got %+v", r.Passes)
	}
	for _, p := range r.Passes {
		if strings.Contains(p.Failure, "expected an array") {
			t.Fatalf("pass bias=%v still died on the array shape: %+v", p.Bias, p)
		}
	}
	if !contains(r.Passes[0].Nodes, "work") {
		t.Fatalf("the true pass did not fire the fan-out branch: %+v", r.Passes[0])
	}
	// The report never lists `work` as unvisited: the true pass reached it.
	for _, id := range r.UnvisitedNodes {
		if id == "work" {
			t.Fatalf("work unvisited: %+v", r.UnvisitedNodes)
		}
	}
}

// #1456 — a compute expression `concat(outputs.a.f, outputs.b.g)` over two
// `json` output fields no longer dies "argument 1 is map, want array" on
// either pass: the two fields are shaped as arrays, `concat` returns an
// array on both passes, the pass finishes.
const concatOverTwoJSONBot = `schema batch:
  items: json

agent first_bank:
  model: "claude-opus-4-7"
  output: batch

agent second_bank:
  model: "claude-opus-4-7"
  output: batch

schema merged:
  all: json

compute merge:
  output: merged
  expr:
    all: "concat(outputs.first_bank.items, outputs.second_bank.items)"

workflow probe:
  worktree: none
  sandbox: none
  entry: first_bank
  first_bank -> second_bank
  second_bank -> merge
  merge -> done
`

func TestAJSONOutputConsumedByAnArrayFunctionIsShapedAsAnArray(t *testing.T) {
	wf := compileBot(t, concatOverTwoJSONBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Passes {
		if strings.Contains(p.Failure, "want array") {
			t.Fatalf("pass bias=%v still died on the array shape: %+v", p.Bias, p)
		}
		if p.Status != "finished" {
			t.Fatalf("pass bias=%v did not finish: %+v", p.Bias, p)
		}
	}
}

// #1318 (unit) — Synthesize shapes a `json` field as an array when the
// caller says so, and as an object otherwise. The rule holds on both
// passes: a shape read as an array is an array whichever end of the enum
// walk the pass takes.
func TestSynthesizeShapesJSONAsArrayWhenConsumerNeedsIt(t *testing.T) {
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{
		{Name: "items", Type: ir.FieldTypeJSON},
		{Name: "note", Type: ir.FieldTypeJSON},
	}}
	arrayFields := map[string]bool{"items": true}
	for _, bias := range []bool{true, false} {
		out := SynthesizeAt(schema, bias, arrayFields)
		if _, ok := out["items"].([]any); !ok {
			t.Fatalf("bias=%v: `items` shaped %T, want []any", bias, out["items"])
		}
		if _, ok := out["note"].(map[string]any); !ok {
			t.Fatalf("bias=%v: `note` shaped %T, want map[string]any", bias, out["note"])
		}
	}
}

// #1325 — a `best_effort` fan-out whose branches ALL die of the same
// program-shape death (a bounded loop with no exit at its cap) is no
// longer read as clean: the pass records the dead branches, and Clean()
// reads false. A branch that ran to a ceiling — the shapes' doing — stays
// what it is (TestACeilingInsideABranchIsACeiling holds that end).
const bestEffortAllBranchesDeadBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: best_effort

workflow br:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 40
  survey -> split
  split -> b1
  split -> b2
  b1 -> b1 when not ok as fix_a(2)
  b2 -> b2 when not ok as fix_b(2)
  b1 -> join when ok
  b2 -> join when ok
  join -> done
`

func TestABestEffortFanOutWhereEveryBranchDiesIsNotClean(t *testing.T) {
	wf := compileBot(t, bestEffortAllBranchesDeadBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// On the false pass every bounded loop is spent (bias=false picks the
	// "not ok" branch), every branch dies LOOP_EXHAUSTED with no exit,
	// best_effort keeps the join and the run finishes at done. The dry run
	// used to read that as clean; it now records the dead branches.
	p := r.Passes[1]
	if p.Status != "finished" {
		t.Fatalf("best_effort with all branches dead should still finish at done: %+v", p)
	}
	if len(p.DeadBranches) == 0 {
		t.Fatalf("best_effort with every branch dying reads no dead branch: %+v", p)
	}
	if r.Clean() {
		t.Fatalf("a fan-out whose branches all died reads clean: %+v", r)
	}
	if out := r.Render(); !strings.Contains(out, "dead branch") {
		t.Fatalf("the rendering does not name the dead branches:\n%s", out)
	}
}

// #1325 (twin) — a `best_effort` fan-out whose branches all end at a
// declared `fail` node reads as CLEAN: a deliberate refusal is the
// program's word, whatever the fan-out mode.
const bestEffortAllBranchesRefuseBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

fail rejected_a:
  code: REJECTED
  message: "a said no"

fail rejected_b:
  code: REJECTED
  message: "b said no"

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: best_effort

workflow br:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split
  split -> b1
  split -> b2
  b1 -> join when ok
  b1 -> rejected_a when not ok
  b2 -> join when ok
  b2 -> rejected_b when not ok
  join -> done
`

func TestABestEffortFanOutWhereEveryBranchRefusesIsClean(t *testing.T) {
	wf := compileBot(t, bestEffortAllBranchesRefuseBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The false pass ends every branch at a declared fail; best_effort
	// converges at join, the run reaches done. No dead branch.
	p := r.Passes[1]
	if p.Status != "finished" {
		t.Fatalf("best_effort with every branch refusing should finish at done: %+v", p)
	}
	if len(p.DeadBranches) != 0 {
		t.Fatalf("a branch that ended at a declared fail is not a dead branch: %+v", p.DeadBranches)
	}
	if !r.Clean() {
		t.Fatalf("a fan-out whose branches all refused as declared is not clean: %+v", r)
	}
}

// #1318 (foreach twin) — `foreach` is the other iteration surface of the
// DSL: a `foreach` edge's collection ref reads a `json` output field or a
// `json` var the same way `fan_out_each`'s `over:` does. The class rule
// is that both iteration surfaces mark their collection as
// array-consumed, so a bot that reads its collection from a `json`-typed
// output or var stays clean under `--strict`.
const foreachIterationBot = `schema plan_out:
  items: json

vars:
  extras: json

agent decompose:
  model: "claude-opus-4-7"
  output: plan_out

agent visit:
  model: "claude-opus-4-7"

agent visit_extra:
  model: "claude-opus-4-7"

workflow probe:
  worktree: none
  sandbox: none
  entry: decompose
  decompose -> visit
  visit -> visit as foreach scan(item in "{{outputs.decompose.items}}")
  visit -> visit_extra
  visit_extra -> visit_extra as foreach scan_extra(item in "{{vars.extras}}")
  visit_extra -> done
`

// arrayConsumers and arrayConsumedVars walk `wf.Foreaches[*].CollectionRefs`
// alongside `RouterNode.OverRefs`, so a `json` field or var read by a
// foreach is shaped as an array. The runtime's own edge semantics
// (idx+1 >= count) fall through for small collections whatever the
// shape, so what this test proves is that the SHAPE walker sees both
// iteration surfaces — the walker is where a future bot's coverage rides.
func TestForeachCollectionRefsAreArrayConsumers(t *testing.T) {
	wf := compileBot(t, foreachIterationBot)
	acOut := arrayConsumers(wf)
	if got := acOut["decompose"]; got == nil || !got["items"] {
		t.Fatalf("arrayConsumers did not mark outputs.decompose.items (foreach): %#v", acOut)
	}
	acVars := arrayConsumedVars(wf)
	if !acVars["extras"] {
		t.Fatalf("arrayConsumedVars did not mark vars.extras (foreach): %#v", acVars)
	}
	// The dry-run still finishes clean: foreach on a self-loop with a
	// small collection falls through to the next outgoing edge whatever
	// the shape, but the shape rule keeps the door open for a future bot
	// whose coverage rides on the iteration count.
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean() {
		t.Fatalf("a foreach bot with json collection sources is not clean: %+v", r)
	}
}

// PR #1491 review R06ab46 + R0e2655 — a nested ref like
// `outputs.a.cfg.items` or `vars.list.items` names a nested field the
// shape rule cannot reach; arrayConsumers / arrayConsumedVars must NOT
// mark the parent (`outputs.a.cfg` / `vars.list`) as array-consumed, or
// a sibling map read (`keys(outputs.a.cfg)`) would die "keys() expects a
// map, got []interface {}". All four sites (outputs OverRefs, outputs
// Foreach.CollectionRefs, outputs ArrayContextRefs and its vars twin,
// vars OverRefs, vars Foreach.CollectionRefs, vars edge expressions,
// vars compute expressions) require an exact leaf ref.
const deepPathBot = `vars:
  cfg: json

schema cfg_out:
  cfg: json

agent survey:
  model: "claude-opus-4-7"
  output: cfg_out

schema merged:
  items: json
  fields: json
  var_items: json
  var_fields: json

compute merge:
  output: merged
  expr:
    items:      "concat(outputs.survey.cfg.items, outputs.survey.cfg.items)"
    fields:     "keys(outputs.survey.cfg)"
    var_items:  "concat(vars.cfg.items, vars.cfg.items)"
    var_fields: "keys(vars.cfg)"

workflow probe:
  worktree: none
  sandbox: none
  entry: survey
  survey -> merge
  merge -> done
`

func TestArrayConsumersMarksOnlyExactLeafRefs(t *testing.T) {
	wf := compileBot(t, deepPathBot)
	ac := arrayConsumers(wf)
	// `outputs.survey.cfg.items` and `outputs.survey.cfg` are both read,
	// but the shape rule only marks EXACT leaf refs (path length 2). The
	// nested `.items` on `cfg` is deeper — leaving `cfg` object-shaped is
	// what `keys(cfg)` expects.
	if got := ac["survey"]; got != nil && got["cfg"] {
		t.Fatalf("arrayConsumers wrongly marked outputs.survey.cfg from a nested ref: %#v", ac)
	}
	// Same for vars: `vars.cfg.items` (nested) must NOT mark `vars.cfg`
	// as array-consumed, or `keys(vars.cfg)` dies (R0e2655, the vars
	// twin of R06ab46 — the compute-expression walker in
	// arrayConsumedVars was the last remaining `>= 1` site).
	acv := arrayConsumedVars(wf)
	if acv["cfg"] {
		t.Fatalf("arrayConsumedVars wrongly marked vars.cfg from a nested ref: %#v", acv)
	}
	// The dry run stays clean: `concat(...)` on a nested-json shape returns
	// nil (indexing an object under a string key), the runtime is lenient
	// on missing keys, and `keys(cfg)` reads the map.
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean() {
		t.Fatalf("a bot with a nested json ref is not clean: %+v", r)
	}
}

// PR #1491 review R085325 — a DYNAMIC subscript (`m[vars.k]`,
// `arr[i]`) cannot be typed statically: evalIndex handles both map and
// array at runtime, and the receiver must stay OBJECT-shaped so a
// legal `cfg[vars.key]` on a `json` map does not die
// EXPRESSION_FAILED. Only an integer-literal subscript asserts the
// receiver as an array.
const dynamicSubscriptBot = `vars:
  key: string

schema cfg_out:
  cfg: json

agent survey:
  model: "claude-opus-4-7"
  output: cfg_out

schema picked:
  value: json

compute pick:
  output: picked
  expr:
    value: "outputs.survey.cfg[vars.key]"

workflow probe:
  worktree: none
  sandbox: none
  entry: survey
  survey -> pick
  pick -> done
`

func TestArrayConsumersLeavesDynamicSubscriptReceiversAlone(t *testing.T) {
	wf := compileBot(t, dynamicSubscriptBot)
	ac := arrayConsumers(wf)
	if got := ac["survey"]; got != nil && got["cfg"] {
		t.Fatalf("arrayConsumers wrongly marked outputs.survey.cfg from a dynamic subscript: %#v", ac)
	}
	r, err := Run(context.Background(), wf, Options{Inputs: map[string]any{"key": "any"}})
	if err != nil {
		t.Fatal(err)
	}
	// `cfg[vars.key]` reads a shaped map by a shaped string key — nil,
	// the runtime is lenient on missing map keys and the compute
	// produces `value: nil`. The pass stays clean.
	if !r.Clean() {
		t.Fatalf("a bot with a dynamic subscript on a json field is not clean: %+v", r)
	}
}

// PR #1491 review Rdabb2b — a best_effort branch that hits the run's
// `budget:` max_iterations is a ceiling the run's circumstances impose,
// not a death of the program. The classifier must match the trunk's
// ceilingOf: it reads `code == BUDGET_EXCEEDED` (or a ceiling
// LoopDeclined.Reason) as a ceiling. checkPreExecBudget writes a plain
// `%w`-wrapped ErrBudgetExceeded, not a typed RuntimeError, so
// errorCode must recognize the sentinel.
const bestEffortBudgetCeilingBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: best_effort

workflow br:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 3
  survey -> split
  split -> b1
  split -> b2
  b1 -> b1 when not ok as fix_a(20)
  b2 -> b2 when not ok as fix_b(20)
  b1 -> join when ok
  b2 -> join when ok
  join -> done
`

func TestABranchThatHitsTheBudgetIsACeilingNotADeadBranch(t *testing.T) {
	wf := compileBot(t, bestEffortBudgetCeilingBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A branch that dies of ErrBudgetExceeded is a run's-circumstance
	// ceiling, not a program death: DeadBranches stays empty and the
	// pass reads clean (via Ceiling on the trunk, symmetric with the
	// branch classifier). Under best_effort a budget hit cancels the
	// siblings via cancelOnFirstFailure and the trunk carries the
	// budget error — the pass's Ceiling flag settles clean.
	for _, p := range r.Passes {
		for _, d := range p.DeadBranches {
			if strings.Contains(d.Error, "budget exceeded") || d.Code == "BUDGET_EXCEEDED" {
				t.Fatalf("a budget-exceeded branch was filed as a dead branch: %+v", d)
			}
		}
	}
	if !r.Clean() {
		t.Fatalf("a bot whose branch hit the budget is not clean: %+v", r)
	}
}

// PR #1491 review question — `Pass.DeadBranches` is scoped to passes
// where a collector swallowed a branch's death: a `wait_all` fan-out
// propagates the death onto the trunk, so the pass's Status carries the
// story and DeadBranches would print twice. The recording is filtered
// after the pass ends: only Status==`finished` && !Deliberate &&
// !Ceiling keeps them.
const waitAllOneBranchDiesBot = `schema verdict:
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "claude-opus-4-7"
  output: verdict

agent b2:
  model: "claude-opus-4-7"
  output: verdict

judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow br:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 40
  survey -> split
  split -> b1
  split -> b2
  b1 -> b1 when not ok as fix_a(2)
  b1 -> join when ok
  b2 -> join
  join -> done
`

func TestWaitAllPassCarriesTheDeathItself(t *testing.T) {
	wf := compileBot(t, waitAllOneBranchDiesBot)
	r, err := Run(context.Background(), wf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The false pass: b1's loop is spent and it dies LOOP_EXHAUSTED; b2
	// finishes at join. wait_all propagates b1's death onto the trunk,
	// so the pass's Status is failed_* and Failure carries the story.
	// DeadBranches is empty — the trunk carries the death alone.
	p := r.Passes[1]
	if p.Status == "finished" {
		t.Fatalf("wait_all should propagate the death onto the trunk: %+v", p)
	}
	if len(p.DeadBranches) != 0 {
		t.Fatalf("wait_all should not double-print the death on DeadBranches: %+v", p.DeadBranches)
	}
	// The pass reads not clean via the status check (died() returns true
	// because Status != "finished" and neither Deliberate nor Ceiling).
	if r.Clean() {
		t.Fatalf("a wait_all bot whose branch died is not clean: %+v", r)
	}
}
