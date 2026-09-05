package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestGoldenMasterSyncHarnessBotCompiles: a sibling workflow is not a
// `main.bot`, so the catalog's parse/compile gates never see it — and
// sync-harness.py rewrites this file's block scalar on every regeneration.
// A graph that a fresh `iterion validate` would reject must fail here.
func TestGoldenMasterSyncHarnessBotCompiles(t *testing.T) {
	src := readHarnessFile(t, "golden-master/sync-harness.bot")
	pr := parser.Parse("golden-master/sync-harness.bot", src)
	for _, d := range pr.Diagnostics {
		t.Logf("parse diagnostic: %s", d.Error())
	}
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Errorf("compile: %s", d.Error())
		}
		t.FailNow()
	}
	for _, node := range []string{"sync_harness", "commit_harness", "gate_replay", "seal_commit"} {
		if _, ok := cr.Workflow.Nodes[node]; !ok {
			t.Errorf("node %s missing from the compiled graph", node)
		}
	}
}

// syncHarnessHeader is the canonical header the gate writes above the body —
// the sync bot must reproduce it byte for byte, or the next gate writes the
// file back and dirties the tree it was meant to leave clean.
const syncHarnessHeader = "#!/usr/bin/env python3\n" +
	"\"\"\"Materialised oracle harness — the decision procedure, not the campaign's to edit.\n" +
	"The reviewable source of truth lives in the golden-master bot bundle; this copy\n" +
	"exists so the emitted runner, CI and later passes judge with the same code.\n" +
	"Regenerated at every gate; edits made here do not survive.\"\"\"\n"

type syncHarnessOut struct {
	Changed       bool   `json:"changed"`
	Selftest      string `json:"selftest"`
	HarnessSHA256 string `json:"harness_sha256"`
	Notice        string `json:"notice"`
}

type syncGateOut struct {
	Passed  bool   `json:"passed"`
	Minutes int    `json:"minutes"`
	Command string `json:"command"`
	LogTail string `json:"log_tail"`
}

type syncCommitOut struct {
	Committed bool   `json:"committed"`
	Commit    string `json:"commit"`
	Notice    string `json:"notice"`
}

// syncHarnessRepo is a target tree: a git repo whose net carries an OLD
// materialised harness (the canonical header over a stale body) and the
// runner the net emits. The workspace IS the repository root — the common
// shape; syncHarnessRepoUnder builds the one where it is not.
func syncHarnessRepo(t *testing.T, oldBody string) (string, func(args ...string) string) {
	t.Helper()
	return syncHarnessRepoUnder(t, oldBody, "")
}

// syncHarnessRepoUnder puts the net (and the workspace the bot is pointed at)
// in `sub` of the repository, returning that subdirectory as the workspace —
// a monorepo package, or a run started below the root.
func syncHarnessRepoUnder(t *testing.T, oldBody, sub string) (string, func(args ...string) string) {
	t.Helper()
	root, git := syncHarnessGitRepo(t)
	ws := filepath.Join(root, sub)
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"harness.py":       syncHarnessHeader + oldBody,
		"verify-oracle.sh": "#!/bin/sh\nexit 0\n",
		"refs/001.txt":     "STATUS 200\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The helper is anchored at the repository root, so the pathspec is too.
	git("add", filepath.Join(sub, ".golden-master"))
	git("commit", "-qm", "net with a stale judge")
	return ws, git
}

// syncHarnessGitRepo is an empty target tree with one committed file, and a
// git helper that fails the test on any error.
func syncHarnessGitRepo(t *testing.T) (string, func(args ...string) string) {
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
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-qm", "target")
	return ws, git
}

