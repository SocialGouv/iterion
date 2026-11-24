package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The --explain reading, driven through the real command surface
// (iterion#2815): the disclosure an author gets for the roster drift is the
// one this command prints, never a second list beside the delegate's
// projection.
func TestValidateExplainDisclosesTheClaudeCodeRosterDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.bot")
	src := `workflow w:
  default_backend: claude_code
  entry: a
  a -> b
  b -> c

agent a:
  tools: [bash, read_file]
  user: "do"

agent b:
  user: "no declaration"

agent c:
  backend: claw
  user: "not a claude_code node"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Human output: the reading is printed, with the reason per name, and
	// never moves the verdict.
	var human bytes.Buffer
	if err := RunValidateWith(path, &Printer{W: &human, Format: OutputHuman}, ValidateOptions{Explain: true}); err != nil {
		t.Fatalf("a valid bot with --explain must still validate: %v", err)
	}
	out := human.String()
	for _, want := range []string{
		"Capability drift",
		`node "a":`,
		"AskUserQuestion — waits for an answer a headless session has nobody to give",
		"Agent — the tools: list does not grant it — declaring `agent` keeps it",
		`node "b" (no tools: declaration`,
		"result: OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the --explain output does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `node "c"`) {
		t.Errorf("node c is a claw node — the claude_code roster drift is not its reading:\n%s", out)
	}

	// JSON output: the structured form carries the same answer.
	var js bytes.Buffer
	if err := RunValidateWith(path, &Printer{W: &js, Format: OutputJSON}, ValidateOptions{Explain: true}); err != nil {
		t.Fatal(err)
	}
	var result ValidateResult
	if err := json.Unmarshal(js.Bytes(), &result); err != nil {
		t.Fatalf("json: %v\n%s", err, js.String())
	}
	if !result.Valid {
		t.Fatal("a valid bot with --explain must stay valid")
	}
	if len(result.Explain) != 2 {
		t.Fatalf("explain holds %d node(s), want the two claude_code ones: %+v", len(result.Explain), result.Explain)
	}
	byID := map[string]ValidateExplainNode{}
	for _, n := range result.Explain {
		byID[n.NodeID] = n
	}
	a := byID["a"]
	if !a.Surface.Declared {
		t.Error("node a declares a tools: list — the surface must say so")
	}
	if !slices.Contains(a.Surface.Granted, "Bash") || !slices.Contains(a.Surface.Granted, "Read") {
		t.Errorf("node a declares [bash, read_file] but the surface keeps %v", a.Surface.Granted)
	}
	var agentWithheld *string
	for _, w := range a.Surface.Withheld {
		if w.Name == "Agent" {
			r := w.Reason
			agentWithheld = &r
		}
		if w.Name == "AskUserQuestion" && (!w.Always || w.Reason == "") {
			t.Errorf("AskUserQuestion's withholding = %+v, want Always with the headless reason", w)
		}
	}
	if agentWithheld == nil || !strings.Contains(*agentWithheld, "`agent`") {
		t.Errorf("node a's list does not declare agent: the surface must withhold Agent naming the declaration that keeps it, got %v", agentWithheld)
	}
	b := byID["b"]
	if b.Surface.Declared {
		t.Error("node b declares no tools: list — the surface must say so")
	}
	if !slices.Contains(b.Surface.Granted, "Agent") {
		t.Errorf("an undeclared node keeps the ambient roster, Agent included: granted %v", b.Surface.Granted)
	}

	// Without the flag, no reading is produced.
	var plain bytes.Buffer
	if err := RunValidateWith(path, &Printer{W: &plain, Format: OutputHuman}, ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "Capability drift") {
		t.Error("without --explain the reading must not be printed — it is asked for, never imposed")
	}
}
