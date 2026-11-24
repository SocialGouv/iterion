package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// suitesPlan is a one-lot contract: extraTop and suites (top-level YAML,
// either possibly empty) ahead of the net and the lot.
func suitesPlan(extraTop, suites, gate, status string) string {
	return "version: 1\n" + extraTop + suites + `oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: ` + status + `
    exit_gate:
      - ` + strconv.Quote(gate) + "\n"
}

// suitesWorkspace commits the contract, then the net, and returns the run's
// base: the commit after both.
func suitesWorkspace(t *testing.T, plan string) (string, string, func(args ...string) string) {
	t.Helper()
	ws, _, git := modernizeRepo(t, plan)
	modernizeNet(t, ws)
	git("add", ".golden-master")
	git("commit", "-qm", "net")
	return ws, git("rev-parse", "HEAD"), git
}

// forgedActHarness answers the extension certifier as a net whose ledger holds
// an act the lot performed itself.
const forgedActHarness = `import json, os
mode = os.environ.get("GM_MODE", "gate")
if mode == "extend-verify":
    print(json.dumps({"acted": [{"id": "E1", "ok": False, "forged": True, "acted_at_base": False, "problems": ["acted by the lot itself"]}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": [], "provenance": "strict"}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestModernizeLotVerifyRunsTheDeclaredSuites pins the repository's own test
// suites, declared ONCE at the top of the contract and run at every lot's gate,
// in lot_gate's conjunction. Measured: a lot moved a route from GET to POST,
// its exit_gate ran no test, it converged — and the trunk's integration suite
// went red on a test still sending the GET.
func TestModernizeLotVerifyRunsTheDeclaredSuites(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	// The suite appends to a marker outside the workspace, and fails while a
	// file next to it says so.
	ctl := t.TempDir()
	suite := fmt.Sprintf(`"sh -c 'echo ran >> %[1]s/suite.marker; test ! -e %[1]s/red'"`, ctl)
	plan := `version: 1
suites:
  - ` + suite + `
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo
    exit_gate:
      - "true"
