package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
    "nul": {"exit_gate": ["true\x00"]},
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
		"nul":    {Gates: nil, Defect: "exit_gate command holding a NUL byte"},
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

// TestCampaignSuiteCommandsNormalises executes the REAL suite_commands from
// campaign's finalize: the suites the contract declares once, read the way
// modernize's lot_verify reads them, so that the supervisor never requalifies
// a lot over suites the executing bot could not read.
func TestCampaignSuiteCommandsNormalises(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fn := extractPyFunc(t, toolScript(t, "campaign/main.bot", "finalize"), "suite_commands")
	driver := fn + `
import json
cases = {
    "absent": {},
    "scalar": {"suites": "make test"},
    "list": {"suites": ["make test", " make it ", ""]},
    "mapping": {"suites": {"unit": "make test"}},
    "empty_list": {"suites": []},
    "blank": {"suites": "  "},
    "null": {"suites": None},
    "multi_line": {"suites": ["make test\nmake it"]},
    "nul": {"suites": ["make test\x00"]},
}
print(json.dumps({k: list(suite_commands(v)) for k, v in cases.items()}))
`
	tmp := filepath.Join(t.TempDir(), "suite_commands.py")
	if err := os.WriteFile(tmp, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("python3", tmp).Output()
	if err != nil {
		t.Fatalf("suite_commands driver failed: %v (out %q)", err, raw)
	}
	var got map[string][2]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("driver output is not JSON: %v (out %q)", err, raw)
	}
	for name, wantCmds := range map[string][]string{
		"absent": {}, "scalar": {"make test"}, "list": {"make test", "make it"},
	} {
		cmds, ok := got[name][0].([]any)
		if !ok || len(cmds) != len(wantCmds) {
			t.Errorf("%s: commands %v, want %v", name, got[name][0], wantCmds)
			continue
		}
		for i, c := range cmds {
			if c != wantCmds[i] {
				t.Errorf("%s: command %d = %v, want %q", name, i, c, wantCmds[i])
			}
		}
	}
	for _, name := range []string{"mapping", "empty_list", "blank", "null", "multi_line", "nul"} {
		if got[name][0] != nil || got[name][1] == "" {
			t.Errorf("%s: %v — an unreadable declaration must be refused, naming why", name, got[name])
		}
	}
}

// campaignSuitesPlan: one lot, blocked, whose own gate is green, under suites
// a test writes to ci/test.sh.
const campaignSuitesPlan = `version: 1
suites:
  - "sh ci/test.sh"
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "move the route"
    status: blocked
    exit_gate:
      - "true"
`

// finalizeRun is what one run of the REAL finalize left behind.
type finalizeRun struct {
	ws      string
	git     func(args ...string) string
	handoff string
	out     map[string]any
}

// finalizeOn runs the REAL finalize on a committed final tree holding the plan
// and files; `after`, when given, then shapes the working tree the way a child
// run could leave it. Finalize must answer: a crash leaves the campaign with
// no handoff at all.
func finalizeOn(t *testing.T, plan string, files map[string]string, after func(ws string, git func(...string) string)) finalizeRun {
	t.Helper()
	requireModernizeTools(t)
	script := toolScript(t, "campaign/main.bot", "finalize")
	ws, _, git := modernizeRepo(t, plan)
	writeContract(t, ws, ".modernize/campaign/journal.tsv", "h\n")
	writeContract(t, ws, "verify.sh", "#!/bin/sh\necho '{\"mode\":\"gate\"}'\nexit 0\n")
	for rel, body := range files {
		writeContract(t, ws, rel, body)
	}
	git("add", "-A")
	git("commit", "-qm", "the final tree")
	if after != nil {
		after(ws, git)
	}
	body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.plan_path}}", strconv.Quote(".modernize/plan.yaml"))
	body = strings.ReplaceAll(body, "{{input.verify_path}}", strconv.Quote("sh verify.sh"))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in finalize near %q", body[i:min(i+40, len(body))])
	}
	p := filepath.Join(t.TempDir(), "finalize.py")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := modernizeChild(t, "python3", p)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("finalize: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	r := finalizeRun{ws: ws, git: git, handoff: readContract(t, ws, ".modernize/campaign/handoff.md")}
	if err := json.Unmarshal(out, &r.out); err != nil {
		t.Fatalf("finalize output is not JSON: %v (out %q)", err, out)
	}
	return r
}

// campaignFinalize is finalizeOn on the committed tree as it stands.
func campaignFinalize(t *testing.T, plan string, files map[string]string) (string, map[string]any) {
	t.Helper()
	r := finalizeOn(t, plan, files, nil)
	return r.handoff, r.out
}

// handoffLine is the handoff's first line holding needle.
func handoffLine(handoff, needle string) string {
	for _, l := range strings.Split(handoff, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// TestCampaignFinalizeRequalifiesOverTheSuites: a blocked lot whose own gate
// is green on the final tree requalifies only when the repository's suites
// are green there too, and the handoff says what the suites gave.
func TestCampaignFinalizeRequalifiesOverTheSuites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, suite, verdict, handoff string
	}{
		{"red suites", "exit 1", "still blocked", "RED"},
		{"green suites", "exit 0", "REQUALIFIED", "GREEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handoff, _ := campaignFinalize(t, campaignSuitesPlan, map[string]string{"ci/test.sh": "#!/bin/sh\n" + tc.suite + "\n"})
			if line := handoffLine(handoff, "**L1**"); !strings.Contains(line, tc.verdict) {
				t.Errorf("the blocked lot reads %q, want %q", line, tc.verdict)
			}
			if !strings.Contains(handoff, "The repository's suites on the final tree: "+tc.handoff) {
				t.Errorf("the handoff does not report the suites as %s:\n%s", tc.handoff, handoff)
			}
		})
	}
}

