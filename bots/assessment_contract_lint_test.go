package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
    brief_targets: []
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
    brief_targets: ["the runtime"]
    intent: |
      The runtime crosses a major; see proposal:datastore-engine for the
      datastore question it does not settle.
    exit_gate:
      - "test -s .modernize/sweeps/L2.md"
      - "bash ci/build.sh"
`

const goodOutcomes = `{"outcomes": [
  {"id": "runtime-under-support",
   "goal_id": "supported-runtime",
   "states": "the served application runs on a runtime under active support",
   "check": "bash ci/runtime-gate.sh",
   "arbitration": ""}
]}`

// gateProbeWall is the per-command wall the gate probe gets in these tests. It
// is short on purpose: a command that hits it leaves its gate UNPROVEN rather
// than refused, so a low wall can only make the lint more permissive — it
// cannot manufacture a refusal, and it makes the timeout path observable in a
// second instead of two minutes.
const gateProbeWall = "3"

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
	// The build the fixture's gates call, failing on the tree the programme
	// starts from: a gate seen to FAIL, for a reason the probe can observe —
	// not a command the shell could not find.
	if err := os.MkdirAll(filepath.Join(dir, "ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci", "build.sh"),
		[]byte("echo 'the build does not pass on this tree yet' >&2\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-qm", "contract")
	return dir
}

// lintContractIn runs the lint over a workspace the caller prepared.
func lintContractIn(t *testing.T, dir, brief string) map[string]any {
	t.Helper()
	return lintContractWithDocuments(t, dir, brief, nil)
}

func lintContract(t *testing.T, contract, outcomes string) map[string]any {
	t.Helper()
	return lintContractAgainst(t, contract, outcomes, aGoodBrief)
}

// lintContractAgainst runs the lint over a contract AND the declared brief it
// is supposed to honour. The brief is produced by the bundle's own
// `brief_read`, not hand-built here: a fixture brief that the real reader
// would refuse would prove the lint against a document no run can hand it.
func lintContractAgainst(t *testing.T, contract, outcomes, brief string) map[string]any {
	t.Helper()
	return lintContractIn(t, contractRepo(t, contract, outcomes), brief)
}

// briefJSON reads a brief through the bundle's own brief_read and returns the
// normalised document the workflow hands the lint.
func briefJSON(t *testing.T, body string) string {
	t.Helper()
	if body == "" {
		return "{}"
	}
	out := readBrief(t, body)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the fixture brief is one brief_read refuses: %s", assessmentString(t, out, "reason"))
	}
	encoded, err := json.Marshal(out["brief"])
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestAssessmentContractLintAcceptsACompleteContract(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
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
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	cases := []struct {
		name     string
		contract string
		wants    string
	}{
		{
			name: "a dependency cycle",
			contract: strings.Replace(aGoodContract,
				`    depends_on: []
    brief_targets: []`,
				`    depends_on: ["L2"]
    brief_targets: []`, 1),
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
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

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
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
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

			lintOut := lintContractIn(t, dir, aGoodBrief)
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
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir := contractRepo(t, aGoodContract, goodOutcomes)

	lint := lintContractIn(t, dir, aGoodBrief)
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

// A GATE THAT CANNOT FAIL. The four spellings a vacuous gate is usually
// written with are refused by name, but the name is not the mechanism: the
// lint RUNS every gate on the tree the programme starts from, and a gate that
// is already green there is green after the lot whatever the lot did.
//
// Both halves are exercised — the ones a list catches, and the one no list
// catches: a real command over a path that is always there.
func TestAssessmentContractLintRefusesAGateThatCannotFail(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	for _, tc := range []struct{ name, gate, wants string }{
		{"true", `      - "true"`, "cannot fail"},
		{"the null command", `      - ":"`, "cannot fail"},
		{"exit 0", `      - "exit 0"`, "cannot fail"},
		{"an echo", `      - "echo the lot is done"`, "cannot fail"},
		{"a bare comment", `      - "# nothing to check here"`, "cannot fail"},
		// The one a list of spellings does NOT catch: an honest command, over
		// a file the input tree already carries. Only running it finds this.
		{"a test over a file that is already there",
			`      - "test -f .modernize/plan.yaml"`, "ALREADY PASSES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, tc.gate, 1)
			if contract == aGoodContract {
				t.Fatal("the mutation did not apply — this case would pass by accident")
			}
			out := lintContract(t, contract, goodOutcomes)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("a gate that passes before the lot begins was accepted (%s)", tc.gate)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
				t.Errorf("the refusal does not say why (%q): %s", tc.wants,
					assessmentString(t, out, "reason"))
			}
		})
	}

	// And the control: the complete contract's gates are all seen to FAIL, so
	// the lint reports them proven rather than merely read.
	t.Run("a gate that bites is counted as proven", func(t *testing.T) {
		out := lintContract(t, aGoodContract, goodOutcomes)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("the complete contract was refused: %s", assessmentString(t, out, "reason"))
		}
		if out["gates_proven_red"] != float64(2) {
			t.Fatalf("gates_proven_red = %v, want 2 — a gate nobody saw fail is a gate nobody has reason to trust",
				out["gates_proven_red"])
		}
	})
}

// The sweep record is certified by a PREDICATE OVER THE FILE, never by the
// path appearing somewhere in the text of a command. A substring search calls
// an `echo` of the path due diligence.
func TestAssessmentContractLintRefusesASweepMentionedButNeverTested(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	for _, tc := range []struct{ name, gate string }{
		{"the path only echoed", `      - "echo see .modernize/sweeps/L2.md"`},
		{"another lot's record tested", `      - "test -s .modernize/sweeps/L1.md"`},
		{"the directory tested, not the record", `      - "test -d .modernize/sweeps/"`},
		// The predicate's own text, inside a command that cannot fail on it.
		{"the predicate neutralised by an or", `      - "test -s .modernize/sweeps/L2.md || true"`},
		{"the predicate in a trailing comment", `      - "bash ci/build.sh # test -s .modernize/sweeps/L2.md"`},
		{"a string test on the path", `      - "test -n .modernize/sweeps/L2.md"`},
		// An existence test admits an EMPTY record: the lot could touch the
		// file and cross the gate with nothing written in it.
		{"an existence test that admits an empty record", `      - "test -f .modernize/sweeps/L2.md"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := strings.Replace(aGoodContract,
				`      - "test -s .modernize/sweeps/L2.md"`, tc.gate, 1)
			if contract == aGoodContract {
				t.Fatal("the mutation did not apply")
			}
			out := lintContract(t, contract, goodOutcomes)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("a major crossed with no file predicate over its own sweep record was accepted (%s)", tc.gate)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), "sweep record") {
				t.Errorf("the refusal does not name the sweep record: %s", assessmentString(t, out, "reason"))
			}
		})
	}
}

