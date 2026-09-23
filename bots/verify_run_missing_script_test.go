package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Every bot in the catalogue that carries a deterministic verify_run splits the
// gate in two: an agent writes the repo's real build+suite into
// <scratch_dir>/verify.sh, and this tool node re-runs it and reports the REAL
// exit code. The whole point is that the verdict is not an LLM judgment.
//
// When the agent produced no script, ten of the eleven carriers used to answer
// `passed: true, skipped: true` — the comment they shipped said it out loud,
// "counted as pass, but surfaced". Nothing read `skipped`: e2e-coverage declares
// it in verify_result and its convergence expression never mentions it, so "the
// gate was skipped" arrived at the continuation decision spelled "the build
// passed". A gate that did not run cannot certify, and #1585 rests the
// unattended-merge decision on exactly these gates.
//
// The carriers are DISCOVERED, not listed. A hand-written roster is how this
// defect spread in the first place: it was reported against one bot and lived in
// ten, and the first version of this very test listed ten while the catalogue
// held eleven (dep-update-guard, whose gate already refused). A new bot cannot
// join the catalogue without joining this guard.

// executesTheScript is the same discriminator TestVerifyRunDriftTailPresentInAllBots
// trusts: a carrier is a node that actually runs the agent-written script. Reading
// it off the COMPILED command also yields the node name for free, so an
// irregularly-named gate (secured-renovacy's p2_verify_run) is found without
// anyone remembering it.
const executesTheScript = "subprocess.run(['sh', script]"

type scriptGateCarrier struct {
	rel     string
	node    string
	command string
	vars    map[string]string
}

// discoverScriptGateCarriers compiles every bundle and returns each tool node
// whose command runs verify.sh, with the bot's OWN var defaults — substituting a
// placeholder with the empty string instead of what ships is how a gate gets
// exercised in a state production never reaches.
func discoverScriptGateCarriers(t *testing.T) []scriptGateCarrier {
	t.Helper()
	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	var found []scriptGateCarrier
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}
		defaults := map[string]string{}
		for name, v := range cr.Workflow.Vars {
			if v != nil && v.HasDefault {
				defaults[name] = fmt.Sprintf("%v", v.Default)
			}
		}
		for node, raw := range cr.Workflow.Nodes {
			tn, ok := raw.(*ir.ToolNode)
			if !ok || !strings.Contains(tn.Command, executesTheScript) {
				continue
			}
			found = append(found, scriptGateCarrier{rel: rel, node: node, command: tn.Command, vars: defaults})
		}
	}
	// Node maps iterate in random order; the subtest order must not be a coin flip.
	sort.Slice(found, func(i, j int) bool {
		if found[i].rel != found[j].rel {
			return found[i].rel < found[j].rel
		}
		return found[i].node < found[j].node
	})
	// A floor, because the failure mode of a discriminator is to match nothing and
	// leave a vacuous guard looking green.
	if len(found) < 11 {
		t.Fatalf("discovered %d verify.sh gate carriers, want >= 11 — the discriminator %q is stale "+
			"and this guard is near-vacuous", len(found), executesTheScript)
	}
	return found
}

type verifyGateResult struct {
	Passed   bool   `json:"passed"`
	Skipped  bool   `json:"skipped"`
	ExitCode int    `json:"exit_code"`
	LogTail  string `json:"log_tail"`
}

// runGate executes a carrier's REAL compiled command through `sh -c`, with the
// workspace and scratch pointed at the fixture and every other placeholder filled
// from the bot's own defaults.
func runGate(t *testing.T, c scriptGateCarrier, ws, scratch string) verifyGateResult {
	t.Helper()
	cmd := c.command
	// The fixture's paths bind FIRST. workspace_dir and scratch_dir carry engine
	// defaults (${PROJECT_DIR}, ${PROJECT_SCRATCH_DIR}); letting the generic loop
	// reach them substitutes a template the shell then expands to nothing, and the
	// gate refuses because it is looking in the wrong place — green for a reason
	// that has nothing to do with what is being tested.
	cmd = strings.ReplaceAll(cmd, "{{vars.workspace_dir}}", ws)
	cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
	for name, val := range c.vars {
		if name == "workspace_dir" || name == "scratch_dir" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, "{{vars."+name+"}}", val)
	}
	if rest := strings.Index(cmd, "{{vars."); rest >= 0 {
		t.Fatalf("a {{vars.…}} placeholder survived substitution near %q — the gate would run in a "+
			"state production never reaches", cmd[rest:min(rest+60, len(cmd))])
	}
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("verify_run command failed to execute: %v (out %q, stderr %q)", err, out, stderr)
	}
	var res verifyGateResult
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("verify_run output is not verify_result JSON: %v (out %q)", uerr, out)
	}
	return res
}

