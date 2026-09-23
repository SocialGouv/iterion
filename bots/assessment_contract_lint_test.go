package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// contract_lint walks EVERY lot of the contract the assessment drafted. The
// execution bot's own reader (`plan_read`) validates the ONE lot it selected,
// which is right for it and leaves a cycle, an unknown dependency or an
// uneditable lot four positions down to be discovered by the run that trips
// over it — hours in, with a repair loop already spent.
//
// These tests pin both halves: what the lint refuses, and that it never
// ACCEPTS something the execution bot would refuse.

// aGoodContract is a complete, synthetic programme. Every fixture in this file
// is synthetic by construction: the bundle ships to a public catalog.
const aGoodContract = `version: 1

oracle:
  dir: .golden-master
  refs_dir: .golden-master/refs
  verify: .golden-master/verify-oracle.sh

gate_timeout_s: 3600

proposals:
  - id: datastore-engine
    question: "which engine does the second datastore lot target?"
    options: ["the incumbent", "the alternative"]
    recommendation: "the alternative, on the support horizon in the brief"

lots:
  - id: L1
    title: "raise the build toolchain"
    status: todo
    rebaseline_allowed: false
    crosses_major: false
    depends_on: []
    intent: |
      The build tool rises one series. Nothing observable may change.
    exit_gate:
      - "bash ci/build.sh"
  - id: L2
    title: "raise the runtime across its major"
    status: todo
    rebaseline_allowed: false
    crosses_major: true
    depends_on: ["L1"]
    intent: |
      The runtime crosses a major; see proposal:datastore-engine for the
      datastore question it does not settle.
    exit_gate:
      - "test -s .modernize/sweeps/L2.md"
      - "bash ci/build.sh"
`

const goodOutcomes = `{"outcomes": [
  {"id": "supported-runtime",
   "states": "the served application runs on a runtime under active support",
   "check": "bash ci/runtime-gate.sh",
   "arbitration": ""}
]}`

// contractRepo writes a contract (and its outcomes) into a throwaway
// repository and returns the workspace.
func contractRepo(t *testing.T, contract, outcomes string) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Run(t, dir, "init", "-q", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "t@example.com")
	gittest.Run(t, dir, "config", "user.name", "t")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(dir, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".modernize", "plan.yaml"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	if outcomes != "" {
		if err := os.WriteFile(filepath.Join(dir, ".modernize", "outcomes.json"), []byte(outcomes), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-qm", "contract")
	return dir
}

func lintContract(t *testing.T, contract, outcomes string) map[string]any {
	t.Helper()
	dir := contractRepo(t, contract, outcomes)
	out, exit, stderr := assessmentRun(t, "contract_lint", map[string]string{
		"{{vars.workspace_dir}}": dir,
		"{{vars.plan_path}}":     ".modernize/plan.yaml",
	}, nil)
	if exit != 0 {
		t.Fatalf("contract_lint exited %d: %s", exit, stderr)
	}
	return out
}

func TestAssessmentContractLintAcceptsACompleteContract(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")
	out := lintContract(t, aGoodContract, goodOutcomes)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a complete contract was refused: %s", assessmentString(t, out, "reason"))
	}
	if out["lots"] != float64(2) {
		t.Fatalf("lots = %v, want 2", out["lots"])
	}
}

// The failures a per-lot reader cannot see, one test each.
func TestAssessmentContractLintRefusesWhatOneLotAtATimeCannotSee(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")

	cases := []struct {
		name     string
		contract string
		wants    string
	}{
		{
			name: "a dependency cycle",
			contract: strings.Replace(aGoodContract,
				`    depends_on: []
    intent: |
      The build tool rises one series.`,
				`    depends_on: ["L2"]
    intent: |
      The build tool rises one series.`, 1),
			wants: "dependency cycle",
		},
		{
			name: "a dependency on a lot nobody declares",
			contract: strings.Replace(aGoodContract, `depends_on: ["L1"]`,
				`depends_on: ["L9"]`, 1),
			wants: "which no lot declares",
		},
		{
			name: "a lot the execution gate could never mark done",
			contract: strings.Replace(aGoodContract,
				"  - id: L1\n    title: \"raise the build toolchain\"\n    status: todo\n",
				"  - title: \"raise the build toolchain\"\n    id: L1\n    status: todo\n", 1),
			wants: "`- id:` line was not found",
		},
		{
			name:     "a major crossed with no sweep record in its gate",
			contract: strings.Replace(aGoodContract, `      - "test -s .modernize/sweeps/L2.md"`+"\n", "", 1),
			wants:    "sweep record",
		},
		{
			name: "a status the gate alone may write",
			contract: strings.Replace(aGoodContract, "  - id: L1\n    title: \"raise the build toolchain\"\n    status: todo",
				"  - id: L1\n    title: \"raise the build toolchain\"\n    status: done", 1),
			wants: "belongs to the execution gate",
		},
		{
			name:     "an intent naming a proposal the contract does not declare",
			contract: strings.Replace(aGoodContract, "proposal:datastore-engine", "proposal:message-broker", 1),
			wants:    "which the contract does not declare",
		},
		{
			name:     "no reference directory to protect",
			contract: strings.Replace(aGoodContract, "  refs_dir: .golden-master/refs\n", "", 1),
			wants:    "oracle.refs_dir",
		},
		{
			name:     "a multi-line gate command",
			contract: strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, "      - |\n        bash ci/build.sh\n        bash ci/extra.sh", 1),
			wants:    "multi-line exit_gate",
		},
		{
			name:     "a wall nobody could honour",
			contract: strings.Replace(aGoodContract, "gate_timeout_s: 3600", "gate_timeout_s: 999999", 1),
			wants:    "gate_timeout_s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.contract == aGoodContract {
				t.Fatal("the mutation did not apply — this case would pass by accident")
			}
			out := lintContract(t, tc.contract, goodOutcomes)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("accepted a contract with %s", tc.name)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
				t.Errorf("refusal does not name the defect (%q): %s", tc.wants,
					assessmentString(t, out, "reason"))
			}
		})
	}
}

