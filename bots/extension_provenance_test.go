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

// The provenance of an extension act: the net's subbot reports the commits it
// made and the blobs it certified, the parent hands them to the harness at
// its gate, and an act outside them — the constrained party acting its own
// request — is a typed refusal. Measured on a live campaign: a lot filed a
// request in one commit, acted it in the next, and its own file became a
// reference of the net that judges it.

// TestModernizeLotVerifyHandsProvenanceToTheCertifier pins the parent's half:
// GM_ACTED_COMMITS / GM_ACTED_BLOBS reach the certifier on EVERY pass — empty
// before any subbot ran, which is exactly the pass that judges a self-act —
// and a forged act comes back typed (`extension_forged`), never as a repair.
func TestModernizeLotVerifyHandsProvenanceToTheCertifier(t *testing.T) {
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
		if err := os.WriteFile(hp, []byte(harness), 0o755); err != nil {
			t.Fatal(err)
		}
		git("add", "-A", ".golden-master")
		git("commit", "-qm", "net")
		return ws, git("rev-parse", "HEAD")
	}
	// A certifier that REFUSES unless the parent handed it provenance — the
	// presence of the variable, even empty, is the contract.
	const strictStub = `import json, os
mode = os.environ.get("GM_MODE")
if mode == "extend-verify":
    if "GM_ACTED_COMMITS" in os.environ and "GM_ACTED_BLOBS" in os.environ:
        print(json.dumps({"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
    else:
        print(json.dumps({"acted": [{"id": "E-1", "ok": False, "forged": True, "problems": ["no provenance handed: the certifier cannot tell the net's act from the lot's"]}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`
	t.Run("the provenance reaches the certifier before any subbot ran", func(t *testing.T) {
		ws, base := setup(t, strictStub)
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.RefsUntouched || res.ExtensionForged {
			t.Fatalf("the certifier was not handed the provenance (empty, present): %+v", res)
		}
	})
	// A certifier that reports a FORGED act: the parent types it.
	const forgedStub = `import json, os
mode = os.environ.get("GM_MODE")
if mode == "extend-verify":
    print(json.dumps({"acted": [{"id": "E-1", "ok": False, "forged": True, "introduced_by": "abc123def456", "author": "lot@run", "problems": ["the act was introduced by abc123def456 (author lot@run), which is not one of the net subbot's commits (none reported)"]}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`
	t.Run("a forged act is a typed refusal, not a red to repair", func(t *testing.T) {
		ws, base := setup(t, forgedStub)
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.ExtensionForged || res.RefsUntouched {
			t.Fatalf("a forged act must be typed and refuse the references: %+v", res)
		}
		if !strings.HasPrefix(res.BlockReason, "EXTENSION_FORGED:") || !strings.Contains(res.BlockReason, "lot@run") {
			t.Fatalf("block_reason must name the forged act and its author: %q", res.BlockReason)
		}
	})
}

type extendBaseOut struct {
	Head      string `json:"head"`
	Pending   []any  `json:"pending"`
	Notice    string `json:"notice"`
	Clean     bool   `json:"clean"`
	PrevName  string `json:"prev_name"`
	PrevEmail string `json:"prev_email"`
}

func runExtendBase(t *testing.T, ws string) extendBaseOut {
	t.Helper()
	body := toolScript(t, "golden-master/extend.bot", "extend_base")
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.oracle_dir}}", strconv.Quote(".golden-master"))
	body = strings.ReplaceAll(body, "{{vars.actor_name}}", strconv.Quote("golden-master extend"))
	body = strings.ReplaceAll(body, "{{vars.actor_email}}", strconv.Quote("extend@golden-master.iterion"))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in extend_base near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "extend_base.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", scriptPath).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("extend_base exited %d: %s (out %q)", ee.ExitCode(), ee.Stderr, out)
		}
		t.Fatalf("extend_base failed to execute: %v", err)
	}
	var res extendBaseOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("extend_base output is not JSON: %v (out %q)", uerr, out)
	}
	return res
}

