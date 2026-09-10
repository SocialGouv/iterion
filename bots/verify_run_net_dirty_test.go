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

// The campaign contract's clean-tree clause tells the agent to end every
// pass on a committed tree so the deterministic gate reads HEAD. When it
// doesn't — a pod dying mid-pass, an oracle-gate retry inheriting a mutant
// left applied by an interrupted falsification pass, a pass stopped
// mid-unit — the tree still carries the previous attempt's uncommitted
// edits, and the gate silently judges THEM as the build's failure.
//
// Incident #799: a lot's verify node ran for 7 676 s, its exec stream cut,
// the engine retried the node on the SAME tree where a golden-master
// mutant had been applied and never reverted; the second attempt called
// that tree the build's, went red on a package the mutant deleted, and
// the run finished not converged with hours of budget left.
//
// #807 (merged) closed the harness half — mutants are now self-reverted
// at the next gate. This test is the ENGINE-SIDE half done in the bot:
// every campaign verify_run refuses a dirty tree with a typed verdict
// BEFORE running verify.sh, so the first red is "commit first" (which
// the campaign loops on) not "the build broke" (which the campaign spends
// a pass trying to fix a phantom).
//
// The class is not "every verify.sh gate carrier": dep-update-guard's
// verify_run gates align → verify_run → commit, so align intentionally
// leaves the tree dirty for verify_run to judge — the whole point of
// its validate-then-commit pattern. Adding this gate there would break
// the bot.
var netDirtyGateCarriers = []struct {
	rel  string
	node string
}{
	{"adr-cartograph/main.bot", "verify_run"},
	{"app-dev/main.bot", "verify_run"},
	{"branch-improve-loop/main.bot", "verify_run"},
	{"e2e-coverage/main.bot", "verify_run"},
	{"feature-dev/main.bot", "verify_run"},
	{"feature-gap-fill/main.bot", "verify_run"},
	{"instrument/main.bot", "verify_run"},
	{"secured-renovacy/main.bot", "p2_verify_run"},
	{"test-coverage/main.bot", "verify_run"},
	{"whole-improve-loop/main.bot", "verify_run"},
}