// A programme can process every lot and still miss what it was for. The
// outcomes are the conjunction that says otherwise, so a contract without them
// is not a contract this bot signs off.
func TestAssessmentContractLintRefusesAProgrammeThatOwesNothing(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")

	t.Run("no outcomes file at all", func(t *testing.T) {
		out := lintContract(t, aGoodContract, "")
		if assessmentBool(t, out, "ok") {
			t.Fatal("a contract with no outcomes file was accepted")
		}
	})

	t.Run("an outcome that closes on a claim", func(t *testing.T) {
		out := lintContract(t, aGoodContract,
			`{"outcomes": [{"id": "supported-runtime", "states": "it runs", "check": ""}]}`)
		if assessmentBool(t, out, "ok") {
			t.Fatal("an outcome with no check command was accepted — it could only ever close on a claim")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "check") {
			t.Errorf("refusal does not name the missing check: %s", assessmentString(t, out, "reason"))
		}
	})
}

// THE COUPLING. Whatever the two readers disagree about, it must never be that
// this lint accepts a contract the execution bot then refuses: the assessment
// would have signed off a programme that cannot start, and the operator would
// learn it from a run that crossed no gate.
//
// Both real scripts are executed over the same corpus. A future edit to either
// that breaks the implication reddens here.
func TestAssessmentContractLintNeverAcceptsWhatPlanReadRefuses(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")
	planRead := toolScript(t, "modernize/main.bot", "plan_read")

	corpus := map[string]string{
		"complete":            aGoodContract,
		"duplicate lot id":    strings.Replace(aGoodContract, "  - id: L2", "  - id: L1", 1),
		"gateless lot":        strings.Replace(aGoodContract, "    exit_gate:\n      - \"bash ci/build.sh\"\n  - id: L2", "  - id: L2", 1),
		"uneditable lot":      strings.Replace(aGoodContract, "  - id: L1\n    title:", "  - title:", 1),
		"unreadable gate":     strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, "      gate: true", 1),
		"multi-line command":  strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, "      - |\n        bash a.sh\n        bash b.sh", 1),
		"empty id":            strings.Replace(aGoodContract, "  - id: L2", `  - id: ""`, 1),
		"cycle":               strings.Replace(aGoodContract, "    depends_on: []", `    depends_on: ["L2"]`, 1),
		"unknown dependency":  strings.Replace(aGoodContract, `depends_on: ["L1"]`, `depends_on: ["L9"]`, 1),
		"worker-written done": strings.Replace(aGoodContract, "    status: todo\n    rebaseline_allowed: false\n    crosses_major: false", "    status: done\n    rebaseline_allowed: false\n    crosses_major: false", 1),
	}

	for name, contract := range corpus {
		t.Run(name, func(t *testing.T) {
			dir := contractRepo(t, contract, goodOutcomes)

			// The execution bot's reader, on its own terms.
			reader := modernizePlanReadAt(t, planRead, dir)
			executorRefuses := reader.Refused || reader.LotNotActionable

			lintOut, exit, stderr := assessmentRun(t, "contract_lint", map[string]string{
				"{{vars.workspace_dir}}": dir,
				"{{vars.plan_path}}":     ".modernize/plan.yaml",
			}, nil)
			if exit != 0 {
				t.Fatalf("contract_lint exited %d: %s", exit, stderr)
			}
			lintAccepts := assessmentBool(t, lintOut, "ok")

			if executorRefuses && lintAccepts {
				t.Fatalf("contract_lint ACCEPTED a contract the execution bot refuses "+
					"(%s / %s). The assessment would sign off a programme that cannot start.",
					reader.Notice, reader.LotStatus)
			}
		})
	}
}

