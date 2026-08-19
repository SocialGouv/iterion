package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestVerifyRunDriftTail guards the deterministic codegen-drift tail shared
// by the catalog bots' verify_run gate. The verify-build SKILL teaches the
// agent to mirror CI's drift gates in verify.sh, but prose alone was gamed
// twice (OpenAPI drift shipped green-locally / red-in-CI): the agent copies
// the example and omits the gate. The tail makes the gate deterministic:
//
//  1. CI-mirror assertion — if the repo's own CI config enforces a drift
//     gate (git diff --exit-code / --quiet / status --porcelain), verify.sh
//     must contain one too, else the gate fails with an instructive log.
//  2. Post-verify tree-drift — a GREEN verify must not leave new changes in
//     the tree (regen output belongs in a commit), else the gate fails and
//     names the paths.
//
// The test extracts the verify_run command from feature-dev's compiled IR
// (byte-shared with whole/branch-improve-loop + feature-gap-fill, guarded by
// the bundle-consistency test) and executes it for real against fixtures.
func TestVerifyRunDriftTail(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	command := verifyRunCommand(t, "feature-dev/main.bot")

	type verifyResult struct {
		Passed   bool   `json:"passed"`
		Skipped  bool   `json:"skipped"`
		ExitCode int    `json:"exit_code"`
		LogTail  string `json:"log_tail"`
	}

	run := func(t *testing.T, ws, scratch string) verifyResult {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("verify_run command failed to execute: %v (out %q)", err, out)
		}
		var res verifyResult
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("verify_run output is not the verify_result JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	gitWorkspace := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v (%s)", err, out)
		}
		return ws
	}

	writeVerifySh := func(t *testing.T, scratch, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ciWithDriftGate := func(t *testing.T, ws string) {
		t.Helper()
		dir := filepath.Join(ws, ".github", "workflows")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		yml := "jobs:\n  test:\n    steps:\n      - run: |\n          task openapi:gen\n          git diff --exit-code openapi.json\n"
		if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("ci_gate_missing_from_verify_sh_fails", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		ciWithDriftGate(t, ws)
		writeVerifySh(t, scratch, "#!/bin/sh\nexit 0\n") // gateless
		res := run(t, ws, scratch)
		if res.Passed {
			t.Fatalf("gateless verify.sh must fail when CI enforces a drift gate: %+v", res)
		}
		if !strings.Contains(res.LogTail, "DRIFT GATE MISSING") || !strings.Contains(res.LogTail, "section 1b") {
			t.Fatalf("log_tail must carry the instructive drift message, got %q", res.LogTail)
		}
	})

	t.Run("updater_commit_if_changed_ci_is_not_a_drift_gate", func(t *testing.T) {
		// A workflow using `git diff --quiet` as commit-if-changed control
		// flow (a Homebrew tap updater, a changelog bump) is NOT a
		// build-failing drift gate — a gateless verify.sh must still pass.
		// Regression: brew-update.yml's `if git diff --staged --quiet; then`
		// forced every feature-dev pass to exit 3, blocking convergence
		// (2026-07-22 treatment dogfood).
		ws, scratch := gitWorkspace(t), t.TempDir()
		dir := filepath.Join(ws, ".github", "workflows")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		yml := "jobs:\n  bump:\n    steps:\n      - run: |\n          git add -A\n          if git diff --staged --quiet; then\n            echo 'nothing to commit'\n          else\n            git commit -m bump && git push\n          fi\n"
		if err := os.WriteFile(filepath.Join(dir, "updater.yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		writeVerifySh(t, scratch, "#!/bin/sh\nexit 0\n") // gateless — and that's fine
		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("commit-if-changed updater CI must not demand a drift gate: %+v", res)
		}
	})

	t.Run("porcelain_assert_ci_is_a_drift_gate", func(t *testing.T) {
		// The porcelain form of a REAL gate (`test -z "$(git status
		// --porcelain)"`) still counts: a gateless verify.sh must fail.
		ws, scratch := gitWorkspace(t), t.TempDir()
		dir := filepath.Join(ws, ".github", "workflows")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		yml := "jobs:\n  test:\n    steps:\n      - run: |\n          task gen\n          test -z \"$(git status --porcelain)\"\n"
		if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		writeVerifySh(t, scratch, "#!/bin/sh\nexit 0\n")
		res := run(t, ws, scratch)
		if res.Passed {
			t.Fatalf("porcelain-assert CI gate must demand a mirror in verify.sh: %+v", res)
		}
	})

	t.Run("green_verify_leaving_new_tree_changes_fails", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir() // no CI config → assertion 1 skipped
		writeVerifySh(t, scratch, "#!/bin/sh\necho generated > gen-out.txt\nexit 0\n")
		res := run(t, ws, scratch)
		if res.Passed {
			t.Fatalf("a green verify that dirties the tree must fail: %+v", res)
		}
		if !strings.Contains(res.LogTail, "UNCOMMITTED REGEN OUTPUT") || !strings.Contains(res.LogTail, "gen-out.txt") {
			t.Fatalf("log_tail must name the leftover paths, got %q", res.LogTail)
		}
	})

	t.Run("clean_green_verify_passes", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		writeVerifySh(t, scratch, "#!/bin/sh\nexit 0\n")
		res := run(t, ws, scratch)
		if !res.Passed || res.ExitCode != 0 {
			t.Fatalf("clean green verify must pass: %+v", res)
		}
	})

	t.Run("ci_gate_mirrored_in_verify_sh_passes", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		ciWithDriftGate(t, ws)
		writeVerifySh(t, scratch, "#!/bin/sh\ngit diff --exit-code\n")
		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("verify.sh mirroring the CI drift gate must pass: %+v", res)
		}
	})

	t.Run("red_verify_reports_real_exit_code_unchanged", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		writeVerifySh(t, scratch, "#!/bin/sh\nexit 7\n")
		res := run(t, ws, scratch)
		if res.Passed || res.ExitCode != 7 {
			t.Fatalf("red verify must keep its real exit code: %+v", res)
		}
	})

	t.Run("missing_verify_sh_still_skips", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		res := run(t, ws, scratch)
		if !res.Passed || !res.Skipped {
			t.Fatalf("missing verify.sh must keep the surfaced-skip contract: %+v", res)
		}
	})

	t.Run("non_git_workspace_degrades_gracefully", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir() // no .git → tree_state None → drift check 2 skipped
		writeVerifySh(t, scratch, "#!/bin/sh\necho artifact > out.txt\nexit 0\n")
		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("non-git workspace must not fail on the drift tail: %+v", res)
		}
	})
}

