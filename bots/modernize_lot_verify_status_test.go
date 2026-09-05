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
	GateTimedOut    bool     `json:"gate_timed_out"`
	BlockReason     string   `json:"block_reason"`
	Unreadable      bool     `json:"contract_unreadable"`
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
	res, exit := modernizeLotVerifyEnv(t, script, ws, lotID, base, exitGate, nil)
	if exit != 0 {
		t.Fatalf("lot_verify exited %d, want a verdict (out %+v)", exit, res)
	}
	return res
}

// modernizeLotVerifyEnv runs lot_verify with an optional environment override
// and returns the parsed report plus the exit code (1 = typed refusal, the
// contract could not be read).
func modernizeLotVerifyEnv(t *testing.T, script, ws, lotID, base, exitGate string, env []string) (modernizeLotVerifyOut, int) {
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
	cmd := exec.Command("python3", scriptPath)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("lot_verify failed to execute: %v (out %q)", err, out)
		}
		exit = ee.ExitCode()
	}
	var res modernizeLotVerifyOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("lot_verify output is not JSON: %v (out %q)", uerr, out)
	}
	return res, exit
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
	requireModernizeTools(t)
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
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
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
	requireModernizeTools(t)
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
		rewrite(t, ws, "    depends_on: [L1]\n    exit_gate:\n      - \"true\"\n", "    depends_on: [L1]\n    exit_gate: \"true\"\n")
		git("commit", "-qam", "reformat L2 gate")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if len(res.ContractRewrite) != 0 {
			t.Fatalf("a reformatted, unchanged gate was flagged: %v", res.ContractRewrite)
		}
	})
}

// TestModernizeLotVerifyReadsTheContractOrRefuses pins the adversarial
// finding that turned both guards into a silent no-op: lot_verify called `yq`
// on the bare PATH and swallowed its absence, so a worker-written `done` and
// an amputated gate converged green with an empty log_tail. The verifier now
// finds yq where plan_read and mark_done do — the target's devbox profile —
// and REFUSES, typed, when it cannot read the contract at all.
func TestModernizeLotVerifyReadsTheContractOrRefuses(t *testing.T) {
	requireModernizeTools(t)
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
	const gate = "sh -c 'echo ran > gate.marker'"
	selfWrittenDone := func(t *testing.T) (string, string, func(args ...string) string) {
		t.Helper()
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(strings.Replace(plan, "status: todo", "status: done", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "worker writes done")
		return ws, base, git
	}
	yqPath, _ := exec.LookPath("yq")

	t.Run("yq off PATH but in the target's devbox profile: the guard still fires", func(t *testing.T) {
		ws, base, _ := selfWrittenDone(t)
		profile := filepath.Join(ws, ".devbox", "nix", "profile", "default", "bin")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(yqPath, filepath.Join(profile, "yq")); err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), "PATH="+restrictedPATH(t, "python3", "git", "sh"))
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, gate, env)
		if exit != 0 || !res.DoneSelfWritten {
			t.Fatalf("exit=%d done_self_written=%v — with yq only in the devbox profile the guard must still fire (%s)", exit, res.DoneSelfWritten, res.LogTail)
		}
		if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err == nil {
			t.Fatalf("the exit_gate ran: the refusal must come first")
		}
	})

	t.Run("no yq anywhere: refused, typed — never a verdict that skipped its checks", func(t *testing.T) {
		ws, base, _ := selfWrittenDone(t)
		env := append(os.Environ(), "PATH="+restrictedPATH(t, "python3", "git", "sh"))
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, gate, env)
		if exit != 0 || !res.Unreadable {
			t.Fatalf("exit=%d contract_unreadable=%v, want the typed refusal as a verdict — report %+v", exit, res.Unreadable, res)
		}
		if !strings.HasPrefix(res.LogTail, "CONTRACT_UNREADABLE") {
			t.Fatalf("log_tail = %q, want CONTRACT_UNREADABLE", res.LogTail)
		}
		if res.GatePassed || res.OraclePassed || res.RefsUntouched {
			t.Fatalf("a refusal must leave every conjunct false: %+v", res)
		}
	})

	t.Run("contract not committed at base: refused, typed", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("rm", "-q", "--cached", ".modernize/plan.yaml")
		git("commit", "-qm", "net, and the contract dropped from the tree")
		base := git("rev-parse", "HEAD")
		// the contract exists only in the working tree, never at base
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(plan), 0o644); err != nil {
			t.Fatal(err)
		}
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, gate, nil)
		if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "not committed at the run's base") {
			t.Fatalf("exit=%d contract_unreadable=%v log_tail=%q, want a typed refusal naming the uncommitted contract", exit, res.Unreadable, res.LogTail)
		}
	})

	t.Run("blocked AND rewritten: the rewrite wins, the run does not stop", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan+"  - id: L2\n    title: next\n    status: todo\n    exit_gate: [ \"true\" ]\n")
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		rewritten := strings.Replace(plan, "status: todo", "status: blocked", 1) + "  - id: L2\n    title: next\n    status: done\n    exit_gate: [ \"true\" ]\n"
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(rewritten), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "L1 blocked, and L2 quietly done")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if len(res.ContractRewrite) == 0 || !strings.Contains(strings.Join(res.ContractRewrite, "|"), "L2.status moved") {
			t.Fatalf("contract_rewritten = %v, want L2's move named", res.ContractRewrite)
		}
		if res.LotBlocked {
			t.Fatalf("lot_blocked stayed true on a rewritten contract: lot_gate.stop would end the run with the rewrite landed")
		}
	})

	t.Run("a homonym lot added ahead: named as a duplicate, refused", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		homonym := strings.Replace(plan, "lots:\n", "lots:\n  - id: L1\n    title: impostor\n    status: todo\n    exit_gate: [ \"true\" ]\n", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(homonym), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "a second L1")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !strings.Contains(strings.Join(res.ContractRewrite, "|"), "duplicate lot id L1") {
			t.Fatalf("contract_rewritten = %v, want the duplicate named", res.ContractRewrite)
		}
	})
}

