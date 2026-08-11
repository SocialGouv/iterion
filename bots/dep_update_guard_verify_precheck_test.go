package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDepUpdateGuardVerifyPrechecks covers the two ways Vetty's verify script
// came back red without ever having verified anything, both observed on the
// 2026-08-10 Dependabot batch.
//
// Both are AUTHORSHIP defects, not red builds, and the difference matters:
// a red build is a fact about the bump, an unwritten or unrunnable script is a
// step the agent skipped. Reported as a red build they are indistinguishable,
// and the operator reads "this bump breaks the repo" about a bump that breaks
// nothing. Reported as a precheck rejection they loop back into verify_build
// with the reason and self-heal, while the commit path stays just as closed.
func TestDepUpdateGuardVerifyPrechecks(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	command := verifyRunCommand(t, "dep-update-guard/main.bot")

	type verifyResult struct {
		Passed           bool   `json:"passed"`
		Skipped          bool   `json:"skipped"`
		ExitCode         int    `json:"exit_code"`
		PrecheckRejected bool   `json:"precheck_rejected"`
		PrecheckReason   string `json:"precheck_reason"`
		LogTail          string `json:"log_tail"`
	}

	run := func(t *testing.T, ws, scratch string) verifyResult {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("verify_run failed to execute: %v (out %q)", err, out)
		}
		var res verifyResult
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("verify_run output is not verify_result JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	// iterion#390: a base-image digest refresh on an unchanged tag. The
	// alignment analysis was right that nothing needed changing — and the run
	// then died because no script was written, with no way back. A bump whose
	// correct alignment is empty could never clear the gate.
	t.Run("no script: rejected for a rewrite, not reported as a red build", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		res := run(t, ws, scratch)
		if res.Passed {
			t.Error("a missing script must not open the commit path")
		}
		if !res.PrecheckRejected {
			t.Error("a missing script is an authorship defect: it must loop back to verify_build, not terminate as a red build")
		}
		if !strings.Contains(res.PrecheckReason, "NO SCRIPT") {
			t.Errorf("the reason must name the defect so the rewrite is targeted, got %q", res.PrecheckReason)
		}
	})

	// iterion#398: verify.sh held ~2300 lines, each a path to a Go test file.
	// sh answered every one with "Permission denied" and the run reported a
	// red build for a bump whose whole test surface was in fact green.
	t.Run("a file listing pasted in as commands is rejected", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		var lines []string
		for _, name := range []string{"a_test.go", "b_test.go", "c_test.go", "d_test.go", "e_test.go"} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, "./"+name)
		}
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if res.Passed {
			t.Error("a script of unrunnable paths must not open the commit path")
		}
		if !res.PrecheckRejected {
			t.Error("this is an authorship defect, not a red build — it must loop back with the reason")
		}
		if !strings.Contains(res.PrecheckReason, "NOT COMMANDS") {
			t.Errorf("the reason must name the defect, got %q", res.PrecheckReason)
		}
		if !strings.Contains(res.PrecheckReason, "a_test.go") {
			t.Errorf("the reason must cite the offending lines so the rewrite is targeted, got %q", res.PrecheckReason)
		}
	})

	// The false positive that would matter: a script legitimately delegating to
	// the repo's own executable helpers. Rejecting those would break every repo
	// that wraps its build in a script.
	t.Run("a script calling executable helpers is not rejected", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"build.sh", "test.sh", "lint.sh", "vet.sh"} {
			p := filepath.Join(ws, "scripts", name)
			if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := "#!/bin/sh\nset -e\ncd " + ws + "\n./scripts/build.sh\n./scripts/test.sh\n./scripts/lint.sh\n./scripts/vet.sh\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("executable helpers are how a repo legitimately wraps its build: %q", res.PrecheckReason)
		}
		if !res.Passed {
			t.Errorf("a green script must pass, got exit %d: %s", res.ExitCode, res.LogTail)
		}
	})
}