// runSyncNode executes one of the bot's tool nodes as the engine would: the
// script body with its template refs bound, run by python3 from a scratch
// file (the driver reads its OWN source, exactly as it does under the engine).
// Optional mutators run on the bound script, for the refusals that are about
// the node's OWN source being mangled.
func runSyncNode(t *testing.T, node, ws string, inputs map[string]string, mutate ...func(string) string) ([]byte, int) {
	t.Helper()
	body := toolScript(t, "golden-master/sync-harness.bot", node)
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.oracle_dir}}", strconv.Quote(".golden-master"))
	body = strings.ReplaceAll(body, "{{vars.gate_cmd}}", strconv.Quote(inputs["gate_cmd"]))
	wall := inputs["gate_timeout_s"]
	if wall == "" {
		wall = "60"
	}
	body = strings.ReplaceAll(body, "{{vars.gate_timeout_s}}", wall)
	for k, v := range inputs {
		if k == "gate_minutes" {
			body = strings.ReplaceAll(body, "{{input."+k+"}}", v)
			continue
		}
		body = strings.ReplaceAll(body, "{{input."+k+"}}", strconv.Quote(v))
	}
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in %s near %q", node, body[i:min(i+40, len(body))])
	}
	for _, m := range mutate {
		body = m(body)
	}
	scriptPath := filepath.Join(t.TempDir(), node+".py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", scriptPath).Output()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s failed to execute: %v (out %q)", node, err, out)
		}
		exit = ee.ExitCode()
	}
	return out, exit
}

func syncHarness(t *testing.T, ws string) (syncHarnessOut, int) {
	t.Helper()
	out, exit := runSyncNode(t, "sync_harness", ws, nil)
	var res syncHarnessOut
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("sync_harness output is not JSON: %v (out %q)", err, out)
	}
	return res, exit
}

// TestGoldenMasterSyncHarnessBotMaterialisesTheCanonicalCopy pins the bot's
// one write: the target's harness becomes the canonical form of THIS
// bundle's harness (the same bytes the gate would write), the selftest ran on
// the written file, and nothing else moved. A second run is a no-op.
func TestGoldenMasterSyncHarnessBotMaterialisesTheCanonicalCopy(t *testing.T) {
	ws, git := syncHarnessRepo(t, "\nimport hashlib\n# a stale judge\n")

	res, exit := syncHarness(t, ws)
	if exit != 0 || !res.Changed {
		t.Fatalf("exit %d changed=%v: %s", exit, res.Changed, res.Notice)
	}
	if !strings.Contains(res.Selftest, "103") && !strings.Contains(strings.ToLower(res.Selftest), "pass") {
		t.Fatalf("selftest line does not read like the harness's own report: %q", res.Selftest)
	}
	got, err := os.ReadFile(filepath.Join(ws, ".golden-master", "harness.py"))
	if err != nil {
		t.Fatal(err)
	}
	standalone := readHarnessFile(t, "golden-master/oracle-harness.py")
	// The body the driver reads from its own script ends with the block
	// scalar's trailing blank line — one "\n" more than the standalone file.
	// That IS the canonical form: the gate composes its copy the same way,
	// and a sync that dropped the byte would be rewritten by the next gate.
	wantBody := standalone[strings.Index(standalone, "\nimport hashlib"):] + "\n"
	if string(got) != syncHarnessHeader+wantBody {
		t.Fatalf("the materialised harness is not header + the standalone body + the block scalar's blank line: %d bytes, want %d", len(got), len(syncHarnessHeader+wantBody))
	}
	if moved := git("status", "--porcelain", "--untracked-files=no"); moved != "M .golden-master/harness.py" { // the helper trims the leading column
		t.Fatalf("tree after the sync: %q, want the harness alone", moved)
	}

	t.Run("a second run is a no-op", func(t *testing.T) {
		git("commit", "-qam", "synced")
		res, exit := syncHarness(t, ws)
		if exit != 0 || res.Changed || !strings.HasPrefix(res.Notice, "IN_SYNC") {
			t.Fatalf("exit %d changed=%v notice=%q, want IN_SYNC", exit, res.Changed, res.Notice)
		}
	})
}

// TestGoldenMasterSyncHarnessBotNetBelowTheRepositoryRoot: git prints porcelain
// paths relative to the REPOSITORY root, not to workspace_dir, and `git -C` does
// not change that. A target whose net sits below the root — a monorepo package,
// a run started from a subdirectory — must sync and commit exactly as one whose
// workspace IS the root, not refuse naming the one file that was to move.
func TestGoldenMasterSyncHarnessBotNetBelowTheRepositoryRoot(t *testing.T) {
	ws, git := syncHarnessRepoUnder(t, "\nimport hashlib\n# stale\n", "pkg/proj")

	res, exit := syncHarness(t, ws)
	if exit != 0 || !res.Changed {
		t.Fatalf("sync: exit %d changed=%v: %s", exit, res.Changed, res.Notice)
	}
	if moved := git("status", "--porcelain", "--untracked-files=no"); moved != "M pkg/proj/.golden-master/harness.py" { // the helper trims the leading column
		t.Fatalf("tree after the sync: %q, want the harness alone", moved)
	}

	c := syncCommitNode(t, ws, res)
	if head := git("rev-parse", "HEAD"); head != c.Commit {
		t.Fatalf("provisional commit %s is not HEAD (%s)", c.Commit, head)
	}
	if files := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(files, "1 file changed") ||
		!strings.Contains(files, "pkg/proj/.golden-master/harness.py") {
		t.Fatalf("the sync commit must carry the harness alone:\n%s", files)
	}
	if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "" {
		t.Fatalf("tree dirty after the commit: %q", dirty)
	}
}

