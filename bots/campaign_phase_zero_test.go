package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Phase 0 is the campaign's own bootstrap: three child bots run before
// `preflight`, each SKIPPED on the artefact that makes it unnecessary and
// each verified in git afterwards. Every decision it takes is a tool node
// reading a file, so every decision is falsifiable in both directions —
// which is what this file does. A skip guard that only ever sees the
// artefact present proves nothing about the branch that launches the child,
// and a landed check that only ever sees a well-behaved child proves
// nothing about the refusal it exists for.

// phaseZeroOut is the union of the phase-0 node outputs the tests read.
type phaseZeroOut struct {
	Disabled      bool   `json:"disabled"`
	RunAssessment bool   `json:"run_assessment"`
	RunNet        bool   `json:"run_net"`
	HeadBefore    string `json:"head_before"`
	OracleDir     string `json:"oracle_dir"`
	VerifyPath    string `json:"verify_path"`
	LedgerPath    string `json:"ledger_path"`
	Mode          string `json:"mode"`
	CatalogPath   string `json:"catalog_path"`
	Notice        string `json:"notice"`
}

// runPhaseZeroNode renders one campaign node's python script — every
// {{…}} reference substituted, none left — and executes it. It returns the
// exit code, the parsed stdout and the stderr, because a refusal is judged
// on BOTH: the JSON carries the notice, the stderr carries the operator's
// copy of it, and a node that refuses silently on one channel is a node an
// operator reads as a success.
func runPhaseZeroNode(t *testing.T, node string, subs map[string]string, path string) (int, phaseZeroOut, string) {
	t.Helper()
	body := toolScript(t, "campaign/main.bot", node)
	for ref, val := range subs {
		body = strings.ReplaceAll(body, ref, val)
	}
	if i := strings.Index(body, "{{"); i >= 0 {
		end := i + 40
		if end > len(body) {
			end = len(body)
		}
		t.Fatalf("%s: unresolved template ref near %q", node, body[i:end])
	}
	scriptPath := filepath.Join(t.TempDir(), node+".py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	if path != "" {
		cmd.Env = append(os.Environ(), "PATH="+path)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s failed to execute: %v (stderr %q)", node, err, stderr.String())
		}
		exit = ee.ExitCode()
	}
	var out phaseZeroOut
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
		t.Fatalf("%s output is not JSON: %v (stdout %q, stderr %q)", node, err, stdout.String(), stderr.String())
	}
	return exit, out, stderr.String()
}

// phaseZeroRepo is a throwaway repository with one committed baseline file:
// the tree every phase-0 node reads.
func phaseZeroRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.invalid")
	git("config", "user.name", "t")
	writeUnder(t, ws, "README.md", "baseline\n")
	git("add", "README.md")
	git("commit", "-qm", "baseline")
	return ws, git
}

func writeUnder(t *testing.T, ws, rel, body string) {
	t.Helper()
	full := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const phaseZeroPlan = `version: 1
oracle:
  dir: .golden-master
lots:
  - id: L1
    title: a lot
    status: todo
`

// TestCampaignPhaseZeroDecidesOnTheArtefact falsifies the first skip in both
// directions, and the two refusals that flank it. The contract present and
// PARSEABLE is the only thing that skips the assessment child; a file of the
// right name that does not read is not a programme, and the child that
// writes programmes is exactly who repairs it.
func TestCampaignPhaseZeroDecidesOnTheArtefact(t *testing.T) {
	requireModernizeTools(t)

	type fixture struct {
		name        string
		plan        string // "" = absent
		brief       string // "" = absent
		enabled     bool
		wantExit    int
		wantDisable bool
		wantRun     bool
		wantIn      string // substring of the notice (exit 0) or stderr (exit 1)
	}
	for _, f := range []fixture{
		{
			name: "contract present and parseable: assessment SKIPPED, and the notice names it",
			plan: phaseZeroPlan, brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: false, wantIn: "assessment SKIPPED: the programme contract at .modernize/plan.yaml",
		},
		{
			name:  "no contract, a brief: assessment RUN, and the notice names the brief",
			brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: true, wantIn: "assessment RUN: no programme contract at .modernize/plan.yaml; the brief at .modernize/brief.yaml",
		},
		{
			name: "contract present but UNPARSEABLE, a brief: assessment RUN, and the notice says why",
			plan: "lots: [\n  - id: L1\n", brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: true, wantIn: "does not parse",
		},
		{
			name:     "no contract and NO brief: refuse, naming the brief a plan would have been guessed from",
			enabled:  true,
			wantExit: 1, wantIn: "no brief at .modernize/brief.yaml to derive one from",
		},
		{
			name: "contract unparseable and no brief: refuse too — the same missing input",
			plan: "lots: [\n  - id: L1\n", enabled: true,
			wantExit: 1, wantIn: "does not parse",
		},
		{
			name:     "switch off with NOTHING there: phase 0 is cut, and nothing is refused",
			enabled:  false,
			wantExit: 0, wantDisable: true, wantRun: false, wantIn: "phase 0 OFF (phase_zero: false)",
		},
		{
			name: "switch off with EVERYTHING there: the switch dominates the artefacts",
			plan: phaseZeroPlan, brief: "goal: x\n", enabled: false,
			wantExit: 0, wantDisable: true, wantRun: false, wantIn: "phase 0 OFF (phase_zero: false)",
		},
	} {
		f := f
		t.Run(f.name, func(t *testing.T) {
			ws, git := phaseZeroRepo(t)
			// COMMITTED, not merely written: phase 0 refuses a tree
			// carrying work in flight before it decides anything, so a
			// fixture that only wrote its files would exercise that
			// refusal instead of the decision it is about.
			if f.plan != "" {
				writeUnder(t, ws, ".modernize/plan.yaml", f.plan)
				git("add", ".modernize/plan.yaml")
			}
			if f.brief != "" {
				writeUnder(t, ws, ".modernize/brief.yaml", f.brief)
				git("add", ".modernize/brief.yaml")
			}
			if f.plan != "" || f.brief != "" {
				git("commit", "-qm", "fixture")
			}
			head := strings.TrimSpace(git("rev-parse", "HEAD"))

			exit, out, stderr := runPhaseZeroNode(t, "phase_zero", map[string]string{
				"{{vars.workspace_dir}}": strconv.Quote(ws),
				"{{vars.phase_zero}}":    strconv.FormatBool(f.enabled),
				"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
				"{{vars.brief_path}}":    strconv.Quote(".modernize/brief.yaml"),
			}, "")

			if exit != f.wantExit {
				t.Fatalf("exit = %d, want %d (notice %q, stderr %q)", exit, f.wantExit, out.Notice, stderr)
			}
			if out.Disabled != f.wantDisable {
				t.Errorf("disabled = %v, want %v", out.Disabled, f.wantDisable)
			}
			if out.RunAssessment != f.wantRun {
				t.Errorf("run_assessment = %v, want %v (notice %q)", out.RunAssessment, f.wantRun, out.Notice)
			}
			// A refusal must reach BOTH channels: the run's structured
			// output and the operator's stderr.
			if !strings.Contains(out.Notice, f.wantIn) {
				t.Errorf("notice = %q, want it to contain %q", out.Notice, f.wantIn)
			}
			if f.wantExit != 0 && !strings.Contains(stderr, f.wantIn) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, f.wantIn)
			}
			// The base sha the landed checks measure against is captured
			// HERE, before any child runs — and is not captured at all when
			// the phase is cut, because nothing will be measured.
			switch {
			case f.wantExit == 0 && f.wantDisable && out.HeadBefore != "":
				t.Errorf("head_before = %q on a cut phase, want empty", out.HeadBefore)
			case f.wantExit == 0 && !f.wantDisable && out.HeadBefore != head:
				t.Errorf("head_before = %q, want HEAD %q", out.HeadBefore, head)
			}
		})
	}
}

