package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const quotedRefBot = `dsl: 2

vars:
  base: string = "main"

tool checkout:
  command: "git checkout '{{vars.base}}'"

workflow w:
  worktree: none
  sandbox: none
  entry: checkout
  checkout -> done
`

// --dry-run lists the edit and writes nothing; fix writes the fixed bytes;
// a broken file beside them is refused while the others are fixed.
func TestFixWritesTheMechanicalRemedies(t *testing.T) {
	inTempWorkspace(t)
	path := writeBot(t, "f/quoted.bot", quotedRefBot)
	broken := writeBot(t, "f/broken.bot", "agent :\n  model\n")
	p, out := humanPrinter()
	res, err := RunFix(FixOptions{Paths: []string{"f"}, DryRun: true, Printer: p})
	if !errors.Is(err, ErrFixRefused) || len(res.Refused) != 1 || len(res.Files) != 1 || !res.Files[0].Changed || res.Files[0].Written {
		t.Fatalf("dry run: %v %+v\n%s", err, res, out.String())
	}
	if got, _ := os.ReadFile(path); string(got) != quotedRefBot {
		t.Fatal("--dry-run wrote the file")
	}
	if !strings.Contains(out.String(), "would fix f/quoted.bot (1 edit(s))") || !strings.Contains(out.String(), "[C137]") {
		t.Fatalf("the report does not list the edit:\n%s", out.String())
	}
	jp, jout := jsonPrinter()
	res, err = RunFix(FixOptions{Paths: []string{path}, Printer: jp})
	if err != nil || !res.Files[0].Written {
		t.Fatalf("fix: %v %+v", err, res)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "command: \"git checkout {{vars.base}}\"") {
		t.Fatalf("the quotes stayed:\n%s", got)
	}
	var decoded struct {
		Files []struct {
			Edits []struct{ Code, From, To string } `json:"edits"`
		} `json:"files"`
	}
	if err := json.Unmarshal(jout.Bytes(), &decoded); err != nil || len(decoded.Files) != 1 || len(decoded.Files[0].Edits) != 1 || decoded.Files[0].Edits[0].Code != "C137" {
		t.Fatalf("the JSON report: %v\n%s", err, jout.String())
	}
	if got, _ := os.ReadFile(broken); string(got) != "agent :\n  model\n" {
		t.Fatal("the refused file was rewritten")
	}
}

