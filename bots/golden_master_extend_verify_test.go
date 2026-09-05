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

type extendVerifyOut struct {
	Committed     bool     `json:"committed"`
	ScopeClean    bool     `json:"scope_clean"`
	AdditionsOK   bool     `json:"additions_ok"`
	Refused       []any    `json:"refused"`
	NoNewRequests bool     `json:"no_new_requests"`
	StillPending  []any    `json:"still_pending"`
	Extended      int      `json:"extended"`
	LogTail       string   `json:"log_tail"`
	OutOfScope    []string `json:"out_of_scope"`
}

// extendVerifyRepo is a target tree whose net carries a STUB harness: the
// extension certifier answers with what `verdict` says, the pending listing
// with what `pending` says. The node under test reads both through the
// tree's own harness, exactly as it does in a run.
func extendVerifyRepo(t *testing.T, verdict, pending string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", ws}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := "import json, os\nmode = os.environ.get(\"GM_MODE\", \"gate\")\n" +
		"if mode == \"extend-verify\":\n    print(json.dumps(" + verdict + "))\n" +
		"elif mode == \"extensions\":\n    print(json.dumps(" + pending + "))\n" +
		"else:\n    print(json.dumps({\"error\": \"stub answers only the extension modes\"}))\n"
	files := map[string]string{
		"harness.py":    harness,
		"EXTENSIONS.md": "# ledger\n<!-- iterion:extension-request\n{\"id\": \"E-L29-1\", \"lot\": \"L29\", \"entries\": [\"118\"]}\n-->\n",
		"refs/001.txt":  "STATUS 200\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".golden-master")
	git("commit", "-qm", "net with a stub certifier")
	return ws, git("rev-parse", "HEAD")
}

func runExtendVerify(t *testing.T, ws, head, pendingJSON string) extendVerifyOut {
	t.Helper()
	body := toolScript(t, "golden-master/extend.bot", "extend_verify")
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.oracle_dir}}", strconv.Quote(".golden-master"))
	body = strings.ReplaceAll(body, "{{input.head}}", strconv.Quote(head))
	body = strings.ReplaceAll(body, "{{input.pending}}", pendingJSON)
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in extend_verify near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "extend_verify.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", scriptPath).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("extend_verify exited %d: %s (out %q)", ee.ExitCode(), ee.Stderr, out)
		}
		t.Fatalf("extend_verify failed to execute: %v", err)
	}
	var res extendVerifyOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("extend_verify output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

// TestGoldenMasterExtendVerifyBaseActIsNotAnExtension pins the verdict
// against a green by absence measured on a live campaign: the subbot
// refused the one request it was handed, the certifier certified an act
// already present at the BASE (`acted_at_base`), and the run came back
// `extended: 1`, `additions_ok: true` — nothing this run did, reported as
// done. An act at the base is certified, not extended; a run that acted
// nothing extended nothing.
func TestGoldenMasterExtendVerifyBaseActIsNotAnExtension(t *testing.T) {
	const verdict = `{"acted": [{"id": "E-L17-1", "ok": True, "acted_at_base": True, "paths": [], "problems": []}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	const pending = `{"pending": [{"id": "E-L29-1", "lot": "L29"}]}`
	ws, head := extendVerifyRepo(t, verdict, pending)
	res := runExtendVerify(t, ws, head, `[{"id": "E-L29-1"}]`)
	if res.Extended != 0 {
		t.Fatalf("extended = %d, want 0: an act already at the base is certified, not extended by this run (%s)", res.Extended, res.LogTail)
	}
	if res.AdditionsOK {
		t.Fatalf("additions_ok = true with nothing acted by this run — a green by absence (%s)", res.LogTail)
	}
	if !strings.Contains(res.LogTail, "acted nothing") {
		t.Fatalf("the verdict must say this run acted nothing: %s", res.LogTail)
	}
	if len(res.StillPending) != 1 || len(res.Refused) != 1 {
		t.Fatalf("still_pending=%v refused=%v, want the refused request reported pending", res.StillPending, res.Refused)
	}
	if !res.Committed || !res.ScopeClean || !res.NoNewRequests {
		t.Fatalf("the untouched terms must stay true: %+v", res)
	}
}

// TestGoldenMasterExtendVerifyActedHereCounts: the same certifier verdict
// with an act made by THIS run is an extension — the count and the term
// follow the act, not the base.
func TestGoldenMasterExtendVerifyActedHereCounts(t *testing.T) {
	const verdict = `{"acted": [{"id": "E-L29-1", "ok": True, "paths": [".golden-master/refs/118.txt"], "problems": []}], "ok_paths": [".golden-master/refs/118.txt"], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	const pending = `{"pending": []}`
	ws, head := extendVerifyRepo(t, verdict, pending)
	res := runExtendVerify(t, ws, head, `[{"id": "E-L29-1"}]`)
	if res.Extended != 1 || !res.AdditionsOK {
		t.Fatalf("extended=%d additions_ok=%v, want 1/true for an act made by this run (%s)", res.Extended, res.AdditionsOK, res.LogTail)
	}
	if len(res.StillPending) != 0 || len(res.Refused) != 0 {
		t.Fatalf("still_pending=%v refused=%v, want nothing pending", res.StillPending, res.Refused)
	}
}
