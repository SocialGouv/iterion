package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// A workflow with one graph defect (an undeclared cycle) and one parse
// defect in a second fixture: the two stages a validate loop has to read.
const undeclaredCycleWorkflow = `schema out:
  ok: bool

agent a:
  model: "test-model"
  output: out

agent b:
  model: "test-model"
  output: out

workflow w:
  entry: a
  a -> b
  b -> a
`

// The parse, compile and bundle stages are concatenated; the reader still
// gets one list in source order, global findings last — a compile finding on
// line 7 is listed before a parse finding on line 12.
func TestValidate_DiagnosticsAreInSourceOrderAcrossStages(t *testing.T) {
	dir := t.TempDir()
	src := "schema out:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: out\n\nworkflow w:\n  entry: a\n  a -> zzz\n\nagent b:\n  model: \"m\"\n  temperature: 0.2\n"
	path := writeFixture(t, dir, "order.bot", src)
	p, buf := newTestPrinter(cli.OutputJSON)
	_ = cli.RunValidate(path, p)
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, buf.String())
	}
	var lines []int
	for _, d := range result.Diagnostics {
		if d.Severity == "error" {
			lines = append(lines, d.Line)
		}
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Fatalf("diagnostics not in source order: lines %v\n%+v", lines, result.Diagnostics)
		}
	}
	if len(lines) < 2 {
		t.Fatalf("expected a compile finding (line 10) and a parse finding (line 14), got %+v", result.Diagnostics)
	}
}

// TestValidate_DiagnosticsCarryPositionAndFix checks what an author (or an
// agent in a validate loop) receives: in --json, one structured object per
// finding with its stage, code, source position and fix line; in the human
// output, the same position and a `fix:` line under the message.
func TestValidate_DiagnosticsCarryPositionAndFix(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "cycle.bot", undeclaredCycleWorkflow)

	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunValidate(path, p); err == nil {
		t.Fatal("expected validation to fail on the undeclared cycle")
	}
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, buf.String())
	}
	var cycle *cli.ValidateDiagnostic
	for i := range result.Diagnostics {
		if result.Diagnostics[i].Code == "C019" {
			cycle = &result.Diagnostics[i]
		}
	}
	if cycle == nil {
		t.Fatalf("expected a C019 diagnostic, got %+v", result.Diagnostics)
	}
	if cycle.Source != "compile" || cycle.Severity != "error" {
		t.Errorf("C019 source/severity = %q/%q", cycle.Source, cycle.Severity)
	}
	if cycle.Hint == "" {
		t.Error("C019 carries no fix line")
	}
	if cycle.Line == 0 || cycle.File == "" {
		t.Errorf("C019 carries no source position: %+v", *cycle)
	}
	if !strings.Contains(strings.Join(result.CompileDiagnostics, "\n"), "C019") {
		t.Error("the legacy compile_diagnostics string list lost the finding")
	}

	p, buf = newTestPrinter(cli.OutputHuman)
	_ = cli.RunValidate(path, p)
	out := buf.String()
	if !strings.Contains(out, "error [C019]") {
		t.Errorf("human output lost the code:\n%s", out)
	}
	if !strings.Contains(out, "fix: ") {
		t.Errorf("human output has no fix line:\n%s", out)
	}
	if !strings.Contains(out, "cycle.bot:") {
		t.Errorf("human output has no source position:\n%s", out)
	}

	// Parse-stage findings ride the same structure, with the parser's own
	// position and fix line.
	bad := writeFixture(t, dir, "bad.bot", "agent a:\n  temperature: 0.2\n")
	p, buf = newTestPrinter(cli.OutputJSON)
	_ = cli.RunValidate(bad, p)
	result = cli.ValidateResult{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v\n%s", err, buf.String())
	}
	var unknown *cli.ValidateDiagnostic
	for i := range result.Diagnostics {
		if result.Diagnostics[i].Code == "E012" {
			unknown = &result.Diagnostics[i]
		}
	}
	if unknown == nil {
		t.Fatalf("expected an E012 diagnostic, got %+v", result.Diagnostics)
	}
	if unknown.Source != "parse" || unknown.Line != 2 || unknown.Hint == "" {
		t.Errorf("E012 = %+v, want source=parse line=2 and a fix line", *unknown)
	}
}
