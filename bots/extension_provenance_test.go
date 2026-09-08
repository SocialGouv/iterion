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
	gitlib "github.com/SocialGouv/iterion/pkg/git"
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
	cmd := exec.Command("git", gitlib.NoAutoMaintenance(full...)...)
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

// TestGoldenMasterExtendBaseSeesQuotedAndRenamedDirt pins the refusal against
// the shapes git does not print plainly. `--porcelain` QUOTES a path with a
// non-ASCII byte, so a raw prefix match read a planted reference as a clean
// net — and the absorption attack the refusal exists to stop needs exactly
// one accented file name. A rename prints its origin as a second token, and a
// reference moved OUT of the net dirties it as surely as one moved in.
func TestGoldenMasterExtendBaseSeesQuotedAndRenamedDirt(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	const pending = `{"pending": [{"id": "E-1", "lot": "L"}]}`

	t.Run("a planted reference with a non-ASCII name", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "é.txt"),
			[]byte("planted by the lot\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runExtendBase(t, ws)
		if res.Clean || len(res.Pending) != 0 || !strings.HasPrefix(res.Notice, "REFUSED") {
			t.Fatalf("a quoted path read as a clean net — the absorption attack is open: %+v", res)
		}
	})
	t.Run("a status git cannot read is not a clean net", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		// The repository made unreadable: git exits non-zero, and whether
		// the net is clean becomes unknown. Every other unknown in this
		// feature fails closed; this one used to fail OPEN, granting exactly
		// the window the refusal exists to close.
		if err := os.Rename(filepath.Join(ws, ".git"), filepath.Join(ws, ".git-moved")); err != nil {
			t.Fatal(err)
		}
		res := runExtendBase(t, ws)
		if res.Clean || len(res.Pending) != 0 {
			t.Fatalf("an unreadable status read as a clean net: %+v", res)
		}
		if !strings.Contains(res.Notice, "UNKNOWN") {
			t.Fatalf("the refusal must name the unknown: %q", res.Notice)
		}
	})
	t.Run("a reference renamed OUT of the net", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		gitInNet(t, ws, "mv", ".golden-master/refs/001.txt", "moved-away.txt")
		res := runExtendBase(t, ws)
		if res.Clean || !strings.HasPrefix(res.Notice, "REFUSED") {
			t.Fatalf("a reference moved out of the net left it reading clean: %+v", res)
		}
	})
	t.Run("dirt outside the net is still only SAID", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		if err := os.WriteFile(filepath.Join(ws, "élan.txt"), []byte("the lot's own work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runExtendBase(t, ws)
		if !res.Clean || len(res.Pending) != 1 {
			t.Fatalf("a quoted path OUTSIDE the net must not refuse the run: %+v", res)
		}
		if !strings.Contains(res.Notice, "already dirty") {
			t.Fatalf("outside dirt must still be said: %q", res.Notice)
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
	res := func(t *testing.T, ws, base string) extendVerifyOut {
		t.Helper()
		return runExtendVerify(t, ws, base, `[{"id": "E-L29-1"}]`)
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
	// Two certified paths, one of them carrying a SPACE: the certificate must
	// come back as one entry per LINE. Space-joined, those two entries are
	// indistinguishable from four tokens, and the reader drops the halves —
	// which is how a certified reference loses its content binding and can be
	// rewritten later while keeping its exemption.
	t.Run("the certificate is one entry per line, spaces and all", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		spaced := ".golden-master/refs/a b.txt"
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(spaced)), []byte("STATUS 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		act(t, ws, "extend@golden-master.iterion")
		lines := strings.Split(res(t, ws, base).ActedBlobs, "\n")
		if len(lines) != 2 {
			t.Fatalf("two certified paths must publish two lines, got %q", lines)
		}
		seen := map[string]bool{}
		for _, l := range lines {
			p := l[:strings.LastIndex(l, "=")]
			seen[p] = true
		}
		if !seen[spaced] || !seen[".golden-master/refs/002.txt"] {
			t.Fatalf("a path with a space did not survive the encoding: %q", lines)
		}
	})
	// The same thing driven through the real tool: an ambient identity is
	// REPORTED and the provenance is published all the same, so the acts stay
	// certifiable. Reported, not refused.
	t.Run("an ambient git identity is said, and certifies all the same", func(t *testing.T) {
		ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
		cmd := exec.Command("git", gitlib.NoAutoMaintenance("-C", ws, "add", "-A")...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("STATUS 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(ws, ".golden-master", "EXTENSIONS.md"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, werr := f.WriteString("<!-- iterion:extension-act\n{\"id\": \"E-L29-1\", \"lot\": \"L29\", \"recorded_paths\": [\".golden-master/refs/002.txt\"]}\n-->\n"); werr != nil {
			t.Fatal(werr)
		}
		f.Close()
		if out, aerr := cmd.CombinedOutput(); aerr != nil {
			t.Fatalf("git add: %v (%s)", aerr, out)
		}
		// The environment wins over `-c user.email`, which is the whole point.
		commit := exec.Command("git", gitlib.NoAutoMaintenance("-C", ws,
			"-c", "user.email=extend@golden-master.iterion",
			"-c", "user.name=x", "commit", "-qm", "act")...)
		commit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_EMAIL=ambient@host", "GIT_COMMITTER_EMAIL=ambient@host",
			"GIT_AUTHOR_NAME=ambient", "GIT_COMMITTER_NAME=ambient")
		if out, cerr := commit.CombinedOutput(); cerr != nil {
			t.Fatalf("git commit: %v (%s)", cerr, out)
		}
		res := runExtendVerify(t, ws, base, `[{"id": "E-L29-1"}]`)
		if res.IdentityOk {
			t.Fatal("an ambient identity must be reported, not hidden")
		}
		if res.ActedCommits == "" || res.ActedBlobs == "" {
			t.Fatalf("the provenance must still be published: the lock is the commit set, not the author: %+v", res)
		}
		if !strings.Contains(res.LogTail, "attribution only") {
			t.Fatalf("the report must say what the difference costs: %q", res.LogTail)
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

// TestGoldenMasterExtendBaseRefusesAnUnreadableIdentityMarker pins the one
// unknown of this feature that failed OPEN. The marker is the DURABLE record
// of the identity a previous extension displaced, and a run that reads it
// unreadable is exactly the run whose local config already holds the NET's
// name. Reading that config as "the previous identity" recorded the LEAK as
// the thing to restore — extend_restore then put it back, the operator's
// checkout committed under the net's name for good, and the one record that
// could have repaired it had just been deleted on the way past.
func TestGoldenMasterExtendBaseRefusesAnUnreadableIdentityMarker(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	const pending = `{"pending": [{"id": "E-L29-1", "lot": "L29"}]}`
	for _, tc := range []struct {
		name, body string
	}{
		{"a truncated marker", `{"name": "t", "ema`},
		{"a marker that is not an identity object", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, _ := extendVerifyRepo(t, verdict, pending)
			// The state the repair exists for: the net's name on the
			// workspace, and a marker that cannot say what it displaced.
			gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
			marker := filepath.Join(gitInNet(t, ws, "rev-parse", "--absolute-git-dir"),
				"iterion-extend-prev-identity")
			if err := os.WriteFile(marker, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			res := runExtendBase(t, ws)
			if res.Clean || len(res.Pending) != 0 {
				t.Fatalf("an unreadable marker read as a startable net: %+v", res)
			}
			if !strings.HasPrefix(res.Notice, "REFUSED") || !strings.Contains(res.Notice, marker) {
				t.Fatalf("the refusal must name the record it could not read: %q", res.Notice)
			}
			// The two halves that made the leak permanent, neither taken:
			// the record survives, and the identity is not mutated on top
			// of it. (`user.name` is the oracle: the fixture leaked only
			// the email, so the net's name appearing here can only have
			// come from this node writing it.)
			b, err := os.ReadFile(marker)
			if err != nil || string(b) != tc.body {
				t.Fatalf("the only record of the operator's identity was destroyed: %s (%v)", b, err)
			}
			if got := gitInNet(t, ws, "config", "--get", "user.name"); got != "t" {
				t.Fatalf("a refused start must not touch the identity, got %q", got)
			}
		})
	}
}

// TestGoldenMasterExtendBaseRefusesAnIdentityRepairGitDidNotTake pins the same
// rule one branch over: the marker parses, so the repair runs — and `git
// config` fails. Its exit code was dropped, so the workspace kept the net's
// name, the `prev_email` read just below recorded THAT as the operator's
// identity, and the marker that could have repaired it was deleted on the way
// past. The unreadable-marker case refuses for exactly this outcome; a repair
// that silently did not happen must reach the same door.
func TestGoldenMasterExtendBaseRefusesAnIdentityRepairGitDidNotTake(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": [{"id": "E-L29-1", "lot": "L29"}]}`)
	gitInNet(t, ws, "config", "user.name", "golden-master extend")
	gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
	gitDir := gitInNet(t, ws, "rev-parse", "--absolute-git-dir")
	marker := filepath.Join(gitDir, "iterion-extend-prev-identity")
	const body = `{"name": "t", "email": "t@example.com"}`
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The failure, taken from git's own vocabulary rather than simulated: a
	// stale lock is what a killed `git config` leaves behind, and it refuses
	// every write to the config while it stands — whoever the test runs as.
	if err := os.WriteFile(filepath.Join(gitDir, "config.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendBase(t, ws)
	if res.Clean || len(res.Pending) != 0 {
		t.Fatalf("a workspace this run could not repair read as a startable net: %+v", res)
	}
	if !strings.HasPrefix(res.Notice, "REFUSED") || !strings.Contains(res.Notice, marker) {
		t.Fatalf("the refusal must name the record it could not apply: %q", res.Notice)
	}
	// The half that made the leak permanent, not taken: the marker still
	// holds the operator's identity, where a run that had gone on would have
	// overwritten it with the net's own name — the record and the repair, both
	// gone in one pass. (What the refused report SAYS about `prev_*` is inert:
	// no node reads those fields, the marker is what the next run repairs
	// from.)
	if b, err := os.ReadFile(marker); err != nil || string(b) != body {
		t.Fatalf("the only record of the operator's identity was destroyed: %s (%v)", b, err)
	}
}

// The THIRD site of the class the two above close. `extend_base` SETS the
// net's identity, and that `git config` dropped its exit code too: on a
// workspace where the write cannot take, the run went on committing under
// the OPERATOR's identity while every line it published claimed the net's —
// the attribution the whole marker dance exists to make legible, wrong and
// silent. A guard on two of three sites is a guard the third walks past.
func TestGoldenMasterExtendBaseRefusesAnIdentityGitWouldNotTake(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": [{"id": "E-L29-1", "lot": "L29"}]}`)
	gitDir := gitInNet(t, ws, "rev-parse", "--absolute-git-dir")
	marker := filepath.Join(gitDir, "iterion-extend-prev-identity")
	// No marker: nothing was displaced before this run, which is the shape
	// that makes the dropped exit code invisible — there is no repair path
	// to trip over it later.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the fixture must start with no marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendBase(t, ws)
	if res.Clean || len(res.Pending) != 0 {
		t.Fatalf("a net whose identity never took read as startable: %+v", res)
	}
	if !strings.HasPrefix(res.Notice, "REFUSED") {
		t.Fatalf("the refusal must be stated, not implied: %q", res.Notice)
	}
	// The marker goes with the refusal: nothing was displaced, so leaving one
	// behind makes the NEXT run read a leak that never happened and "restore"
	// over a live identity.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a marker survived a refusal that displaced nothing: %v", err)
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

// TestGoldenMasterExtendRestoreKeepsAnUnreadableMarker pins the other end of
// the same rule: the marker is removed only once the identity it held is
// BACK. A marker this node could not read is the last remaining record of
// what the operator's identity was, on a workspace still wearing the net's —
// deleting it there ends the repair for good, where keeping it makes the next
// extend_base refuse and say so.
func TestGoldenMasterExtendRestoreKeepsAnUnreadableMarker(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": []}`)
	gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
	marker := filepath.Join(gitInNet(t, ws, "rev-parse", "--absolute-git-dir"), "iterion-extend-prev-identity")
	const corrupt = `{"name": "t", "ema`
	if err := os.WriteFile(marker, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendRestore(t, ws)
	if res["restored"] != false || !strings.Contains(res["notice"].(string), "unreadable") {
		t.Fatalf("an unreadable marker must be said, never reported restored: %+v", res)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != corrupt {
		t.Fatalf("the record the next run repairs from was deleted: %s (%v)", b, err)
	}
}

// TestGoldenMasterExtendRestoreKeepsAMarkerGitDidNotHonour pins the third case
// of that same rule: the marker READS, so the restore runs — and `git config`
// fails. Its exit code was dropped, so the node reported `restored: true` and
// deleted the last record of the operator's identity on a workspace still
// wearing the net's. "Back" is what git says, not what this node asked for.
func TestGoldenMasterExtendRestoreKeepsAMarkerGitDidNotHonour(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, _ := extendVerifyRepo(t, verdict, `{"pending": []}`)
	gitInNet(t, ws, "config", "user.name", "golden-master extend")
	gitInNet(t, ws, "config", "user.email", "extend@golden-master.iterion")
	gitDir := gitInNet(t, ws, "rev-parse", "--absolute-git-dir")
	marker := filepath.Join(gitDir, "iterion-extend-prev-identity")
	const body = `{"name": "t", "email": "t@example.com"}`
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExtendRestore(t, ws)
	if res["restored"] != false {
		t.Fatalf("a restore git did not honour was reported done: %+v", res)
	}
	if n, _ := res["notice"].(string); !strings.Contains(n, "could NOT be put back") ||
		!strings.Contains(n, marker) {
		t.Fatalf("the notice must name the record and the failure: %q", res["notice"])
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != body {
		t.Fatalf("the record the next run repairs from was deleted: %s (%v)", b, err)
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

	// A deterministic refusal must not buy an LLM pass. `clean` is read once
	// and carried unchanged, so the conjunct it feeds is false forever: the
	// campaign edge would pay a full agent run, then max_passes more of them,
	// to report what node 1 already knew — and would hand the workspace to an
	// agent on a start that was refused.
	var refusedAt, campaignAt = -1, -1
	for i, e := range cr.Workflow.Edges {
		if e.From != "extend_base" {
			continue
		}
		switch e.To {
		case "extend_refused":
			refusedAt = i
			if e.Condition != "clean" || !e.Negated {
				t.Fatalf("the refusal edge must be `when not clean`, got condition=%q negated=%v", e.Condition, e.Negated)
			}
		case "extend_campaign":
			campaignAt = i
		}
	}
	if refusedAt < 0 {
		t.Fatal("no `extend_base -> extend_refused when not clean` edge: a refused start still buys an agent pass")
	}
	if campaignAt >= 0 && campaignAt < refusedAt {
		t.Fatal("the campaign edge is declared before the refusal: the engine takes the first `when` that holds, and an unconditional edge is only the fallback — but declaration order is what the reader checks")
	}
	if _, ok := cr.Workflow.Nodes["extend_refused"]; !ok {
		t.Fatal("extend_refused is not a node")
	}

	// Attribution is EVIDENCE, never a term of convergence. Git's identity
	// ENV outranks every config source and the agent cannot change the
	// process environment, so a conjunct on it is unsatisfiable wherever a
	// host exports it — the subbot would burn max_passes campaigns against a
	// wall no pass can move. The lock is the commit set; this PR's own
	// doctrine says the author is prose.
	gate, ok := cr.Workflow.Nodes["extend_gate"].(*ir.ComputeNode)
	if !ok {
		t.Fatal("extend_gate is not a compute node")
	}
	for _, ex := range gate.Exprs {
		if ex.Key == "converged" && strings.Contains(ex.Raw, "identity_ok") {
			t.Fatalf("identity_ok is a conjunct of convergence: an ambient GIT_AUTHOR_EMAIL would make it unsatisfiable, and no pass of the agent can move it — %s", ex.Raw)
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
	// Read from extend_verify, not from the agent node: a summary the agent
	// omitted renders null, and `string + null` is a typed error that would
	// fail the compute and take down a run that was only reporting a refusal.
	// The tool node makes it a string once.
	for _, want := range []string{"outputs.extend_verify.agent_summary", "outputs.extend_gate.fail_log"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the published notice drops %s — the subbot's stated cause and the gate's deterministic verdict travel together, never one without the other: %s", want, notice)
		}
	}
}

// TestGoldenMasterExtendVerifyCertifiesNothingOnARefusedStart pins the second
// half of the absorption refusal. extend_base refuses a dirty net by handing
// the agent no pending request — prose, which the agent may ignore — while
// this node published the run's commits as the net subbot's provenance
// regardless. The lot's route: leave the reference and its act block
// uncommitted, commit only the request so it routes here, and let the
// subbot's tidy-up commit introduce the act inside `acted_commits`.
func TestGoldenMasterExtendVerifyCertifiesNothingOnARefusedStart(t *testing.T) {
	const verdict = `{"acted": [], "ok_paths": [], "ledger_append_only": True, "requests_added": 0, "problems": []}`
	ws, base := extendVerifyRepo(t, verdict, `{"pending": []}`)
	// Whatever gets committed in that window — here, the very content that
	// made the net dirty.
	if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("the lot's own file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInNet(t, ws, "add", "-A")
	gitInNet(t, ws, "-c", "user.email=extend@golden-master.iterion", "-c", "user.name=x",
		"commit", "-qm", "tidy-up")

	clean := runExtendVerifyClean(t, ws, base, `[{"id": "E-1"}]`, true)
	if clean.ActedCommits == "" {
		t.Fatal("fixture is wrong: a CLEAN start must publish the run's commits")
	}
	refused := runExtendVerifyClean(t, ws, base, `[{"id": "E-1"}]`, false)
	if refused.ActedCommits != "" || refused.ActedIds != "" || refused.ActedBlobs != "" {
		t.Fatalf("a refused start certified something: %+v", refused)
	}
	if refused.CleanStart {
		t.Fatalf("clean_start must carry the refusal: %+v", refused)
	}
}

// TestHarnessReadsTheCertificateOneEntryPerLine pins the parser against the
// encoding, end to end: a surface path with a SPACE (both ids and paths are
// lot-authored, and their guards reject slashes and dots, not whitespace) used
// to split into two tokens — the left dropped, the right a bogus key — so the
// real path had no certified blob and the post-act rewrite check never fired
// for it. An entry the judge cannot read is now refused, not dropped.
func TestHarnessReadsTheCertificateOneEntryPerLine(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, env ...string) (map[string]any, int) {
		t.Helper()
		ws := t.TempDir()
		gm := filepath.Join(ws, ".golden-master")
		if merr := os.MkdirAll(gm, 0o755); merr != nil {
			t.Fatal(merr)
		}
		g := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", gitlib.NoAutoMaintenance(append([]string{"-C", ws}, args...)...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			if out, gerr := cmd.CombinedOutput(); gerr != nil {
				t.Fatalf("git %v: %v (%s)", args, gerr, out)
			}
		}
		g("init", "-q", "-b", "main")
		g("config", "user.email", "t@t")
		g("config", "user.name", "t")
		if werr := os.WriteFile(filepath.Join(gm, "corpus.json"), []byte(`{"entries": []}`), 0o644); werr != nil {
			t.Fatal(werr)
		}
		g("add", "-A")
		g("commit", "-qm", "base")
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(append(os.Environ(),
			"GM_MODE=extend-verify", "GM_WORKSPACE="+ws, "GM_DIR=.golden-master",
			"GM_BASE=HEAD"), env...)
		out, _ := cmd.Output()
		exit := 0
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
		var v map[string]any
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); uerr != nil {
			t.Fatalf("no verdict: %v (out %q)", uerr, out)
		}
		return v, exit
	}

	t.Run("a path with a space keeps its certificate", func(t *testing.T) {
		v, exit := run(t, "GM_ACTED_COMMITS=", "GM_ACTED_IDS=E 1",
			"GM_ACTED_BLOBS=.golden-master/refs/a b.txt=1111111111111111111111111111111111111111")
		if exit != 0 || v["error"] != nil {
			t.Fatalf("a legal path was refused: exit %d %v", exit, v["error"])
		}
		// The proof the entry survived whole: `provenance` says the strict
		// rule ran, and nothing was dropped on the floor.
		if v["provenance"] != "strict" {
			t.Fatalf("the provenance was not honoured: %v", v)
		}
	})
	// The ids carry the same defect as the blobs, and it is the one Revi's
	// finding did not name: an id with a space split OUT of the set, so the
	// act it covers silently lost the content rule's protection. Driven
	// through a real ledger, because only an act that NEEDS the hatch can
	// show whether its id survived the parser.
	t.Run("an act id with a space keeps its cover", func(t *testing.T) {
		ws := t.TempDir()
		gm := filepath.Join(ws, ".golden-master")
		if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
		g := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", gitlib.NoAutoMaintenance(append([]string{"-C", ws}, args...)...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			out, gerr := cmd.CombinedOutput()
			if gerr != nil {
				t.Fatalf("git %v: %v (%s)", args, gerr, out)
			}
			return strings.TrimSpace(string(out))
		}
		g("init", "-q", "-b", "main")
		g("config", "user.email", "t@t")
		g("config", "user.name", "t")
		if err := os.WriteFile(filepath.Join(gm, "corpus.json"), []byte(`{"entries": []}`), 0o644); err != nil {
			t.Fatal(err)
		}
		g("add", "-A")
		g("commit", "-qm", "base")
		base := g("rev-parse", "HEAD")
		ledger := func(blocks string) {
			if err := os.WriteFile(filepath.Join(gm, "EXTENSIONS.md"), []byte(blocks), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		const id = "E 1"
		req := "<!-- iterion:extension-request\n" + `{"id": "` + id + `", "lot": "L", "type": "add-file", "paths": [".golden-master/refs/2.txt"]}` + "\n-->\n"
		ledger(req)
		g("add", "-A")
		g("commit", "-qm", "the lot files it")
		ledger(req + "<!-- iterion:extension-act\n" + `{"id": "` + id + `", "lot": "L", "recorded_paths": [".golden-master/refs/2.txt"]}` + "\n-->\n")
		if err := os.WriteFile(filepath.Join(gm, "refs", "2.txt"), []byte("STATUS 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g("add", "-A")
		g("commit", "-qm", "acted")
		blob := g("rev-parse", "HEAD:.golden-master/refs/2.txt")

		// Strict provenance, no subbot commit reported: only the CONTENT rule
		// can cover this act, and only if its id survived the parser.
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_MODE=extend-verify", "GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master", "GM_BASE="+base, "GM_ACTED_COMMITS=",
			"GM_ACTED_IDS="+id,
			"GM_ACTED_BLOBS=.golden-master/refs/2.txt="+blob)
		out, _ := cmd.Output()
		var v struct {
			Acted []struct {
				OK     bool `json:"ok"`
				Forged bool `json:"forged"`
			} `json:"acted"`
		}
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); uerr != nil {
			t.Fatalf("no verdict: %v (%q)", uerr, out)
		}
		if len(v.Acted) != 1 || !v.Acted[0].OK || v.Acted[0].Forged {
			t.Fatalf("an id with a space lost its cover — the parser split it out of the set: %+v", v.Acted)
		}
	})
	// The twin of the test above, and its opposite: an id must not inherit the
	// cover of one that merely LOOKS like it. The reader normalised each line
	// while the verdict compares the id verbatim, so two ids differing only by
	// surrounding whitespace collapsed into one — and the act the subbot
	// REFUSED came back covered by the id it had acted. That is a refused
	// extension converging, reopened by an encoding.
	t.Run("a near-identical id does not inherit the cover of the acted one", func(t *testing.T) {
		ws, base, blob := extensionLedgerRepo(t, "E-2")
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_MODE=extend-verify", "GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master", "GM_BASE="+base, "GM_ACTED_COMMITS=",
			// The subbot acted " E-2". The ledger's act is "E-2" — a DIFFERENT
			// id, written by the constrained party in its own commit.
			"GM_ACTED_IDS= E-2",
			"GM_ACTED_BLOBS=.golden-master/refs/2.txt="+blob)
		out, _ := cmd.Output()
		var v struct {
			Acted []struct {
				OK     bool `json:"ok"`
				Forged bool `json:"forged"`
			} `json:"acted"`
		}
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); uerr != nil {
			t.Fatalf("no verdict: %v (%q)", uerr, out)
		}
		if len(v.Acted) != 1 || !v.Acted[0].Forged {
			t.Fatalf("the lot's own act rode the certificate of a DIFFERENT id — "+
				"the reader normalised what the verdict compares raw: %+v", v.Acted)
		}
	})
	// The other shape the encoding leaves open: an id carrying a line break
	// publishes as TWO lines, and the reader seeds the covered set with a
	// fragment no subbot ever acted. It cannot round-trip, so it is refused at
	// the parse point rather than repaired.
	t.Run("an id carrying a line break is refused, not split into fragments", func(t *testing.T) {
		ws, base, blob := extensionLedgerRepo(t, "E\n2")
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_MODE=extend-verify", "GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master", "GM_BASE="+base, "GM_ACTED_COMMITS=",
			"GM_ACTED_IDS=E\n2",
			"GM_ACTED_BLOBS=.golden-master/refs/2.txt="+blob)
		out, _ := cmd.Output()
		var v struct {
			Problems []string `json:"problems"`
		}
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); uerr != nil {
			t.Fatalf("no verdict: %v (%q)", uerr, out)
		}
		if !strings.Contains(strings.Join(v.Problems, " | "), "one-id-per-line") {
			t.Fatalf("an id that cannot survive the line transport was accepted: %+v", v.Problems)
		}
	})
	// Containment: refusing the poisoned block is only half the defence. While
	// it was merely REPORTED, the readable blocks beside it went on exempting
	// their paths — so a ledger that escalates still handed out the exemption,
	// and any caller reading ok_paths without also reading problems never saw
	// the escalation at all.
	t.Run("an unreadable block stops the ledger certifying anything beside it", func(t *testing.T) {
		poison := "<!-- iterion:extension-act\n" +
			`{"id": "E\n9", "lot": "L", "recorded_paths": [".golden-master/refs/2.txt"]}` +
			"\n-->\n"
		ws, base, blob := extensionLedgerRepo(t, "E-2", poison)
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_MODE=extend-verify", "GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master", "GM_BASE="+base, "GM_ACTED_COMMITS=",
			// The subbot really did act E-2 and certify its blob: without the
			// poisoned block beside it, this act would be covered.
			"GM_ACTED_IDS=E-2",
			"GM_ACTED_BLOBS=.golden-master/refs/2.txt="+blob)
		out, _ := cmd.Output()
		var v struct {
			OKPaths  []string `json:"ok_paths"`
			Problems []string `json:"problems"`
			Acted    []struct {
				OK bool `json:"ok"`
			} `json:"acted"`
		}
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); uerr != nil {
			t.Fatalf("no verdict: %v (%q)", uerr, out)
		}
		if len(v.Problems) == 0 {
			t.Fatalf("the unreadable block was not escalated at all: %+v", v)
		}
		if len(v.OKPaths) != 0 {
			t.Fatalf("a ledger the judge cannot read still exempted paths: %+v", v.OKPaths)
		}
		for _, a := range v.Acted {
			if a.OK {
				t.Fatalf("an act was certified from a ledger carrying an unreadable block: %+v", v.Acted)
			}
		}
	})
	t.Run("an entry the judge cannot read is refused, not dropped", func(t *testing.T) {
		v, exit := run(t, "GM_ACTED_COMMITS=", "GM_ACTED_BLOBS=no-equals-sign-here")
		if exit == 0 {
			t.Fatalf("an unreadable certificate passed: %v", v)
		}
		msg, _ := v["error"].(string)
		if !strings.Contains(msg, "not `path=blob`") {
			t.Fatalf("the refusal must name what it could not read: %q", msg)
		}
	})
}

// extensionLedgerRepo builds the smallest repo an extension verdict can judge:
// a base commit, a request filed by the lot, then an act answering it and the
// reference it records — both introduced by the LOT's own commit, so only the
// content rule can ever cover the act. Returns the workspace, the base sha and
// the blob of the recorded reference. The id is marshalled, never interpolated:
// a test whose id carries a line break must produce a LEGAL JSON block, or it
// would prove the escape rather than the guard.
func extensionLedgerRepo(t *testing.T, id string, extraBlocks ...string) (ws, base, blob string) {
	t.Helper()
	ws = t.TempDir()
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", gitlib.NoAutoMaintenance(append([]string{"-C", ws}, args...)...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, gerr := cmd.CombinedOutput()
		if gerr != nil {
			t.Fatalf("git %v: %v (%s)", args, gerr, out)
		}
		return strings.TrimSpace(string(out))
	}
	g("init", "-q", "-b", "main")
	g("config", "user.email", "t@t")
	g("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(gm, "corpus.json"), []byte(`{"entries": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-qm", "base")
	base = g("rev-parse", "HEAD")

	jid, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	ledger := func(blocks string) {
		if werr := os.WriteFile(filepath.Join(gm, "EXTENSIONS.md"), []byte(blocks), 0o644); werr != nil {
			t.Fatal(werr)
		}
	}
	req := "<!-- iterion:extension-request\n" +
		`{"id": ` + string(jid) + `, "lot": "L", "type": "add-file", "paths": [".golden-master/refs/2.txt"]}` +
		"\n-->\n"
	// Extra blocks are appended to EVERY revision of the ledger, so a block
	// that cannot be read stands beside the readable ones from the first
	// commit — an append-only trail, exactly as the judge requires.
	req += strings.Join(extraBlocks, "")
	ledger(req)
	g("add", "-A")
	g("commit", "-qm", "the lot files it")
	ledger(req + "<!-- iterion:extension-act\n" +
		`{"id": ` + string(jid) + `, "lot": "L", "recorded_paths": [".golden-master/refs/2.txt"]}` +
		"\n-->\n")
	if err := os.WriteFile(filepath.Join(gm, "refs", "2.txt"), []byte("STATUS 200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-qm", "acted")
	return ws, base, g("rev-parse", "HEAD:.golden-master/refs/2.txt")
}
