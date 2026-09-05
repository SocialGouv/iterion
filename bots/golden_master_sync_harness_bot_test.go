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
// runner the net emits.
func syncHarnessRepo(t *testing.T, oldBody string) (string, func(args ...string) string) {
	t.Helper()
	ws, git := syncHarnessGitRepo(t)
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
	git("add", ".golden-master")
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
func runSyncNode(t *testing.T, node, ws string, inputs map[string]string) ([]byte, int) {
	t.Helper()
	body := toolScript(t, "golden-master/sync-harness.bot", node)
	body = strings.ReplaceAll(body, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{vars.oracle_dir}}", strconv.Quote(".golden-master"))
	body = strings.ReplaceAll(body, "{{vars.gate_cmd}}", strconv.Quote(inputs["gate_cmd"]))
	body = strings.ReplaceAll(body, "{{vars.gate_timeout_s}}", "60")
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
}

// TestGoldenMasterSyncHarnessBotGateAndCommit walks the two nodes after a
// sync: a green gate leads to one commit carrying the harness alone and both
// proofs; a red gate restores the previous harness and commits nothing.
func TestGoldenMasterSyncHarnessBotGateAndCommit(t *testing.T) {

	t.Run("green gate, then one commit with the proofs", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, exit := syncHarness(t, ws)
		if exit != 0 || !res.Changed {
			t.Fatalf("sync: exit %d %s", exit, res.Notice)
		}
		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": "", "harness_sha256": res.HarnessSHA256})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if !gate.Passed || gate.Command != ".golden-master/verify-oracle.sh" {
			t.Fatalf("gate = %+v, want the net's runner replayed green", gate)
		}
		out, exit = runSyncNode(t, "commit_harness", ws, map[string]string{
			"selftest": res.Selftest, "gate_command": gate.Command, "gate_minutes": "0", "harness_sha256": res.HarnessSHA256,
		})
		var c syncCommitOut
		if err := json.Unmarshal(out, &c); err != nil || exit != 0 || !c.Committed {
			t.Fatalf("commit_harness: exit %d, %v, out %q", exit, err, out)
		}
		if head := git("rev-parse", "HEAD"); head != c.Commit {
			t.Fatalf("reported commit %s is not HEAD %s", c.Commit, head)
		}
		if files := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(files, "1 file changed") || !strings.Contains(files, "harness.py") {
			t.Fatalf("the sync commit must carry the harness alone:\n%s", files)
		}
		msg := git("log", "-1", "--format=%B")
		for _, want := range []string{"Harness sha256: " + res.HarnessSHA256, "Selftest in this tree: ", "Full gate: `.golden-master/verify-oracle.sh` GREEN"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("commit body lacks %q:\n%s", want, msg)
			}
		}
		if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "" {
			t.Fatalf("tree dirty after the commit: %q", dirty)
		}
	})

	t.Run("red gate restores the previous harness, nothing to commit", func(t *testing.T) {
		ws, git := syncHarnessRepo(t, "\nimport hashlib\n# stale\n")
		res, _ := syncHarness(t, ws)
		out, exit := runSyncNode(t, "gate_replay", ws, map[string]string{"gate_cmd": "sh -c 'echo divergence; exit 3'", "harness_sha256": res.HarnessSHA256})
		var gate syncGateOut
		if err := json.Unmarshal(out, &gate); err != nil || exit != 0 {
			t.Fatalf("gate_replay: exit %d, %v, out %q", exit, err, out)
		}
		if gate.Passed || !strings.Contains(gate.LogTail, "divergence") {
			t.Fatalf("gate = %+v, want red with the gate's own words", gate)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".golden-master", "harness.py"))
		if !strings.Contains(string(got), "# stale") {
			t.Fatal("a red gate must restore the previous harness")
		}
		if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "" {
			t.Fatalf("tree dirty after a red gate: %q", dirty)
		}
		out, exit = runSyncNode(t, "commit_harness", ws, map[string]string{
			"selftest": res.Selftest, "gate_command": "x", "gate_minutes": "0", "harness_sha256": res.HarnessSHA256,
		})
		if exit != 1 {
			t.Fatalf("commit_harness on a restored tree: exit %d, want the refusal (out %q)", exit, out)
		}
	})
}
