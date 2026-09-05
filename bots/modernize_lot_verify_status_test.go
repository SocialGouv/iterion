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

// modernizeLotVerifyOut is the subset of lot_verify's report the tests read.
type modernizeLotVerifyOut struct {
	GatePassed      bool     `json:"gate_passed"`
	OraclePassed    bool     `json:"oracle_passed"`
	RefsUntouched   bool     `json:"refs_untouched"`
	LotBlocked      bool     `json:"lot_blocked"`
	DoneSelfWritten bool     `json:"done_self_written"`
	ContractRewrite []string `json:"contract_rewritten"`
	LogTail         string   `json:"log_tail"`
}

// modernizeNet drops the smallest net lot_verify accepts into ws: a runner
// that exits 0, a reference, and a harness stub answering the two extension
// modes with empty verdicts — so the refs-immutability and extension
// certification paths run for real without an application to boot.
func modernizeNet(t *testing.T, ws string) {
	t.Helper()
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"verify-oracle.sh": "#!/bin/sh\nexit 0\n",
		"refs/001.txt":     "STATUS 200\n",
		"harness.py": `import json, os, sys
mode = os.environ.get("GM_MODE", "gate")
if mode == "extend-verify":
    print(json.dumps({"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func modernizeLotVerify(t *testing.T, script, ws, lotID, base, exitGate string) modernizeLotVerifyOut {
	t.Helper()
	body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{input.plan_path}}", strconv.Quote(".modernize/plan.yaml"))
	body = strings.ReplaceAll(body, "{{input.lot_id}}", strconv.Quote(lotID))
	body = strings.ReplaceAll(body, "{{input.base_sha}}", strconv.Quote(base))
	body = strings.ReplaceAll(body, "{{input.refs_dir}}", strconv.Quote(".golden-master/refs"))
	body = strings.ReplaceAll(body, "{{input.exit_gate}}", strconv.Quote(exitGate))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in lot_verify near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "lot_verify.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", scriptPath).Output()
	if err != nil {
		t.Fatalf("lot_verify failed: %v (out %q)", err, out)
	}
	var res modernizeLotVerifyOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("lot_verify output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

// TestModernizeLotVerifyRefusesWorkerWrittenDone pins the asymmetry in how
// the verifier believes the contract's status. `blocked` is a STOP the worker
// may declare; `done` is the gate's word. A `done` the worker wrote is
// refused BEFORE any gate command runs — the marker file proves the gate was
// never reached — so the revert costs the worker seconds, never the hour a
// full gate would spend confirming a verdict it had written for itself.
//
// The failure this guards: a bank taken after such a write carried `done`
// into a relaunch, which finished green at zero minutes having proven
// nothing (measured, four times in 24 h on a live campaign).
func TestModernizeLotVerifyRefusesWorkerWrittenDone(t *testing.T) {
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	const plan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo
    exit_gate:
      - "true"
`
	// The gate leaves a marker when it runs: its absence proves fail-fast.
	const gate = "sh -c 'echo ran > gate.marker'"

	t.Run("worker wrote done: refused before the gate runs", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base = git("rev-parse", "HEAD")
		flipped := strings.Replace(plan, "status: todo", "status: done", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(flipped), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "chore(plan): L1 done — written by the worker")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.DoneSelfWritten {
			t.Fatalf("done_self_written=false on a worker-written done (%s)", res.LogTail)
		}
		if res.GatePassed || res.OraclePassed || res.RefsUntouched {
			t.Fatalf("a refused verdict must be red on every conjunct: %+v", res)
		}
		if !strings.Contains(res.LogTail, "gate's word") || !strings.Contains(res.LogTail, "Revert that status line") {
			t.Fatalf("log_tail = %q, want the cause and the way out named", res.LogTail)
		}
		if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err == nil {
			t.Fatalf("the exit_gate ran: a worker-written done must be refused before any gate command")
		}
	})

	t.Run("status untouched: the gate runs and the lot converges", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if res.DoneSelfWritten {
			t.Fatalf("done_self_written=true on an untouched status (%s)", res.LogTail)
		}
		if !res.GatePassed || !res.OraclePassed || !res.RefsUntouched {
			t.Fatalf("expected a green verdict on a clean tree, got %+v", res)
		}
		if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err != nil {
			t.Fatalf("the exit_gate never ran on the happy path")
		}
	})

	t.Run("done already at base is not the worker's word", func(t *testing.T) {
		// A lot done at base is never selected by plan_read; if a verifier is
		// nevertheless pointed at it, the status is the base's, not a
		// self-report, and the verdict is taken on the tree.
		ws, _, git := modernizeRepo(t, strings.Replace(plan, "status: todo", "status: done", 1))
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if res.DoneSelfWritten {
			t.Fatalf("a done inherited from base was flagged as self-written")
		}
	})

	t.Run("blocked is believed as a stop", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(strings.Replace(plan, "status: todo", "status: blocked", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "L1 blocked, reason committed")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.LotBlocked || res.DoneSelfWritten {
			t.Fatalf("blocked=%v self_written=%v, want the stop believed and no refusal", res.LotBlocked, res.DoneSelfWritten)
		}
	})
}