// TestCampaignFinalizeSaysAnUnreadPlan: a final tree whose plan cannot be read
// — a stray tab, a bad merge — is said in the handoff and the notice, never
// read as a programme of zero lots that declares no suite.
func TestCampaignFinalizeSaysAnUnreadPlan(t *testing.T) {
	t.Parallel()
	for name, plan := range map[string]string{
		"a plan that does not parse":   campaignSuitesPlan + "  bad: [unclosed\n",
		"a plan that is not a mapping": "- id: L1\n",
	} {
		t.Run(name, func(t *testing.T) {
			handoff, out := campaignFinalize(t, plan, map[string]string{"ci/test.sh": "#!/bin/sh\nexit 1\n"})
			if !strings.Contains(handoff, "Programme: NOT READ") ||
				!strings.Contains(handoff, "The repository's suites on the final tree: NOT READ") {
				t.Fatalf("the handoff must say the plan was not read:\n%s", handoff)
			}
			if n, _ := out["notice"].(string); !strings.Contains(n, "was not read") {
				t.Fatalf("notice = %q, want it to say the plan was not read", n)
			}
		})
	}
}

// TestCampaignFinalizeReadsOutputThatIsNotUTF8: a legacy application's test
// log in Latin-1 is read with replacement characters, never raised on.
func TestCampaignFinalizeReadsOutputThatIsNotUTF8(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ suite, want string }{
		{"printf 'R\\351sultat OK\\n'", "GREEN"},
		{"printf 'R\\351sultat KO\\n'; exit 1", "RED"},
	} {
		handoff, _ := campaignFinalize(t, campaignSuitesPlan, map[string]string{"ci/test.sh": "#!/bin/sh\n" + tc.suite + "\n"})
		if line := handoffLine(handoff, "suites on the final tree"); !strings.Contains(line, tc.want) {
			t.Fatalf("%q: the suites read %q, want %s", tc.suite, line, tc.want)
		}
	}
}

// TestCampaignFinalizeRunsUnderTheContractsWalls: finalize gives a command the
// wall lot_verify gave it — a lot's gate its lot's gate_timeout_s, the oracle
// and the suites the longest wall any lot's gate ran them under — and a
// command past its wall is a red the handoff names, never a crash that leaves
// no handoff.
func TestCampaignFinalizeRunsUnderTheContractsWalls(t *testing.T) {
	t.Parallel()
	plan := func(top, status, lotWall, gate string) string {
		return "version: 1\n" + top + "oracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n" +
			"    title: \"move the route\"\n    status: " + status + "\n" + lotWall +
			"    exit_gate:\n      - " + strconv.Quote(gate) + "\n"
	}
	for _, tc := range []struct {
		name, plan, verdict string
	}{
		{"a gate past its lot's wall stays blocked",
			plan("", "blocked", "    gate_timeout_s: 1\n", "sleep 3"), "still blocked"},
		{"a gate within its lot's wall requalifies, whatever the top level says",
			plan("gate_timeout_s: 1\n", "blocked", "    gate_timeout_s: 10\n", "sleep 2"), "REQUALIFIED"},
		{"a lot whose wall is unreadable is not requalifiable",
			plan("", "blocked", "    gate_timeout_s: soon\n", "true"), "unreadable gate_timeout_s at lot L1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handoff, _ := campaignFinalize(t, tc.plan, nil)
			if line := handoffLine(handoff, "**L1**"); !strings.Contains(line, tc.verdict) {
				t.Fatalf("the blocked lot reads %q, want %q", line, tc.verdict)
			}
		})
	}
	suites := "suites:\n  - \"sh ci/test.sh\"\n"
	for _, tc := range []struct {
		name, plan, suite, want string
	}{
		{"a suite past the wall is red, and named",
			plan("gate_timeout_s: 1\n"+suites, "done", "", "true"), "sleep 3", "RED — `sh ci/test.sh` timed out"},
		{"the suites get the longest wall a lot's gate gave them",
			plan("gate_timeout_s: 1\n"+suites, "done", "    gate_timeout_s: 10\n", "true"), "sleep 2", "GREEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handoff, _ := campaignFinalize(t, tc.plan, map[string]string{"ci/test.sh": "#!/bin/sh\n" + tc.suite + "\n"})
			if line := handoffLine(handoff, "suites on the final tree"); !strings.Contains(line, tc.want) {
				t.Fatalf("the suites read %q, want %q", line, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, plan, verify string
		green              bool
	}{
		{"an oracle past the wall is red, and says it timed out",
			plan("gate_timeout_s: 1\n", "done", "", "true"), "sleep 3", false},
		{"the oracle gets the longest wall a lot's gate gave it",
			plan("gate_timeout_s: 1\n", "done", "    gate_timeout_s: 10\n", "true"), "sleep 2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handoff, out := campaignFinalize(t, tc.plan, map[string]string{
				"verify.sh": "#!/bin/sh\n" + tc.verify + "\necho '{\"mode\":\"gate\"}'\n"})
			if out["oracle_green"] != tc.green {
				t.Fatalf("oracle_green = %v, want %v:\n%s", out["oracle_green"], tc.green, handoff)
			}
			if !tc.green && !strings.Contains(handoff, "RED (timed out)") {
				t.Fatalf("the handoff must say the oracle timed out:\n%s", handoff)
			}
		})
	}
}