// The neighbours that ARE the predicate, on the same bench: the bracket form,
// and the path quoted. A rule that refused them would be refusing the record.
func TestAssessmentContractLintAcceptsTheSweepPredicateInItsTwoForms(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	for _, gate := range []string{
		`      - "[ -s .modernize/sweeps/L2.md ]"`,
		`      - "test -s '.modernize/sweeps/L2.md'"`,
	} {
		contract := strings.Replace(aGoodContract, `      - "test -s .modernize/sweeps/L2.md"`, gate, 1)
		if contract == aGoodContract {
			t.Fatal("the mutation did not apply")
		}
		out := lintContract(t, contract, goodOutcomes)
		if !assessmentBool(t, out, "ok") {
			t.Errorf("the sweep predicate written %s was refused: %s", gate, assessmentString(t, out, "reason"))
		}
	}
}

// THE RECORD IS WRITTEN BY THE LOT. A record already on the input tree makes
// the predicate green before the sweep has run, and a gate red on the input
// tree ONLY because the record is absent turns green the moment one is
// written, whatever the lot did to the code. Both are the vacuous gate again,
// with the sweep record as its disguise.
func TestAssessmentContractLintRefusesASweepThatCannotTellTheLotApart(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	t.Run("a record already on the input tree", func(t *testing.T) {
		dir := contractRepo(t, aGoodContract, goodOutcomes)
		record := filepath.Join(dir, ".modernize", "sweeps", "L2.md")
		if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(record, []byte("# a sweep nobody ran for this lot\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := lintContractIn(t, dir, aGoodBrief)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a sweep record present before the lot begins was accepted as the lot's due diligence")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "already exists") {
			t.Errorf("the refusal does not say the record predates the lot: %s", assessmentString(t, out, "reason"))
		}
	})

	for _, tc := range []struct{ name, gate, wants string }{
		{"the record is the whole gate", `      - "test -s .modernize/sweeps/L2.md"`, "nothing else"},
		{"the rest of the gate already passes",
			"      - \"test -s .modernize/sweeps/L2.md\"\n      - \"test -f .modernize/plan.yaml\"",
			"ALREADY PASSES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := strings.Replace(aGoodContract,
				"      - \"test -s .modernize/sweeps/L2.md\"\n      - \"bash ci/build.sh\"\n", tc.gate+"\n", 1)
			if contract == aGoodContract {
				t.Fatal("the mutation did not apply")
			}
			out := lintContract(t, contract, goodOutcomes)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("a crossing lot whose gate only the sweep record keeps red was accepted (%s)", tc.gate)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
				t.Errorf("the refusal does not say why (%q): %s", tc.wants, assessmentString(t, out, "reason"))
			}
		})
	}
}

// A contract this bot has just WRITTEN carries `todo` and nothing else, and a
// dependency on a lot that is not `todo` is satisfied by BOTH readers — the
// lot behind it starts on work nobody did.
func TestAssessmentContractLintRefusesABlockedLotAndABlockedDependency(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	blocked := strings.Replace(aGoodContract,
		"  - id: L1\n    title: \"raise the build toolchain\"\n    status: todo",
		"  - id: L1\n    title: \"raise the build toolchain\"\n    status: blocked", 1)
	if blocked == aGoodContract {
		t.Fatal("the mutation did not apply")
	}
	out := lintContract(t, blocked, goodOutcomes)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a freshly drafted contract carrying a `blocked` lot was accepted")
	}
	reason := assessmentString(t, out, "reason")
	if !strings.Contains(reason, "nobody has executed yet") {
		t.Errorf("the refusal does not say a drafted lot is `todo`: %s", reason)
	}
	// L2 depends on L1, and the dependency refusal is its own: the executor
	// reads `blocked` as unavailable for work AND as a met dependency.
	if !strings.Contains(reason, "would start on work nobody did") {
		t.Errorf("the refusal does not name the dependency satisfied by a blocked lot: %s", reason)
	}
}

