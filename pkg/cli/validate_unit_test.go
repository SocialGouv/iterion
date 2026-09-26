package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const validateUnitMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

// Pinned model and backend: the compile refuses C018 on a credential-less
// host, and the test measures the unit.
const validateUnitNodes = "schema out:\n  ok: bool\n\nprompt mission:\n  Do the thing.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"

func writeValidateUnit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunValidate_ReadsTheUnit: `iterion validate` on a bot in several
// files validates the program the files make — the node a fragment
// declares is counted, nothing is reported as unresolved — and reports
// what is wrong in a fragment at the fragment's own path, C144 included:
// each file reads under its own profile.
func TestRunValidate_ReadsTheUnit(t *testing.T) {
	dir := t.TempDir()
	writeValidateUnit(t, dir, map[string]string{"main.bot": validateUnitMain, "lib/nodes.bot": validateUnitNodes})
	mainBot := filepath.Join(dir, "main.bot")

	res, err := runValidateDiagnosticsJSON(t, mainBot)
	if err != nil || !res.Valid {
		t.Fatalf("validate a two-file bot: valid=%v err=%v diagnostics=%+v", res.Valid, err, res.Diagnostics)
	}
	if res.NodeCount < 1 {
		t.Fatalf("node count %d: the fragment's node was not compiled", res.NodeCount)
	}
	for _, d := range res.Diagnostics {
		if d.Code == string(ir.DiagUnresolvedImports) {
			t.Fatalf("the main was compiled alone: %+v", d)
		}
	}

	// A fragment that does not parse is named at its own path.
	writeValidateUnit(t, dir, map[string]string{"lib/nodes.bot": "agent worker:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n  bogus_prop: 1\n"})
	res, err = runValidateDiagnosticsJSON(t, mainBot)
	if err == nil || res.Valid {
		t.Fatalf("a broken fragment validated: valid=%v err=%v", res.Valid, err)
	}
	if !hasDiagnosticIn(res, "parse", filepath.Join("lib", "nodes.bot")) {
		t.Fatalf("no parse diagnostic at the fragment's path: %+v", res.Diagnostics)
	}

	// A headerless fragment that profile 2 would read otherwise is told so,
	// at its own path — the main, which has nothing profile 2 reads
	// differently, is not.
	writeValidateUnit(t, dir, map[string]string{"lib/nodes.bot": "schema out:\n  ok: bool\n\nprompt mission:\n  First paragraph.\n\n  Second paragraph.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"})
	res, err = runValidateDiagnosticsJSON(t, mainBot)
	if err != nil || !res.Valid {
		t.Fatalf("validate with a headerless fragment: valid=%v err=%v diagnostics=%+v", res.Valid, err, res.Diagnostics)
	}
	var c144 []string
	for _, d := range res.Diagnostics {
		if d.Code == string(ir.DiagProfileOneMatters) {
			c144 = append(c144, d.File)
		}
	}
	if len(c144) != 1 || !strings.HasSuffix(c144[0], filepath.Join("lib", "nodes.bot")) {
		t.Fatalf("C144 at %v, want the fragment's path only", c144)
	}
}

func hasDiagnosticIn(res validateDiagnosticsJSON, source, fileSuffix string) bool {
	for _, d := range res.Diagnostics {
		if d.Source == source && strings.HasSuffix(d.File, fileSuffix) {
			return true
		}
	}
	return false
}

type validateDiagnosticsJSON struct {
	Valid       bool `json:"valid"`
	NodeCount   int  `json:"node_count"`
	Diagnostics []struct {
		Source   string `json:"source"`
		Code     string `json:"code"`
		Severity string `json:"severity"`
		File     string `json:"file"`
		Message  string `json:"message"`
		Hint     string `json:"hint"`
	} `json:"diagnostics"`
}

func runValidateDiagnosticsJSON(t *testing.T, path string) (validateDiagnosticsJSON, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	p := &cli.Printer{W: buf, Format: cli.OutputJSON}
	err := cli.RunValidate(path, p)
	var res validateDiagnosticsJSON
	if jsonErr := json.Unmarshal(buf.Bytes(), &res); jsonErr != nil {
		t.Fatalf("unmarshal validate JSON: %v\nraw: %s", jsonErr, buf.String())
	}
	return res, err
}

