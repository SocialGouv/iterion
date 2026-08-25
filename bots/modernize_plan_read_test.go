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

// TestModernizePlanReadExitGateForms pins the contract surface of the
// `exit_gate` field. A YAML scalar and a YAML sequence are both legitimate
// ways to declare a lot's gate, and the reader must hand the verifier whole
// commands either way.
//
// The failure this guards: `"\n".join(gate)` over a bare string iterates its
// CHARACTERS, so the verifier's first command is the single letter `t`
// (exit 127: `t: not found`) and the lot can never converge — a red verdict
// manufactured by the reader, not earned by the tree.
func TestModernizePlanReadExitGateForms(t *testing.T) {
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}

	script := toolScript(t, "modernize/main.bot", "plan_read")

	type planReadOut struct {
		NothingToDo bool   `json:"nothing_to_do"`
		LotID       string `json:"lot_id"`
		ExitGate    string `json:"exit_gate"`
		Notice      string `json:"notice"`
	}

	run := func(t *testing.T, planYAML string, wantExit int) planReadOut {
		t.Helper()
		ws := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			full := append([]string{"-C", ws}, args...)
			cmd := exec.Command("git", full...)
			// The throwaway repo must not inherit the operator's global or
			// system config: a core.hooksPath or commit.gpgsign there fails
			// `git commit` for reasons unrelated to what this test pins.
			cmd.Env = append(os.Environ(),
				"GIT_CONFIG_GLOBAL=/dev/null",
				"GIT_CONFIG_SYSTEM=/dev/null",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v (%s)", args, err, out)
			}
		}
		git("init", "-q", "-b", "main")
		git("config", "user.email", "t@example.com")
		git("config", "user.name", "t")
		if err := os.MkdirAll(filepath.Join(ws, ".modernize"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(planYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")

		body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", strconv.Quote(ws))
		body = strings.ReplaceAll(body, "{{vars.plan_path}}", strconv.Quote(".modernize/plan.yaml"))
		body = strings.ReplaceAll(body, "{{vars.only_lot}}", strconv.Quote(""))
		if i := strings.Index(body, "{{"); i >= 0 {
			t.Fatalf("unresolved template ref in plan_read near %q", body[i:min(i+40, len(body))])
		}
		scriptPath := filepath.Join(t.TempDir(), "plan_read.py")
		if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("python3", scriptPath).Output()
		exit := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("plan_read failed to execute: %v (out %q)", err, out)
			}
			exit = ee.ExitCode()
		}
		if exit != wantExit {
			t.Fatalf("plan_read exited %d, want %d (out %q)", exit, wantExit, out)
		}
		var res planReadOut
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("plan_read output is not JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	t.Run("scalar gate reaches the verifier whole", func(t *testing.T) {
		res := run(t, `version: 1
lots:
  - id: T1
    title: toolchain
    rebaseline_allowed: false
    exit_gate: test -f .tool-versions && bash scripts/env/probe.sh
    status: todo
`, 0)
		if res.NothingToDo {
			t.Fatalf("nothing_to_do on a contract with a todo lot (notice %q)", res.Notice)
		}
		want := "test -f .tool-versions && bash scripts/env/probe.sh"
		if res.ExitGate != want {
			t.Fatalf("exit_gate = %q, want the whole command %q", res.ExitGate, want)
		}
	})

	t.Run("sequence gate joins one command per line", func(t *testing.T) {
		res := run(t, `version: 1
lots:
  - id: L1
    title: build
    exit_gate:
      - ./gradlew --no-daemon build -x test
      - ./gradlew --no-daemon bootJar
    status: todo
`, 0)
		want := "./gradlew --no-daemon build -x test\n./gradlew --no-daemon bootJar"
		if res.ExitGate != want {
			t.Fatalf("exit_gate = %q, want %q", res.ExitGate, want)
		}
	})

	t.Run("mapping gate is refused as unreadable", func(t *testing.T) {
		res := run(t, `version: 1
lots:
  - id: T2
    title: slip
    exit_gate:
      build: ./gradlew build
    status: todo
`, 1)
		if !strings.Contains(res.Notice, "unreadable shape") {
			t.Fatalf("notice = %q, want an unreadable-shape refusal", res.Notice)
		}
	})
}