// validate --json carries the remedy on the diagnostic it fixes: an agent's
// loop reads `edit` and applies it without a second command.
func TestValidateCarriesTheEditOnTheDiagnostic(t *testing.T) {
	inTempWorkspace(t)
	path := writeBot(t, "quoted.bot", quotedRefBot)
	jp, out := jsonPrinter()
	if err := RunValidateWith(path, jp, ValidateOptions{}); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	var res struct {
		Diagnostics []struct {
			Code string `json:"code"`
			Edit *struct {
				Line int    `json:"line"`
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"edit"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range res.Diagnostics {
		if d.Code == "C137" && d.Edit != nil && d.Edit.Line == 7 && d.Edit.To == "\"git checkout {{vars.base}}\"" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no C137 diagnostic carries its edit:\n%s", out.String())
	}

	// Two quoted references in the command and one in the postcondition:
	// three diagnostics, two edits — each diagnostic carries the edit of
	// its OWN literal, the postcondition's never the command's.
	three := "dsl: 2\n\nvars:\n  base: string = \"main\"\n  other: string = \"x\"\n\ntool checkout:\n  command: \"git checkout '{{vars.base}}' '{{vars.other}}'\"\n  postcondition: \"test -n '{{vars.base}}'\"\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: checkout\n  checkout -> done\n"
	path3 := writeBot(t, "three.bot", three)
	jp3, out3 := jsonPrinter()
	if err := RunValidateWith(path3, jp3, ValidateOptions{}); err != nil {
		t.Fatalf("validate: %v\n%s", err, out3.String())
	}
	var res3 struct {
		Diagnostics []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Edit    *struct {
				Property string   `json:"property"`
				Refs     []string `json:"refs"`
				To       string   `json:"to"`
			} `json:"edit"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(out3.Bytes(), &res3); err != nil {
		t.Fatal(err)
	}
	var command, postcondition int
	for _, d := range res3.Diagnostics {
		if d.Code != "C137" {
			continue
		}
		if d.Edit == nil {
			t.Fatalf("a C137 diagnostic carries no edit: %s", d.Message)
		}
		switch {
		case strings.Contains(d.Message, `"checkout" command:`) && d.Edit.Property == "command" && d.Edit.To == "\"git checkout {{vars.base}} {{vars.other}}\"":
			command++
		case strings.Contains(d.Message, `"checkout" postcondition:`) && d.Edit.Property == "postcondition" && d.Edit.To == "\"test -n {{vars.base}}\"":
			postcondition++
		default:
			t.Fatalf("a diagnostic carries the edit of another literal: %s → %+v", d.Message, d.Edit)
		}
	}
	if command != 2 || postcondition != 1 {
		t.Fatalf("command edits on %d diagnostics, postcondition on %d; want 2 and 1:\n%s", command, postcondition, out3.String())
	}

	// A command whose quotes are not mechanically removable (the same
	// reference twice) beside a fixable postcondition: the postcondition's
	// edit rides the postcondition's diagnostic — never the command's,
	// which carry none — whatever order the diagnostics come in.
	twice := "dsl: 2\n\nvars:\n  base: string = \"main\"\n\ntool checkout:\n  command: \"A='{{vars.base}}' B='{{vars.base}}'\"\n  postcondition: \"test -n '{{vars.base}}'\"\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: checkout\n  checkout -> done\n"
	path4 := writeBot(t, "twice.bot", twice)
	jp4, out4 := jsonPrinter()
	if err := RunValidateWith(path4, jp4, ValidateOptions{}); err != nil {
		t.Fatalf("validate: %v\n%s", err, out4.String())
	}
	res3.Diagnostics = nil // a decode into kept elements would keep an absent edit
	if err := json.Unmarshal(out4.Bytes(), &res3); err != nil {
		t.Fatal(err)
	}
	var bare, carried int
	for _, d := range res3.Diagnostics {
		if d.Code != "C137" {
			continue
		}
		switch {
		case strings.Contains(d.Message, `"checkout" command:`) && d.Edit == nil:
			bare++
		case strings.Contains(d.Message, `"checkout" postcondition:`) && d.Edit != nil && d.Edit.Property == "postcondition":
			carried++
		default:
			t.Fatalf("an edit landed on a diagnostic of another literal: %s → %+v", d.Message, d.Edit)
		}
	}
	if bare != 2 || carried != 1 {
		t.Fatalf("command diagnostics without an edit: %d, postcondition with: %d; want 2 and 1:\n%s", bare, carried, out4.String())
	}
}

// A C137 in a fragment is fixed whether the main, the fragment or the
// bundle directory is named: the unit is read from the bot's main, and
// fixing a bot fixes its unit.
func TestFixReachesAFragmentThroughItsMain(t *testing.T) {
	inTempWorkspace(t)
	frag := "vars:\n  base: string = \"main\"\n\ntool doit:\n  command: \"test -n '{{vars.base}}'\"\n"
	mainText := "import \"lib/tools.bot\"\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: doit\n  doit -> done\n"
	for _, named := range []string{"b/main.bot", "b/lib/tools.bot", "b"} {
		writeBot(t, "b/main.bot", mainText)
		fragPath := writeBot(t, "b/lib/tools.bot", frag)
		res, err := RunFix(FixOptions{Paths: []string{named}})
		if err != nil {
			t.Fatalf("%s: %v %+v", named, err, res)
		}
		got, _ := os.ReadFile(fragPath)
		if !strings.Contains(string(got), "command: \"test -n {{vars.base}}\"") {
			t.Fatalf("%s named: the fragment's quotes stayed:\n%s\n%+v", named, got, res.Files)
		}
		if mainNow, _ := os.ReadFile("b/main.bot"); string(mainNow) != mainText {
			t.Fatalf("%s named: the main was rewritten", named)
		}
	}
}
