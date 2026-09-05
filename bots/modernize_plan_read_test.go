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
	requireModernizeTools(t)

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
	NothingToDo      bool   `json:"nothing_to_do"`
	LotID            string `json:"lot_id"`
	ExitGate         string `json:"exit_gate"`
	Notice           string `json:"notice"`
	LotNotActionable bool   `json:"lot_not_actionable"`
	LotStatus        string `json:"lot_status"`
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
	requireModernizeTools(t)
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
	// A non-actionable explicit request is a VERDICT the graph reads
	// (lot_not_actionable + lot_status → work_gate -> fail), never a tool
	// error: an exit 1 here would be retried once by the engine and end the
	// run failed_resumable — a typed fail is neither.
	refused := func(t *testing.T, lot, status string) modernizePlanReadOut {
		t.Helper()
		res := modernizePlanRead(t, script, plan, lot, 0)
		if !res.LotNotActionable || res.LotStatus != status || res.NothingToDo {
			t.Fatalf("only_lot=%s: lot_not_actionable=%v lot_status=%q nothing_to_do=%v, want the typed verdict with status %q (%s)", lot, res.LotNotActionable, res.LotStatus, res.NothingToDo, status, res.Notice)
		}
		if res.LotID != "" {
			t.Fatalf("only_lot=%s: a refused request must select no lot, got %q", lot, res.LotID)
		}
		return res
	}

	t.Run("done lot is refused, typed", func(t *testing.T) {
		res := refused(t, "L1", "done")
		if !strings.Contains(res.Notice, "'done'") {
			t.Fatalf("notice = %q, want the status named", res.Notice)
		}
	})
	t.Run("blocked lot is refused", func(t *testing.T) {
		res := refused(t, "L2", "blocked")
		if !strings.Contains(res.Notice, "'blocked'") {
			t.Fatalf("notice = %q, want the status named", res.Notice)
		}
	})
	t.Run("undeclared lot is refused", func(t *testing.T) {
		res := refused(t, "L9", "absent")
		if !strings.Contains(res.Notice, "does not exist") {
			t.Fatalf("notice = %q, want the undeclared lot named", res.Notice)
		}
	})
	t.Run("lot behind an unmet dependency is refused", func(t *testing.T) {
		res := refused(t, "L4", "waiting")
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

// TestModernizePlanReadContractShape pins what plan_read refuses BEFORE a
// token is spent: duplicate or non-string ids (a key declared twice cannot be
// read consistently by three nodes), an explicit request on a gate-less lot
// (the green no-op the typed refusal exists to remove), and a lot block the
// gate could never edit its `done` into.
func TestModernizePlanReadContractShape(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "plan_read")

	t.Run("duplicate ids are unreadable, in both modes", func(t *testing.T) {
		dup := `version: 1
lots:
  - id: L1
    title: first
    status: todo
    exit_gate: [ "true" ]
  - id: L1
    title: impostor
    status: todo
    exit_gate: [ "false" ]
`
		for _, only := range []string{"", "L1"} {
			res := modernizePlanRead(t, script, dup, only, 1)
			if !strings.HasPrefix(res.Notice, "CONTRACT_UNREADABLE: duplicate lot id") {
				t.Fatalf("only=%q notice = %q", only, res.Notice)
			}
		}
	})
	t.Run("a non-string id is unreadable", func(t *testing.T) {
		res := modernizePlanRead(t, script, `version: 1
lots:
  - id: 1.0
    title: numeric
    status: todo
    exit_gate: [ "true" ]
`, "", 1)
		if !strings.HasPrefix(res.Notice, "CONTRACT_UNREADABLE") {
			t.Fatalf("notice = %q", res.Notice)
		}
	})
	t.Run("explicit request on a gate-less lot is refused, typed", func(t *testing.T) {
		res := modernizePlanRead(t, script, `version: 1
lots:
  - id: L1
    title: no gate
    status: todo
`, "L1", 0)
		if !res.LotNotActionable || res.LotStatus != "no_gate" || !strings.Contains(res.Notice, "no exit_gate") {
			t.Fatalf("lot_not_actionable=%v lot_status=%q notice=%q, want the typed no_gate verdict", res.LotNotActionable, res.LotStatus, res.Notice)
		}
	})
	t.Run("unfiltered mode keeps the documented no-op on a gate-less lot", func(t *testing.T) {
		res := modernizePlanRead(t, script, `version: 1
lots:
  - id: L5
    title: no gate
    status: todo
`, "", 0)
		if !res.NothingToDo {
			t.Fatalf("expected the legitimate no-op, got %+v", res)
		}
	})
	t.Run("a flow-mapping lot cannot be edited by the gate: refused before spend", func(t *testing.T) {
		res := modernizePlanRead(t, script, `version: 1
lots:
  - {id: L1, title: flow, status: todo, exit_gate: ["true"]}
`, "L1", 1)
		if !strings.HasPrefix(res.Notice, "LOT_UNEDITABLE") {
			t.Fatalf("notice = %q", res.Notice)
		}
	})
	// The pre-check uses mark_done's regex, anchored to the end of the line:
	// a status line the gate could not flip — trailing blanks, a second
	// token — is refused HERE, before the lot is paid for, never at the last
	// node after the gate has run (review finding: the two regexes differed
	// on exactly that, and `status: todo  ` passed plan_read to fail mark_done).
	for _, line := range []string{"    status: todo  ", "    status: todo\t", "    status: todo extra"} {
		t.Run("a status line the gate could not flip is refused before spend: "+strconv.Quote(line), func(t *testing.T) {
			res := modernizePlanRead(t, script, "version: 1\nlots:\n  - id: L1\n    title: t\n"+line+"\n    exit_gate:\n      - \"true\"\n", "L1", 1)
			if !strings.HasPrefix(res.Notice, "LOT_UNEDITABLE") {
				t.Fatalf("notice = %q, want LOT_UNEDITABLE", res.Notice)
			}
		})
	}
}