// TestCampaignPhaseZeroRefusesWorkInFlight. Every phase-0 decision reads the
// CHECKOUT — deliberately, so an operator's uncommitted draft contract skips
// the child rather than being overwritten by it. That is only sound while the
// checkout IS the commit. preflight makes the same refusal, but hours of child
// runs later, so phase 0 makes it first — and only when phase 0 is ON, because
// off it must behave exactly as the campaign did before.
//
// The two exclusions are exercised, not assumed: iterion lays its own
// `.claude/` scaffold in every run workspace, and the engine materialises the
// executing node's script at the workspace root. A guard that dropped either
// would refuse every run that ever reached it.
func TestCampaignPhaseZeroRefusesWorkInFlight(t *testing.T) {
	requireModernizeTools(t)

	run := func(t *testing.T, ws string, enabled bool) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "phase_zero", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.phase_zero}}":    strconv.FormatBool(enabled),
			"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
			"{{vars.brief_path}}":    strconv.Quote(".modernize/brief.yaml"),
		}, "")
	}
	withContract := func(t *testing.T) (string, func(args ...string) string) {
		t.Helper()
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")
		return ws, git
	}

	t.Run("an uncommitted file: refused before any child is launched", func(t *testing.T) {
		ws, _ := withContract(t)
		// At the root: `git status --porcelain` collapses an untracked
		// DIRECTORY to its name, here as in preflight, so a nested fixture
		// would assert on git's summarising rather than on the guard.
		writeUnder(t, ws, "inflight.txt", "somebody's work\n")
		exit, out, stderr := run(t, ws, true)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "uncommitted change(s) before phase 0") || !strings.Contains(ch, "inflight.txt") {
				t.Errorf("channel = %q, want the refusal to name the count and the file", ch)
			}
		}
	})

	t.Run("a tracked file edited: refused too", func(t *testing.T) {
		ws, _ := withContract(t)
		writeUnder(t, ws, "README.md", "baseline\nedited\n")
		if exit, out, _ := run(t, ws, true); exit != 1 || !strings.Contains(out.Notice, "README.md") {
			t.Fatalf("exit = %d, notice = %q; want 1 naming README.md", exit, out.Notice)
		}
	})

	t.Run("the same tree with phase 0 OFF: not refused", func(t *testing.T) {
		ws, _ := withContract(t)
		// At the root: `git status --porcelain` collapses an untracked
		// DIRECTORY to its name, here as in preflight, so a nested fixture
		// would assert on git's summarising rather than on the guard.
		writeUnder(t, ws, "inflight.txt", "somebody's work\n")
		exit, out, _ := run(t, ws, false)
		if exit != 0 || !out.Disabled {
			t.Fatalf("exit = %d, disabled = %v; want 0/true — off, phase 0 refuses nothing (notice %q)", exit, out.Disabled, out.Notice)
		}
	})

	t.Run("iterion's own scaffold and node script alone: not work in flight", func(t *testing.T) {
		ws, _ := withContract(t)
		// What the engine itself lays in every run workspace.
		writeUnder(t, ws, ".claude/skills/bot.md", "# scaffold\n")
		writeUnder(t, ws, ".claude/settings.json", "{}\n")
		writeUnder(t, ws, ".iterion-script-abc123.py", "print('node')\n")
		exit, out, stderr := run(t, ws, true)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 — the engine's own files are not the operator's work (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if out.RunAssessment {
			t.Errorf("run_assessment = true with a committed contract (notice %q)", out.Notice)
		}
	})

	t.Run("the scaffold plus one real stray: refused, and only the stray is named", func(t *testing.T) {
		ws, _ := withContract(t)
		writeUnder(t, ws, ".claude/skills/bot.md", "# scaffold\n")
		writeUnder(t, ws, ".iterion-script-abc123.py", "print('node')\n")
		writeUnder(t, ws, "stray.py", "x\n")
		exit, out, _ := run(t, ws, true)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "1 uncommitted change(s)") || !strings.Contains(out.Notice, "stray.py") {
			t.Errorf("notice = %q, want exactly one change, named stray.py", out.Notice)
		}
		if strings.Contains(out.Notice, ".claude/") || strings.Contains(out.Notice, ".iterion-script-") {
			t.Errorf("notice = %q, want the engine's own files left out of the count", out.Notice)
		}
	})
}

// TestCampaignPhaseZeroRefusesWithoutYq: with a contract file present and no
// yq, whether that contract READS cannot be decided. Guessing either way
// costs something real — skipping the child that repairs an unparseable
// plan, or running one that overwrites a good one — so the node refuses,
// the way preflight already refuses for the same missing tool.
func TestCampaignPhaseZeroRefusesWithoutYq(t *testing.T) {
	requireModernizeTools(t)
	ws, git := phaseZeroRepo(t)
	writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
	writeUnder(t, ws, ".modernize/brief.yaml", "goal: x\n")
	git("add", ".modernize/plan.yaml", ".modernize/brief.yaml")
	git("commit", "-qm", "fixture")

	subs := map[string]string{
		"{{vars.workspace_dir}}": strconv.Quote(ws),
		// The engine renders a bool var as a JSON literal, not a Python one:
		// the fixture must carry the producer's exact spelling or the node's
		// own `true, false, null` prelude would go untested.
		"{{vars.phase_zero}}": "true",
		"{{vars.plan_path}}":  strconv.Quote(".modernize/plan.yaml"),
		"{{vars.brief_path}}": strconv.Quote(".modernize/brief.yaml"),
	}
	// git only: yq is gone, and so is the devbox profile fallback.
	exit, out, stderr := runPhaseZeroNode(t, "phase_zero", subs, restrictedPATH(t, "git", "python3"))
	if exit != 1 {
		t.Fatalf("exit = %d without yq, want 1 (notice %q)", exit, out.Notice)
	}
	if !strings.Contains(out.Notice, "yq is not on PATH") || !strings.Contains(stderr, "yq is not on PATH") {
		t.Errorf("the refusal must name yq on both channels: notice %q, stderr %q", out.Notice, stderr)
	}
	// And the same tree WITH yq skips cleanly — so the refusal above is the
	// missing tool, not the fixture.
	exit, out, _ = runPhaseZeroNode(t, "phase_zero", subs, "")
	if exit != 0 || out.RunAssessment {
		t.Fatalf("with yq back: exit = %d, run_assessment = %v; want 0/false (notice %q)", exit, out.RunAssessment, out.Notice)
	}
}

