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

	run := func(t *testing.T, planYAML string, wantExit int) modernizePlanReadOut {
		return modernizePlanRead(t, script, planYAML, "", wantExit)
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

	t.Run("single-command block scalar is one command, trailing newline stripped", func(t *testing.T) {
		res := run(t, `version: 1
lots:
  - id: T4
    title: block
    exit_gate: |
      test -f .tool-versions
    status: todo
`, 0)
		if res.ExitGate != "test -f .tool-versions" {
			t.Fatalf("exit_gate = %q, want the stripped single command", res.ExitGate)
		}
	})

	t.Run("block-scalar gate is refused as multi-line", func(t *testing.T) {
		res := run(t, `version: 1
lots:
  - id: T3
    title: loop
    exit_gate: |
      for f in a b; do
        test -f "$f"
      done
    status: todo
`, 1)
		if !strings.Contains(res.Notice, "multi-line") {
			t.Fatalf("notice = %q, want a multi-line refusal", res.Notice)
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

// modernizePlanReadOut is the subset of plan_read's output the tests read.
type modernizePlanReadOut struct {
	NothingToDo bool   `json:"nothing_to_do"`
	LotID       string `json:"lot_id"`
	ExitGate    string `json:"exit_gate"`
	Notice      string `json:"notice"`
}

// modernizePlanRead executes the REAL plan_read script against a throwaway
// contract, with `only_lot` bound to onlyLot, and pins the exit code. Shared
// by every plan_read test so a change to the reader's contract surface is
// measured in one place.
func modernizePlanRead(t *testing.T, script, planYAML, onlyLot string, wantExit int) modernizePlanReadOut {
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
	body = strings.ReplaceAll(body, "{{vars.only_lot}}", strconv.Quote(onlyLot))
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
	var res modernizePlanReadOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("plan_read output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

// TestModernizePlanReadOnlyLotRefusal pins the answer to an EXPLICIT request
// that cannot be carried out. The unfiltered reader may legitimately find
// nothing to do; a launch that names a lot the contract carries as landed,
// does not declare, or holds behind an unmet dependency is refused, typed,
// instead of finishing green at zero minutes.
//
// The failure this guards, measured on a live campaign: four `finished` runs
// in 24 h that crossed no gate, every one a relaunch from a banked branch
// whose contract carried a `done` no gate had proven. An operator reading
// convergence where nothing was measured.
func TestModernizePlanReadOnlyLotRefusal(t *testing.T) {
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := toolScript(t, "modernize/main.bot", "plan_read")
	const plan = `version: 1
lots:
  - id: L1
    title: done already
    status: done
    exit_gate: [ "true" ]
  - id: L2
    title: blocked with a reason
    status: blocked
    exit_gate: [ "true" ]
  - id: L3
    title: ready
    status: todo
    exit_gate: [ "true" ]
  - id: L4
    title: waits on L3
    status: todo
    depends_on: [L3]
    exit_gate: [ "true" ]
`
	refused := func(t *testing.T, lot string) modernizePlanReadOut {
		t.Helper()
		res := modernizePlanRead(t, script, plan, lot, 1)
		if !strings.HasPrefix(res.Notice, "LOT_NOT_ACTIONABLE: ") {
			t.Fatalf("only_lot=%s: notice = %q, want a typed LOT_NOT_ACTIONABLE refusal", lot, res.Notice)
		}
		if res.LotID != "" {
			t.Fatalf("only_lot=%s: a refused request must select no lot, got %q", lot, res.LotID)
		}
		return res
	}

	t.Run("done lot is refused, and names the way out", func(t *testing.T) {
		res := refused(t, "L1")
		if !strings.Contains(res.Notice, "`done`") || !strings.Contains(res.Notice, "todo") {
			t.Fatalf("notice = %q, want the status named and the `todo` flip suggested", res.Notice)
		}
	})
	t.Run("blocked lot is refused", func(t *testing.T) {
		res := refused(t, "L2")
		if !strings.Contains(res.Notice, "`blocked`") {
			t.Fatalf("notice = %q, want the status named", res.Notice)
		}
	})
	t.Run("undeclared lot is refused", func(t *testing.T) {
		res := refused(t, "L9")
		if !strings.Contains(res.Notice, "does not declare") {
			t.Fatalf("notice = %q, want the undeclared lot named", res.Notice)
		}
	})
	t.Run("lot behind an unmet dependency is refused", func(t *testing.T) {
		res := refused(t, "L4")
		if !strings.Contains(res.Notice, "waits on L3") {
			t.Fatalf("notice = %q, want the unmet dependency named", res.Notice)
		}
	})
	t.Run("ready lot is selected as before", func(t *testing.T) {
		res := modernizePlanRead(t, script, plan, "L3", 0)
		if res.NothingToDo || res.LotID != "L3" {
			t.Fatalf("only_lot=L3 selected %q (nothing_to_do=%v), want L3", res.LotID, res.NothingToDo)
		}
	})
	t.Run("unfiltered mode keeps its legitimate no-op", func(t *testing.T) {
		res := modernizePlanRead(t, script, `version: 1
lots:
  - id: L1
    title: landed
    status: done
    exit_gate: [ "true" ]
`, "", 0)
		if !res.NothingToDo {
			t.Fatalf("an exhausted programme without only_lot must stay a clean no-op, got %+v", res)
		}
	})
}
