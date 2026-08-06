package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2ECoverageMatrixGate guards the deterministic MATRIX CONTRACT half of
// e2e-coverage's verify_run gate — the bot-specific anti-façade floor: the
// committed feature×coverage matrix must exist, carry the contract marker,
// parse, use only allowed statuses, justify terminal exceptions
// (covered-live / unit-only / excluded need Notes), and every covered-* row
// must cite at least one test reference that RESOLVES in the tree (file
// exists / name found in file / bare name greps). An ORPHAN CLAIM — a
// covered row whose cited tests resolve nowhere — is matrix_ok=false, which
// the compute gate turns into a red pass regardless of a green suite.
//
// The test extracts the verify_run command from the compiled IR and executes
// it for real against fixture workspaces (same harness as
// TestVerifyRunDriftTail).
func TestE2ECoverageMatrixGate(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	command := toolCommand(t, "e2e-coverage/main.bot", "verify_run")

	type verifyResult struct {
		Passed        bool   `json:"passed"`
		Skipped       bool   `json:"skipped"`
		MatrixOK      bool   `json:"matrix_ok"`
		MatrixRows    int    `json:"matrix_rows"`
		UncoveredRows int    `json:"uncovered_rows"`
		NewTestCode   bool   `json:"new_test_code"`
		ExitCode      int    `json:"exit_code"`
		LogTail       string `json:"log_tail"`
	}

	const matrixRel = "docs/e2e-coverage-matrix.md"

	run := func(t *testing.T, ws, scratch string) verifyResult {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
		cmd = strings.ReplaceAll(cmd, "{{vars.matrix_path}}", matrixRel)
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

	greenVerify := func(t *testing.T, scratch string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFile := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const header = "<!-- e2e-coverage-matrix: v1 -->\n\n# E2E coverage matrix\n\n| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"

	t.Run("missing_matrix_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("missing matrix must be matrix_ok=false: %+v", res)
		}
		if !res.Passed {
			t.Fatalf("the suite verdict must stay independent of the matrix verdict: %+v", res)
		}
		if !strings.Contains(res.LogTail, "matrix file missing") {
			t.Fatalf("log_tail must name the missing matrix, got %q", res.LogTail)
		}
	})

	t.Run("marker_missing_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, "# E2E coverage matrix\n\n| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n| a.b | Thing | a | uncovered | | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "marker") {
			t.Fatalf("matrix without the contract marker must be red with an instructive log: %+v", res)
		}
	})

	t.Run("valid_matrix_with_resolving_claims_passes", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/resume_test.go", "package e2e\n\nfunc TestResumeFromCheckpoint(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| runtime.resume | resume replays from checkpoint | runtime | covered-deterministic | TestResumeFromCheckpoint (e2e/resume_test.go) | |\n"+
			"| runtime.fanout | fan-out branches | runtime | uncovered | | plan: stub a branch |\n"+
			"| util.slug | slug helper | util | unit-only | | pure function, asserted in unit table |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("valid matrix must be matrix_ok=true: %+v", res)
		}
		if res.MatrixRows != 3 || res.UncoveredRows != 1 {
			t.Fatalf("row accounting wrong (want 3 rows / 1 uncovered): %+v", res)
		}
	})

	t.Run("orphan_claim_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| runtime.resume | resume replays from checkpoint | runtime | covered-deterministic | TestGhost (e2e/ghost_test.go) | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a covered row citing a non-existent test must be red: %+v", res)
		}
		if !strings.Contains(res.LogTail, "ORPHAN CLAIM") {
			t.Fatalf("log_tail must name the orphan claim, got %q", res.LogTail)
		}
	})

	t.Run("name_cited_but_absent_from_file_is_orphan", func(t *testing.T) {
		// The file exists but does not contain the cited test name — the
		// claim must not resolve on file existence alone.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/resume_test.go", "package e2e\n\nfunc TestSomethingElse(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| runtime.resume | resume | runtime | covered-deterministic | TestResumeFromCheckpoint (e2e/resume_test.go) | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "ORPHAN CLAIM") {
			t.Fatalf("cited name absent from the cited file must be an orphan claim: %+v", res)
		}
	})

	t.Run("bare_name_resolves_via_grep_including_untracked", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		// Untracked test file — git grep --untracked must still find it.
		writeFile(t, ws, "tests/test_resume.py", "def test_resume_from_checkpoint():\n    assert True\n")
		writeFile(t, ws, matrixRel, header+
			"| runtime.resume | resume | runtime | covered-deterministic | test_resume_from_checkpoint | |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("a bare name present in an untracked file must resolve: %+v", res)
		}
	})

	t.Run("invalid_status_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | coverd-deterministic | | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "invalid status") {
			t.Fatalf("a typo'd status must be red with an instructive log: %+v", res)
		}
	})

	t.Run("unjustified_exclusion_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | excluded | | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "justification") {
			t.Fatalf("excluded without a Notes justification must be red: %+v", res)
		}
	})

	t.Run("covered_without_test_refs_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "without any test reference") {
			t.Fatalf("covered without a Tests cell must be red: %+v", res)
		}
	})

	t.Run("empty_matrix_table_is_red", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header)
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "no feature rows") {
			t.Fatalf("a matrix with zero feature rows must be red: %+v", res)
		}
	})

	t.Run("covered_live_requires_note_and_ref", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/live_test.go", "//go:build live\npackage e2e\n\nfunc TestLiveModelQuality(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| backends.live | live model quality | backends | covered-live | TestLiveModelQuality (e2e/live_test.go) | essence of the feature is the live model |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("a justified covered-live row with a resolving ref must pass: %+v", res)
		}
	})
}