// TestGoldenMasterSyncHarnessBotRefusals: every precondition is a typed
// refusal that leaves the tree as it found it — never a sync into a tree
// this bot does not own the shape of.
func TestGoldenMasterSyncHarnessBotRefusals(t *testing.T) {

	t.Run("a harness without the materialised header is not overwritten", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "")
		hp := filepath.Join(ws, ".golden-master", "harness.py")
		if err := os.WriteFile(hp, []byte("#!/usr/bin/env python3\n# hand-written judge\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "hand-written")
		res, exit := syncHarness(t, ws)
		if exit != 1 || !strings.Contains(res.Notice, "materialised header") {
			t.Fatalf("exit %d notice=%q, want the header refusal", exit, res.Notice)
		}
		if got, _ := os.ReadFile(hp); !strings.Contains(string(got), "hand-written judge") {
			t.Fatal("the refused harness was overwritten")
		}
	})

	t.Run("a dirty tree is refused before the write", func(t *testing.T) {
		ws, _ := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("edited, not committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, exit := syncHarness(t, ws)
		if exit != 1 || !strings.Contains(res.Notice, "uncommitted tracked changes") {
			t.Fatalf("exit %d notice=%q, want the dirty-tree refusal", exit, res.Notice)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".golden-master", "harness.py"))
		if !strings.Contains(string(got), "# stale") {
			t.Fatal("the harness was written on a dirty tree")
		}
	})

	t.Run("no net at all is refused", func(t *testing.T) {
		ws, _ := syncHarnessGitRepo(t)
		res, exit := syncHarness(t, ws)
		if exit != 1 || !strings.Contains(res.Notice, "no net") {
			t.Fatalf("exit %d notice=%q, want the no-net refusal", exit, res.Notice)
		}
	})

	// The write is in place and its only undo is git's, so a target git cannot
	// put back has to be refused before it is clobbered. Both shapes read clean
	// under `--untracked-files=no`, so nothing downstream would catch them.
	t.Run("an untracked harness is refused, not overwritten", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# untracked\n")
		hp := filepath.Join(ws, ".golden-master", "harness.py")
		git("rm", "-q", "--cached", "--", ".golden-master/harness.py")
		git("commit", "-qm", "harness untracked")
		res, exit := syncHarness(t, ws)
		if exit != 1 || !strings.Contains(res.Notice, "not a tracked regular file") {
			t.Fatalf("exit %d notice=%q, want the untracked refusal", exit, res.Notice)
		}
		if got, _ := os.ReadFile(hp); !strings.Contains(string(got), "# untracked") {
			t.Fatalf("the untracked harness was overwritten — git checkout could never put it back: %q", got)
		}
	})

	// The node reads its OWN source to compose the canonical copy, so a
	// half-applied sync-harness.py is a shape it must NAME, not die on: the
	// marker kept over a stripped body used to reach str.index and raise,
	// leaving the run with a traceback instead of any of the typed refusals.
	t.Run("the node's own source, marker over a stripped body, is refused not raised", func(t *testing.T) {
		ws, _ := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		strip := func(s string) string { return s[:strings.Index(s, "\nimport hashlib")] + "\n" }
		out, exit := runSyncNode(t, "sync_harness", ws, nil, strip)
		var res syncHarnessOut
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("no typed refusal on stdout (%v) — the node died instead of refusing: %q", err, out)
		}
		if exit != 1 || !strings.Contains(res.Notice, "expected shape") {
			t.Fatalf("exit %d notice=%q, want the mangled-source refusal", exit, res.Notice)
		}
	})

	t.Run("a symlinked harness is refused, its referent untouched", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "")
		outside := filepath.Join(t.TempDir(), "real.py")
		if err := os.WriteFile(outside, []byte(syncHarnessHeader+"\nimport hashlib\n# outside the tree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		hp := filepath.Join(ws, ".golden-master", "harness.py")
		if err := os.Remove(hp); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, hp); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		git("add", "--", ".golden-master/harness.py")
		git("commit", "-qm", "harness is a link")
		res, exit := syncHarness(t, ws)
		if exit != 1 || !strings.Contains(res.Notice, "not a tracked regular file") {
			t.Fatalf("exit %d notice=%q, want the symlink refusal", exit, res.Notice)
		}
		if got, _ := os.ReadFile(outside); !strings.Contains(string(got), "# outside the tree") {
			t.Fatalf("the write followed the link out of the tree: %q", got)
		}
	})
}