// One identifier naming both a proposal and a lot: the two are edited by
// different hands and anchored by the same line.
func TestAssessmentContractLintRefusesAProposalIdThatIsAlsoALotId(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	// The proposal is moved BELOW the lots on purpose. With it above, the
	// block-shape check finds the proposal's `- id: L2` first and refuses for
	// a different reason — which is itself the finding, and leaves this guard
	// unexercised. Below the lots, only the collision is left to catch it.
	proposals := `proposals:
  - id: datastore-engine
    question: "which engine does the second datastore lot target?"
    options: ["the incumbent", "the alternative"]
    recommendation: "the alternative, on the support horizon in the brief"

`
	if !strings.Contains(aGoodContract, proposals) {
		t.Fatal("the proposals block is not where this test expects it")
	}
	collided := strings.Replace(aGoodContract, proposals, "", 1)
	collided += "\n" + strings.Replace(proposals, "  - id: datastore-engine", "  - id: L2", 1)
	collided = strings.Replace(collided, "proposal:datastore-engine", "proposal:L2", 1)
	out := lintContract(t, collided, goodOutcomes)
	if assessmentBool(t, out, "ok") {
		t.Fatal("an id naming both a proposal and a lot was accepted")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "both a proposal and a lot") {
		t.Errorf("the refusal does not name the collision: %s", assessmentString(t, out, "reason"))
	}
}