// TestCampaignPlanLandedReadsGitNotTheChild falsifies the first landed
// check. Campy already refuses to believe a lot that says it landed
// something; a phase-0 child gets exactly the same treatment.
func TestCampaignPlanLandedReadsGitNotTheChild(t *testing.T) {
	requireModernizeTools(t)

	run := func(t *testing.T, ws, before string) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "plan_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
			"{{input.before}}":       strconv.Quote(before),
		}, "")
	}

	t.Run("the child committed the contract: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")
		exit, out, stderr := run(t, ws, before)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "assessment landed: .modernize/plan.yaml is committed") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the child committed NOTHING: refused, HEAD is the measurement", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		exit, out, stderr := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "it committed nothing") {
				t.Errorf("channel = %q, want it to say the child committed nothing", ch)
			}
		}
	})

	t.Run("HEAD moved but the contract is not in the commit: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, "NOTES.md", "the child committed something else\n")
		git("add", "NOTES.md")
		git("commit", "-qm", "not the contract")
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "is not in that commit") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the contract is written but never committed: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, "NOTES.md", "a commit that is not the contract\n")
		git("add", "NOTES.md")
		git("commit", "-qm", "moved HEAD")
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "is not in that commit") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("committed then edited: refused, preflight gets a committed tree", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan+"# edited after the commit\n")
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "uncommitted changes") {
			t.Errorf("notice = %q", out.Notice)
		}
	})
}

// netContract is a programme contract whose `oracle:` block is exactly the
// given YAML lines ("" = no oracle block at all).
func netContract(oracle string) string {
	c := "version: 1\n"
	if oracle != "" {
		c += "oracle:\n" + oracle
	}
	return c + "lots:\n  - id: L1\n    title: a lot\n    status: todo\n"
}

// netRepo is a throwaway repository carrying the given contract, committed:
// both edges into net_gate guarantee a contract file, and phase 0 refuses a
// tree carrying work in flight before it gets there.
func netRepo(t *testing.T, contract string) (string, func(args ...string) string) {
	t.Helper()
	ws, git := phaseZeroRepo(t)
	writeUnder(t, ws, ".modernize/plan.yaml", contract)
	git("add", ".modernize/plan.yaml")
	git("commit", "-qm", "contract")
	return ws, git
}

func runNetGate(t *testing.T, ws string) (int, phaseZeroOut, string) {
	t.Helper()
	return runPhaseZeroNode(t, "net_gate", map[string]string{
		"{{vars.workspace_dir}}": strconv.Quote(ws),
		"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
	}, "")
}

// TestCampaignNetGateDecidesOnTheWholeNet falsifies the second skip in both
// directions: the net is its three files, and each ONE of them missing
// launches the child. It runs on the default location AND on one the
// contract names, so the decision is read where the contract says, not from
// a hard-coded `.golden-master`.
func TestCampaignNetGateDecidesOnTheWholeNet(t *testing.T) {
	requireModernizeTools(t)

	for _, c := range []struct{ name, oracle, dir string }{
		{"the contract names no oracle.dir", "", ".golden-master"},
		{"the contract names oracle.dir .net", "  dir: .net\n", ".net"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ws, git := netRepo(t, netContract(c.oracle))
			head := strings.TrimSpace(git("rev-parse", "HEAD"))

			exit, out, stderr := runNetGate(t, ws)
			if exit != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", exit, stderr)
			}
			if !out.RunNet {
				t.Errorf("run_net = false with no net at all under %s (notice %q)", c.dir, out.Notice)
			}
			if !strings.Contains(out.Notice, "golden-master RUN: the net at "+c.dir+" (") || !strings.Contains(out.Notice, "is missing") {
				t.Errorf("notice = %q", out.Notice)
			}
			if out.HeadBefore != head {
				t.Errorf("head_before = %q, want HEAD %q", out.HeadBefore, head)
			}
			if out.OracleDir != c.dir || out.VerifyPath != filepath.Join(c.dir, "verify-oracle.sh") {
				t.Errorf("location = %q / %q, want %q and its verify-oracle.sh", out.OracleDir, out.VerifyPath, c.dir)
			}

			// A net is its three files, the same three net_landed requires.
			// Each ONE of them missing must still launch the child, and the
			// notice must name which — a leftover entry point from an
			// aborted run is exactly what would otherwise skip the child
			// that repairs it.
			whole := []string{"verify-oracle.sh", "corpus.json", "feature-coverage.json"}
			for _, missing := range whole {
				for _, name := range whole {
					path := filepath.Join(ws, c.dir, name)
					if name == missing {
						if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
							t.Fatal(err)
						}
						continue
					}
					writeUnder(t, ws, filepath.Join(c.dir, name), "x\n")
				}
				_, out, _ := runNetGate(t, ws)
				if !out.RunNet {
					t.Errorf("a net without %s must not count as a net (notice %q)", missing, out.Notice)
				}
				if !strings.Contains(out.Notice, filepath.Join(c.dir, missing)) {
					t.Errorf("notice = %q, want it to name the missing %s", out.Notice, missing)
				}
			}

			for _, name := range whole {
				writeUnder(t, ws, filepath.Join(c.dir, name), "x\n")
			}
			exit, out, _ = runNetGate(t, ws)
			if exit != 0 || out.RunNet {
				t.Fatalf("exit = %d, run_net = %v with the whole net present; want 0/false (notice %q)", exit, out.RunNet, out.Notice)
			}
			if !strings.Contains(out.Notice, "golden-master SKIPPED: the net at "+c.dir+" (") ||
				!strings.Contains(out.Notice, filepath.Join(c.dir, "feature-coverage.json")) {
				t.Errorf("notice = %q, want it to name the artefacts that caused the skip", out.Notice)
			}
		})
	}
}