// modernizePlanReadAt runs the execution bot's real reader against an existing
// workspace — the same script modernize ships, no re-implementation.
func modernizePlanReadAt(t *testing.T, script, ws string) modernizePlanReadOut {
	t.Helper()
	body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", `"`+ws+`"`)
	body = strings.ReplaceAll(body, "{{vars.plan_path}}", `".modernize/plan.yaml"`)
	body = strings.ReplaceAll(body, "{{vars.only_lot}}", `""`)
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in plan_read")
	}
	path := filepath.Join(t.TempDir(), "plan_read.py")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return runPlanReadScript(t, path)
}

// runPlanReadScript executes a prepared plan_read script and decodes its
// verdict. Exit code pinned at 0: the reader's refusals are VERDICTS, printed
// as JSON, never a tool failure.
func runPlanReadScript(t *testing.T, path string) modernizePlanReadOut {
	t.Helper()
	out, err := exec.Command("python3", path).Output()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("plan_read failed to execute: %v", err)
		}
		t.Fatalf("plan_read exited %d (stderr %q)", ee.ExitCode(), ee.Stderr)
	}
	var res modernizePlanReadOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("plan_read output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

// The end-to-end claim of this bundle, on a synthetic fixture: the contract it
// produces is accepted by its own whole-contract lint AND is actionable by the
// execution bot's reader — a real lot selected, with the gate the contract
// declares, and no refusal.
//
// A contract that satisfies one reader and not the other is the failure that
// costs a campaign: the assessment signs off, the first execution run crosses
// no gate, and `finished` reads as convergence.
func TestAssessmentProducedContractIsAcceptedByBothReaders(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")
	dir := contractRepo(t, aGoodContract, goodOutcomes)

	lint, exit, stderr := assessmentRun(t, "contract_lint", map[string]string{
		"{{vars.workspace_dir}}": dir,
		"{{vars.plan_path}}":     ".modernize/plan.yaml",
	}, nil)
	if exit != 0 {
		t.Fatalf("contract_lint exited %d: %s", exit, stderr)
	}
	if !assessmentBool(t, lint, "ok") {
		t.Fatalf("the whole-contract lint refused the contract: %s", assessmentString(t, lint, "reason"))
	}

	reader := modernizePlanReadAt(t, toolScript(t, "modernize/main.bot", "plan_read"), dir)
	if reader.Refused {
		t.Fatalf("the execution bot refuses to READ the contract: %s", reader.Notice)
	}
	if reader.LotNotActionable {
		t.Fatalf("the execution bot finds no actionable lot (%s): %s", reader.LotStatus, reader.Notice)
	}
	if reader.NothingToDo {
		t.Fatalf("the execution bot reads the contract as already finished: %s", reader.Notice)
	}
	if reader.LotID != "L1" {
		t.Fatalf("the execution bot selected %q, want the first ready lot", reader.LotID)
	}
	if reader.ExitGate != "bash ci/build.sh" {
		t.Fatalf("the gate reaching the verifier is %q, not the one the contract declares", reader.ExitGate)
	}
	t.Logf("both readers accept: lint ok over %v lot(s); execution bot selected %s with gate %q",
		lint["lots"], reader.LotID, reader.ExitGate)
}