// THE BRIEF IS AN INPUT TO THE LINT. Without it the lint checks a contract
// against ITSELF — internally consistent, and free to contradict every
// decision somebody took.
func TestAssessmentContractLintChecksTheContractAgainstTheBrief(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	t.Run("a decided target no lot carries", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `    brief_targets: ["the runtime"]`,
			`    brief_targets: []`, 1)
		if contract == aGoodContract {
			t.Fatal("the mutation did not apply")
		}
		out := lintContract(t, contract, goodOutcomes)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a target the brief decided, carried by no lot, was accepted — the programme drops the decision silently")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "silently drops") {
			t.Errorf("the refusal is not the one for a dropped target — the case is being caught "+
				"by a neighbouring rule, and this guard is unexercised: %s",
				assessmentString(t, out, "reason"))
		}
	})

	t.Run("a major the brief crosses, declared away by the lot", func(t *testing.T) {
		contract := strings.Replace(aGoodContract,
			"    crosses_major: true\n    depends_on: [\"L1\"]",
			"    crosses_major: false\n    depends_on: [\"L1\"]", 1)
		// The sweep line goes with it: a lot that does not cross a major is
		// not asked for a record, so only the brief can catch this.
		contract = strings.Replace(contract, `      - "test -s .modernize/sweeps/L2.md"`+"\n", "", 1)
		out := lintContract(t, contract, goodOutcomes)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a lot declared its target's major crossing away, and the contract was accepted — the sweep record becomes optional")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "across a major") {
			t.Errorf("the refusal does not name the crossing: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a brief_targets entry the brief does not declare", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `    brief_targets: ["the runtime"]`,
			`    brief_targets: ["the runtime", "a component nobody decided"]`, 1)
		out := lintContract(t, contract, goodOutcomes)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a lot claiming to advance a target the brief never declared was accepted")
		}
	})

	t.Run("a target whose crossing cannot be established", func(t *testing.T) {
		vague := strings.Replace(aGoodBrief, `    current: "2.9"`, `    current: "the series in the tree"`, 1)
		if vague == aGoodBrief {
			t.Fatal("the mutation did not apply")
		}
		out := lintContractAgainst(t, aGoodContract, goodOutcomes, vague)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a target stating neither parseable versions nor crosses_major was accepted — whether the sweep is due would be a guess")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "not a thing to infer") {
			t.Errorf("the refusal does not say why: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("an outcome answering no declared goal", func(t *testing.T) {
		out := lintContract(t, aGoodContract,
			`{"outcomes": [{"id": "runtime-under-support", "goal_id": "a-goal-nobody-wrote",
			  "states": "it runs", "check": "bash ci/runtime-gate.sh"}]}`)
		if assessmentBool(t, out, "ok") {
			t.Fatal("an outcome answering a goal the brief does not carry was accepted — the programme converges on an objective it gave itself")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "a-goal-nobody-wrote") {
			t.Errorf("the refusal does not name the unknown goal: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("an outcome naming no goal at all", func(t *testing.T) {
		out := lintContract(t, aGoodContract,
			`{"outcomes": [{"id": "runtime-under-support", "states": "it runs", "check": "bash ci/runtime-gate.sh"}]}`)
		if assessmentBool(t, out, "ok") {
			t.Fatal("an outcome naming no goal was accepted")
		}
	})

	t.Run("a lot announcing what the brief forbids", func(t *testing.T) {
		guarded := strings.Replace(aGoodBrief,
			`forbidden_changes:
  - "the public interface of the reporting module"`,
			`forbidden_changes:
  - statement: "the public interface of the reporting module"
    pattern: "public interface|reporting module"`, 1)
		if guarded == aGoodBrief {
			t.Fatal("the mutation did not apply")
		}
		contract := strings.Replace(aGoodContract,
			"      The build tool rises one series. Nothing observable may change.",
			"      Rework the reporting module's callers while the build tool rises.", 1)
		out := lintContractAgainst(t, contract, goodOutcomes, guarded)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a lot announcing work the brief FORBIDS was accepted")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "FORBIDS") {
			t.Errorf("the refusal does not say the brief forbids it: %s", assessmentString(t, out, "reason"))
		}
	})
}

// A GATE NOBODY SAW FAIL IS NOT A GATE PROVEN. A timeout and a failed spawn
// are both "not green", which is why neither refuses the lot — but neither is
// an observed verdict either, and counting them let a contract whose gates all
// hang report `gates_proven_red == lots` while nothing had been seen.
func TestAssessmentGatesProvenRedCountsOnlyObservedFailures(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	t.Run("a gate that hangs is not counted as proven", func(t *testing.T) {
		// Longer than the probe's per-command wall, and the lot is not refused
		// for it: only gates seen to PASS are refused.
		hanging := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
			`      - "sleep 60"`, 1)
		if hanging == aGoodContract {
			t.Fatal("the mutation did not apply")
		}
		out := lintContract(t, hanging, goodOutcomes)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a gate that timed out was refused; a timeout is not a gate seen to pass: %s",
				assessmentString(t, out, "reason"))
		}
		if out["gates_proven_red"] != float64(1) {
			t.Fatalf("gates_proven_red = %v, want 1 — the hanging lot was counted as a gate "+
				"somebody saw fail", out["gates_proven_red"])
		}
	})

	// The shell's own "not found" (127) and "not executable" (126) are not a
	// gate failing: the gate did not run. The bundle does not provision the
	// target's toolchain, so a real gate meets this more often than not here,
	// and counting it would report such a contract fully proven.
	for _, tc := range []struct{ name, gate string }{
		{"a gate whose command does not exist is not counted as proven",
			`      - "a-command-that-is-on-no-path"`},
		{"a gate whose script cannot be executed is not counted as proven",
			`      - "./ci/build.sh"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			absent := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, tc.gate, 1)
			if absent == aGoodContract {
				t.Fatal("the mutation did not apply")
			}
			out := lintContract(t, absent, goodOutcomes)
			if !assessmentBool(t, out, "ok") {
				t.Fatalf("a gate that could not run was refused: %s", assessmentString(t, out, "reason"))
			}
			if out["gates_proven_red"] != float64(1) {
				t.Fatalf("gates_proven_red = %v, want 1 — a gate the shell could not run was counted "+
					"as a gate somebody saw fail", out["gates_proven_red"])
			}
		})
	}
}

// THE PROBE RUNS AGENT-WRITTEN SHELL, so it runs it in an allowlisted
// environment. Those commands come from a repository this bundle's own prompts
// declare untrusted, and they run before any human has read the contract:
// inheriting the process environment would hand every credential the run
// carries to the first command that echoes one.
func TestAssessmentGateProbeDoesNotInheritTheRunsEnvironment(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	t.Setenv("ITERION_A_SECRET_THE_RUN_CARRIES", "a-value-no-gate-may-see")

	leaking := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
		`      - "test -z \"$ITERION_A_SECRET_THE_RUN_CARRIES\""`, 1)
	if leaking == aGoodContract {
		t.Fatal("the mutation did not apply")
	}
	// The gate passes iff the variable is ABSENT from the probe environment —
	// and a gate that passes on the input tree is refused. So a refusal here
	// means the secret did not travel.
	out := lintContract(t, leaking, goodOutcomes)
	if assessmentBool(t, out, "ok") {
		t.Fatal("the gate saw the run's own environment variable — every credential the run " +
			"carries was handed to an agent-written command")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "ALREADY PASSES") {
		t.Errorf("the refusal is not the one that proves the variable was absent: %s",
			assessmentString(t, out, "reason"))
	}
}

// THE PROBE'S HOME IS AN EMPTY ONE. A variable allowlist keeps the run's
// credentials out of the gate commands' environment and does nothing about
// the credential FILES under the operator's home, one `cat ~/...` away from an
// agent-written command that runs before anyone has read the contract.
func TestAssessmentGateProbeRunsInAnEmptyHome(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "operator-credential"), []byte("not for a gate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// Green iff the probe HAS a home and it is not the operator's — and a gate
	// green on the input tree is refused. So the refusal is the proof.
	probing := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
		`      - "test -d \"$HOME\" && test ! -e \"$HOME/operator-credential\""`, 1)
	if probing == aGoodContract {
		t.Fatal("the mutation did not apply")
	}
	out := lintContract(t, probing, goodOutcomes)
	if assessmentBool(t, out, "ok") {
		t.Fatal("the gate probe ran in the operator's home — every credential file under it was one " +
			"agent-written command away")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "ALREADY PASSES") {
		t.Errorf("the refusal is not the one that proves the home was empty: %s",
			assessmentString(t, out, "reason"))
	}
}

// A MALFORMED PROFILE IS A TYPED REFUSAL, not a traceback. An operator profile
// is a file somebody wrote; every shape below used to reach an uncaught
// exception, and a node that exits non-zero with no JSON replaces its own
// verdict with the engine's generic tool failure.
func TestAssessmentMalformedProfileRefusesInsteadOfCrashing(t *testing.T) {
	requireAssessmentTools(t)
	profile, err := os.ReadFile(filepath.Join("assessment", "skills", "measurement-profile.md"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, from, to string }{
		{"a metric with no key", `{"key": "systems", "label": "distinct systems the application talks to", "discrete": true}`,
			`{"label": "distinct systems the application talks to", "discrete": true}`},
		{"no usable metric at all", `"metrics": [`, `"metrics": [] , "unused": [`},
		{"empty bands", `"bands": ["XS", "S", "M", "L", "XL", "XXL"]`, `"bands": []`},
		{"a thresholds ladder one rung short", `"thresholds": [0.5, 1.0, 2.0, 4.0, 8.0]`,
			`"thresholds": [0.5, 1.0, 2.0, 4.0]`},
		{"thresholds that are not a list", `"thresholds": [0.5, 1.0, 2.0, 4.0, 8.0]`, `"thresholds": 5`},
		// Quoted numbers pass a truthiness test and reach arithmetic: the
		// discrete spread adds one to the anchor, the band compares the index
		// with each threshold, the domain compares the lines with its floor.
		{"a quoted anchor on a discrete metric", `"entrypoints": 40,`, `"entrypoints": "40",`},
		{"a boolean anchor", `"systems": 4`, `"systems": true`},
		{"a quoted threshold", `"thresholds": [0.5, 1.0, 2.0, 4.0, 8.0]`,
			`"thresholds": [0.5, 1.0, "2.0", 4.0, 8.0]`},
		{"a quoted first-party floor", `"min_first_party_lines": 2000`, `"min_first_party_lines": "2000"`},
		{"a zero anchor", `"deployables": 2,`, `"deployables": 0,`},
		// A canonical exclusion the tool has no mechanism for would be
		// advertised beside the letter and applied by nobody.
		{"a canonical exclusion nothing applies", `"canonical_exclusions": [`,
			`"canonical_exclusions": [{"kind": "vendored", "why": "third-party code"}, `},
		{"canonical exclusions in an unreadable shape", `"canonical_exclusions": [`,
			`"canonical_exclusions": ["generated", `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(string(profile), tc.from, tc.to, 1)
			if mutated == string(profile) {
				t.Fatal("the mutation did not apply")
			}
			out := measureUnderProfile(t, mutated)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("a profile with %s published a letter", tc.name)
			}
			if code := assessmentString(t, out, "code"); code != "MEASUREMENT_REFUSED" {
				t.Fatalf("code = %q, want MEASUREMENT_REFUSED", code)
			}
		})
	}

	// The control, on the same bench: the profile the bundle ships passes every
	// one of these checks. A check that refused it would be refusing the scale
	// itself, and every case above would be red for that reason instead.
	t.Run("the shipped profile is accepted", func(t *testing.T) {
		out := measureUnderProfile(t, string(profile))
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("the bundle's own profile was refused: %s", assessmentString(t, out, "reason"))
		}
	})
}

// measureUnderProfile runs the measurement over an in-domain survey with the
// given profile as the bundle's own copy.
func measureUnderProfile(t *testing.T, profile string) map[string]any {
	t.Helper()
	ws := t.TempDir()
	writeSkills(t, bundleSkills(ws), map[string]string{"measurement-profile.md": profile})
	scratch := t.TempDir()
	out, exit, stderr := assessmentRun(t, "measure", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       scratch,
		"{{vars.profile_path}}":      "",
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         "deadbeefdeadbeef",
		"{{input.survey_path}}": writeSurvey(t, t.TempDir(), "deadbeefdeadbeef",
			[]map[string]any{{"id": "synth", "evidence": "a", "supported": true}}, inDomainSurvey(2)),
		"{{input.floor_path}}": writeFloor(t, scratch, floorLines(20000)),
	}, map[string]string{
		"{{input.extractor_outputs}}":  "[]",
		"{{input.stacks_unsupported}}": "[]",
		"{{input.stacks_covered}}":     "[]",
		"{{input.coverage_degraded}}":  "false",
		"{{input.coverage_missing}}":   "[]",
		"{{input.stacks_errored}}":     "[]",
	})
	if exit != 0 {
		t.Fatalf("measure exited %d with no verdict — the operator is handed the engine's "+
			"generic tool failure instead of MEASUREMENT_REFUSED: %s", exit, stderr)
	}
	return out
}

// gateRepo is a contract workspace carrying one more tracked file, the kind a
// gate command could rewrite: a lock file an install regenerates.
func gateRepo(t *testing.T, contract string) string {
	t.Helper()
	dir := contractRepo(t, contract, goodOutcomes)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "lock.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "add", "--", "src/lock.txt")
	gittest.Run(t, dir, "commit", "-qm", "a tracked lock file")
	return dir
}

// A GATE IS A CHECK. It runs on the live workspace, before the assessment is
// committed and before the next bot starts on the same tree: a gate that
// rewrites a tracked file changes the thing it checks, and leaves behind a
// tree nobody committed. Build output it leaves UNTRACKED is not that, and is
// accepted on the same bench.
func TestAssessmentGateProbeRefusesAGateThatWritesToTheTree(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	t.Run("a gate rewriting a tracked file", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
			`      - "bash -c 'echo regenerated >> src/lock.txt; exit 1'"`, 1)
		if contract == aGoodContract {
			t.Fatal("the mutation did not apply")
		}
		out := lintContractIn(t, gateRepo(t, contract), aGoodBrief)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a gate that rewrote a tracked file was accepted — the lint certified a gate " +
				"that edits the tree it checks, and left that edit behind")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "WRITES to what it checks") ||
			!strings.Contains(assessmentString(t, out, "reason"), "src/lock.txt") {
			t.Errorf("the refusal does not name the write: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a gate leaving untracked build output is a check", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
			`      - "bash -c 'mkdir -p build && echo out > build/out.txt; exit 1'"`, 1)
		dir := gateRepo(t, contract)
		// The tree declares its build directory disposable — real repos do —
		// and the untracked-source guard honours --exclude-standard.
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, dir, "add", "--", ".gitignore")
		gittest.Run(t, dir, "commit", "-qm", "the build directory is disposable")
		out := lintContractIn(t, dir, aGoodBrief)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a gate whose only trace is untracked build output was refused: %s",
				assessmentString(t, out, "reason"))
		}
	})

	// A command killed on the wall takes its children with it: a build left
	// running writes after the probe has moved on, where nothing attributes
	// the write to a gate.
	t.Run("a gate's children die with it on the wall", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
			`      - "bash -c '(sleep 4; echo late >> src/lock.txt) & sleep 60'"`, 1)
		dir := gateRepo(t, contract)
		out := lintContractIn(t, dir, aGoodBrief)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a gate that timed out was refused: %s", assessmentString(t, out, "reason"))
		}
		// Past the child's own delay: a survivor would have written by now.
		time.Sleep(3 * time.Second)
		if body := mustRead(t, filepath.Join(dir, "src", "lock.txt")); body != "resolved\n" {
			t.Fatalf("a child of a gate killed on the wall kept running and wrote to the tree: %q", body)
		}
	})
}

// The probe's third outcome is PUBLISHED. A gate that could not be observed —
// a timeout, a command the shell cannot find or execute — is not a failure and
// not proof either; the lot lands in gates_never_ran, because silence here
// reads exactly like proof.
func TestAssessmentContractLintPublishesTheGatesItCouldNotObserve(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
		`      - "bash ci/no-such-script-on-any-path.sh"`, 1)
	if contract == aGoodContract {
		t.Fatal("the mutation did not apply — this case would pass by accident")
	}
	out := lintContract(t, contract, goodOutcomes)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a lot whose gate could not run was refused outright — the toolchain gap is "+
			"real and the contract is still honest: %s", assessmentString(t, out, "reason"))
	}
	if out["gates_proven_red"] != float64(1) {
		t.Fatalf("gates_proven_red = %v, want 1 — a command the shell cannot find is not a gate "+
			"seen to fail", out["gates_proven_red"])
	}
	never, ok := out["gates_never_ran"].([]any)
	if !ok || len(never) != 1 {
		t.Fatalf("gates_never_ran = %v, want exactly the one lot whose gate never ran",
			out["gates_never_ran"])
	}
}

// THE OTHER DIRECTION of the goal rule: every goal the brief declares must be
// answered. A goal no outcome covers disappears from the programme, and the
// document publishes as though the objective had never been agreed.
func TestAssessmentContractLintRefusesAGoalNoOutcomeAnswers(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	twoGoals := strings.Replace(aGoodBrief,
		"  - id: supported-runtime",
		"  - id: an-objective-nobody-owes\n"+
			"    statement: \"something else the platform asked for\"\n"+
			"    rationale: \"decided elsewhere\"\n"+
			"  - id: supported-runtime", 1)
	if twoGoals == aGoodBrief {
		t.Fatal("the mutation did not apply — this case would pass by accident")
	}
	out := lintContractAgainst(t, aGoodContract, goodOutcomes, twoGoals)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a brief goal no outcome answers vanished from the programme")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "an-objective-nobody-owes") {
		t.Errorf("the refusal does not name the uncovered goal: %s",
			assessmentString(t, out, "reason"))
	}
}

// A CLEAN EXIT is not an EMPTY group: a gate that backgrounds a child and
// exits leaves it running, and a child writing after the probe returned
// forged the artefacts after the fingerprint that vouches for them.
// Reproduced on this fixture by the review gate; pinned here.
func TestAssessmentContractLintReapsAProbeChildOnASuccessPath(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	attack := `(sleep 4; printf forged >> .modernize/plan.yaml) & test -f ci/build.sh-wrote-nowhere`
	contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, "      - "+strconv.Quote(attack), 1)
	if contract == aGoodContract {
		t.Fatal("the mutation did not apply — this case would pass by accident")
	}
	dir := contractRepo(t, contract, goodOutcomes)
	before, err := os.ReadFile(filepath.Join(dir, ".modernize", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := lintContractIn(t, dir, aGoodBrief)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the lint refused a contract whose gate failed for a reason the probe observed: %s",
			assessmentString(t, out, "reason"))
	}
	// Give a surviving child its whole window before reading the tree.
	time.Sleep(5 * time.Second)
	after, err := os.ReadFile(filepath.Join(dir, ".modernize", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a background child of the gate survived the probe and wrote into the contract after it was validated")
	}
}

// A tool writing its own cache into the probe's disposable HOME is not
// tampering with this run's artefacts: the HOME was fingerprinted as output,
// and the refusal was one the cache, not the contract, earned.
func TestAssessmentContractLintDoesNotReadAProbeCacheAsTampering(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	attack := `mkdir -p "$HOME/.cache/tool" && printf log > "$HOME/.cache/tool/log" && test -f ci/build.sh-wrote-nowhere`
	contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, "      - "+strconv.Quote(attack), 1)
	if contract == aGoodContract {
		t.Fatal("the mutation did not apply — this case would pass by accident")
	}
	dir := contractRepo(t, contract, goodOutcomes)
	out := lintContractIn(t, dir, aGoodBrief)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a probe cache in the probe's own disposable home was refused as tampering: %s",
			assessmentString(t, out, "reason"))
	}
}

// THE RENDER'S HAND, VERIFIED. Between the render and this lint the drafting
// agent holds write tools; a document rewritten in that window was
// fingerprinted as found. Reproduced on this fixture by the review gate;
// pinned here with the render's own sealed digest.
func TestAssessmentContractLintRefusesDocumentsRewrittenAfterRender(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir := contractRepo(t, aGoodContract, goodOutcomes)
	assessed := filepath.Join(dir, "docs", "assessment")
	if err := os.MkdirAll(assessed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assessed, "00-state-of-the-repository.md"),
		[]byte("# state as rendered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assessed, ".plan-judgement.md"),
		[]byte("# judgement as rendered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := renderedDigest(t, dir, t.TempDir())
	// THE WINDOW: the render sealed its digest; the drafting agent's write
	// tools are still live.
	if err := os.WriteFile(filepath.Join(assessed, "00-state-of-the-repository.md"),
		[]byte("999 critical vulnerabilities were measured.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := lintContractWithDigest(t, dir, aGoodBrief, digest)
	if assessmentBool(t, out, "ok") {
		t.Fatal("documents rewritten between the render and the lint were fingerprinted as found")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "changed between the render") {
		t.Errorf("the refusal does not name the rewrite: %s", assessmentString(t, out, "reason"))
	}

	// THE SCOPE IS THE CHAIN, not the two documents: the measured facts are
	// read after the render and published by render_plan — rewritten here
	// with the WIDE digest sealed, they must refuse all the same.
	baseline := treeBaseline(t, dir) // the tree is untouched by a scratch write
	facts := filepath.Join(dir, "scratch", "facts.json")
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, []byte(`{"facts": {"size.band": "S"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wide := renderedDigest(t, dir, filepath.Join(dir, "scratch"))
	if err := os.WriteFile(facts, []byte(`{"facts": {"size.band": "XXL"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out = lintContractWithDigestAtPlan(t, dir, aGoodBrief, wide, baseline, filepath.Join(dir, "scratch"), ".modernize/plan.yaml")
	if assessmentBool(t, out, "ok") {
		t.Fatal("facts rewritten between the render and the lint were outside the sealed scope")
	}
}

// THE LOT ID IS INTERPOLATED INTO COMMANDS: the sweep record is
// `.modernize/sweeps/<id>.md`, probed by a shell outside the ordinary
// probe's fingerprint and process group. An id carrying `$(...)` executed
// its own payload there — reproduced by the review gate. A lot id is a name.
func TestAssessmentContractLintRefusesAShellShapedLotID(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	attack := `L$(printf forged >> .modernize/plan.yaml)`
	contract := strings.Replace(aGoodContract, `  - id: L2`,
		"  - id: "+strconv.Quote(attack), 1)
	if contract == aGoodContract {
		t.Fatal("the mutation did not apply — check the fixture's lot id")
	}
	dir := contractRepo(t, contract, goodOutcomes)
	before, err := os.ReadFile(filepath.Join(dir, ".modernize", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := lintContractIn(t, dir, aGoodBrief)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a contract whose lot id carries shell punctuation was accepted — the sweep probe would execute it")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "a lot id is a NAME") {
		t.Errorf("the refusal does not name the id rule: %s", assessmentString(t, out, "reason"))
	}
	time.Sleep(1 * time.Second)
	after, err := os.ReadFile(filepath.Join(dir, ".modernize", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the injected payload WROTE into the contract")
	}
}

// THE TREE THE RENDER MEASURED IS THE TREE THAT GETS VALIDATED: a source
// edit made by the drafting step — which holds write tools between the
// render and this lint — became the gate probes' accepted baseline, and a
// gate green on the rewritten tree read as proven red. Pinned with the
// render's own sealed baseline and a tracked-file edit in the window.
func TestAssessmentContractLintRefusesASourceEditInTheDraftingWindow(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir := contractRepo(t, aGoodContract, goodOutcomes)
	scratch := t.TempDir()
	digest := renderedDigest(t, dir, scratch)
	baseline := treeBaseline(t, dir)
	// THE WINDOW: a tracked source file is edited after the render sealed
	// the tree and before the lint validates the contract.
	build := filepath.Join(dir, "ci", "build.sh")
	body, err := os.ReadFile(build)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(build, []byte(string(body)+"\necho touched in the window\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := lintContractWithDigestAtPlan(t, dir, aGoodBrief, digest, baseline, scratch, ".modernize/plan.yaml")
	if assessmentBool(t, out, "ok") {
		t.Fatal("a source edit made in the drafting window became the probes' accepted baseline")
	}
	if assessmentString(t, out, "code") != "TREE_REWRITTEN" {
		t.Errorf("code = %q, want TREE_REWRITTEN: %s", assessmentString(t, out, "code"),
			assessmentString(t, out, "reason"))
	}
	_ = baseline
}

// UNTRACKED SOURCE IS SOURCE TOO: a NEW file outside the artefact roots
// never appears in a diff against HEAD, and a gate probed on it was
// baseline'd without it. Sealed by the render, refused by the lint.
func TestAssessmentContractLintRefusesAnUntrackedSourceFileInTheWindow(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir := contractRepo(t, aGoodContract, goodOutcomes)
	scratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs", "assessment"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "assessment", "00-state-of-the-repository.md"),
		[]byte("# state as rendered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "assessment", ".plan-judgement.md"),
		[]byte("# judgement as rendered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := renderedDigest(t, dir, scratch)
	baseline := treeBaseline(t, dir)
	// THE WINDOW: a new source file appears after the seal.
	if err := os.WriteFile(filepath.Join(dir, "ci", "extra.sh"), []byte("echo forged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := lintContractWithDigestAtPlan(t, dir, aGoodBrief, digest, baseline, scratch, ".modernize/plan.yaml")
	if assessmentBool(t, out, "ok") {
		t.Fatal("an untracked source file written in the window escaped the seal")
	}
	if assessmentString(t, out, "code") != "TREE_REWRITTEN" {
		t.Errorf("code = %q, want TREE_REWRITTEN: %s", assessmentString(t, out, "code"),
			assessmentString(t, out, "reason"))
	}
}

// A gate that CREATES an untracked source file in a claimed subtree left it
// for later probes and for the executor: the probe's state guard now sees
// untracked files outside the artefact roots, and refuses the write.
func TestAssessmentContractLintRefusesAGateThatCreatesASourceFile(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
		`      - "bash -c 'printf injected > src/injected.py; exit 1'"`, 1)
	dir := gateRepo(t, contract)
	out := lintContractIn(t, dir, aGoodBrief)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a gate that created an untracked source file was accepted and counted as proven red — the file stayed behind for every later reader")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "WRITES to what it checks") {
		t.Errorf("the refusal is not the write detection's: %s", assessmentString(t, out, "reason"))
	}
}

// THE LICENSED-WRITE EXCLUDES FOLLOW THE PLAN PATH: an operator may move the
// contract (plan_path is a launch var), and the seal recomputes its excludes
// from the same var — a write to the moved contract is licensed, not
// tampering.
func TestAssessmentContractLintHonoursACustomPlanPathInTheSeal(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir := contractRepo(t, aGoodContract, goodOutcomes)
	if err := os.MkdirAll(filepath.Join(dir, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "mv", ".modernize/plan.yaml", "plans/plan.yaml")
	gittest.Run(t, dir, "commit", "-qm", "the contract moves to a custom plan path")

	scratch := t.TempDir()
	digest := renderedDigest(t, dir, scratch)
	baseline := treeBaselineExcluding(t, dir, "plans/plan.yaml", "plans/outcomes.json")

	// THE WINDOW: the licensed plan file is edited — still valid YAML, still
	// the contract the drafting step wrote.
	f, err := os.OpenFile(filepath.Join(dir, "plans", "plan.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("# a drafting note the operator left\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out := lintContractWithDigestAtPlan(t, dir, aGoodBrief, digest, baseline, scratch, "plans/plan.yaml")
	if code := assessmentString(t, out, "code"); code == "TREE_REWRITTEN" {
		t.Fatalf("a write to the licensed plan file (at its custom path) was flagged as tampering:\n%s",
			assessmentString(t, out, "reason"))
	}
}