// TestCampaignNetLocationIsTheContracts: phase 0 builds and judges the net
// where the CONTRACT puts it, because that is where preflight — and every
// lot after it — looks. A location from anywhere else would, on a contract
// naming another directory, skip a net that is there or build one nobody
// reads.
func TestCampaignNetLocationIsTheContracts(t *testing.T) {
	requireModernizeTools(t)

	whole := func(t *testing.T, ws, dir string) {
		t.Helper()
		for _, name := range []string{"verify-oracle.sh", "corpus.json", "feature-coverage.json"} {
			writeUnder(t, ws, filepath.Join(dir, name), "x\n")
		}
	}
	gm := "  dir: .gm\n"

	t.Run("the contract names .gm and the net is complete THERE: skipped", func(t *testing.T) {
		ws, _ := netRepo(t, netContract(gm))
		whole(t, ws, ".gm")
		exit, out, stderr := runNetGate(t, ws)
		if exit != 0 || out.RunNet {
			t.Fatalf("exit = %d, run_net = %v; want 0/false — the net the contract names is complete (notice %q, stderr %q)", exit, out.RunNet, out.Notice, stderr)
		}
		if out.OracleDir != ".gm" || !strings.Contains(out.Notice, "named by the contract") {
			t.Errorf("oracle_dir = %q, notice = %q; want .gm, attributed to the contract", out.OracleDir, out.Notice)
		}
	})

	t.Run("the contract names .gm and the net is complete ELSEWHERE: the child builds at .gm", func(t *testing.T) {
		ws, _ := netRepo(t, netContract(gm))
		whole(t, ws, ".golden-master")
		exit, out, _ := runNetGate(t, ws)
		if exit != 0 || !out.RunNet {
			t.Fatalf("exit = %d, run_net = %v; want 0/true — a net where the contract does not look is no net (notice %q)", exit, out.RunNet, out.Notice)
		}
		if out.OracleDir != ".gm" || !strings.Contains(out.Notice, filepath.Join(".gm", "verify-oracle.sh")) {
			t.Errorf("oracle_dir = %q, notice = %q; want the child sent to .gm", out.OracleDir, out.Notice)
		}
	})

	// The derivation must be preflight's, term for term: run BOTH real nodes
	// on the same committed tree and compare what they conclude. This is the
	// guard against the two copies drifting apart — preflight is not edited
	// by phase 0, so the copy is the one that must follow.
	for _, c := range []struct{ name, oracle, dir, verify string }{
		{"no oracle block", "", ".golden-master", ".golden-master/verify-oracle.sh"},
		{"an oracle block naming only refs_dir", "  refs_dir: .golden-master/refs\n", ".golden-master", ".golden-master/verify-oracle.sh"},
		{"oracle.dir only", "  dir: .gm\n", ".gm", ".gm/verify-oracle.sh"},
		{"oracle.dir and its own verify", "  dir: .gm\n  verify: .gm/run-net.sh\n", ".gm", ".gm/run-net.sh"},
		{"oracle.verify only", "  verify: ci/verify-net.sh\n", ".golden-master", "ci/verify-net.sh"},
		{"a nested directory", "  dir: quality/net\n", "quality/net", "quality/net/verify-oracle.sh"},
	} {
		c := c
		t.Run("same location as preflight: "+c.name, func(t *testing.T) {
			ws, git := netRepo(t, netContract(c.oracle))
			writeUnder(t, ws, c.verify, "x\n")
			writeUnder(t, ws, filepath.Join(c.dir, "corpus.json"), "x\n")
			writeUnder(t, ws, filepath.Join(c.dir, "feature-coverage.json"), "x\n")
			git("add", "-A")
			git("commit", "-qm", "net")

			exit, gate, stderr := runNetGate(t, ws)
			if exit != 0 || gate.RunNet {
				t.Fatalf("net_gate: exit = %d, run_net = %v; want 0/false (notice %q, stderr %q)", exit, gate.RunNet, gate.Notice, stderr)
			}
			exit, pre, stderr := runPhaseZeroNode(t, "preflight", map[string]string{
				"{{vars.workspace_dir}}": strconv.Quote(ws),
				"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
			}, "")
			if exit != 0 {
				t.Fatalf("preflight: exit = %d, want 0 on the same tree (notice %q, stderr %q)", exit, pre.Notice, stderr)
			}
			if gate.VerifyPath != pre.VerifyPath || gate.OracleDir != filepath.Dir(pre.LedgerPath) {
				t.Errorf("net_gate says %q / %q, preflight says %q / %q — phase 0 would build a net preflight does not read",
					gate.OracleDir, gate.VerifyPath, filepath.Dir(pre.LedgerPath), pre.VerifyPath)
			}
			if gate.OracleDir != c.dir || gate.VerifyPath != c.verify {
				t.Errorf("location = %q / %q, want %q / %q", gate.OracleDir, gate.VerifyPath, c.dir, c.verify)
			}
		})
	}

	// The child writes its entry point at <dir>/verify-oracle.sh, never where
	// a contract chooses. A contract naming another one that is absent asks
	// for something no run of that child produces.
	t.Run("a contract-chosen entry point that is absent: refused before the child is launched", func(t *testing.T) {
		ws, _ := netRepo(t, netContract("  dir: .gm\n  verify: ci/verify-net.sh\n"))
		exit, out, stderr := runNetGate(t, ws)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "names the net's entry point ci/verify-net.sh") || !strings.Contains(ch, ".gm/verify-oracle.sh") {
				t.Errorf("channel = %q, want the refusal to name both entry points", ch)
			}
		}
	})
	t.Run("the same contract with that entry point committed: the child completes the rest", func(t *testing.T) {
		ws, git := netRepo(t, netContract("  dir: .gm\n  verify: ci/verify-net.sh\n"))
		writeUnder(t, ws, "ci/verify-net.sh", "x\n")
		git("add", "ci/verify-net.sh")
		git("commit", "-qm", "entry point")
		exit, out, _ := runNetGate(t, ws)
		if exit != 0 || !out.RunNet || out.VerifyPath != "ci/verify-net.sh" {
			t.Fatalf("exit = %d, run_net = %v, verify = %q; want 0/true/ci/verify-net.sh (notice %q)", exit, out.RunNet, out.VerifyPath, out.Notice)
		}
	})

	for _, c := range []struct{ name, oracle, named string }{
		{"an absolute oracle.dir", "  dir: /srv/net\n", "/srv/net"},
		{"an oracle.dir escaping the workspace", "  dir: ../net\n", "../net"},
		{"an oracle.verify escaping the workspace", "  verify: ../verify.sh\n", "../verify.sh"},
	} {
		c := c
		t.Run(c.name+": refused, by name", func(t *testing.T) {
			ws, _ := netRepo(t, netContract(c.oracle))
			exit, out, stderr := runNetGate(t, ws)
			if exit != 1 {
				t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
			}
			for _, ch := range []string{out.Notice, stderr} {
				if !strings.Contains(ch, "puts the net at "+c.named+", outside the workspace") {
					t.Errorf("channel = %q, want the refusal to name %s", ch, c.named)
				}
			}
		})
	}

	t.Run("a contract that does not read: refused rather than guessed", func(t *testing.T) {
		ws, _ := netRepo(t, "lots: [\n  - id: L1\n")
		exit, out, stderr := runNetGate(t, ws)
		if exit != 1 || !strings.Contains(out.Notice, "does not read as a mapping") || !strings.Contains(stderr, "does not read as a mapping") {
			t.Fatalf("exit = %d, notice %q, stderr %q; want 1 and a named refusal on both channels", exit, out.Notice, stderr)
		}
	})

	t.Run("no yq: refused, and the same tree with yq decides", func(t *testing.T) {
		ws, _ := netRepo(t, netContract(gm))
		subs := map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
		}
		exit, out, stderr := runPhaseZeroNode(t, "net_gate", subs, restrictedPATH(t, "git", "python3"))
		if exit != 1 || !strings.Contains(out.Notice, "yq is not on PATH") || !strings.Contains(stderr, "yq is not on PATH") {
			t.Fatalf("exit = %d, notice %q, stderr %q; want 1 naming yq on both channels", exit, out.Notice, stderr)
		}
		if exit, out, _ := runPhaseZeroNode(t, "net_gate", subs, ""); exit != 0 || out.OracleDir != ".gm" {
			t.Fatalf("with yq back: exit = %d, oracle_dir = %q; want 0/.gm (notice %q)", exit, out.OracleDir, out.Notice)
		}
	})
}

