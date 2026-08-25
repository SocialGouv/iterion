package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCampaignGateCommandsNormalises executes the REAL gate_commands from
// campaign's finalize node — the requalification half of the exit_gate
// contract, and the half that actually shelled out one command per character
// in the measured incident (modernize's reader was fixed first; this twin
// re-reads the same field on the final tree).
//
// The two readers cannot share text (a function inside one node's script vs
// an inline block in another bot's), so what keeps them aligned is executing
// both against the same contract forms: modernize_plan_read_test pins the
// reader, this pins the requalifier.
func TestCampaignGateCommandsNormalises(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	fn := extractPyFunc(t, toolScript(t, "campaign/main.bot", "finalize"), "gate_commands")

	driver := fn + `
import json, sys
cases = {
    "scalar": {"exit_gate": "test -f a && b.sh"},
    "list": {"exit_gate": ["./gradlew build", "./verify.sh"]},
    "block_single": {"exit_gate": "test -f a\n"},
    "block_multi": {"exit_gate": "for f in a b; do\n  test -f \"$f\"\ndone\n"},
    "mapping": {"exit_gate": {"build": "./gradlew build"}},
    "list_with_none": {"exit_gate": ["./ok.sh", None]},
    "empty": {"exit_gate": []},
    "absent": {},
}
out = {}
for name, lot in cases.items():
    gates, defect = gate_commands(lot)
    out[name] = {"gates": gates, "defect": defect}
print(json.dumps(out))
`
	tmp := filepath.Join(t.TempDir(), "gate_commands.py")
	if err := os.WriteFile(tmp, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("python3", tmp).Output()
	if err != nil {
		t.Fatalf("gate_commands driver failed: %v (out %q)", err, raw)
	}

	type verdict struct {
		Gates  []string `json:"gates"`
		Defect string   `json:"defect"`
	}
	var got map[string]verdict
	if uerr := json.Unmarshal(raw, &got); uerr != nil {
		t.Fatalf("driver output is not JSON: %v (out %q)", uerr, raw)
	}

	want := map[string]verdict{
		"scalar":       {Gates: []string{"test -f a && b.sh"}},
		"list":         {Gates: []string{"./gradlew build", "./verify.sh"}},
		"block_single": {Gates: []string{"test -f a"}},
		"block_multi":  {Gates: nil, Defect: "multi-line exit_gate command"},
		"mapping":      {Gates: nil, Defect: "unreadable exit_gate (dict)"},
		"list_with_none": {Gates: nil,
			Defect: "unreadable exit_gate (list holding NoneType)"},
		"empty":  {Gates: []string{}},
		"absent": {Gates: []string{}},
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: missing from driver output", name)
			continue
		}
		if strings.Join(g.Gates, "\x00") != strings.Join(w.Gates, "\x00") ||
			(g.Gates == nil) != (w.Gates == nil) {
			t.Errorf("%s: gates = %q, want %q", name, g.Gates, w.Gates)
		}
		if g.Defect != w.Defect {
			t.Errorf("%s: defect = %q, want %q", name, g.Defect, w.Defect)
		}
	}
}

// extractPyFunc lifts one top-level-of-its-indent `def` (and its whole body)
// out of a node script, dedented so it can run standalone.
func extractPyFunc(t *testing.T, script, name string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	start := -1
	indent := 0
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if strings.HasPrefix(trimmed, "def "+name+"(") {
			start = i
			indent = len(l) - len(trimmed)
			break
		}
	}
	if start < 0 {
		t.Fatalf("def %s not found in node script", name)
	}
	var body []string
	for _, l := range lines[start:] {
		if len(body) > 0 {
			trimmed := strings.TrimLeft(l, " ")
			if trimmed != "" && len(l)-len(trimmed) <= indent {
				break
			}
		}
		if len(l) >= indent {
			body = append(body, l[indent:])
		} else {
			body = append(body, "")
		}
	}
	return strings.Join(body, "\n") + "\n"
}
