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
	return runExtendBaseIn(t, ws, ".golden-master")
}

// runExtendBaseIn is runExtendBase with the operator's own spelling of
// `oracle_dir` — the var whose value decides which paths the refusal watches.
func runExtendBaseIn(t *testing.T, ws, oracleDir string) extendBaseOut {
	t.Helper()
	body := toolScript(t, "golden-master/extend.bot", "extend_base")
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.oracle_dir}}", strconv.Quote(oracleDir))
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

// gitInNet runs git in the fixture with the AMBIENT identity dropped. The
// tests below assert who authored a commit, and they say it with
// `-c user.email=…` — which is CONFIG, and `GIT_AUTHOR_EMAIL` outranks
// config. Every forge runner exports one (this repository's own does), so
// without this filter the fixture commits under the host's name and
// `identity_ok` reads false: a red that is the machine, not the code.
func gitInNet(t *testing.T, ws string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", ws}, args...)
	cmd := exec.Command("git", full...)
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_AUTHOR_") || strings.HasPrefix(kv, "GIT_COMMITTER_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
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
	// The three ways the refusal was walked through while reading CLEAN. Each
	// was reproduced against real git before the fix; each is the SAME
	// absorption — a planted reference the run then commits as the net's own —
	// wearing a spelling the prefix match could not see.
	for _, c := range []struct {
		name string
		dirt func(t *testing.T, ws string)
	}{{
		// `--porcelain` quotes a path with a non-ASCII byte, a `"`, a `\` or a
		// control char, so `l[3:]` starts with `"` and matches no prefix.
		"a reference planted under a non-ASCII name",
		func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "é.txt"), []byte("planted\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		// A staged rename reads `orig -> dest`: the DESTINATION — the side that
		// lands under the net — is invisible to a match on the whole line.
		"a tracked file renamed INTO the net",
		func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, "outside.txt"), []byte("the lot's own file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitInNet(t, ws, "add", "outside.txt")
			gitInNet(t, ws, "commit", "-qm", "a tracked file outside the net")
			gitInNet(t, ws, "mv", "outside.txt", ".golden-master/refs/moved.txt")
		},
	}, {
		// A status that cannot answer is not an answer that the net is clean.
		"git cannot report the state at all",
		func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, ".git", "index"), []byte("not an index"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}} {
		t.Run(c.name+": refused", func(t *testing.T) {
			ws, _ := extendVerifyRepo(t, verdict, pending)
			c.dirt(t, ws)
			res := runExtendBase(t, ws)
			if res.Clean || len(res.Pending) != 0 || !strings.HasPrefix(res.Notice, "REFUSED") {
				t.Fatalf("the net is dirty and the run read it clean — the absorption "+
					"this refusal exists to stop is reachable: %+v", res)
			}
			if got := gitInNet(t, ws, "config", "--get", "user.email"); got != "t@example.com" {
				t.Fatalf("a refused start must not touch the identity, got %q", got)
			}
		})
	}
	// `oracle_dir` is an operator var: a `./` spelling that matched nothing
	// would disarm the refusal wholesale rather than tighten it.
	t.Run("the net's prefix is normalised before it is matched", func(t *testing.T) {
		ws, _ := extendVerifyRepo(t, verdict, pending)
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "refs", "002.txt"), []byte("forged by the lot\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runExtendBaseIn(t, ws, "./.golden-master/")
		if res.Clean || !strings.HasPrefix(res.Notice, "REFUSED") {
			t.Fatalf("a ./-spelled net must refuse the same dirt: %+v", res)
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
		// The host's own identity, set hostile on purpose: GIT_AUTHOR_EMAIL
		// outranks the `-c user.email` the fixture commits with, so a fixture
		// that does not drop it reads the HOST as the author and identity_ok
		// goes false — a red that is the machine. Set here so the guard is
		// exercised on every host, not only the ones that export it.
		t.Setenv("GIT_AUTHOR_NAME", "hostile")
		t.Setenv("GIT_AUTHOR_EMAIL", "hostile@host")
		t.Setenv("GIT_COMMITTER_NAME", "hostile")
		t.Setenv("GIT_COMMITTER_EMAIL", "hostile@host")
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
			cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
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
			cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
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

// TestGoldenMasterHarnessSelftestSurvivesAHostIdentity pins the harness's own
// selftest — the gate that decides whether this harness may be synced into a
// target tree — against the host it runs on. Its extension fixture asserts WHO
// authored a commit, and it says so with `-c user.email=…`, which is config;
// `GIT_AUTHOR_EMAIL` outranks config and every forge runner exports one (this
// repository's own does). Unpinned, the fixture commits under the host's name
// and the provenance check reads a stranger: the sync bot then refuses forever,
// on a machine rather than on a defect. Measured red before the fix, on
// exactly this environment.
func TestGoldenMasterHarnessSelftestSurvivesAHostIdentity(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	cmd := exec.Command("python3", harness)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"GM_MODE=selftest", "GM_WORKSPACE="+ws, "GM_DIR=.golden-master",
		"GIT_AUTHOR_NAME=hostile", "GIT_AUTHOR_EMAIL=hostile@host",
		"GIT_COMMITTER_NAME=hostile", "GIT_COMMITTER_EMAIL=hostile@host")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the harness selftest is RED under a host identity — the sync gate "+
			"would refuse on the machine, not on the code: %v\n%s", err, out)
	}
}