// TestCampaignNetLandedRequiresTheWholeNet falsifies the second landed
// check. The three files are one artefact: the entry point CI runs, the
// corpus it compares, and the feature inventory the docs child's gate reads
// next. Two of three is a partial net, and lots reported done against a
// partial net are lots reported done against nothing.
func TestCampaignNetLandedRequiresTheWholeNet(t *testing.T) {
	requireModernizeTools(t)

	const oracle = ".golden-master"
	runAt := func(t *testing.T, ws, before, dir, verify string) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "net_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{input.before}}":       strconv.Quote(before),
			"{{input.oracle_dir}}":   strconv.Quote(dir),
			"{{input.verify_path}}":  strconv.Quote(verify),
		}, "")
	}
	run := func(t *testing.T, ws, before string) (int, phaseZeroOut, string) {
		t.Helper()
		return runAt(t, ws, before, oracle, filepath.Join(oracle, "verify-oracle.sh"))
	}
	commitNet := func(t *testing.T, ws string, git func(...string) string, names ...string) {
		t.Helper()
		for _, n := range names {
			writeUnder(t, ws, filepath.Join(oracle, n), "x\n")
			git("add", filepath.Join(oracle, n))
		}
		git("commit", "-qm", "net")
	}
	whole := []string{"verify-oracle.sh", "corpus.json", "feature-coverage.json"}

	t.Run("the whole net committed: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		exit, out, stderr := run(t, ws, before)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "golden-master landed") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the child committed NOTHING: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		exit, out, _ := run(t, ws, before)
		if exit != 1 || !strings.Contains(out.Notice, "it committed nothing") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a named refusal", exit, out.Notice)
		}
	})

	// Each file alone is the one missing piece, so no single name carries
	// the whole check.
	for _, missing := range whole {
		missing := missing
		t.Run("without "+missing+": refused, and the refusal names it", func(t *testing.T) {
			ws, git := phaseZeroRepo(t)
			before := strings.TrimSpace(git("rev-parse", "HEAD"))
			var partial []string
			for _, n := range whole {
				if n != missing {
					partial = append(partial, n)
				}
			}
			commitNet(t, ws, git, partial...)
			exit, out, _ := run(t, ws, before)
			if exit != 1 {
				t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
			}
			if !strings.Contains(out.Notice, filepath.Join(oracle, missing)+" not committed") {
				t.Errorf("notice = %q, want it to name %s", out.Notice, missing)
			}
		})
	}

	// The location is net_gate's, carried on the edge: the gate judges the
	// net where the child was TOLD to build it.
	t.Run("told .gm, the net committed at .gm: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		for _, n := range whole {
			writeUnder(t, ws, filepath.Join(".gm", n), "x\n")
			git("add", filepath.Join(".gm", n))
		}
		git("commit", "-qm", "net")
		if exit, out, stderr := runAt(t, ws, before, ".gm", ".gm/verify-oracle.sh"); exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
	})
	t.Run("told .gm, the net committed at the default instead: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		exit, out, _ := runAt(t, ws, before, ".gm", ".gm/verify-oracle.sh")
		if exit != 1 || !strings.Contains(out.Notice, ".gm/corpus.json") {
			t.Fatalf("exit = %d, notice = %q; want 1 naming the net where it was told to be", exit, out.Notice)
		}
	})
	t.Run("no location on the edge: refused rather than guessed", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		exit, out, stderr := runAt(t, ws, before, "", "")
		if exit != 1 || !strings.Contains(out.Notice, "received no net location") || !strings.Contains(stderr, "received no net location") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a named refusal on both channels", exit, out.Notice)
		}
	})

	t.Run("the whole net committed, then something left dirty: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		writeUnder(t, ws, filepath.Join(oracle, "refs", "stray.txt"), "uncommitted\n")
		exit, out, _ := run(t, ws, before)
		if exit != 1 || !strings.Contains(out.Notice, "uncommitted changes") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a dirt refusal", exit, out.Notice)
		}
	})
}