// TestModernizeLotVerifyRefusesContractRewrite pins the contract's read-only
// rule inside a lot, enforced in git rather than asked nicely: an existing
// lot's gate, intent or dependencies changed, a lot dropped, or another lot's
// status moved is refused BEFORE any gate command runs, with the rewrites
// named. Adding a lot is a proposal, not a rewrite, and passes.
func TestModernizeLotVerifyRefusesContractRewrite(t *testing.T) {
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	const plan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo
    exit_gate:
      - "true"
      - "test -f build.gradle"
  - id: L2
    title: "raise the runtime"
    status: todo
    depends_on: [L1]
    exit_gate:
      - "true"
`
	const gate = "sh -c 'echo ran > gate.marker'"
	setup := func(t *testing.T) (string, string, func(args ...string) string) {
		t.Helper()
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		if err := os.WriteFile(filepath.Join(ws, "build.gradle"), []byte("// build\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "-qm", "net + build")
		return ws, git("rev-parse", "HEAD"), git
	}
	rewrite := func(t *testing.T, ws string, from, to string) {
		t.Helper()
		if !strings.Contains(plan, from) {
			t.Fatalf("fixture has no %q", from)
		}
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(strings.Replace(plan, from, to, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	refusedBeforeGate := func(t *testing.T, ws string, res modernizeLotVerifyOut, want string) {
		t.Helper()
		if len(res.ContractRewrite) == 0 || !strings.Contains(strings.Join(res.ContractRewrite, "|"), want) {
			t.Fatalf("contract_rewritten = %v, want %q named (%s)", res.ContractRewrite, want, res.LogTail)
		}
		if res.GatePassed || res.OraclePassed || res.RefsUntouched {
			t.Fatalf("a refused verdict must be red on every conjunct: %+v", res)
		}
		if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err == nil {
			t.Fatalf("the exit_gate ran: a rewritten contract must be refused before any gate command")
		}
	}

	t.Run("own gate loosened: refused, gate never ran", func(t *testing.T) {
		ws, base, git := setup(t)
		rewrite(t, ws, "      - \"test -f build.gradle\"\n", "")
		git("commit", "-qam", "drop a gate command")
		refusedBeforeGate(t, ws, modernizeLotVerify(t, script, ws, "L1", base, gate), "L1.exit_gate changed")
	})
	t.Run("another lot's status moved: refused", func(t *testing.T) {
		ws, base, _ := setup(t)
		rewrite(t, ws, "    status: todo\n    depends_on: [L1]", "    status: done\n    depends_on: [L1]")
		// uncommitted on purpose: the tree the gate judges is what is read
		refusedBeforeGate(t, ws, modernizeLotVerify(t, script, ws, "L1", base, gate), "L2.status moved")
	})
	t.Run("a lot removed: refused", func(t *testing.T) {
		ws, base, git := setup(t)
		short := plan[:strings.Index(plan, "  - id: L2")]
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(short), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "drop L2")
		refusedBeforeGate(t, ws, modernizeLotVerify(t, script, ws, "L1", base, gate), "lot L2 was REMOVED")
	})
	t.Run("a lot added: a proposal, not a rewrite — the gate runs", func(t *testing.T) {
		ws, base, git := setup(t)
		added := plan + "  - id: L3\n    title: \"pin the locale\"\n    status: todo\n    exit_gate: [ \"true\" ]\n"
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(added), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "propose L3")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if len(res.ContractRewrite) != 0 || res.DoneSelfWritten {
			t.Fatalf("an added lot was flagged: %v", res.ContractRewrite)
		}
		if !res.GatePassed || !res.OraclePassed || !res.RefsUntouched {
			t.Fatalf("expected a green verdict, got %+v", res)
		}
	})
	t.Run("scalar and list gates with the same command are the same gate", func(t *testing.T) {
		ws, base, git := setup(t)
		rewrite(t, ws, "    exit_gate:\n      - \"true\"\n", "    exit_gate: \"true\"\n")
		git("commit", "-qam", "reformat L2 gate")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if len(res.ContractRewrite) != 0 {
			t.Fatalf("a reformatted, unchanged gate was flagged: %v", res.ContractRewrite)
		}
	})
}