// TestVerifyRunDriftTailPresentInAllBots asserts every catalog bot carrying a
// verify_run-style gate ships the deterministic drift tail (the body is
// copy-pasted per bot — no DSL include — so a new bot or an edit can silently
// drop it).
func TestVerifyRunDriftTailPresentInAllBots(t *testing.T) {
	bots := map[string]string{
		"feature-dev/main.bot":         "verify_run",
		"whole-improve-loop/main.bot":  "verify_run",
		"branch-improve-loop/main.bot": "verify_run",
		"feature-gap-fill/main.bot":    "verify_run",
		"test-coverage/main.bot":       "verify_run",
		"e2e-coverage/main.bot":        "verify_run",
		"dep-update-guard/main.bot":    "verify_run",
		"adr-cartograph/main.bot":      "verify_run",
		"instrument/main.bot":          "verify_run",
		// docs-refresh dropped the build-verify apparatus (a docs-only
		// campaign can't break the build) — commit 8aee22894, converges on
		// scope_ok ∧ docs_aligned alone. No verify_run node to guard.
		"secured-renovacy/main.bot": "p2_verify_run",
	}
	for rel, node := range bots {
		t.Run(rel, func(t *testing.T) {
			cmd := toolCommand(t, rel, node)
			for _, marker := range []string{"tree_state", "DRIFT GATE MISSING", "UNCOMMITTED REGEN OUTPUT"} {
				if !strings.Contains(cmd, marker) {
					t.Errorf("%s %s lacks the deterministic drift tail (missing %q)", rel, node, marker)
				}
			}
		})
	}
}

// verifyRunCommand compiles a bot and returns its verify_run tool command.
func verifyRunCommand(t *testing.T, rel string) string {
	t.Helper()
	return toolCommand(t, rel, "verify_run")
}

func toolCommand(t *testing.T, rel, node string) string {
	t.Helper()
	src, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	pr := parser.Parse(rel, string(src))
	if pr.File == nil {
		t.Fatalf("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile produced no Workflow")
	}
	raw, ok := cr.Workflow.Nodes[node]
	if !ok {
		t.Fatalf("no %s node", node)
	}
	tn, ok := raw.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is %T, want *ir.ToolNode", node, raw)
	}
	return tn.Command
}