// TestCampaignDocsGateWritesTheCatalogOutOfTree falsifies the docs
// decision: the mode is read from whether the documentation directory
// already carries pages, and the generated catalog is written OUT of the
// workspace — a catalog written into the tree would be the uncommitted
// change preflight then refuses.
func TestCampaignDocsGateWritesTheCatalogOutOfTree(t *testing.T) {
	requireModernizeTools(t)

	const docsDir = "docs/client"
	scratch := filepath.Join(t.TempDir(), "scratch", "campaign")
	subsFor := func(ws, productID string) map[string]string {
		return map[string]string{
			"{{vars.workspace_dir}}":   strconv.Quote(ws),
			"{{vars.docs_dir}}":        strconv.Quote(docsDir),
			"{{vars.docs_product_id}}": strconv.Quote(productID),
			"{{vars.scratch_dir}}":     strconv.Quote(scratch),
		}
	}

	t.Run("no page yet: full, and the tree stays clean", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		exit, out, stderr := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", exit, stderr)
		}
		if out.Mode != "full" {
			t.Errorf("mode = %q with no page, want full (notice %q)", out.Mode, out.Notice)
		}
		if !filepath.IsAbs(out.CatalogPath) {
			t.Errorf("catalog_path = %q, want an absolute path the child can read as-is", out.CatalogPath)
		}
		if strings.HasPrefix(out.CatalogPath, ws+string(os.PathSeparator)) {
			t.Errorf("catalog_path = %q is inside the workspace %q — it would dirty the tree preflight judges", out.CatalogPath, ws)
		}
		if dirt := strings.TrimSpace(git("status", "--porcelain")); dirt != "" {
			t.Errorf("the workspace is dirty after docs_gate: %q", dirt)
		}

		// The catalog carries the frozen shape, read back through yq —
		// not through a string match on what the node wrote.
		//
		// This asserts the PRODUCER only: a green here is not a working
		// integration. TestCampaignPhaseZeroChildrenReadWhatTheyAreHanded
		// feeds this catalog to the docs child's own catalog_ingest.
		raw, err := exec.Command("yq", "-o=json", out.CatalogPath).Output()
		if err != nil {
			t.Fatalf("the generated catalog does not parse: %v", err)
		}
		var got struct {
			ID   string `json:"id"`
			Docs struct {
				ProductDir string `json:"product_dir"`
			} `json:"docs"`
			Repos []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"repos"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("catalog JSON: %v (%s)", err, raw)
		}
		if got.ID != "product" || got.Docs.ProductDir != docsDir {
			t.Errorf("catalog id/product_dir = %q/%q, want %q/%q", got.ID, got.Docs.ProductDir, "product", docsDir)
		}
		if len(got.Repos) != 1 || got.Repos[0].ID != "product" || got.Repos[0].Path != "." {
			t.Errorf("catalog repos = %+v, want exactly one local source {id: product, path: .}", got.Repos)
		}
	})

	t.Run("a page already there: incremental", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		exit, out, _ := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if exit != 0 || out.Mode != "incremental" {
			t.Fatalf("exit = %d, mode = %q; want 0/incremental (notice %q)", exit, out.Mode, out.Notice)
		}
	})

	t.Run("a non-markdown file is not a page: still full", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, ".gitkeep"), "")
		_, out, _ := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if out.Mode != "full" {
			t.Errorf("mode = %q, want full — only pages count (notice %q)", out.Mode, out.Notice)
		}
	})

	t.Run("no product id: refused rather than a catalog naming nothing", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		exit, out, stderr := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "  "), "")
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "docs_product_id is empty") || !strings.Contains(stderr, "docs_product_id is empty") {
			t.Errorf("notice %q / stderr %q, want both to name the empty id", out.Notice, stderr)
		}
	})
}

// TestCampaignDocsLandedRequiresCommittedPages falsifies the third landed
// check. This child is the one that legitimately commits nothing — an
// incremental pass over documentation nothing changed — so HEAD is NOT the
// measurement here; the pages are.
func TestCampaignDocsLandedRequiresCommittedPages(t *testing.T) {
	requireModernizeTools(t)

	const docsDir = "docs/client"
	// pagesBefore is docs_gate's count, taken BEFORE the child ran: the one
	// thing that lets this gate speak about THIS run rather than about a
	// product that arrived documented.
	run := func(t *testing.T, ws string, pagesBefore int) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "docs_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.docs_dir}}":      strconv.Quote(docsDir),
			"{{input.pages_before}}": strconv.Itoa(pagesBefore),
		}, "")
	}

	t.Run("pages landed where there were none: accepted, and the notice says so", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		exit, out, stderr := run(t, ws, 0)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "1 page(s), where the product had none") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	// The honesty half. On a product that arrived documented, "there are
	// pages at HEAD" was true before the child ran at all — so a child that
	// finalised its series elsewhere leaves this gate seeing exactly what it
	// saw before. It may not claim to have caught that; it must say which of
	// the two it is looking at.
	t.Run("an already-documented product, nothing new: accepted, and the notice refuses to claim more", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "next.md"), "# next\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"), filepath.Join(docsDir, "guide", "next.md"))
		git("commit", "-qm", "pages")
		exit, out, _ := run(t, ws, 2)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 — an incremental pass may land nothing (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "the same 2 page(s) it started from") ||
			!strings.Contains(out.Notice, "cannot tell") {
			t.Errorf("notice = %q, want it to say the gate cannot tell a no-op pass from a child that committed elsewhere", out.Notice)
		}
	})

	t.Run("an already-documented product, pages added: the notice counts THIS run", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "a.md"), "# a\n")
		writeUnder(t, ws, filepath.Join(docsDir, "b.md"), "# b\n")
		writeUnder(t, ws, filepath.Join(docsDir, "c.md"), "# c\n")
		git("add", filepath.Join(docsDir, "a.md"), filepath.Join(docsDir, "b.md"), filepath.Join(docsDir, "c.md"))
		git("commit", "-qm", "pages")
		exit, out, _ := run(t, ws, 1)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "2 page(s) more than the 1 this run started from") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	// The three landed gates scope their dirt check to their own artefact,
	// which is what makes their refusals name a child. A child writing
	// OUTSIDE its paths passes all three — and preflight then refuses the
	// campaign once the whole bootstrap has been paid. This is the
	// chokepoint for that class.
	t.Run("a stray left outside every checked artefact: refused here, not by preflight hours later", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		// Neither under docs_dir, nor the contract, nor the oracle dir.
		writeUnder(t, ws, "survey.json", "{}\n")
		exit, out, stderr := run(t, ws, 0)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "outside the artefacts it checks") || !strings.Contains(ch, "survey.json") {
				t.Errorf("channel = %q, want the refusal to name the class and the file", ch)
			}
		}
	})

	t.Run("iterion's own scaffold and node script alone: not a stray", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		writeUnder(t, ws, ".claude/skills/bot.md", "# scaffold\n")
		writeUnder(t, ws, ".iterion-script-abc123.py", "print('node')\n")
		if exit, out, stderr := run(t, ws, 0); exit != 0 {
			t.Fatalf("exit = %d, want 0 — the engine's own files are not a stray (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
	})

	t.Run("no page committed: refused, and the refusal names the branch case", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		exit, out, stderr := run(t, ws, 0)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "committed no page under "+docsDir) || !strings.Contains(ch, "onto a branch") {
				t.Errorf("channel = %q, want the refusal to name the directory and the branch case", ch)
			}
		}
	})

	t.Run("pages written but never committed: refused", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		exit, out, _ := run(t, ws, 0)
		if exit != 1 || !strings.Contains(out.Notice, "committed no page under") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a named refusal", exit, out.Notice)
		}
	})

	t.Run("one page committed, another left dirty: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "next.md"), "# next\n")
		exit, out, _ := run(t, ws, 1)
		if exit != 1 || !strings.Contains(out.Notice, "uncommitted changes") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a dirt refusal", exit, out.Notice)
		}
	})
}