// assertRefused is the contract every carrier owes on a missing script. exit_code
// is asserted non-zero rather than pinned: dep-update-guard answers 3 because its
// refusal rides a precheck lane with its own consumer, the other ten answer 6, and
// what must never happen is a refusal reporting success.
func assertRefused(t *testing.T, res verifyGateResult) {
	t.Helper()
	if res.Passed {
		t.Fatalf("no verify.sh was produced and the gate reported a PASS — the continuation "+
			"decision reads 'the build passed' about a build that never ran, and an unattended "+
			"merge would rest on it. Result: %+v", res)
	}
	if !res.Skipped {
		t.Errorf("the refusal must say WHICH refusal it is: skipped=true is what distinguishes "+
			"'no script was written' from 'the build went red'. Result: %+v", res)
	}
	if res.ExitCode == 0 {
		t.Errorf("a refusal must not carry a success exit code. Result: %+v", res)
	}
	if !strings.Contains(res.LogTail, "NO VERIFY SCRIPT") {
		t.Errorf("log_tail must name the defect for the operator and the agent — a required gate "+
			"that is refused and unexplained blocks without telling anyone why. Got %q", res.LogTail)
	}
}

func cleanRepo(t *testing.T) (ws, scratch string) {
	t.Helper()
	ws, scratch = t.TempDir(), t.TempDir()
	gittest.Run(t, ws, "init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "add", "README.md")
	gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
	if _, err := os.Stat(filepath.Join(scratch, "verify.sh")); !os.IsNotExist(err) {
		t.Fatalf("scratch dir must start without a verify.sh, stat said %v", err)
	}
	return ws, scratch
}

// TestVerifyRunRefusesMissingScript stands each DISCOVERED carrier's real
// verify_run command up against a clean git repo and an empty scratch dir, and
// asserts the gate refuses instead of reporting a pass.
func TestVerifyRunRefusesMissingScript(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	for _, c := range discoverScriptGateCarriers(t) {
		c := c
		t.Run(c.rel+"/"+c.node, func(t *testing.T) {
			ws, scratch := cleanRepo(t)
			assertRefused(t, runGate(t, c, ws, scratch))
		})
	}
}

// TestVerifyRunRefusalSurvivesALoudWorkspace pins the half a gentle fixture cannot
// see. Two carriers do NOT exit early on a missing script: e2e-coverage and
// test-coverage set the verdict and fall through into their matrix / new-test
// sections, which APPEND to the same buffer that becomes log_tail. The refusal is
// written at the FRONT of that buffer, so a tail-only truncation drops it — and
// log_tail is exactly what e2e-coverage's fail_log hands the next pass.
//
// Measured before the fix: a matrix with 40 invalid rows carrying ordinary human
// ids pushed the whole refusal out of a 4000-char tail, leaving the agent reading
// forty matrix complaints and never learning the gate wanted a verify.sh.
//
// With t.TempDir() alone the assertion above is vacuous on this property: the
// problem list is one line long. This fixture is what makes it able to redden.
func TestVerifyRunRefusalSurvivesALoudWorkspace(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	var rows strings.Builder
	rows.WriteString("# e2e-coverage-matrix\n\n")
	rows.WriteString("| ID | Feature | Family | Status | Tests | Notes |\n")
	rows.WriteString("|----|---------|--------|--------|-------|-------|\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&rows, "| FEAT-%03d-the-long-human-readable-identifier-an-operator-actually-writes-"+
			"for-a-real-product-feature-row-number-%03d | thing %d | api | unknown-status-%03d | | |\n",
			i, i, i, i)
	}

	// Run on EVERY carrier rather than on the ones that happen to declare a
	// matrix_path: selecting by a var name would be selecting by spelling, and the
	// next carrier to grow an appender would be excluded in silence. The honest
	// limit, stated rather than hidden: the loud fixture only reaches e2e-coverage's
	// appenders today, so it is that carrier's subtest that can redden. For the
	// others the assertion is cheap and becomes real the day they compose.
	for _, c := range discoverScriptGateCarriers(t) {
		c := c
		t.Run(c.rel+"/"+c.node, func(t *testing.T) {
			ws, scratch := cleanRepo(t)
			if matrixRel, ok := c.vars["matrix_path"]; ok && matrixRel != "" {
				dst := filepath.Join(ws, matrixRel)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dst, []byte(rows.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, ws, "add", "-A")
				gittest.Run(t, ws, "commit", "-q", "-m", "matrix")
			}

			res := runGate(t, c, ws, scratch)
			if !strings.Contains(res.LogTail, "NO VERIFY SCRIPT") {
				t.Fatalf("the refusal was truncated out of log_tail by the sections that append "+
					"after it — the next pass is told about the matrix and never learns the gate "+
					"never ran. len(log_tail)=%d, starts %q", len(res.LogTail), res.LogTail[:min(160, len(res.LogTail))])
			}
			assertRefused(t, res)
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
