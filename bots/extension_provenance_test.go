package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
    print(json.dumps({"provenance": "strict", "acted": [{"id": "E-1", "ok": False, "forged": True, "introduced_by": "abc123def456", "author": "lot@run", "problems": ["the act was introduced by abc123def456 (author lot@run), which is not one of the net subbot's commits (none reported)"]}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
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
	// The escape the two older refusals already guard against: `stop` is
	// `converged || lot_blocked`, `lot_blocked` is what the CONSTRAINED party
	// wrote into its own contract, and `lot_gate -> done when stop` is
	// declared ahead of the fail edges — so a lot that forges an act AND
	// declares itself blocked would end `finished`, its forged reference
	// banked and mergeable.
	t.Run("a forged act cannot ride out through a self-declared blocked lot", func(t *testing.T) {
		ws, _, git := modernizeRepo(t, plan)
		modernizeNet(t, ws)
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "harness.py"), []byte(forgedStub), 0o755); err != nil {
			t.Fatal(err)
		}
		git("add", "-A", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"),
			[]byte(strings.Replace(plan, "status: todo", "status: blocked", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "the worker declares its own lot blocked")
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.ExtensionForged || res.RefsUntouched {
			t.Fatalf("the forged act must still be typed and refuse the references: %+v", res)
		}
		if res.LotBlocked {
			t.Fatalf("a refused verdict is never a clean stop — lot_blocked must be cleared: %+v", res)
		}
	})
	// The certifier is the TARGET's harness copy — a file the lot could write.
	// One that CERTIFIES an act it also calls forged is the modelled hostile
	// shape, and the parent's own term must refuse the references on it rather
	// than read `ok` and exempt the path.
	const hostileStub = `import json, os
mode = os.environ.get("GM_MODE")
if mode == "extend-verify":
    print(json.dumps({"provenance": "strict", "acted": [{"id": "E-1", "ok": True, "forged": True, "introduced_by": "abc123def456", "author": "lot@run", "paths": [".golden-master/refs/002.txt"], "problems": []}], "ok_paths": [".golden-master/refs/002.txt"], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`
	t.Run("a certifier that calls an act both ok and forged refuses the references", func(t *testing.T) {
		ws, base := setup(t, hostileStub)
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.ExtensionForged || res.RefsUntouched {
			t.Fatalf("a forged act refuses the references whatever the certifier calls it: %+v", res)
		}
	})
	// A net synced BEFORE the provenance rule answers `extend-verify` and
	// passes the capability probe, while judging by the same-commit rule
	// alone — the rule a lot passes by acting in a second commit. Nothing in
	// its answer says so, so the parent asserts the echo.
	const legacyStub = `import json, os
mode = os.environ.get("GM_MODE")
if mode == "extend-verify":
    print(json.dumps({"acted": [{"id": "E-1", "ok": True, "paths": [".golden-master/refs/002.txt"]}], "ok_paths": [".golden-master/refs/002.txt"], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`
	t.Run("a certifier that never says it applied the rule is refused, not believed", func(t *testing.T) {
		ws, base := setup(t, legacyStub)
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if !res.ExtensionForged || res.RefsUntouched || res.LotBlocked {
			t.Fatalf("an unechoed provenance must refuse the lot: %+v", res)
		}
		if !strings.HasPrefix(res.BlockReason, "EXTENSION_UNCERTIFIED:") || !strings.Contains(res.BlockReason, "Sync the") {
			t.Fatalf("block_reason must name the missing sync: %q", res.BlockReason)
		}
	})
	// ... and an act ALREADY certified at the base needs no echo: an older net
	// whose ledger carries one must not refuse every lot launched from it.
	const legacyAtBaseStub = `import json, os
mode = os.environ.get("GM_MODE")
if mode == "extend-verify":
    print(json.dumps({"acted": [{"id": "E-1", "ok": True, "acted_at_base": True, "paths": [], "problems": []}], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}))
elif mode == "extensions":
    print(json.dumps({"pending": []}))
else:
    print(json.dumps({"error": "stub answers only the extension modes"}))
`
	t.Run("an act already certified at the base is not re-refused", func(t *testing.T) {
		ws, base := setup(t, legacyAtBaseStub)
		res := modernizeLotVerify(t, script, ws, "L1", base, gate)
		if res.ExtensionForged || !res.RefsUntouched {
			t.Fatalf("an act certified at the base needs no echo: %+v", res)
		}
	})
}

// TestModernizeProvenanceRefusalIsTerminalAndDeclaredFirst pins the WIRING,
// not the verdict: the refusal must be routed by an edge declared ahead of the
// subbot edges and the repair loop — a verdict no pass can repair, on a tree
// that may also carry a pending request the net's own subbot would act on. It
// lands on a TYPED terminal, so the operator reads a code instead of the
// FAIL_NODE a forged `done` also produces.
func TestModernizeProvenanceRefusalIsTerminalAndDeclaredFirst(t *testing.T) {
	const rel = "modernize/main.bot"
	src, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(rel, string(src))
	if pr.File == nil {
		t.Fatal("main.bot does not parse")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("main.bot does not compile")
	}
	forgedAt, target := -1, ""
	var i int
	var after []string
	for _, e := range cr.Workflow.Edges {
		if e.From != "lot_gate" {
			continue
		}
		if e.Condition == "forged" && !e.Negated {
			forgedAt, target = i, e.To
		} else if forgedAt < 0 {
			after = append(after, e.To)
		}
		i++
	}
	if forgedAt < 0 {
		t.Fatal("no `lot_gate -> ... when forged` edge: a provenance refusal would fall through to the repair loop")
	}
	for _, before := range after {
		switch before {
		case "gate_timeout", "oracle_environment", "contract_unreadable":
			// The other typed terminals may precede: each is its own wall.
		default:
			t.Fatalf("edge to %q is declared before the provenance refusal — the engine takes the first `when` that holds, so that verdict would route to %q first", before, before)
		}
	}
	node, ok := cr.Workflow.Nodes[target]
	if !ok {
		t.Fatalf("the provenance refusal targets %q, which is not a node", target)
	}
	fail, isFail := node.(*ir.FailNode)
	if !isFail {
		t.Fatalf("the provenance refusal must be terminal, targets a %v node", node.NodeKind())
	}
	if fail.Code == "" {
		t.Fatal("the provenance refusal must carry its own failure code: on the bare `fail` the operator reads the same FAIL_NODE a forged `done` produces")
	}
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
	// extend_verify runs on the repair loop's body, so it must NOT restore:
	// the loop re-enters the agent, and a restored identity there disarms
	// every pass after the first.
	t.Run("the identity stays set while the loop can still run", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		// The state extend_base leaves behind: the net's identity on the
		// workspace, and the marker holding the one it displaced.
		gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
		if err := os.WriteFile(filepath.Join(gitInNet(t, ws, "rev-parse", "--absolute-git-dir"),
			"iterion-extend-prev-identity"), []byte(`{"name": "t", "email": "t@example.com"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		act(t, ws, "extend@golden-master.iterion")
		runExtendVerify(t, ws, base, `[{"id": "E-L29-1"}]`)
		if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "extend@golden-master.iterion" {
			t.Fatalf("the identity was restored inside the loop body — pass 2's agent would commit as %q and identity_ok could never hold again", got)
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

// TestGoldenMasterExtendBaseRepairsALeakedIdentity pins the failure path:
// extend_base writes the net's identity into the workspace's local git config
// and only extend_verify restores it, so an LLM error, a budget stop or a
// cancel in between leaves the LOT committing under the net's name — and the
// next run would read that leak as the state to restore, making it permanent.
func TestGoldenMasterExtendBaseRepairsALeakedIdentity(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": []}`)
	// A previous extension died after setting the identity: the net's name is
	// on the workspace, and the marker holds the identity it displaced.
	gitInNet(t, ws, "config", "user.name", "golden-master extend")
	gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
	gitDir := gitInNet(t, ws, "rev-parse", "--absolute-git-dir")
	marker := filepath.Join(gitDir, "iterion-extend-prev-identity")
	if err := os.WriteFile(marker, []byte(`{"name": "t", "email": "t@example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendBase(t, ws)
	if res.PrevEmail != "t@example.com" || res.PrevName != "t" {
		t.Fatalf("the leak must be repaired BEFORE this run reads the identity it will restore: %+v", res)
	}
	if !strings.Contains(res.Notice, "did not restore") {
		t.Fatalf("a repaired leak must be said, not silently fixed: %q", res.Notice)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("this run must leave its own marker for the next one: %v", err)
	}
	if !strings.Contains(string(b), "t@example.com") {
		t.Fatalf("the marker must hold the repaired identity, not the leak: %s", b)
	}
	if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "extend@golden-master.iterion" {
		t.Fatalf("this run still commits under the net's identity, got %q", got)
	}
}

// runExtendRestore runs the subbot's terminal restore node against ws.
func runExtendRestore(t *testing.T, ws string) map[string]any {
	t.Helper()
	body := toolScript(t, "golden-master/extend.bot", "extend_restore")
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in extend_restore near %q", body[i:min(i+40, len(body))])
	}
	p := filepath.Join(t.TempDir(), "extend_restore.py")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", p).Output()
	if err != nil {
		t.Fatalf("extend_restore failed: %v (out %q)", err, out)
	}
	var res map[string]any
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("extend_restore output is not JSON: %v (%q)", uerr, out)
	}
	return res
}

// TestGoldenMasterExtendVerifyNamesTheRightCauseForAReportFile pins the
// production finding: a subbot that REFUSES everything and writes its refusal
// to a file has acted nothing, and calling that "smuggling" sends the operator
// hunting for an act nobody made.
func TestGoldenMasterExtendVerifyNamesTheRightCauseForAReportFile(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	write := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitInNet(t, ws, "add", "-A")
		gitInNet(t, ws, "-c", "user.email=extend@golden-master.iterion", "-c", "user.name=x",
			"commit", "-qm", "refusal")
	}
	t.Run("acted nothing: the file is not an act that overreached", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		write(t, ws, ".modernize/E-1-refusal.md", "the request needs judge code\n")
		res := runExtendVerify(t, ws, base, `[{"id": "E-1"}]`)
		if res.ScopeClean {
			t.Fatalf("a path outside the surface must still refuse: %+v", res)
		}
		if !strings.Contains(res.LogTail, "acted NOTHING on the surface") ||
			strings.Contains(res.LogTail, "may write through it") {
			t.Fatalf("the reason must name the real situation, not smuggling: %q", res.LogTail)
		}
	})
	t.Run("acted on the surface too: the boundary is stated as smuggling", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("STATUS 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		write(t, ws, ".modernize/E-1-note.md", "and a file beside it\n")
		res := runExtendVerify(t, ws, base, `[{"id": "E-1"}]`)
		if res.ScopeClean || !strings.Contains(res.LogTail, "may write through it") {
			t.Fatalf("an act PLUS an outside path is the smuggling case: %+v", res)
		}
	})
}

// TestGoldenMasterExtendRestoreReturnsTheIdentityOnce pins the terminal node:
// the identity comes back on the way OUT of the subbot — once, from the
// marker, idempotently — instead of at the end of every verify pass, which is
// inside the repair loop's body.
func TestGoldenMasterExtendRestoreReturnsTheIdentityOnce(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": []}`)
	gitInNet(t, ws, "config", "user.name", "golden-master extend")
	gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
	marker := filepath.Join(gitInNet(t, ws, "rev-parse", "--absolute-git-dir"), "iterion-extend-prev-identity")
	if err := os.WriteFile(marker, []byte(`{"name": "t", "email": "t@example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendRestore(t, ws)
	if res["restored"] != true {
		t.Fatalf("the identity was not restored: %+v", res)
	}
	if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "t@example.com" {
		t.Fatalf("the lot's later commits would still wear the net's name, got %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the marker survived: the next run would repair a leak that is not there (%v)", err)
	}
	// Idempotent: nothing to restore is said, not guessed at.
	again := runExtendRestore(t, ws)
	if again["restored"] != false || !strings.Contains(again["notice"].(string), "no identity marker") {
		t.Fatalf("a second restore must be a stated no-op: %+v", again)
	}
}

// TestGoldenMasterExtendRestoreIsOnEveryWayOut pins the WIRING the finding
// named: the repair loop re-enters the agent, so a restore anywhere in the
// loop body disarms the identity for every pass after the first. Every
// non-loop edge out of the gate must reach the restore.
func TestGoldenMasterExtendRestoreIsOnEveryWayOut(t *testing.T) {
	const rel = "golden-master/extend.bot"
	src, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(rel, string(src))
	if pr.File == nil {
		t.Fatal("extend.bot does not parse")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("extend.bot does not compile")
	}
	seen := 0
	for _, e := range cr.Workflow.Edges {
		if e.From != "extend_gate" {
			continue
		}
		if e.LoopName != "" {
			continue // the repair pass, which must NOT restore
		}
		seen++
		if e.To != "extend_restore" {
			t.Fatalf("edge extend_gate -> %q leaves the subbot without restoring the identity it set", e.To)
		}
	}
	if seen < 2 {
		t.Fatalf("expected both exits of the gate (converged and exhausted), saw %d", seen)
	}
	for _, e := range cr.Workflow.Edges {
		if e.From == "extend_restore" && e.To != "extend_result" {
			t.Fatalf("extend_restore -> %q: the restore must sit between the gate and the report", e.To)
		}
	}

	// The refusal the subbot states must travel with the verdict, or an agent
	// told to "report in summary" writes a file to make its reasoning survive
	// — and its own gate then refuses the file (production finding).
	node, ok := cr.Workflow.Nodes["extend_result"].(*ir.ComputeNode)
	if !ok {
		t.Fatal("extend_result is not a compute node")
	}
	var notice string
	for _, ex := range node.Exprs {
		if ex.Key == "notice" {
			notice = ex.Raw
		}
	}
	if notice == "" {
		t.Fatal("extend_result publishes no notice")
	}
	for _, want := range []string{"outputs.extend_campaign.summary", "outputs.extend_gate.fail_log"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the published notice drops %s — the subbot's stated cause and the gate's deterministic verdict travel together, never one without the other: %s", want, notice)
		}
	}
}