type syncSealOut struct {
	Sealed bool   `json:"sealed"`
	Commit string `json:"commit"`
	Notice string `json:"notice"`
}

// syncCommitNode runs commit_harness after a sync and returns the provisional
// commit (selftest in, gate pending).
func syncCommitNode(t *testing.T, ws string, res syncHarnessOut) syncCommitOut {
	t.Helper()
	out, exit := runSyncNode(t, "commit_harness", ws, map[string]string{"selftest": res.Selftest, "harness_sha256": res.HarnessSHA256})
	var c syncCommitOut
	if err := json.Unmarshal(out, &c); err != nil || exit != 0 || !c.Committed {
		t.Fatalf("commit_harness: exit %d, %v, out %q", exit, err, out)
	}
	return c
}

// A gate stub that behaves like the real harness on the one point that
// matters here: it REFUSES an uncommitted tree (mutant reverts would restore
// HEAD mid-gate), so a bot that replayed the gate before committing would
// go red — which is what the first version of this bot did on a real net.
const syncGateWantsCommittedTree = "sh -c 'test -z \"$(git status --porcelain --untracked-files=no)\" || { echo WORKSPACE NOT COMMITTED; exit 1; }'"

// TestGoldenMasterSyncHarnessBotGateAndCommit walks the nodes after a sync in
// the order the graph runs them: a provisional commit (the harness alone,
// selftest in the body), the target's full gate on that COMMITTED tree, then
// the gate's verdict sealed into the commit; a red gate drops the commit and
// leaves the previous harness at HEAD.
func TestGoldenMasterSyncHarnessBotGateAndCommit(t *testing.T) {
	t.Run("commit, green gate on the committed tree, verdict sealed", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		base := git("rev-parse", "HEAD")
		res, exit := syncHarness(t, ws)
		if exit != 0 || !res.Changed {
			t.Fatalf("sync: exit %d %s", exit, res.Notice)
		}
		c := syncCommitNode(t, ws, res)
		if head := git("rev-parse", "HEAD"); head != c.Commit || head == base {
			t.Fatalf("provisional commit %s is not HEAD (%s)", c.Commit, head)
		}
		if msg := git("log", "-1", "--format=%B"); !strings.Contains(msg, "Full gate: pending") || !strings.Contains(msg, "Harness sha256: "+res.HarnessSHA256) {
			t.Fatalf("provisional commit body:\n%s", msg)
		}
		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": syncGateWantsCommittedTree, "harness_sha256": res.HarnessSHA256, "sync_commit": c.Commit})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if !gate.Passed {
			t.Fatalf("gate = %+v, want green on the committed tree", gate)
		}
		out, exit = runSyncNode(t, "seal_commit", ws, map[string]string{"sync_commit": c.Commit, "gate_command": gate.Command, "gate_minutes": "0"})
		var seal syncSealOut
		if err := json.Unmarshal(out, &seal); err != nil || exit != 0 || !seal.Sealed {
			t.Fatalf("seal_commit: exit %d, %v, out %q", exit, err, out)
		}
		if head := git("rev-parse", "HEAD"); head != seal.Commit {
			t.Fatalf("sealed commit %s is not HEAD %s", seal.Commit, head)
		}
		if files := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(files, "1 file changed") || !strings.Contains(files, "harness.py") {
			t.Fatalf("the sync commit must carry the harness alone:\n%s", files)
		}
		msg := git("log", "-1", "--format=%B")
		if strings.Contains(msg, "Full gate: pending") || !strings.Contains(msg, "GREEN in 0 min") {
			t.Fatalf("sealed body lacks the gate verdict:\n%s", msg)
		}
		if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "" {
			t.Fatalf("tree dirty after sealing: %q", dirty)
		}
	})

	// `git commit --amend` with no pathspec rebuilds HEAD from the CURRENT
	// INDEX. Only checking that HEAD is still the sync commit lets whatever is
	// staged ride into the commit whose subject claims the harness alone — and
	// the body then stamps it GREEN, a verdict the gate produced for a tree
	// that did not contain it.
	t.Run("a staged change is not folded into the sealed commit", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		c := syncCommitNode(t, ws, res)
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("staged behind the gate's back\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "README.md")

		out, exit := runSyncNode(t, "seal_commit", ws, map[string]string{"sync_commit": c.Commit, "gate_command": "x", "gate_minutes": "0"})
		var seal syncSealOut
		if err := json.Unmarshal(out, &seal); err != nil {
			t.Fatalf("seal_commit output is not JSON: %v (out %q)", err, out)
		}
		if exit != 1 || seal.Sealed || !strings.Contains(seal.Notice, "index carries changes") {
			t.Fatalf("exit %d sealed=%v notice=%q, want the staged-index refusal", exit, seal.Sealed, seal.Notice)
		}
		if files := git("show", "--stat", "--format=", "HEAD"); strings.Contains(files, "README.md") {
			t.Fatalf("the amend folded staged work into the harness-only commit:\n%s", files)
		}
		if msg := git("log", "-1", "--format=%B"); !strings.Contains(msg, "Full gate: pending") {
			t.Fatalf("a refused seal must leave the commit as it was:\n%s", msg)
		}
	})

	t.Run("red gate drops the sync commit, previous harness back at HEAD", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		base := git("rev-parse", "HEAD")
		res, _ := syncHarness(t, ws)
		c := syncCommitNode(t, ws, res)
		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": "sh -c 'echo divergence; exit 3'", "harness_sha256": res.HarnessSHA256, "sync_commit": c.Commit})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if gate.Passed || !strings.Contains(gate.LogTail, "divergence") {
			t.Fatalf("gate = %+v, want red with the gate's own words", gate)
		}
		if head := git("rev-parse", "HEAD"); head != base {
			t.Fatalf("HEAD=%s, want the base %s: a red gate must drop the sync commit", head, base)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".golden-master", "harness.py"))
		if !strings.Contains(string(got), "# stale") {
			t.Fatal("a red gate must leave the previous harness at HEAD")
		}
		if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "" {
			t.Fatalf("tree dirty after a red gate: %q", dirty)
		}
		out, exit = runSyncNode(t, "seal_commit", ws, map[string]string{"sync_commit": c.Commit, "gate_command": "x", "gate_minutes": "0"})
		if exit != 1 {
			t.Fatalf("seal_commit after a dropped commit: exit %d, want the refusal (out %q)", exit, out)
		}
	})

	// The drop is declined when HEAD is no longer the sync commit — resetting
	// would take another hand's work. Declining must cost the tree NOTHING:
	// the window it opens is the whole gate (hours, on a live checkout), and a
	// blanket `checkout -- .` ahead of the HEAD check discarded every unstaged
	// tracked edit made in it while dropping no commit at all. And it must SAY
	// so: a red gate ends the run at `fail` whether or not the drop succeeded,
	// so a silent decline reads as "nothing landed" while the sync commit is
	// still in the branch's history for whoever owns landings.
	t.Run("a declined drop leaves the working tree alone, and says so", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		c := syncCommitNode(t, ws, res)
		// Another hand commits while the gate runs, so the drop will decline.
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("moved on\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-qam", "another hand")
		later := git("rev-parse", "HEAD")
		// ...and leaves work in progress behind, uncommitted.
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("work in progress\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": "sh -c 'echo divergence; exit 3'", "harness_sha256": res.HarnessSHA256, "sync_commit": c.Commit})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if gate.Passed {
			t.Fatalf("gate = %+v, want red", gate)
		}
		if head := git("rev-parse", "HEAD"); head != later {
			t.Fatalf("HEAD=%s, want %s: the drop must never reach a commit that is not the sync's", head, later)
		}
		if got, _ := os.ReadFile(filepath.Join(ws, "README.md")); string(got) != "work in progress\n" {
			t.Fatalf("README.md = %q: a declined drop discarded an unstaged edit it never had to touch", got)
		}
		if !strings.Contains(gate.LogTail, "NOT dropped") || !strings.Contains(gate.LogTail, c.Commit[:12]) {
			t.Fatalf("log_tail must name the sync commit it left behind:\n%s", gate.LogTail)
		}
		if !strings.Contains(gate.LogTail, "divergence") {
			t.Fatalf("the gate's own words must survive the notice:\n%s", gate.LogTail)
		}
		if !strings.Contains(git("log", "--format=%H"), c.Commit) {
			t.Fatal("the sync commit is gone from history — then the notice is the lie")
		}
	})

	// An expired wall is a red gate, and a red gate hard-resets the tree. So
	// the wall has to reach the whole gate: `shell=True` makes the direct child
	// a shell, and subprocess's own timeout kill leaves its descendants — the
	// gate itself, doing mutant reverts via `git checkout` — running on the
	// tree drop_sync_commit is about to reset.
	t.Run("a gate that outruns the wall is killed with its descendants", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		c := syncCommitNode(t, ws, res)
		orphan := filepath.Join(ws, "orphan-outlived-the-wall")
		// A descendant that would touch the tree well after the wall expires.
		gate := "sh -c '(sleep 5; touch " + orphan + ") & sleep 120'"

		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{
			"gate_cmd": gate, "gate_timeout_s": "1",
			"harness_sha256": res.HarnessSHA256, "sync_commit": c.Commit,
		})
		var g syncGateOut
		if err := json.Unmarshal(out, &g); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if g.Passed || !strings.Contains(g.LogTail, "exceeded gate_timeout_s") {
			t.Fatalf("gate = %+v, want the expired-wall verdict", g)
		}
		// Well past the descendant's own delay: if it survived the kill it has
		// written by now, on a tree the drop already reset.
		time.Sleep(7 * time.Second)
		if _, err := os.Stat(orphan); err == nil {
			t.Fatal("a gate descendant outlived the wall and touched the tree after the sync commit was dropped")
		}
		if head := git("rev-parse", "HEAD"); head == c.Commit {
			t.Fatal("an expired wall is a red gate: the sync commit must be dropped")
		}
	})

	// ...and the node must not then hang on the pipe. A descendant that put
	// itself in its OWN session survives the group kill still holding the
	// inherited write end, so a second unbounded read of it waits on a process
	// the wall exists to have given up on — past the run's max_duration, with
	// the sync commit neither dropped nor sealed.
	t.Run("a descendant that escapes the group kill does not hold the node", func(t *testing.T) {
		ws, _ := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		c := syncCommitNode(t, ws, res)
		// setsid() from a child of the gate shell: outside the killed group,
		// stdout still the pipe the node reads.
		escapee := `python3 -c "import os,time; os.setsid(); time.sleep(45)" & sleep 45`

		started := time.Now()
		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{
			"gate_cmd": escapee, "gate_timeout_s": "1",
			"harness_sha256": res.HarnessSHA256, "sync_commit": c.Commit,
		})
		elapsed := time.Since(started)
		var g syncGateOut
		if err := json.Unmarshal(out, &g); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if g.Passed || !strings.Contains(g.LogTail, "exceeded gate_timeout_s") {
			t.Fatalf("gate = %+v, want the expired-wall verdict", g)
		}
		if elapsed > 20*time.Second {
			t.Fatalf("the node took %s on a 1 s wall — it waited on the escaped descendant's pipe", elapsed)
		}
	})

	t.Run("a gate run before the commit is what the real harness refuses — the graph never does it", func(t *testing.T) {
		ws, _ := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		// Not committed: the committed-tree gate stub goes red, exactly like the harness.
		out, _ := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": syncGateWantsCommittedTree, "harness_sha256": res.HarnessSHA256, "sync_commit": "0000000000000000000000000000000000000000"})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil {
			t.Fatal(err)
		}
		if gate.Passed || !strings.Contains(gate.LogTail, "WORKSPACE NOT COMMITTED") {
			t.Fatalf("gate = %+v, want the uncommitted-tree refusal", gate)
		}
	})
}