// TestCampaignPhaseZeroTopology pins the graph phase 0 adds, against the
// FROZEN interface of the three children — the sources they live at and the
// vars they are handed. A `with:` key that drifts compiles clean and hands
// the child its own default instead, silently documenting or netting
// somewhere the supervisor never looks.
func TestCampaignPhaseZeroTopology(t *testing.T) {
	wf := compileBot(t, "campaign")
	if wf == nil {
		t.Fatal("campaign did not compile")
	}

	if wf.Entry != "phase_zero" {
		t.Errorf("entry = %q, want phase_zero — phase 0 runs BEFORE preflight", wf.Entry)
	}

	for _, want := range []struct {
		node, source string
		with         []string
	}{
		{"assessment", "../assessment/main.bot", []string{"brief_path", "plan_path", "workspace_dir"}},
		{"golden_master", "../golden-master/main.bot", []string{"oracle_dir", "workspace_dir"}},
		{"product_docs", "../product-docs/main.bot", []string{"catalog_path", "mode", "oracle_dir", "product_id", "workspace_dir"}},
	} {
		sb, ok := wf.Nodes[want.node].(*ir.SubbotNode)
		if !ok {
			t.Errorf("%s is %T, want a subbot node", want.node, wf.Nodes[want.node])
			continue
		}
		if sb.Source != want.source {
			t.Errorf("%s source = %q, want %q", want.node, sb.Source, want.source)
		}
		var keys []string
		for _, m := range sb.With {
			keys = append(keys, m.Key)
		}
		sortStrings(keys)
		if strings.Join(keys, ",") != strings.Join(want.with, ",") {
			t.Errorf("%s with: keys = %v, want %v", want.node, keys, want.with)
		}
		// No `output:`: the supervisor believes nothing a child says of
		// itself, and the landed checks measure git instead.
		if sb.OutputSchema != "" {
			t.Errorf("%s declares output: %q — progress is judged in git, never from the child's own report", want.node, sb.OutputSchema)
		}
		if sb.Isolated {
			t.Errorf("%s is isolated: the child must write into THIS checkout", want.node)
		}
	}

	// The order is not a preference: the docs child's gate reads the net's
	// inventory, so it cannot run before the net exists.
	for _, e := range []struct {
		from, to, cond string
		neg            bool
	}{
		{"phase_zero", "preflight", "disabled", false},
		{"phase_zero", "assessment", "run_assessment", false},
		{"phase_zero", "net_gate", "", false},
		{"assessment", "plan_landed", "", false},
		{"plan_landed", "net_gate", "", false},
		{"net_gate", "golden_master", "run_net", false},
		{"net_gate", "docs_gate", "", false},
		{"golden_master", "net_landed", "", false},
		{"net_landed", "docs_gate", "", false},
		{"docs_gate", "product_docs", "", false},
		{"product_docs", "docs_landed", "", false},
		{"docs_landed", "preflight", "", false},
		{"preflight", "run_lot", "", false},
	} {
		if !hasEdge(wf, e.from, e.to, e.cond, e.neg) {
			t.Errorf("missing edge %s -> %s (when %q, negated %v)", e.from, e.to, e.cond, e.neg)
		}
	}

	// The base sha the landed checks measure against travels on the edge,
	// captured by the graph before the child ran — never re-read after. So
	// does the net's location: derived once, by net_gate, from the contract.
	for _, e := range []struct{ from, key, ref string }{
		{"assessment", "before", "{{outputs.phase_zero.head_before}}"},
		{"golden_master", "before", "{{outputs.net_gate.head_before}}"},
		{"golden_master", "oracle_dir", "{{outputs.net_gate.oracle_dir}}"},
		{"golden_master", "verify_path", "{{outputs.net_gate.verify_path}}"},
	} {
		if !edgeCarries(wf, e.from, e.key, e.ref) {
			t.Errorf("the edge out of %s does not carry %s: %s", e.from, e.key, e.ref)
		}
	}
	for _, child := range []string{"golden_master", "product_docs"} {
		sb, _ := wf.Nodes[child].(*ir.SubbotNode)
		if sb == nil {
			continue
		}
		for _, m := range sb.With {
			if m.Key == "oracle_dir" && m.Raw != "{{outputs.net_gate.oracle_dir}}" {
				t.Errorf("%s is handed oracle_dir %q, want net_gate's derivation — the net lives where the contract says", child, m.Raw)
			}
		}
	}
	// A var would be a second source for the net's location, and a campaign
	// setting it would build a net preflight never reads.
	if wf.Vars["oracle_dir"] != nil {
		t.Errorf("oracle_dir is a var again: the net's location is the contract's to say")
	}

	// Every new var has a default: a campaign that declared phase 0 without
	// one would refuse to launch rather than skip it.
	for _, name := range []string{"phase_zero", "brief_path", "docs_dir", "docs_product_id", "scratch_dir"} {
		v := wf.Vars[name]
		if v == nil {
			t.Errorf("var %q is not declared", name)
			continue
		}
		if !v.HasDefault {
			t.Errorf("var %q has no default", name)
		}
	}
	if wf.Vars["docs_dir"] != nil && wf.Vars["docs_dir"].Default != "docs/client" {
		t.Errorf("docs_dir default = %v, want docs/client", wf.Vars["docs_dir"].Default)
	}
}