`
	netAndBase := func(t *testing.T, planYAML string) (string, string, func(args ...string) string) {
		t.Helper()
		// Each subtest starts with no marker: one a previous subtest's suite
		// left would read as this one's.
		_ = os.Remove(filepath.Join(ctl, "suite.marker"))
		ws, _, git := modernizeRepo(t, planYAML)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		return ws, git("rev-parse", "HEAD"), git
	}
	ran := func(t *testing.T) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(ctl, "suite.marker"))
		defer os.Remove(filepath.Join(ctl, "suite.marker"))
		return err == nil
	}
	setRed := func(t *testing.T, red bool) {
		t.Helper()
		if red {
			if err := os.WriteFile(filepath.Join(ctl, "red"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
		_ = os.Remove(filepath.Join(ctl, "red"))
	}

	t.Run("a red suite refuses the lot", func(t *testing.T) {
		setRed(t, true)
		defer setRed(t, false)
		ws, base, _ := netAndBase(t, plan)
		res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if exit != 0 || !ran(t) {
			t.Fatalf("exit %d: the suite did not run", exit)
		}
		if res.SuitesPassed || !res.GatePassed || !strings.Contains(res.LogTail, "suites command failed") {
			t.Fatalf("a red suite must read suites_passed=false with the exit gate green, naming the suite: %+v", res)
		}
		if lotGateHolds(t, "converged", raw) || lotGateHolds(t, "stop", raw) {
			t.Fatal("lot_gate converged over a red suite")
		}
	})
	t.Run("a green suite lets the lot converge", func(t *testing.T) {
		ws, base, _ := netAndBase(t, plan)
		res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if exit != 0 || !ran(t) || !res.SuitesPassed {
			t.Fatalf("exit %d: a green suite must read suites_passed=true: %+v", exit, res)
		}
		if !lotGateHolds(t, "converged", raw) {
			t.Fatalf("lot_gate refused a lot whose every term holds: %+v", res)
		}
	})
	t.Run("no suites declared: the gate as it was", func(t *testing.T) {
		bare := strings.Replace(plan, "suites:\n  - "+suite+"\n", "", 1)
		ws, base, _ := netAndBase(t, bare)
		res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if exit != 0 || ran(t) || !res.SuitesPassed || !lotGateHolds(t, "converged", raw) {
			t.Fatalf("exit %d: a contract with no suites must converge as before: %+v", exit, res)
		}
	})
	t.Run("one command as a string is read as one suite", func(t *testing.T) {
		single := strings.Replace(plan, "suites:\n  - "+suite+"\n", "suites: "+suite+"\n", 1)
		ws, base, _ := netAndBase(t, single)
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if !ran(t) || !res.SuitesPassed {
			t.Fatalf("a suite declared as a string did not run as one command: %+v", res)
		}
	})
	t.Run("a suite past the wall is the GATE_TIMEOUT verdict", func(t *testing.T) {
		slow := strings.Replace(plan, "version: 1\n", "version: 1\ngate_timeout_s: 1\n", 1)
		slow = strings.Replace(slow, suite, `"sleep 3"`, 1)
		ws, base, _ := netAndBase(t, slow)
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if !res.GateTimedOut || res.SuitesPassed || !strings.HasPrefix(res.BlockReason, "GATE_TIMEOUT: suites command") {
			t.Fatalf("a suite that outlived the wall must be the GATE_TIMEOUT verdict: %+v", res)
		}
	})
	t.Run("no suite runs after the exit gate ran into the wall", func(t *testing.T) {
		walled := strings.Replace(plan, "version: 1\n", "version: 1\ngate_timeout_s: 1\n", 1)
		ws, base, _ := netAndBase(t, walled)
		res := modernizeLotVerify(t, script, ws, "L1", base, "sleep 3")
		if !res.GateTimedOut || res.SuitesPassed || ran(t) {
			t.Fatalf("a suite ran after the exit gate hit the wall, or read as passed: %+v", res)
		}
	})
	t.Run("no suite runs after a red exit gate", func(t *testing.T) {
		ws, base, _ := netAndBase(t, plan)
		res := modernizeLotVerify(t, script, ws, "L1", base, "false")
		if res.GatePassed || res.SuitesPassed || res.GateTimedOut || ran(t) {
			t.Fatalf("a suite ran after a red exit gate, or read as passed: %+v", res)
		}
		if !strings.Contains(res.LogTail, "exit gate is red") {
			t.Fatalf("the log must say the suites wait for the gate: %s", res.LogTail)
		}
	})
	t.Run("a red exit gate and a hanging suite go back to the worker, not to the wall", func(t *testing.T) {
		slow := strings.Replace(plan, "version: 1\n", "version: 1\ngate_timeout_s: 1\n", 1)
		slow = strings.Replace(slow, suite, `"sleep 3"`, 1)
		ws, base, _ := netAndBase(t, slow)
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "false", nil)
		if res.GateTimedOut || lotGateHolds(t, "stop", raw) {
			t.Fatalf("a repairable red gate ended on the wall a suite ran into: %+v", res)
		}
	})
	t.Run("a lot declared blocked over red suites says they are red", func(t *testing.T) {
		setRed(t, true)
		defer setRed(t, false)
		ws, base, git := netAndBase(t, plan)
		blocked := strings.Replace(plan, "    status: todo\n", "    status: blocked\n", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(blocked), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "blocked")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if !res.LotBlocked || !lotGateHolds(t, "stop", raw) || !strings.Contains(res.BlockReason, "suites are red on this tree") {
			t.Fatalf("a blocked lot over red suites must stop saying so: %+v", res)
		}
	})
	t.Run("suites declared twice are unreadable", func(t *testing.T) {
		twice := strings.Replace(plan, "suites:\n  - "+suite+"\n", "suites:\n  - "+suite+"\nsuites: \"true\"\n", 1)
		ws, base, _ := netAndBase(t, twice)
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, "true", nil)
		if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "more than once") || ran(t) {
			t.Fatalf("exit %d: a key declared twice must be CONTRACT_UNREADABLE before any command: %+v", exit, res)
		}
	})
	t.Run("suites the worker moved are a contract rewrite", func(t *testing.T) {
		ws, base, git := netAndBase(t, plan)
		moved := strings.Replace(plan, suite, `"true"`, 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "make the suites green")
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if !strings.Contains(strings.Join(res.ContractRewrite, "\n"), "top-level suites") {
			t.Fatalf("suites the worker rewrote must read as a contract rewrite: %+v", res)
		}
	})
	for name, bad := range map[string]string{
		"a mapping":            "suites:\n  unit: \"true\"\n",
		"a number in a list":   "suites:\n  - 3\n",
		"a multi-line command": "suites:\n  - |\n    true\n    true\n",
		"an empty list":        "suites: []\n",
		"an empty command":     "suites: \"  \"\n",
		"a NUL byte":           "suites:\n  - \"true\\0\"\n",
	} {
		t.Run("unreadable suites ("+name+") are refused before any command", func(t *testing.T) {
			badPlan := strings.Replace(plan, "suites:\n  - "+suite+"\n", bad, 1)
			ws, base, _ := netAndBase(t, badPlan)
			res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, "sh -c 'echo ran > gate.marker'", nil)
			if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "CONTRACT_UNREADABLE") || !strings.Contains(res.LogTail, "suites") {
				t.Fatalf("exit %d contract_unreadable=%v, log %q — want the typed refusal naming suites", exit, res.Unreadable, res.LogTail)
			}
			if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err == nil {
				t.Fatal("the gate ran under suites the node could not read")
			}
		})
	}
}

// TestModernizeLotVerifySuitesJudgeOnlyWhatLands: `done` lands HEAD, so the
// suites never judge a working tree that differs from what a landing carries.
// Every such tree is refused before any gate command — the gate and the
// oracle would spend their hours on a lot its suites cannot judge — and the
// refusal names the path. What git ignores is not the lot's work.
func TestModernizeLotVerifySuitesJudgeOnlyWhatLands(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	ctl := t.TempDir()
	suiteMarker, gateMarker := filepath.Join(ctl, "suite.marker"), filepath.Join(ctl, "gate.marker")
	plan := suitesPlan("", "suites:\n  - "+strconv.Quote("sh -c 'echo ran >> "+suiteMarker+"'")+"\n", "true", "todo")
	gate := "sh -c 'echo ran >> " + gateMarker + "'"
	write := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		writeContract(t, ws, rel, body)
	}
	for name, tc := range map[string]struct {
		leave func(t *testing.T, ws string, git func(args ...string) string)
		names string // the path the refusal names; empty: the lot is judged
		says  string // the way out it names; empty: the one for paths
	}{
		"a tracked change left uncommitted": {func(t *testing.T, ws string, _ func(...string) string) {
			write(t, ws, "src/app.txt", "left\n")
		}, "src/app.txt", ""},
		"a staged change": {func(t *testing.T, ws string, git func(...string) string) {
			write(t, ws, "src/app.txt", "staged\n")
			git("add", "src/app.txt")
		}, "src/app.txt", ""},
		"a deletion": {func(t *testing.T, ws string, _ func(...string) string) {
			if err := os.Remove(filepath.Join(ws, "src/app.txt")); err != nil {
				t.Fatal(err)
			}
		}, "src/app.txt", ""},
		"a mode change": {func(t *testing.T, ws string, _ func(...string) string) {
			if err := os.Chmod(filepath.Join(ws, "src/run.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "src/run.sh", ""},
		"a retargeted symlink": {func(t *testing.T, ws string, _ func(...string) string) {
			link := filepath.Join(ws, "src/link")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("run.sh", link); err != nil {
				t.Fatal(err)
			}
		}, "src/link", ""},
		"a file swapped for a directory": {func(t *testing.T, ws string, _ func(...string) string) {
			if err := os.Remove(filepath.Join(ws, "src/app.txt")); err != nil {
				t.Fatal(err)
			}
			write(t, ws, "src/app.txt/inner", "x\n")
		}, "src/app.txt", ""},
		"an untracked file": {func(t *testing.T, ws string, _ func(...string) string) {
			write(t, ws, "src/new.txt", "left\n")
		}, "src/new.txt", ""},
		"a test run's report git does not ignore": {func(t *testing.T, ws string, _ func(...string) string) {
			write(t, ws, "junit.xml", "<testsuite/>\n")
		}, "junit.xml", ""},
		"a change behind assume-unchanged": {func(t *testing.T, ws string, git func(...string) string) {
			git("update-index", "--assume-unchanged", "src/app.txt")
			write(t, ws, "src/app.txt", "hidden\n")
		}, "src/app.txt", ""},
		"a change behind skip-worktree": {func(t *testing.T, ws string, git func(...string) string) {
			git("update-index", "--skip-worktree", "src/app.txt")
			write(t, ws, "src/app.txt", "hidden\n")
		}, "src/app.txt", ""},
		"a deletion behind assume-unchanged": {func(t *testing.T, ws string, git func(...string) string) {
			git("update-index", "--assume-unchanged", "src/app.txt")
			if err := os.Remove(filepath.Join(ws, "src/app.txt")); err != nil {
				t.Fatal(err)
			}
		}, "src/app.txt", ""},
		"a deletion behind skip-worktree": {func(t *testing.T, ws string, git func(...string) string) {
			git("update-index", "--skip-worktree", "src/app.txt")
			if err := os.Remove(filepath.Join(ws, "src/app.txt")); err != nil {
				t.Fatal(err)
			}
		}, "src/app.txt", ""},
		"a submodule with work of its own": {func(t *testing.T, ws string, git func(...string) string) {
			sub := t.TempDir()
			gittest.Run(t, sub, "init", "-q", "-b", "main")
			write(t, sub, "lib.txt", "v1\n")
			gittest.Run(t, sub, "add", "lib.txt")
			gittest.Run(t, sub, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "lib")
			git("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
			git("commit", "-qm", "the lot's work: a submodule")
			write(t, ws, "sub/lib.txt", "left\n")
		}, "sub", "git-ignore it in the submodule's own .gitignore"},
		"a tree git cannot stage": {func(t *testing.T, ws string, _ func(...string) string) {
			gittest.Run(t, ws, "init", "-q", "scratch-repo")
		}, "scratch-repo", "remove it, or git-ignore it, in a commit"},
		"an ignored build output": {func(t *testing.T, ws string, _ func(...string) string) {
			write(t, ws, "build/out.txt", "left\n")
		}, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			for _, m := range []string{suiteMarker, gateMarker} {
				_ = os.Remove(m)
			}
			ws, base, git := suitesWorkspace(t, plan)
			write(t, ws, "src/app.txt", "v1\n")
			write(t, ws, "src/run.sh", "#!/bin/sh\n")
			write(t, ws, ".gitignore", "build/\n")
			if err := os.Symlink("app.txt", filepath.Join(ws, "src/link")); err != nil {
				t.Fatal(err)
			}
			git("add", "src", ".gitignore")
			git("commit", "-qm", "the lot's work")
			tc.leave(t, ws, git)
			res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, gate, nil)
			if exit != 0 {
				t.Fatalf("lot_verify exited %d: %+v", exit, res)
			}
			judged := tc.names == ""
			if pathExists(gateMarker) != judged || pathExists(suiteMarker) != judged || res.SuitesPassed != judged {
				t.Fatalf("gate ran=%v, suites ran=%v passed=%v, want %v: %s", pathExists(gateMarker),
					pathExists(suiteMarker), res.SuitesPassed, judged, res.LogTail)
			}
			if judged {
				if !lotGateHolds(t, "converged", raw) {
					t.Fatalf("a lot whose work is all committed did not converge: %+v", res)
				}
				return
			}
			if lotGateHolds(t, "converged", raw) || lotGateHolds(t, "stop", raw) {
				t.Fatal("lot_gate converged over work no landing carries")
			}
			says := tc.says
			if says == "" {
				says = "are not committed"
			}
			if res.Unreadable || !strings.Contains(res.LogTail, tc.names) || !strings.Contains(res.LogTail, says) {
				t.Fatalf("the refusal must go back to the worker, name %s and say %q: %+v", tc.names, says, res)
			}
		})
	}
}

// TestModernizeLotVerifySuitesLeaveTheEnginesScaffoldAlone: what the engine
// lays in the workspace at run start — the `.claude/` mirror, a tool node's
// script scratch — is not the lot's uncommitted work. The engine names it in
// $ITERION_TREE_NOISE, which every tool node receives.
func TestModernizeLotVerifySuitesLeaveTheEnginesScaffoldAlone(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	marker := filepath.Join(t.TempDir(), "suite.marker")
	ws, base, _ := suitesWorkspace(t, suitesPlan("", "suites:\n  - "+strconv.Quote("touch "+marker)+"\n", "true", "todo"))
	writeContract(t, ws, ".claude/skills/bot.md", "the engine's mirror\n")
	writeContract(t, ws, ".iterion-script-0a1b2c.py", "print()\n")
	env := append(modernizeChildEnv(t), treenoise.TreeNoiseEnvVar+"="+treenoise.EnvValue())
	res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", env)
	if exit != 0 || !pathExists(marker) || !res.SuitesPassed || !lotGateHolds(t, "converged", raw) {
		t.Fatalf("exit %d, suites ran=%v: the engine's scaffold read as the lot's work: %+v", exit, pathExists(marker), res)
	}
}

// TestModernizeLotVerifySuitesAfterTheGate pins what happens between the lot's
// own gate and its suites, and what a lot the worker declared blocked is told.
func TestModernizeLotVerifySuitesAfterTheGate(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	commitBlocked := func(t *testing.T, ws, plan string, git func(args ...string) string) {
		t.Helper()
		writeContract(t, ws, ".modernize/plan.yaml", strings.Replace(plan, "status: todo", "status: blocked", 1))
		git("commit", "-qam", "blocked")
	}

	t.Run("a gate that rewrites a tracked path leaves the suites unrun", func(t *testing.T) {
		// A formatter, a lockfile rewritten by an install, generated code: the
		// suites would judge a tree no landing carries.
		gate := "sed -i s/broken/fixed/ src/app.txt"
		ws, base, git := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"grep -q fixed src/app.txt\"\n", gate, "todo"))
		writeContract(t, ws, "src/app.txt", "broken\n")
		git("add", "src/app.txt")
		git("commit", "-qm", "the lot's work, broken")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, gate, nil)
		if !res.GatePassed || res.SuitesPassed || lotGateHolds(t, "converged", raw) {
			t.Fatalf("the suites judged a tree the gate rewrote: %+v", res)
		}
		if !strings.Contains(res.LogTail, "left a tree no landing carries") || !strings.Contains(res.LogTail, "src/app.txt") {
			t.Fatalf("the log must name the path the gate rewrote: %s", res.LogTail)
		}
	})
	t.Run("a gate that writes a file git does not ignore leaves the suites unrun", func(t *testing.T) {
		// Generated code the suites read and no landing carries.
		gate := "sh -c 'mkdir -p src && echo generated > src/generated.txt'"
		ws, base, _ := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"test -f src/generated.txt\"\n", gate, "todo"))
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, gate, nil)
		if !res.GatePassed || res.SuitesPassed || lotGateHolds(t, "converged", raw) ||
			!strings.Contains(res.LogTail, "src/generated.txt") {
			t.Fatalf("the suites judged a file the gate wrote and HEAD does not carry: %+v", res)
		}
	})
	for _, configured := range []bool{false, true} {
		t.Run("a deletion behind skip-worktree, core.sparseCheckout="+strconv.FormatBool(configured)+", is still refused", func(t *testing.T) {
			// The exemption must not key on a value the worker writes: with
			// the config set, a deleted test disappears from every git view.
			// The suite globs, as a real suite does: a file the glob no
			// longer sees must not read as a suite that passed.
			ws, base, git := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"sh -c 'for t in tests/*.sh; do sh \\\"$t\\\" || exit 1; done'\"\n", "true", "todo"))
			writeContract(t, ws, "tests/red.sh", "exit 1\n")
			git("add", "tests")
			git("commit", "-qm", "the lot's work, which breaks a test")
			git("update-index", "--skip-worktree", "tests/red.sh")
			if configured {
				git("config", "core.sparseCheckout", "true")
			}
			if err := os.Remove(filepath.Join(ws, "tests/red.sh")); err != nil {
				t.Fatal(err)
			}
			res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
			if lotGateHolds(t, "converged", raw) || !strings.Contains(res.LogTail, "tests/red.sh") {
				t.Fatalf("a deleted test behind a flag read as judged: %+v", res)
			}
		})
	}
	t.Run("what the oracle leaves is measured on the way out", func(t *testing.T) {
		// The net's application rewrites a tracked file after the suites ran:
		// a green suites verdict over a tree that changed since judges a
		// landing that does not exist.
		ws, base, git := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"true\"\n", "true", "todo"))
		writeContract(t, ws, "src/app.txt", "v1\n")
		git("add", "src")
		git("commit", "-qm", "the lot's work")
		writeContract(t, ws, ".golden-master/verify-oracle.sh",
			"#!/bin/sh\necho rewritten > src/app.txt\nprintf '{\"mode\":\"gate\"}\\n'\nexit 0\n")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if lotGateHolds(t, "converged", raw) || res.SuitesPassed ||
			!strings.Contains(res.LogTail, "changed after they ran") {
			t.Fatalf("a green suites verdict over a tree the oracle rewrote: %+v", res)
		}
	})
	t.Run("the bank's scratch index is not the lot's work, even with TMPDIR in the workspace", func(t *testing.T) {
		ws, base, _ := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"true\"\n", "true", "todo"))
		if err := os.MkdirAll(filepath.Join(ws, ".tmp"), 0o755); err != nil {
			t.Fatal(err)
		}
		env := append(modernizeChildEnv(t), "TMPDIR="+filepath.Join(ws, ".tmp"))
		res, _, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", env)
		// The oracle's own guard refuses a report tempfile inside the
		// workspace — an environment failure of the run, said as such. What
		// must never come back is the suites' refusal over the bank's own
		// scratch index.
		if exit != 0 || strings.Contains(res.LogTail, "modernize-bank") ||
			strings.Contains(res.LogTail, "cannot judge this tree") {
			t.Fatalf("the suites refused a clean lot over the node's own scratch: %+v", res)
		}
	})
	t.Run("a tree git cannot stage at the record check goes back to the worker", func(t *testing.T) {
		ws, base, _ := suitesWorkspace(t, suitesPlan("", "", "git init -q scratch-repo", "todo"))
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "git init -q scratch-repo", nil)
		if res.Unreadable || lotGateHolds(t, "unreadable", raw) || lotGateHolds(t, "converged", raw) ||
			!strings.Contains(res.LogTail, "could not be re-judged") {
			t.Fatalf("a staging failure at the record check took the wrong door: %+v", res)
		}
	})
	t.Run("a blocked lot whose suite runs into the wall stops as declared", func(t *testing.T) {
		plan := suitesPlan("gate_timeout_s: 1\n", "suites:\n  - \"sleep 3\"\n", "true", "todo")
		ws, base, git := suitesWorkspace(t, plan)
		commitBlocked(t, ws, plan, git)
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if res.GateTimedOut || lotGateHolds(t, "timed_out", raw) || !lotGateHolds(t, "stop", raw) {
			t.Fatalf("a lot the worker declared blocked must stop as such, not on a wall: %+v", res)
		}
		if !strings.Contains(res.BlockReason, "suites were not run to the end") {
			t.Fatalf("the stop must say the suites did not finish: %q", res.BlockReason)
		}
	})
	t.Run("a blocked lot whose gate is red is not told its suites are red", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		plan := suitesPlan("", "suites:\n  - "+strconv.Quote("touch "+marker)+"\n", "false", "todo")
		ws, base, git := suitesWorkspace(t, plan)
		commitBlocked(t, ws, plan, git)
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "false", nil)
		if pathExists(marker) || !lotGateHolds(t, "stop", raw) {
			t.Fatalf("suites ran=%v stop=%v: %+v", pathExists(marker), lotGateHolds(t, "stop", raw), res)
		}
		if strings.Contains(res.BlockReason, "suites are red") || !strings.Contains(res.BlockReason, "suites were not run: its own gate is red") {
			t.Fatalf("the stop says what the suites never said: %q", res.BlockReason)
		}
	})
	t.Run("a suite whose output is not UTF-8 is read, not raised on", func(t *testing.T) {
		// A legacy application's test log in Latin-1: one byte the strict
		// decoder raises on, a tool error the engine replays.
		for _, tc := range []struct {
			suite string
			green bool
		}{
			{`printf 'R\\351sultat OK\\n'`, true},
			{`printf 'R\\351sultat KO\\n'; exit 1`, false},
		} {
			ws, base, _ := suitesWorkspace(t, suitesPlan("", "suites:\n  - \""+tc.suite+"\"\n", "true", "todo"))
			res, raw, exit := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
			if exit != 0 || res.SuitesPassed != tc.green || lotGateHolds(t, "converged", raw) != tc.green {
				t.Fatalf("exit %d, suites_passed=%v, want %v: %+v", exit, res.SuitesPassed, tc.green, res)
			}
			if !tc.green && !strings.Contains(res.LogTail, "R\ufffdsultat KO") {
				t.Fatalf("the failure's output must reach the worker, the byte replaced: %s", res.LogTail)
			}
		}
	})
	t.Run("a tree the suites cannot judge still has its extension certificate read", func(t *testing.T) {
		// The refusal skips the gate, the suites and the oracle — never the
		// references and the acts certified against them: a lot the worker
		// declared blocked would otherwise carry a forged act out through
		// its stop.
		plan := suitesPlan("", "suites:\n  - \"true\"\n", "true", "todo")
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		writeContract(t, ws, ".golden-master/harness.py", forgedActHarness)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		commitBlocked(t, ws, plan, git)
		writeContract(t, ws, "notes.txt", "scratch\n")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if !res.ExtensionForged || !lotGateHolds(t, "forged", raw) || lotGateHolds(t, "stop", raw) {
			t.Fatalf("a forged act left the lot through its stop: %+v", res)
		}
	})
	for _, config := range [][2]string{{"core.trustctime", "false"}, {"core.checkStat", "minimal"}} {
		t.Run("an edit hidden by "+config[0]+"="+config[1]+" is still seen", func(t *testing.T) {
			// The repository's configuration is the worker's to write: an
			// edit that keeps the size and puts the mtime back reads as none.
			ws, base, git := suitesWorkspace(t, suitesPlan("", "suites:\n  - \"sh tests/t.sh\"\n", "true", "todo"))
			edited := filepath.Join(ws, "tests/t.sh")
			writeContract(t, ws, "tests/t.sh", "exit 1\n")
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(edited, old, old); err != nil {
				t.Fatal(err)
			}
			git("add", "tests")
			git("commit", "-qm", "the lot's work, which breaks the suite")
			git("config", config[0], config[1])
			time.Sleep(1100 * time.Millisecond) // the edit comes a second after the index entry
			if err := os.WriteFile(edited, []byte("exit 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(edited, old, old); err != nil {
				t.Fatal(err)
			}
			res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
			if lotGateHolds(t, "converged", raw) || !strings.Contains(res.LogTail, "tests/t.sh") {
				t.Fatalf("the suites judged an edit git was told to trust its cache over: %+v", res)
			}
		})
	}
	t.Run("a gate past its wall is stopped with everything it started", func(t *testing.T) {
		late := filepath.Join(t.TempDir(), "late")
		gate := "sh -c '(sleep 2; touch " + late + ") & sleep 5'"
		ws, base, _ := suitesWorkspace(t, suitesPlan("gate_timeout_s: 1\n", "", gate, "todo"))
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.GateTimedOut {
			t.Fatalf("the gate did not run into its wall: %+v", res)
		}
		time.Sleep(3 * time.Second)
		if pathExists(late) {
			t.Fatal("a process the gate started outlived the gate's wall and wrote after the verdict")
		}
	})
	t.Run("suites repeated in the worker's tree are a rewrite, whatever the base repeats", func(t *testing.T) {
		plan := suitesPlan("notes: \"first\"\n", "suites:\n  - \"true\"\n", "true", "todo") + "notes: \"second\"\n"
		ws, base, git := suitesWorkspace(t, plan)
		writeContract(t, ws, ".modernize/plan.yaml", plan+"suites:\n  - \"true\"\n")
		git("commit", "-qam", "the worker's tree repeats suites")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if lotGateHolds(t, "converged", raw) || !strings.Contains(strings.Join(res.ContractRewrite, "\n"), "suites declared more than once") {
			t.Fatalf("a contract the next plan_read refuses converged: %+v", res)
		}
	})
	t.Run("a key other than suites declared twice at the base refuses nothing", func(t *testing.T) {
		plan := suitesPlan("notes: \"first\"\n", "", "true", "todo") + "notes: \"second\"\n"
		ws, base, _ := suitesWorkspace(t, plan)
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if res.Unreadable || !lotGateHolds(t, "converged", raw) {
			t.Fatalf("a contract the lot left as it was must converge: %+v", res)
		}
	})
	t.Run("a second lots block the worker writes is a rewrite, not an unreadable base", func(t *testing.T) {
		plan := suitesPlan("", "", "true", "todo")
		ws, base, git := suitesWorkspace(t, plan)
		writeContract(t, ws, ".modernize/plan.yaml", plan+"lots:\n  - id: L99\n    title: \"a proposal\"\n    status: todo\n    exit_gate:\n      - \"true\"\n")
		git("commit", "-qam", "propose L99")
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if res.Unreadable || lotGateHolds(t, "unreadable", raw) || lotGateHolds(t, "converged", raw) {
			t.Fatalf("a worker's rewrite must go back to the worker, not take the terminal door: %+v", res)
		}
	})
}

// TestModernizeLotVerifySeesAReferenceRewrittenBehindGitsCache: a reference
// rewritten on disk while git answers from its index — an entry flagged
// assume-unchanged or skip-worktree, a stat cache the repository's
// configuration tells git to trust — and the oracle reads the disk.
func TestModernizeLotVerifySeesAReferenceRewrittenBehindGitsCache(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	const ref = ".golden-master/refs/001.txt"
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			ws, base, git := suitesWorkspace(t, suitesPlan("", "", "true", "todo"))
			git("update-index", flag, ref)
			writeContract(t, ws, ref, "STATUS 500\n")
			res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
			if res.RefsUntouched || lotGateHolds(t, "converged", raw) {
				t.Fatalf("a rewritten reference behind %s read as untouched: %+v", flag, res)
			}
			if !strings.Contains(res.LogTail, ref) {
				t.Fatalf("the verdict must name the flagged reference: %s", res.LogTail)
			}
		})
	}
	t.Run("core.trustctime=false", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, suitesPlan("", "", "true", "todo"))
		modernizeNet(t, ws)
		old := time.Now().Add(-time.Hour).Truncate(time.Second)
		if err := os.Chtimes(filepath.Join(ws, ref), old, old); err != nil {
			t.Fatal(err)
		}
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		git("config", "core.trustctime", "false")
		time.Sleep(1100 * time.Millisecond)
		// "STATUS 200" to "STATUS 500": the same size, the mtime put back.
		writeContract(t, ws, ref, "STATUS 500\n")
		if err := os.Chtimes(filepath.Join(ws, ref), old, old); err != nil {
			t.Fatal(err)
		}
		res, raw, _ := modernizeLotVerifyRaw(t, script, ws, "L1", base, "true", nil)
		if res.RefsUntouched || lotGateHolds(t, "converged", raw) {
			t.Fatalf("a reference rewritten behind git's stat cache read as untouched: %+v", res)
		}
	})
}

// TestModernizeLotVerifySuitesLeaveTheNetToItsOwnChecks: the net's directory is
// the references' parent however refs_dir is spelled, and a file there is not
// the lot's uncommitted work.
func TestModernizeLotVerifySuitesLeaveTheNetToItsOwnChecks(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	plan := suitesPlan("", "suites:\n  - \"true\"\n", "true", "todo")
	for _, shape := range []string{".golden-master/refs", "./.golden-master/refs/", ".golden-master/refs/", ".golden-master//refs"} {
		t.Run(shape, func(t *testing.T) {
			ws, base, _ := suitesWorkspace(t, plan)
			writeContract(t, ws, ".golden-master/scratch.txt", "x\n")
			res, _, exit := modernizeLotVerifyRaw(t, strings.ReplaceAll(script, "{{input.refs_dir}}", strconv.Quote(shape)), ws, "L1", base, "true", nil)
			if exit != 0 || !res.SuitesPassed || strings.Contains(res.LogTail, "not committed") {
				t.Fatalf("refs_dir %q: the net's own file read as the lot's uncommitted work: %+v", shape, res)
			}
		})
	}
}