func gitInNet(t *testing.T, ws string, args ...string) string {
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

// TestGoldenMasterExtendBaseRefusesADirtyNet pins the subbot's start: a
// clean net gets the engine's identity (the parent's gate attributes acts
// to their author) and its pending requests; an uncommitted path under the
// net refuses the run before the agent — this run committing it would
// certify the constrained party's file as the net's own (the absorption
// attack of the plan review).
func TestGoldenMasterExtendBaseRefusesADirtyNet(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	const pending = `{"pending": [{"id": "E-L29-1", "lot": "L29"}]}`
	t.Run("a clean net: identity set, pending handed to the agent", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		res := runExtendBase(t, ws)
		if !res.Clean || len(res.Pending) != 1 || res.PrevEmail != "t@example.com" {
			t.Fatalf("clean start expected: %+v", res)
		}
		if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "extend@golden-master.iterion" {
			t.Fatalf("the engine must set the net's identity for this run, got %q", got)
		}
	})
	t.Run("an uncommitted path under the net: refused, nothing handed, identity untouched", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("forged by the lot\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runExtendBase(t, ws)
		if res.Clean || len(res.Pending) != 0 || !strings.HasPrefix(res.Notice, "REFUSED") {
			t.Fatalf("a dirty net must be refused before the agent: %+v", res)
		}
		if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "t@example.com" {
			t.Fatalf("a refused start must not touch the identity, got %q", got)
		}
	})
}

// TestGoldenMasterExtendVerifyPublishesItsProvenance pins the subbot's half:
// the commits of this run (base..HEAD), the blob certified per surface path,
// the ids acted, the identity of every commit — and that its own harness
// call carries the same provenance the parent will hand it.
func TestGoldenMasterExtendVerifyPublishesItsProvenance(t *testing.T) {
	// The certifier refuses unless handed provenance: the subbot must pass its
	// own commits to its own call, or passing here and failing there is back.
	const verdict = `{"acted": [{"id": "E-L29-1", "ok": True, "paths": [".golden-master/refs/002.txt"]}], "ok_paths": [".golden-master/refs/002.txt"], "ledger_append_only": True, "requests_added": 0, "problems": ([] if "GM_ACTED_COMMITS" in os.environ else ["no provenance handed"])}`
	act := func(t *testing.T, ws, email string) (string, string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("STATUS 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(ws, ".golden-master", "EXTENSIONS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("<!-- iterion:extension-act\n{\"id\": \"E-L29-1\", \"lot\": \"L29\", \"recorded_paths\": [\".golden-master/refs/002.txt\"]}\n-->\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		gitInNet(t, ws, "add", "-A")
		gitInNet(t, ws, "-c", "user.email="+email, "-c", "user.name=x", "commit", "-qm", "act")
		return gitInNet(t, ws, "rev-parse", "HEAD"), gitInNet(t, ws, "rev-parse", "HEAD:.golden-master/refs/002.txt")
	}
	t.Run("commits, blobs, ids and identity are published", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		sha, blob := act(t, ws, "extend@golden-master.iterion")
		res := runExtendVerify(t, ws, base, `[{"id": "E-L29-1"}]`)
		if res.ActedCommits != sha || res.ActedBlobs != ".golden-master/refs/002.txt="+blob || res.ActedIds != "E-L29-1" {
			t.Fatalf("provenance not published: %+v (sha %s blob %s)", res, sha, blob)
		}
		if !res.IdentityOk || !res.CleanStart || !res.AdditionsOK {
			t.Fatalf("a run under the engine's identity on a clean net must converge: %+v", res)
		}
	})
	t.Run("a commit under another identity is said", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		act(t, ws, "lot@run")
		res := runExtendVerify(t, ws, base, `[{"id": "E-L29-1"}]`)
		if res.IdentityOk || !strings.Contains(res.LogTail, "another identity") {
			t.Fatalf("a commit under the lot's identity must be reported: %+v", res)
		}
	})
}