// TestModernizeLotVerifyOlderNetIsNotRefused pins the pre-extension
// behaviour for a net materialised before the extension feature: no
// `extend-verify` mode means no certificate and no exemption — never "the
// certifier returned no usable verdict", which refused every lot on every
// older net. A certifier that EXISTS and cannot answer stays a refusal.
func TestModernizeLotVerifyOlderNetIsNotRefused(t *testing.T) {
	requireModernizeTools(t)
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
	const gate = "sh -c 'echo ran > gate.marker'"
	setup := func(t *testing.T, harness string) (string, string) {
		t.Helper()
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		hp := filepath.Join(ws, ".golden-master", "harness.py")
		if harness == "" {
			if err := os.Remove(hp); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(hp, []byte(harness), 0o755); err != nil {
			t.Fatal(err)
		}
		git("add", "-A", ".golden-master")
		git("commit", "-qm", "net")
		return ws, git("rev-parse", "HEAD")
	}

	t.Run("no harness at all: the references are judged in git, nothing is refused", func(t *testing.T) {
		ws, base := setup(t, "")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.GatePassed || !res.OraclePassed || !res.RefsUntouched {
			t.Fatalf("an older net without a harness must not be refused: %+v", res)
		}
		if strings.Contains(res.LogTail, "no usable verdict") {
			t.Fatalf("log_tail carries the certifier refusal on a net that has no certifier: %q", res.LogTail)
		}
	})
	t.Run("a harness without the extend-verify mode: same, the older net's legal state", func(t *testing.T) {
		ws, base := setup(t, "import json\nprint(json.dumps({\"mode\": \"gate\"}))\n")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.RefsUntouched || strings.Contains(res.LogTail, "no usable verdict") {
			t.Fatalf("older-net harness refused: %+v", res)
		}
	})
	t.Run("a certifier that exists and answers garbage is still a refusal", func(t *testing.T) {
		ws, base := setup(t, "import os\nmode = os.environ.get(\"GM_MODE\")\nif mode == \"extend-verify\":\n    print(\"not json at all\")\nelse:\n    print(\"{}\")\n")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if res.RefsUntouched || !strings.Contains(res.LogTail, "no usable verdict") {
			t.Fatalf("a mute certifier must refuse: %+v", res)
		}
	})
}

// TestModernizeLotVerifyGateTimeoutIsAVerdict pins the wall each gate command
// gets. Measured on a live campaign: a target whose full oracle gate replays
// ~150 mutants in ~58 min hit the fixed 3600 s subprocess timeout at 3646 s —
// a Python exception, which the engine read as a tool error, retried ONCE (the
// same hour, replayed) and then left `failed_resumable` with no verdict line
// to read. The wall is now the contract's (`gate_timeout_s`, lot over top
// level, read at the BASE), and an expiry is a verdict: `gate_timed_out`,
// `lot_blocked`, a GATE_TIMEOUT block_reason — the run stops with its work
// banked instead of paying four passes against the same wall.
func TestModernizeLotVerifyGateTimeoutIsAVerdict(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	const plan = `version: 1
gate_timeout_s: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo
    exit_gate:
      - "true"
`
	netAndBase := func(t *testing.T, planYAML, oracle string) (string, string, func(args ...string) string) {
		t.Helper()
		ws, _, git := modernizeRepo(t, planYAML)
		modernizeNet(t, ws)
		if oracle != "" {
			if err := os.WriteFile(filepath.Join(ws, ".golden-master", "verify-oracle.sh"), []byte(oracle), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		git("add", ".golden-master")
		git("commit", "-qm", "net")
		return ws, git("rev-parse", "HEAD"), git
	}

	t.Run("the oracle outlives the wall: a GATE_TIMEOUT verdict, not an exception", func(t *testing.T) {
		ws, base, _ := netAndBase(t, plan, "#!/bin/sh\nsleep 3\nexit 0\n")
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if !res.GateTimedOut || res.LotBlocked || !strings.HasPrefix(res.BlockReason, "GATE_TIMEOUT:") {
			t.Fatalf("expiry not read as the GATE_TIMEOUT verdict (gate_timed_out, NOT a worker-declared block): %+v", res)
		}
		if !res.GatePassed || res.OraclePassed {
			t.Fatalf("the exit gate ran green and the oracle never answered — want gate_passed=true oracle_passed=false, got %+v", res)
		}
		if !strings.Contains(res.LogTail, "gate_timeout_s=1") {
			t.Fatalf("the verdict must name the wall it hit: %s", res.LogTail)
		}
	})
	t.Run("the lot's own wall beats the top level, and an exit_gate command is stopped by it", func(t *testing.T) {
		lotPlan := strings.Replace(plan, "gate_timeout_s: 1\n", "gate_timeout_s: 3600\n", 1)
		lotPlan = strings.Replace(lotPlan, "    status: todo\n", "    status: todo\n    gate_timeout_s: 1\n", 1)
		ws, base, _ := netAndBase(t, lotPlan, "")
		res := modernizeLotVerify(t, script, ws, "L1", base, "sleep 3")
		if !res.GateTimedOut || res.GatePassed || res.LotBlocked {
			t.Fatalf("the lot's 1 s wall did not stop a 3 s exit_gate: %+v", res)
		}
	})
	t.Run("a TOP-LEVEL wall the worker moved is a contract rewrite", func(t *testing.T) {
		// The next run reads its wall from its base, which is this tree once
		// landed: one committed line would disarm the wall for the rest of
		// the programme (review finding, executed with 3600 -> 1).
		ws, base, git := netAndBase(t, plan, "")
		moved := strings.Replace(plan, "gate_timeout_s: 1\n", "gate_timeout_s: 86400\n", 1)
		moved = strings.Replace(moved, "  refs_dir: .golden-master/refs\n", "  refs_dir: .elsewhere/refs\n", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "move the wall and the net")
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		joined := strings.Join(res.ContractRewrite, "\n")
		if !strings.Contains(joined, "top-level gate_timeout_s") || !strings.Contains(joined, "top-level oracle") {
			t.Fatalf("top-level normative keys moved without a rewrite refusal: %+v", res)
		}
	})
	t.Run("a lot-level wall the worker added is a contract rewrite", func(t *testing.T) {
		ws, base, git := netAndBase(t, plan, "")
		moved := strings.Replace(plan, "    status: todo\n", "    status: todo\n    gate_timeout_s: 7200\n", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "move the wall")
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if !strings.Contains(strings.Join(res.ContractRewrite, "\n"), "gate_timeout_s") {
			t.Fatalf("a worker-added gate_timeout_s on the lot must read as a rewrite: %+v", res)
		}
	})
	for _, bad := range []string{"soon", "0", "86401", "true", `"7200"`} {
		t.Run("unreadable wall "+bad+" is refused before any command", func(t *testing.T) {
			badPlan := strings.Replace(plan, "gate_timeout_s: 1\n", "gate_timeout_s: "+bad+"\n", 1)
			ws, base, _ := netAndBase(t, badPlan, "")
			res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, "sh -c 'echo ran > gate.marker'", nil)
			if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "CONTRACT_UNREADABLE") || !strings.Contains(res.LogTail, "gate_timeout_s") {
				t.Fatalf("exit %d contract_unreadable=%v, log %q — want the typed refusal naming gate_timeout_s", exit, res.Unreadable, res.LogTail)
			}
			if _, err := os.Stat(filepath.Join(ws, "gate.marker")); err == nil {
				t.Fatal("the gate ran under a wall the node could not read")
			}
		})
	}
}

// TestModernizeLotVerifyRefsNeedANetAtBase: `refs_untouched` over a net that
// does not exist at the run's base would be true by absence — nothing was
// there to touch — which is not a term. The absent oracle already reads red;
// this keeps the second conjunct honest on its own.
func TestModernizeLotVerifyRefsNeedANetAtBase(t *testing.T) {
	requireModernizeTools(t)
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
	ws, base, _ := modernizeRepo(t, plan) // the base carries NO net
	res := modernizeLotVerify(t, script, ws, "L1", base, "true")
	if res.RefsUntouched {
		t.Fatalf("refs_untouched=true over an absent net — true by absence: %+v", res)
	}
	if !strings.Contains(res.LogTail, "no net at the run's base") {
		t.Fatalf("the verdict must say the net is absent at the base: %s", res.LogTail)
	}
}

// TestModernizeLotVerifyDirtyNetVoidSurvives pins the refusal a dirty net
// carries all the way to the verdict. The void was written into
// refs_untouched when `git status` saw an uncommitted path under the net —
// and the diff block below it REASSIGNED the same key from `git diff <base>`,
// which cannot see an untracked file or an uncommitted edit to a ledger the
// diff exempts. A dirty net converged and the gate committed `done` (review
// finding, executed).
func TestModernizeLotVerifyDirtyNetVoidSurvives(t *testing.T) {
	requireModernizeTools(t)
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
	for _, tc := range []struct {
		name string
		mess func(ws string) error
	}{
		{"an uncommitted edit to the extension ledger (exempt from the diff)", func(ws string) error {
			return os.WriteFile(filepath.Join(ws, ".golden-master", "EXTENSIONS.md"), []byte("# ledger\n- request: something\n"), 0o644)
		}},
		{"an untracked file dropped under the net", func(ws string) error {
			return os.WriteFile(filepath.Join(ws, ".golden-master", "json.py"), []byte("# certifier hijack\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, _, git := modernizeRepo(t, plan)
			modernizeNet(t, ws)
			if err := os.WriteFile(filepath.Join(ws, ".golden-master", "EXTENSIONS.md"), []byte("# ledger\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".golden-master")
			git("commit", "-qm", "net")
			base := git("rev-parse", "HEAD")
			if err := tc.mess(ws); err != nil {
				t.Fatal(err)
			}
			res := modernizeLotVerify(t, script, ws, "L1", base, "true")
			if res.RefsUntouched {
				t.Fatalf("refs_untouched=true on a dirty net — the void was overwritten by the diff's own result: %+v", res)
			}
			if !strings.Contains(res.LogTail, "uncommitted path") {
				t.Fatalf("the verdict must name the dirty net: %s", res.LogTail)
			}
		})
	}
}