// TestRunValidate_NamesAFragmentValidatedAlone: a file under lib/ holds no
// workflow by design; validated alone it can only fail, so the refusal
// says what it is and where it is validated.
func TestRunValidate_NamesAFragmentValidatedAlone(t *testing.T) {
	dir := t.TempDir()
	writeValidateUnit(t, dir, map[string]string{"main.bot": validateUnitMain, "lib/nodes.bot": validateUnitNodes})
	res, err := runValidateDiagnosticsJSON(t, filepath.Join(dir, "lib", "nodes.bot"))
	if err == nil || res.Valid {
		t.Fatalf("a fragment validated alone passed: valid=%v err=%v", res.Valid, err)
	}
	named := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "fragment") && strings.Contains(d.Message, "imports it") && strings.HasSuffix(d.File, filepath.Join("lib", "nodes.bot")) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the refusal does not name the fragment and its main: %+v", res.Diagnostics)
	}
	// A loose file with no workflow, not under lib/, is refused as before.
	loose := filepath.Join(dir, "loose.bot")
	if err := os.WriteFile(loose, []byte("agent a:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = runValidateDiagnosticsJSON(t, loose)
	if err == nil || res.Valid {
		t.Fatalf("a loose file with no workflow passed: valid=%v err=%v", res.Valid, err)
	}
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "fragment") {
			t.Fatalf("a loose file was called a fragment: %+v", d)
		}
	}
	// The JSON path carries the reason (#1728): a plain "no workflow found"
	// diagnostic with its remedy, the same words the human renderer prints —
	// an automation reading --json learns WHY, not only THAT.
	carried := false
	for _, d := range res.Diagnostics {
		if d.Source == "parse" && d.Severity == "error" && strings.Contains(d.Message, "no workflow found") && d.Hint != "" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the JSON result of a loose no-workflow file carries no diagnostic: %+v", res.Diagnostics)
	}
}

// TestRunValidate_ABundleThatImportsAsksForItsFloor: a bundle in several
// files is told (C252) that its manifest must declare the release that
// reads `import`; once it does, nothing is said.
func TestRunValidate_ABundleThatImportsAsksForItsFloor(t *testing.T) {
	dir := t.TempDir()
	writeValidateUnit(t, dir, map[string]string{"main.bot": validateUnitMain, "lib/nodes.bot": validateUnitNodes, "manifest.yaml": "name: demo\n"})
	res, err := runValidateDiagnosticsJSON(t, dir)
	if err != nil || !res.Valid {
		t.Fatalf("validate a two-file bundle: valid=%v err=%v %+v", res.Valid, err, res.Diagnostics)
	}
	var floor bool
	for _, d := range res.Diagnostics {
		if d.Code == "C252" && strings.Contains(d.Message, "`import` (main.bot)") {
			floor = true
		}
	}
	if !floor {
		t.Fatalf("no C252 for a bundle that imports without a floor: %+v", res.Diagnostics)
	}
	writeValidateUnit(t, dir, map[string]string{"manifest.yaml": "name: demo\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n"})
	res, err = runValidateDiagnosticsJSON(t, dir)
	if err != nil || !res.Valid {
		t.Fatalf("validate with the floor: valid=%v err=%v", res.Valid, err)
	}
	for _, d := range res.Diagnostics {
		if d.Code == "C252" {
			t.Fatalf("C252 drawn with the floor declared: %+v", d)
		}
	}
}

// TestRunValidate_NamesOneCauseOnce pins the F2 boundary of #1728: a file
// that fails to PARSE carries its parse error and no second "no workflow
// found" error — the workflow is missing because the program did not
// parse; one cause, one finding. The reason still reaches the JSON path
// for a file that parses (the #1728 case), through the E006 refusal here.
func TestRunValidate_NamesOneCauseOnce(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.bot")
	if err := os.WriteFile(broken, []byte("workflow w:\n  entry: a\n\nagent a:\n  backend: \"claude_code\"\n  system: \"caf\xe9\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := runValidateDiagnosticsJSON(t, broken)
	if err == nil || res.Valid {
		t.Fatalf("a non-UTF-8 file validated: valid=%v err=%v", res.Valid, err)
	}
	noWorkflow, e006 := 0, 0
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "no workflow found") {
			noWorkflow++
		}
		if d.Code == "E006" {
			e006++
		}
	}
	if e006 != 1 {
		t.Fatalf("E006 count = %d, want exactly the encoding refusal: %+v", e006, res.Diagnostics)
	}
	if noWorkflow != 0 {
		t.Fatalf("a parse failure also reported %d 'no workflow found' error(s) — one cause, one finding", noWorkflow)
	}
}