// TestVerifyRunRefusesDirtyTree stands each carrier's REAL verify_run
// command up against a real git repo whose tree carries an uncommitted
// edit, and asserts the gate refuses BEFORE running verify.sh — with a
// typed net_dirty verdict, a passed=false, and a log_tail that names
// the offending path and tells the agent what to do about it.
//
// The gate must mord: it refuses on the pre-existing dirty tree, not on
// output verify.sh happened to leave behind. To prove that, verify.sh is
// benign here — a script that would be trivially green on a clean tree.
// Today (pre-fix), that green passes and the drift tail's post-pre check
// stays quiet because no NEW paths appear; the run reads "build passed"
// while the tree it just judged is one nobody committed. The `net_dirty`
// verdict is what the fix installs, and what this test pins down.
func TestVerifyRunRefusesDirtyTree(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	type verifyResult struct {
		Passed   bool   `json:"passed"`
		Skipped  bool   `json:"skipped"`
		ExitCode int    `json:"exit_code"`
		NetDirty bool   `json:"net_dirty"`
		LogTail  string `json:"log_tail"`
	}

	// runVerifyRun executes the carrier's verify_run command against
	// (ws, scratch) via `sh -c`, the same shell iterion's tool nodes use.
	runVerifyRun := func(t *testing.T, cmd, ws, scratch string) verifyResult {
		t.Helper()
		expanded := strings.ReplaceAll(cmd, "{{vars.workspace_dir}}", ws)
		expanded = strings.ReplaceAll(expanded, "{{vars.scratch_dir}}", scratch)
		// Some carriers thread more vars (verify_timeout_s / matrix_path /
		// target); default any that remain so the shell can still run.
		expanded = strings.ReplaceAll(expanded, "{{vars.verify_timeout_s}}", "600")
		expanded = strings.ReplaceAll(expanded, "{{vars.matrix_path}}", "")
		expanded = strings.ReplaceAll(expanded, "{{vars.target}}", "")
		out, err := exec.Command("sh", "-c", expanded).Output()
		if err != nil {
			t.Fatalf("verify_run command failed to execute: %v (out %q)", err, out)
		}
		var res verifyResult
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("verify_run output is not verify_result JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	// setupDirtyRepo makes a real git repo with one committed baseline
	// file, then edits it in place so `git status --porcelain` reports
	// it dirty. The scratch dir carries a benign verify.sh whose exit
	// code alone would be a green pass — the test proves the gate refuses
	// on the tree state, not on what the script did.
	setupDirtyRepo := func(t *testing.T, dirty string) (ws, scratch string) {
		t.Helper()
		ws, scratch = t.TempDir(), t.TempDir()
		gittest.Run(t, ws, "init", "-q")
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, ws, "add", "README.md")
		gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
		if err := os.WriteFile(filepath.Join(ws, dirty), []byte("uncommitted mid-pass edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"),
			[]byte("#!/bin/sh\nset -e\necho ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return ws, scratch
	}

	for _, c := range netDirtyGateCarriers {
		c := c
		t.Run(c.rel+"/"+c.node, func(t *testing.T) {
			cmd := toolCommand(t, c.rel, c.node)

			t.Run("dirty_tree_is_refused_with_net_dirty_verdict", func(t *testing.T) {
				ws, scratch := setupDirtyRepo(t, "README.md")
				res := runVerifyRun(t, cmd, ws, scratch)
				if res.Passed {
					t.Fatalf("dirty tree passed the gate — it judged the previous pass's "+
						"uncommitted work as HEAD. Result: %+v", res)
				}
				if !res.NetDirty {
					t.Errorf("verify_result lacks the typed net_dirty verdict — the campaign "+
						"cannot tell 'commit first' from 'the build broke', so it spends a "+
						"pass fixing a phantom. Result: %+v", res)
				}
				if !strings.Contains(res.LogTail, "NET DIRTY") {
					t.Errorf("log_tail must name the defect for the operator and the agent, "+
						"got %q", res.LogTail)
				}
				if !strings.Contains(res.LogTail, "README.md") {
					t.Errorf("log_tail must name the offending path so the agent knows what "+
						"to commit or discard; got %q", res.LogTail)
				}
			})

			t.Run("dirty_untracked_file_is_refused", func(t *testing.T) {
				// A brand-new file the agent forgot to `git add`. `git status --porcelain`
				// reports it as `?? path`, so tree_state() sees it.
				ws, scratch := setupDirtyRepo(t, "half_done.py")
				// Undo the modification setupDirtyRepo made to README.md so the
				// ONLY dirt is the new untracked file — proves untracked paths
				// alone are enough to refuse.
				gittest.Run(t, ws, "checkout", "--", "README.md")
				res := runVerifyRun(t, cmd, ws, scratch)
				if res.Passed || !res.NetDirty {
					t.Fatalf("an untracked half-done file must refuse the gate, got %+v", res)
				}
				if !strings.Contains(res.LogTail, "half_done.py") {
					t.Errorf("log_tail must name the untracked path, got %q", res.LogTail)
				}
			})

			t.Run("clean_tree_still_passes", func(t *testing.T) {
				// Symmetry: the gate must not shift green→red on a clean tree.
				// A committed baseline plus a benign verify.sh must PASS.
				ws, scratch := t.TempDir(), t.TempDir()
				gittest.Run(t, ws, "init", "-q")
				if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, ws, "add", "README.md")
				gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
				if err := os.WriteFile(filepath.Join(scratch, "verify.sh"),
					[]byte("#!/bin/sh\nset -e\necho ok\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				res := runVerifyRun(t, cmd, ws, scratch)
				if !res.Passed {
					t.Fatalf("clean tree + green verify.sh must pass, got %+v", res)
				}
				if res.NetDirty {
					t.Errorf("clean tree must not carry net_dirty=true, got %+v", res)
				}
			})
		})
	}
}

// TestVerifyRunNetDirtyGatePresentInAllCarriers is the anti-regression
// grep: every campaign carrier ships the guard. Complements the functional
// test above (which proves the guard mords) with a marker check that
// catches a copy-paste omission a functional test only reveals as a
// mysterious silent pass.
//
// The class is enumerated in netDirtyGateCarriers rather than discovered.
// A discovered list would silently accept a new campaign bot that forgot
// the gate; an enumerated one fails loudly on the missing entry, which is
// the point of the class rule ("fix at the CLASS, not one site").
func TestVerifyRunNetDirtyGatePresentInAllCarriers(t *testing.T) {
	for _, c := range netDirtyGateCarriers {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			src, err := os.ReadFile(c.rel)
			if err != nil {
				t.Fatalf("read %s: %v", c.rel, err)
			}
			body := string(src)
			for _, marker := range []string{"NET DIRTY", "'net_dirty': True"} {
				if !strings.Contains(body, marker) {
					t.Errorf("%s lacks the net_dirty gate (missing %q) — a retried "+
						"attempt of an interrupted node will inherit the previous "+
						"attempt's dirty tree and be judged on it", c.rel, marker)
				}
			}
		})
	}
}
