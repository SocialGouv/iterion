package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// asWrittenBot walks `hit` only when every launch value arrives as its
// `${VAR:-default}` resolves with no environment: n is 3, tags the list
// ["a","b"], mode "fast".
const asWrittenBot = "vars:\n  n: int = 1\n  tags: string[] = \"x\"\n  mode: string [enum: \"fast\", \"slow\"] = \"slow\"\n  dir: string = \"\"\n  paths: string[] = \"\"\n\nschema pick_out:\n  hit: bool\n\nschema verdict:\n  ok: bool\n\ncompute pick:\n  output: pick_out\n  expr:\n    hit: \"vars.n == 3 && length(vars.tags) == 2 && contains(vars.tags, 'a') && contains(vars.tags, 'b') && vars.mode == 'fast' && length(vars.paths) == 2 && contains(vars.paths, vars.dir)\"\n\nagent hit:\n  model: \"claude-opus-4-7\"\n  output: verdict\n\nagent miss:\n  model: \"claude-opus-4-7\"\n  output: verdict\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: pick\n  pick -> hit when hit\n  pick -> miss when not hit\n  hit -> done\n  miss -> done\n"

// asWrittenVars are launch values a run resolves through the environment.
// The shell running validate sets every one of those names to a value that
// would send the walk to `miss`: the verdict must not read them.
var asWrittenVars = []string{"n=${N:-3}", "tags=${LIST:-a,b}", "mode=${MODE:-fast}", "dir=${PROJECT_DIR}/b", "paths=${PROJECT_DIR}/a,${PROJECT_DIR}/b"}

func setMisleadingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("N", "7")
	t.Setenv("LIST", "q")
	t.Setenv("MODE", "slow")
}

// `iterion validate --var` reads a launch value as a published contract
// reads a default: `${VAR:-default}` is its default, the shell's environment
// is not consulted, and a reference with no default stays for the engine.
func TestDryRunInputsReadTheValuesAsWritten(t *testing.T) {
	setMisleadingEnv(t)
	got, err := dryRunInputs(compileSourceForTest(t, asWrittenBot), ValidateOptions{Vars: asWrittenVars})
	if err != nil {
		t.Fatalf("dryRunInputs refused values a run reads: %v", err)
	}
	want := map[string]any{
		"n":    int64(3),
		"tags": []any{"a", "b"},
		"mode": "fast",
		// No answer without the run: the text goes to the engine, which
		// resolves ${PROJECT_DIR} in every element as a run does.
		"dir":   "${PROJECT_DIR}/b",
		"paths": "${PROJECT_DIR}/a,${PROJECT_DIR}/b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launch values = %#v, want %#v", got, want)
	}
}

// The public path: the dry run accepts the values, and walks the branch the
// as-written reading decides, on a shell whose environment says otherwise.
func TestRunValidate_VarIsReadAsWrittenNotFromTheShell(t *testing.T) {
	setMisleadingEnv(t)
	inTempWorkspace(t)
	if err := os.WriteFile("w.bot", []byte(asWrittenBot), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidateWith("w.bot", jp, ValidateOptions{Vars: asWrittenVars}); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("the JSON result does not decode: %v\n%s", err, out.String())
	}
	if res.ExecError != "" || res.Exec == nil || len(res.Exec.Passes) == 0 {
		t.Fatalf("the dry run did not run on the values: exec_error=%q exec=%+v", res.ExecError, res.Exec)
	}
	for i, p := range res.Exec.Passes {
		if !contains(p.Nodes, "hit") || contains(p.Nodes, "miss") {
			t.Fatalf("pass %d walked %v: the values did not arrive as written", i, p.Nodes)
		}
	}
}
