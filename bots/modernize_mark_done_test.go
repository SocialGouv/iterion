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

// modernizeMarkDoneOut is the subset of mark_done's output the tests read.
type modernizeMarkDoneOut struct {
	Marked bool   `json:"marked"`
	Commit string `json:"commit"`
	Notice string `json:"notice"`
}

// modernizeRepo builds a throwaway git repository carrying one committed
// contract and returns its path, the base commit, and a git runner bound to
// it. Shared by the mark_done and lot_verify tests.
func modernizeRepo(t *testing.T, planYAML string) (string, string, func(args ...string) string) {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", ws}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
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
	return ws, git("rev-parse", "HEAD"), git
}

func modernizeMarkDone(t *testing.T, script, ws, lotID, base string, wantExit int) modernizeMarkDoneOut {
	t.Helper()
	body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{input.plan_path}}", strconv.Quote(".modernize/plan.yaml"))
	body = strings.ReplaceAll(body, "{{input.lot_id}}", strconv.Quote(lotID))
	body = strings.ReplaceAll(body, "{{input.base_sha}}", strconv.Quote(base))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in mark_done near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "mark_done.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", scriptPath).Output()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("mark_done failed to execute: %v (out %q)", err, out)
		}
		exit = ee.ExitCode()
	}
	if exit != wantExit {
		t.Fatalf("mark_done exited %d, want %d (out %q)", exit, wantExit, out)
	}
	var res modernizeMarkDoneOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("mark_done output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

// TestModernizeMarkDone pins the one write the gate makes to the contract:
// the converged lot's status, one line, one commit, nothing else. `done` is
// the gate's word — a worker that writes it is refused by lot_verify — so
// this node is the only place the programme's "accepted" ever gets written,
// and a landing has exactly one commit to check for it.
func TestModernizeMarkDone(t *testing.T) {
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := toolScript(t, "modernize/main.bot", "mark_done")
	const plan = `version: 1
# the programme, as a human wrote it
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo   # a bookmark, never evidence
    rebaseline_allowed: false
    intent: |
      what may change, and what may not
    exit_gate:
      - "true"
  - id: L2
    title: "raise the runtime"
    status: todo
    depends_on: [L1]
    exit_gate:
      - "true"
`

	t.Run("converged lot: one line flipped, one commit, comments intact", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked || res.Commit == "" {
			t.Fatalf("marked=%v commit=%q, want the status written and committed (%s)", res.Marked, res.Commit, res.Notice)
		}
		got, err := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done   # a bookmark, never evidence", 1)
		if string(got) != want {
			t.Fatalf("contract after mark_done:\n%s\nwant exactly one status line changed:\n%s", got, want)
		}
		if subj := git("log", "-1", "--format=%s"); subj != "L1: done — gate, oracle and references green at "+base[:12] {
			t.Fatalf("commit subject = %q", subj)
		}
		if files := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(files, "1 file changed") {
			t.Fatalf("the done commit must carry the contract alone, got:\n%s", files)
		}
		if git("rev-parse", "HEAD") != res.Commit {
			t.Fatalf("reported commit %s is not HEAD", res.Commit)
		}
	})

	t.Run("idempotent: a contract already done is left alone, nothing committed", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done", 1))
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if res.Marked || res.Commit != "" {
			t.Fatalf("marked=%v commit=%q on an already-done lot, want no write", res.Marked, res.Commit)
		}
		if head := git("rev-parse", "HEAD"); head != base {
			t.Fatalf("HEAD moved to %s on the idempotent path", head)
		}
	})

	t.Run("undeclared lot is refused", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		res := modernizeMarkDone(t, script, ws, "L9", base, 1)
		if !strings.Contains(res.Notice, "not in the contract") {
			t.Fatalf("notice = %q", res.Notice)
		}
		if head := git("rev-parse", "HEAD"); head != base {
			t.Fatalf("HEAD moved to %s on a refusal", head)
		}
	})

	t.Run("a block without a status line is refused, contract untouched", func(t *testing.T) {
		noStatus := strings.Replace(plan, "    status: todo   # a bookmark, never evidence\n", "", 1)
		ws, base, _ := modernizeRepo(t, noStatus)
		res := modernizeMarkDone(t, script, ws, "L1", base, 1)
		if !strings.Contains(res.Notice, "no `status:` line") {
			t.Fatalf("notice = %q", res.Notice)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
		if string(got) != noStatus {
			t.Fatalf("a refused edit must leave the contract byte-identical")
		}
	})
}
