package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// emitRunner runs golden-master's emit_runner tool node against ws and returns
// the path of the wrapper it materialised.
func emitRunner(t *testing.T, ws string) string {
	t.Helper()
	body := toolScript(t, "golden-master/main.bot", "emit_runner")
	for ref, val := range map[string]string{
		"{{vars.workspace_dir}}":     strconv.Quote(ws),
		"{{vars.oracle_dir}}":        strconv.Quote(".golden-master"),
		"{{vars.mutation_floor}}":    "90",
		"{{input.score_pct}}":        "100",
		"{{input.holdout_detected}}": "0",
		"{{input.holdout_total}}":    "0",
	} {
		body = strings.ReplaceAll(body, ref, val)
	}
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in emit_runner near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "emit_runner.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("python3", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("emit_runner failed: %v\n%s", err, out)
	}
	runner := filepath.Join(ws, ".golden-master", "verify-oracle.sh")
	if _, err := os.Stat(runner); err != nil {
		t.Fatalf("emit_runner wrote no wrapper: %v", err)
	}
	return runner
}

// TestGoldenMasterWrapperRemovesItsReportOnARedGate pins the wrapper's report
// lifecycle on the path that matters: a RED verdict. The wrapper runs under
// `set -e`; a bare verdict command exiting 1 ended the script before the
// report was removed, and a report left behind reads, at the next gate, as
// uncommitted work of whoever ran this one — a pre-#795 wrapper defaults it
// to a dotfile INSIDE the net.
func TestGoldenMasterWrapperRemovesItsReportOnARedGate(t *testing.T) {
	requireModernizeTools(t)
	ws := t.TempDir()
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "canon"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The net the wrapper insists on before judging: canon tests that pass,
	// a harness whose self-test passes, and a gate verdict — RED here.
	const harness = `import json, os, sys
mode = os.environ.get("GM_MODE", "gate")
if mode == "selftest":
    sys.exit(0)
red = os.environ.get("STUB_VERDICT", "red") == "red"
mode = "gate-subset" if os.environ.get("GM_MUTANTS") else "gate"
print(json.dumps({"mode": mode, "stable": not red, "noop_silent": True, "revert_clean": True,
                  "collateral": 0, "uncontrolled": [], "blind_lanes": ["lane-a"] if red else [],
                  "missing_archetypes": [], "runner_replayable": True, "holdout_reused": False,
                  "holdout_detected": 0, "holdout_total": 0}))
`
	for name, body := range map[string]string{
		"canon/test_rules.py": "print('ok')\n",
		"harness.py":          harness,
	} {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := emitRunner(t, ws)

	run := func(t *testing.T, verdict string, extraEnv ...string) (exit int, out string, reportLeft bool) {
		t.Helper()
		report := filepath.Join(t.TempDir(), "report.json")
		cmd := exec.Command("sh", runner)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_REPORT_TMP="+report, "STUB_VERDICT="+verdict, "GM_WORKSPACE="+ws)
		cmd.Env = append(cmd.Env, extraEnv...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("wrapper failed to execute: %v\n%s", err, b)
			}
			exit = ee.ExitCode()
		}
		_, statErr := os.Stat(report)
		return exit, string(b), statErr == nil
	}

	t.Run("a red gate exits 1 and removes its report", func(t *testing.T) {
		exit, out, left := run(t, "red")
		if exit != 1 || !strings.Contains(out, "GATE RED") {
			t.Fatalf("a red gate must exit 1 with the verdict: exit %d\n%s", exit, out)
		}
		if !strings.Contains(out, `"blind_lanes": ["lane-a"]`) {
			t.Fatalf("the report must be printed before the verdict:\n%s", out)
		}
		if left {
			t.Fatalf("the report survived a red gate — the next gate reads it as uncommitted work:\n%s", out)
		}
	})
	t.Run("a green gate exits 0 and removes its report", func(t *testing.T) {
		exit, out, left := run(t, "green")
		if exit != 0 {
			t.Fatalf("a green gate must exit 0: exit %d\n%s", exit, out)
		}
		if left {
			t.Fatalf("the report survived a green gate:\n%s", out)
		}
	})
	t.Run("an inherited VERDICT_RC or GATE_RC does not turn a green gate red", func(t *testing.T) {
		exit, out, _ := run(t, "green", "VERDICT_RC=7", "GATE_RC=7")
		if exit != 0 {
			t.Fatalf("an ambient exit-code variable decided the verdict: exit %d\n%s", exit, out)
		}
	})
	t.Run("a subset under a gate run is refused, never a silent exit 0", func(t *testing.T) {
		exit, out, left := run(t, "green", "GM_MUTANTS=m01")
		if exit != 3 || !strings.Contains(out, "a subset is not a verdict") {
			t.Fatalf("a gate-subset report under a gate run must exit 3 with the cause: exit %d\n%s", exit, out)
		}
		if left {
			t.Fatalf("the report survived a refused subset:\n%s", out)
		}
	})
}
