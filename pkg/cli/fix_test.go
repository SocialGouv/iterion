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
}
