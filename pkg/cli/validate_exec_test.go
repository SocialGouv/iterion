package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A bot that compiles, whose first run would meet an unresolved reference
// and a command the shell refuses, and a child it hands work to.
const execBot = "vars:\n  goal: string\n\nschema verdict:\n  ok: bool\n  note: string\n\nprompt u:\n  Do {{vars.goal}}; last {{outputs.assess.note}}.\n\nagent survey:\n  model: \"claude-opus-4-7\"\n  user: u\n  output: verdict\n\ntool broken:\n  command: \"if [ -f x ]; then echo\"\n\njudge assess:\n  model: \"claude-opus-4-7\"\n  output: verdict\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: survey\n  budget:\n    max_iterations: 20\n  survey -> broken\n  broken -> assess\n  assess -> survey when not ok as retry(2)\n  assess -> done when ok\n"

// `validate --exec` runs the compiled program under a dry run and carries
// its report — in the JSON as `exec`, in the human output as a block — and
// keeps the verdict: findings of the dry run are what the first run would
// have met, not diagnostics. It never runs on a program that does not
// compile.
func TestRunValidate_ExecReportsTheDryRun(t *testing.T) {
	inTempWorkspace(t)
	if err := os.WriteFile("e.bot", []byte(execBot), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidateWith("e.bot", jp, ValidateOptions{Exec: true}); err != nil {
		t.Fatalf("validate --exec: %v\n%s", err, out.String())
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("the JSON result does not decode: %v\n%s", err, out.String())
	}
	if !res.Valid || res.Exec == nil || len(res.Exec.Passes) != 2 {
		t.Fatalf("the result carries no dry run: %+v\n%s", res.Exec, out.String())
	}
	var kinds []string
	for _, f := range res.Exec.Findings {
		kinds = append(kinds, f.Node+":"+string(f.Kind))
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "broken:shell_syntax") || !strings.Contains(joined, "survey:unresolved_ref") {
		t.Fatalf("the dry run's findings are not in the result: %s\n%s", joined, out.String())
	}
	if res.Exec.Clean() {
		t.Fatal("a program with a refused command reads clean")
	}
	hp, hout := testPrinter()
	if err := RunValidateWith("e.bot", hp, ValidateOptions{Exec: true}); err != nil {
		t.Fatalf("validate --exec: %v\n%s", err, hout.String())
	}
	for _, want := range []string{"Dry run —", "broken [shell_syntax] command", "result: OK"} {
		if !strings.Contains(hout.String(), want) {
			t.Errorf("the human output lacks %q:\n%s", want, hout.String())
		}
	}
	// Without --exec, nothing runs and nothing is carried.
	jp, out = jsonPrinter()
	if err := RunValidate("e.bot", jp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"exec"`) {
		t.Fatalf("a plain validate carries a dry run:\n%s", out.String())
	}
	// A program that does not compile is never run.
	broken := strings.Replace(execBot, "assess -> done", "assess -> nowhere", 1)
	if err := os.WriteFile("b.bot", []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidateWith("b.bot", jp, ValidateOptions{Exec: true}); err == nil {
		t.Fatalf("an invalid program validated:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"exec"`) {
		t.Fatalf("a program that does not compile was run:\n%s", out.String())
	}
}

// --fixtures answers the named nodes from a file, in either shape, and
// implies --exec; a node without a fixture is reported.
func TestRunValidate_FixturesAnswerNodes(t *testing.T) {
	inTempWorkspace(t)
	if err := os.WriteFile("e.bot", []byte(execBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("fx.json", []byte(`[{"node": "assess", "output": {"ok": true, "note": "fine"}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidateWith("e.bot", jp, ValidateOptions{Fixtures: "fx.json"}); err != nil {
		t.Fatalf("validate --fixtures: %v\n%s", err, out.String())
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Exec == nil || !strings.Contains(strings.Join(res.Exec.Pinned, ","), "assess") {
		t.Fatalf("the fixture did not pin assess: %+v", res.Exec)
	}
	seen := false
	for _, f := range res.Exec.Findings {
		if f.Kind == "no_fixture" && f.Node == "survey" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the node without a fixture was not named: %+v", res.Exec.Findings)
	}
	if err := os.WriteFile("bad.json", []byte(`"not a fixture file"`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A malformed fixture file is refused — said beside the compile verdict,
	// which is printed, the error after it marked reported.
	jp2, out2 := jsonPrinter()
	if err := RunValidateWith("e.bot", jp2, ValidateOptions{Fixtures: "bad.json"}); !errors.Is(err, ErrReported) {
		t.Fatalf("a malformed fixture file was accepted, or refused before the result: %v", err)
	}
	var refused ValidateResult
	if err := json.Unmarshal(out2.Bytes(), &refused); err != nil || !refused.Valid || refused.Exec != nil || !strings.Contains(refused.ExecError, "fixtures") {
		t.Fatalf("the refusal is not in the result: %v %+v\n%s", err, refused, out2.String())
	}
}

// A subbot child is read within the bundle's collection and simulated: its
// findings come back prefixed by the parent's node.
func TestRunValidate_ExecSimulatesTheChildren(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "parent")
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "name: parent\ndisplay_name: Parent\n")
	write("kids/k.bot", "vars:\n  goal: string\n\nschema out:\n  url: string\n\ntool bad:\n  command: \"for f in; do\"\n\nworkflow k:\n  worktree: none\n  sandbox: none\n  entry: bad\n  bad -> done\n")
	write("main.bot", "vars:\n  goal: string\n\nschema result:\n  url: string\n\nsubbot child:\n  source: \"kids/k.bot\"\n  with { goal: \"{{vars.goal}}\" }\n  output: result\n\nworkflow p:\n  worktree: none\n  sandbox: none\n  entry: child\n  child -> done\n")
	jp, out := jsonPrinter()
	if err := RunValidateWith(filepath.Join(dir, "main.bot"), jp, ValidateOptions{Exec: true}); err != nil {
		t.Fatalf("validate --exec: %v\n%s", err, out.String())
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Exec == nil {
		t.Fatalf("no dry run:\n%s", out.String())
	}
	var found, ran bool
	for _, f := range res.Exec.Findings {
		if f.Node == "child/bad" && f.Kind == "shell_syntax" {
			found = true
		}
	}
	for _, c := range res.Exec.Children {
		if c.Node == "child" && strings.Contains(c.Source, "kids/k.bot") && c.Status == "finished" {
			ran = true
		}
	}
	if !found || !ran {
		t.Fatalf("the child's broken command was not met through the parent, or its pass not carried: %+v %+v", res.Exec.Findings, res.Exec.Children)
	}
}

// A dry run that could not run — here, fixtures that cannot be read — is
// said beside the compile verdict, which stands and is printed; the error
// comes after, marked reported, so the MCP tool returns the result.
func TestRunValidate_SaysWhenTheDryRunDidNotRun(t *testing.T) {
	inTempWorkspace(t)
	if err := os.WriteFile("ok.bot", []byte("schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	err := RunValidateWith("ok.bot", jp, ValidateOptions{Fixtures: "nowhere.json"})
	if err == nil || !errors.Is(err, ErrReported) {
		t.Fatalf("a dry run that did not run returned %v, want an error marked reported", err)
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("no result printed: %v\n%s", err, out.String())
	}
	if !res.Valid || res.Exec != nil || !strings.Contains(res.ExecError, "nowhere.json") {
		t.Fatalf("result %+v", res)
	}
	hp, hout := testPrinter()
	if err := RunValidateWith("ok.bot", hp, ValidateOptions{Fixtures: "nowhere.json"}); err == nil || errors.Is(err, ErrReported) {
		t.Fatalf("human mode: %v", err)
	}
	if !strings.Contains(hout.String(), "result: OK") || !strings.Contains(hout.String(), "dry run: not run") {
		t.Fatalf("the human output lacks the verdict or the reason:\n%s", hout.String())
	}
}

// --strict makes the dry run's verdict the exit code: a report that is not
// clean fails the command, after the result is printed and marked
// reported; without --strict the exit code stays the compiler's.
func TestRunValidate_StrictFailsANotCleanDryRun(t *testing.T) {
	inTempWorkspace(t)
	bot := "schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n\ntool broken:\n  command: \"if [ -f x ]; then echo\"\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> broken\n  broken -> done\n"
	if err := os.WriteFile("dirty.bot", []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidateWith("dirty.bot", jp, ValidateOptions{Exec: true}); err != nil {
		t.Fatalf("without --strict the exit code is the compiler's: %v", err)
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Exec == nil || res.Exec.Clean() {
		t.Fatalf("the fixture's dry run is clean or missing: %v %+v", err, res.Exec)
	}
	jp2, out2 := jsonPrinter()
	err := RunValidateWith("dirty.bot", jp2, ValidateOptions{Strict: true})
	if !errors.Is(err, ErrReported) {
		t.Fatalf("--strict on a not-clean report: %v", err)
	}
	var strict ValidateResult
	if err := json.Unmarshal(out2.Bytes(), &strict); err != nil || !strict.Valid || strict.Exec == nil {
		t.Fatalf("the result was not printed before the error: %v\n%s", err, out2.String())
	}
}

// --exec-timeout bounds a pass; a pass that runs out of time is said so in
// the report and, under --strict, named as the bound's doing.
func TestRunValidate_ExecTimeoutIsTheOperators(t *testing.T) {
	inTempWorkspace(t)
	bot := "schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> done\n"
	if err := os.WriteFile("slow.bot", []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidateWith("slow.bot", jp, ValidateOptions{Exec: true, ExecTimeout: time.Nanosecond}); err != nil {
		t.Fatalf("without --strict the exit code is the compiler's: %v", err)
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Exec == nil || len(res.Exec.Passes) == 0 || !res.Exec.Passes[0].TimedOut {
		t.Fatalf("the pass under a nanosecond is not said timed out: %v %+v", err, res.Exec)
	}
	hp, hout := testPrinter()
	err := RunValidateWith("slow.bot", hp, ValidateOptions{Strict: true, ExecTimeout: time.Nanosecond})
	if err == nil || !strings.Contains(err.Error(), "ran out of time") || !strings.Contains(err.Error(), "--exec-timeout") {
		t.Fatalf("--strict on a timed-out pass: %v\n%s", err, hout.String())
	}
	if err := RunValidateWith("slow.bot", jsonPrinterOnly(), ValidateOptions{Strict: true, ExecTimeout: time.Minute}); err != nil {
		t.Fatalf("a pass within its bound: %v", err)
	}
}

func jsonPrinterOnly() *Printer {
	p, _ := jsonPrinter()
	return p
}

// A bot that guards its entry on a var, as the gallery teaches: without a
// value the gate refuses on every pass and the graph behind it is never
// walked; a value from --var, a preset or typed inputs opens it.
const gatedBot = "vars:\n  release_tag: string = \"\"\n\npresets:\n  ship:\n    release_tag: \"v9\"\n\nschema check_out:\n  configured: bool\n\nschema verdict:\n  ok: bool\n\ncompute gate:\n  output: check_out\n  expr:\n    configured: \"!!vars.release_tag\"\n\nagent work:\n  model: \"claude-opus-4-7\"\n  output: verdict\n\nfail unset:\n  code: TAG_UNSET\n  message: \"release_tag must be set\"\n\nworkflow g:\n  worktree: none\n  sandbox: none\n  entry: gate\n  gate -> work when configured\n  gate -> unset when not configured\n  work -> done\n"

func TestRunValidate_LaunchValuesReachTheDryRun(t *testing.T) {
	inTempWorkspace(t)
	if err := os.WriteFile("g.bot", []byte(gatedBot), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(opts ValidateOptions) ValidateResult {
		t.Helper()
		jp, out := jsonPrinter()
		if err := RunValidateWith("g.bot", jp, opts); err != nil {
			t.Fatalf("validate: %v\n%s", err, out.String())
		}
		var res ValidateResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("the JSON result does not decode: %v\n%s", err, out.String())
		}
		if res.Exec == nil || len(res.Exec.Passes) != 2 {
			t.Fatalf("the result carries no dry run: %+v\n%s", res.Exec, out.String())
		}
		return res
	}
	ended := func(res ValidateResult) []string {
		var last []string
		for _, p := range res.Exec.Passes {
			last = append(last, p.Nodes[len(p.Nodes)-1])
		}
		return last
	}
	// Without a value: refused at the gate on both passes, `work` never walked.
	res := run(ValidateOptions{Exec: true})
	if got := ended(res); got[0] != "unset" || got[1] != "unset" || !res.Exec.Passes[0].Deliberate {
		t.Fatalf("without a value the gate did not refuse on both passes: %v %+v", got, res.Exec.Passes)
	}
	if !contains(res.Exec.UnvisitedNodes, "work") {
		t.Fatalf("the node behind the gate is not said unvisited: %v", res.Exec.UnvisitedNodes)
	}
	// --var opens it, and implies --exec.
	res = run(ValidateOptions{Vars: []string{"release_tag=v1.2.3"}})
	if got := ended(res); got[0] != "done" || got[1] != "done" || res.Exec.Passes[0].Status != "finished" {
		t.Fatalf("--var did not reach the dry run: %v %+v", got, res.Exec.Passes)
	}
	// A preset opens it as well; --var wins over it.
	res = run(ValidateOptions{Preset: "ship"})
	if got := ended(res); got[0] != "done" {
		t.Fatalf("--preset did not reach the dry run: %v", got)
	}
	// Typed inputs (the MCP tool's) open it too.
	res = run(ValidateOptions{Inputs: map[string]any{"release_tag": "v2"}})
	if got := ended(res); got[0] != "done" {
		t.Fatalf("inputs did not reach the dry run: %v", got)
	}
	// A malformed flag is the dry run's error, beside the compile verdict.
	jp, out := jsonPrinter()
	err := RunValidateWith("g.bot", jp, ValidateOptions{Vars: []string{"release_tag"}})
	var bad ValidateResult
	if derr := json.Unmarshal(out.Bytes(), &bad); derr != nil {
		t.Fatalf("the JSON result does not decode: %v\n%s", derr, out.String())
	}
	if err == nil || !bad.Valid || !strings.Contains(bad.ExecError, "invalid --var format") {
		t.Fatalf("a malformed --var is not said as the dry run's error: err=%v result=%+v", err, bad)
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