// TestCampaignPhaseZeroChildrenReadWhatTheyAreHanded holds phase 0 against
// its REAL children, not against the shape it was built to. Two failures pass
// every test that looks at the producer alone: a `with:` key the child does not
// declare is dropped without a word, and a catalog source the docs child
// cannot resolve is recorded `degraded` — the campaign would then document a
// product from nothing. So every key is checked against the child's declared
// vars, and the catalog docs_gate writes is fed to the docs child's own
// catalog_ingest.
//
// The docs child's half of the interface is its `oracle_dir` var and its
// local-source catalog form (`repos[].path`), which arrive together. While the
// docs child in this tree declares no `oracle_dir`, the one thing that may hold
// is that phase 0 defaults OFF — and that is asserted, so the day either side
// moves alone this test reddens instead of a campaign.
func TestCampaignPhaseZeroChildrenReadWhatTheyAreHanded(t *testing.T) {
	requireModernizeTools(t)

	campy := compileBot(t, "campaign")
	docs := compileBot(t, "product-docs")
	if campy == nil || docs == nil {
		t.Fatal("campaign or product-docs did not compile")
	}
	docsReady := docs.Vars["oracle_dir"] != nil
	phaseZeroOnByDefault := campy.Vars["phase_zero"] != nil && campy.Vars["phase_zero"].Default == true

	for node, bundle := range map[string]string{
		"assessment":    "assessment",
		"golden_master": "golden-master",
		"product_docs":  "product-docs",
	} {
		sb, ok := campy.Nodes[node].(*ir.SubbotNode)
		if !ok {
			t.Errorf("%s is %T, want a subbot node", node, campy.Nodes[node])
			continue
		}
		child := compileBot(t, bundle)
		if child == nil {
			t.Errorf("%s does not compile", bundle)
			continue
		}
		for _, m := range sb.With {
			if child.Vars[m.Key] != nil {
				continue
			}
			if node == "product_docs" && m.Key == "oracle_dir" && !docsReady && !phaseZeroOnByDefault {
				continue
			}
			t.Errorf("%s hands %s a %q it does not declare: the value is dropped without a word", node, bundle, m.Key)
		}
	}

	// The catalog and the net, end to end: net_gate derives where the net
	// lives from the contract, docs_gate writes the catalog, and the docs
	// child reads both — with the values phase 0 hands over, not literals.
	// The contract names a non-default directory so a child that ignored the
	// handed value and fell back to its own default could not pass.
	const docsDir = "docs/client"
	ws, git := netRepo(t, netContract("  dir: .gm\n"))
	for _, name := range []string{"verify-oracle.sh", "corpus.json", "feature-coverage.json"} {
		writeUnder(t, ws, filepath.Join(".gm", name), "{}\n")
	}
	git("add", ".gm")
	git("commit", "-qm", "net")
	exit, net, stderr := runNetGate(t, ws)
	if exit != 0 || net.OracleDir != ".gm" {
		t.Fatalf("net_gate: exit = %d, oracle_dir = %q; want 0/.gm (notice %q, stderr %q)", exit, net.OracleDir, net.Notice, stderr)
	}
	exit, gate, stderr := runPhaseZeroNode(t, "docs_gate", map[string]string{
		"{{vars.workspace_dir}}":   strconv.Quote(ws),
		"{{vars.docs_dir}}":        strconv.Quote(docsDir),
		"{{vars.docs_product_id}}": strconv.Quote("product"),
		"{{vars.scratch_dir}}":     strconv.Quote(filepath.Join(t.TempDir(), "campaign")),
	}, "")
	if exit != 0 {
		t.Fatalf("docs_gate: exit = %d (notice %q, stderr %q)", exit, gate.Notice, stderr)
	}
	secretGlobs, _ := docs.Vars["secret_globs"].Default.(string)
	// Every var catalog_ingest reads, at the value the run gives it: the four
	// phase 0 hands over, the child's own defaults for the rest. A superset —
	// resolveCommand fails on a ref left behind, not on one unused.
	cmd := resolveCommand(t, toolCommand(t, "product-docs/main.bot", "catalog_ingest"), map[string]string{
		"vars.workspace_dir": ws,
		"vars.catalog_path":  gate.CatalogPath,
		"vars.product_id":    "product",
		"vars.oracle_dir":    net.OracleDir,
		"vars.scratch_dir":   filepath.Join(t.TempDir(), "product-docs"),
		"vars.clone_depth":   strconv.FormatInt(docs.Vars["clone_depth"].Default.(int64), 10),
		"vars.secret_globs":  secretGlobs,
	})
	var got struct {
		ProductDir string           `json:"product_dir"`
		Inventory  []map[string]any `json:"inventory"`
		OKCount    int              `json:"ok_count"`
		OraclePath string           `json:"oracle_path"`
		Log        string           `json:"log"`
	}
	runJSON(t, cmd, &got)
	if got.ProductDir != docsDir {
		t.Errorf("the docs child resolved product_dir %q from the generated catalog, want %q", got.ProductDir, docsDir)
	}

	if !docsReady {
		if phaseZeroOnByDefault {
			t.Fatalf("phase_zero defaults ON while the docs child declares no oracle_dir and reads the generated catalog as: %s", got.Log)
		}
		t.Logf("the docs child declares no oracle_dir: its half of the interface is not in this tree, and phase 0 defaults OFF (catalog read as: %s)", got.Log)
		return
	}
	if len(got.Inventory) != 1 || got.OKCount != 1 {
		t.Fatalf("the docs child read %d source(s), %d ok, from the generated catalog; want exactly this checkout, ok (%s)", len(got.Inventory), got.OKCount, got.Log)
	}
	if e := got.Inventory[0]; e["status"] != "ok" || e["local"] != true {
		t.Errorf("source entry = %v, want status ok and local — this checkout, read from disk without a forge", e)
	}
	wantNet, _ := filepath.EvalSymlinks(filepath.Join(ws, net.OracleDir))
	gotNet, _ := filepath.EvalSymlinks(got.OraclePath)
	if got.OraclePath == "" || gotNet != wantNet {
		t.Errorf("the docs child found its net at %q, want %q — the exhaustiveness gate would not arm (%s)", got.OraclePath, wantNet, got.Log)
	}
}

// TestCampaignPreflightStillRefusesWhatItAlwaysRefused: phase 0 PRODUCES,
// preflight JUDGES. The node is untouched — same refusals, same position
// ahead of the worker — and phase 0 is wired around it, not through it.
func TestCampaignPreflightStillRefusesWhatItAlwaysRefused(t *testing.T) {
	wf := compileBot(t, "campaign")
	if wf == nil {
		t.Fatal("campaign did not compile")
	}
	if _, ok := wf.Nodes["preflight"].(*ir.ToolNode); !ok {
		t.Fatalf("preflight is %T, want a tool node", wf.Nodes["preflight"])
	}
	script := toolScript(t, "campaign/main.bot", "preflight")
	for _, sentinel := range []string{
		"no programme contract at %s",
		"write the plan first",
		"no behavioural oracle at %s",
		"build the net",
		"uncommitted change(s) at campaign start",
	} {
		if !strings.Contains(script, sentinel) {
			t.Errorf("preflight no longer carries the refusal %q — phase 0 must not soften the node that judges it", sentinel)
		}
	}
	// Nothing enters the worker except through preflight.
	for _, e := range wf.Edges {
		if e.To == "run_lot" && e.From != "preflight" && e.From != "loop_gate" {
			t.Errorf("edge %s -> run_lot bypasses preflight", e.From)
		}
	}
}

func hasEdge(wf *ir.Workflow, from, to, cond string, negated bool) bool {
	for _, e := range wf.Edges {
		if e.From == from && e.To == to && e.Condition == cond && e.Negated == negated && e.Expression == nil {
			return true
		}
	}
	return false
}

func edgeCarries(wf *ir.Workflow, from, key, raw string) bool {
	for _, e := range wf.Edges {
		if e.From != from {
			continue
		}
		for _, m := range e.With {
			if m.Key == key && m.Raw == raw {
				return true
			}
		}
	}
	return false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
