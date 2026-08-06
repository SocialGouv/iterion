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
		Scoped        bool   `json:"scoped"`
		NewTestCode   bool   `json:"new_test_code"`
		ExitCode      int    `json:"exit_code"`
		LogTail       string `json:"log_tail"`
	}

	const matrixRel = "docs/e2e-coverage-matrix.md"

	// The engine substitutes a var through shellEscapeValue, which always
	// wraps the value in single quotes (pkg/backend/model/executor_tool.go
	// shellEscape). `target` is free text — reproduce that quoting here or
	// the harness would test a shape production never produces.
	shellQuote := func(s string) string {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}

	runTarget := func(t *testing.T, ws, scratch, target string) verifyResult {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
		cmd = strings.ReplaceAll(cmd, "{{vars.matrix_path}}", matrixRel)
		cmd = strings.ReplaceAll(cmd, "{{vars.target}}", shellQuote(target))
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

	// The whole-application shape (no target) is the strict one: the gate's
	// own uncovered count, not the agent's claim, decides convergence.
	run := func(t *testing.T, ws, scratch string) verifyResult {
		t.Helper()
		return runTarget(t, ws, scratch, "")
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

	// ---------------------------------------------------------------
	// Bypass regressions. Each case below was an EXECUTED false-green
	// against the first cut of this gate (adversarial review, 2026-08-06):
	// the matrix proved nothing, or did not count what it claimed, or was
	// not even the table the operator was reading — and the gate said OK.
	// ---------------------------------------------------------------

	t.Run("bypass_directory_citation_is_not_a_test", func(t *testing.T) {
		// `.` (or any existing directory) satisfied a plain os.path.exists.
		for _, ref := range []string{".", "..", "e2e", "docs"} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, "e2e/keep.txt", "x") // make e2e/ exist
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+ref+" | |\n")
			res := run(t, ws, scratch)
			if res.MatrixOK {
				t.Fatalf("citation %q resolved to a directory/non-test and passed: %+v", ref, res)
			}
		}
	})

	t.Run("bypass_non_test_file_citation", func(t *testing.T) {
		// A real file that is not a test file proves nothing.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "README.md", "docs, not a test")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | README.md | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "ORPHAN CLAIM") {
			t.Fatalf("a non-test file must not resolve a coverage claim: %+v", res)
		}
	})

	t.Run("bypass_matrix_citing_itself", func(t *testing.T) {
		// Both citation forms pointed at the matrix itself; the Feature cell
		// then supplies the "name" the parenthesised form looks for.
		//
		// The matrix path used here is deliberately TEST-SHAPED
		// (`..._test.md`): with an ordinary path the test-file regex alone
		// would reject the citation, so it would pass even with the
		// self-citation guard removed — measured, and the reason this case
		// is written this way.
		selfRel := "docs/coverage_matrix_test.md"
		for _, ref := range []string{selfRel, "(" + selfRel + ")", "Payment (" + selfRel + ")"} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
			cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
			cmd = strings.ReplaceAll(cmd, "{{vars.matrix_path}}", selfRel)
			cmd = strings.ReplaceAll(cmd, "{{vars.target}}", shellQuote(""))
			writeFile(t, ws, selfRel, header+
				"| a.b | Payment | a | covered-deterministic | "+ref+" | |\n")
			out, err := exec.Command("sh", "-c", cmd).Output()
			if err != nil {
				t.Fatalf("verify_run: %v (%s)", err, out)
			}
			var res verifyResult
			if uerr := json.Unmarshal(out, &res); uerr != nil {
				t.Fatalf("not verify_result JSON: %v (%s)", uerr, out)
			}
			if res.MatrixOK {
				t.Fatalf("the matrix citing itself must not resolve (%q): %+v", ref, res)
			}
		}
	})

	t.Run("bypass_short_bare_name_greps_anything", func(t *testing.T) {
		// `a` matched almost every source file through git grep.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/thing_test.go", "package e2e // unrelated content with a\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | a | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a 1-char citation must not resolve: %+v", res)
		}
	})

	t.Run("bypass_short_row_hides_a_feature", func(t *testing.T) {
		// A row with too few cells read as status="" and was skipped
		// silently — the feature vanished from the accounting entirely.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | uncovered | | plan |\n"+
			"| ghost | Payment feature never tested |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "malformed row") {
			t.Fatalf("a short row must be an explicit error, never a silent skip: %+v", res)
		}
	})

	t.Run("bypass_blank_line_truncates_the_table", func(t *testing.T) {
		// The row scan stopped at the first blank line, hiding every row
		// after it — uncovered rows included.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n"+
			"\n"+
			"| hidden1 | Payment | a | uncovered | | plan |\n"+
			"| hidden2 | Auth | a | uncovered | | plan |\n")
		res := run(t, ws, scratch)
		if res.MatrixRows != 3 || res.UncoveredRows != 2 {
			t.Fatalf("rows after a blank line must still be counted (want 3 rows / 2 uncovered): %+v", res)
		}
	})

	t.Run("bypass_decoy_table_before_the_matrix", func(t *testing.T) {
		// A summary table carrying Feature+Status ahead of the real one
		// became "the matrix": 1 row, 0 uncovered, green.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel,
			"<!-- e2e-coverage-matrix: v1 -->\n\n"+
				"| Feature | Status | Notes |\n|---|---|---|\n| Decoy | excluded | out of scope |\n\n"+
				"# Real matrix\n\n"+
				"| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"+
				"| real1 | Payment | a | uncovered | | plan |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a second Feature+Status table must be rejected, not silently preferred: %+v", res)
		}
	})

	t.Run("bypass_fenced_example_table", func(t *testing.T) {
		// The coverage-matrix skill itself ships an example table in a
		// fence; pasted above the real one it used to become the matrix.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		fence := strings.Repeat("`", 3)
		writeFile(t, ws, matrixRel,
			"<!-- e2e-coverage-matrix: v1 -->\n\n"+fence+"\n"+
				"| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"+
				"| example | Example | a | excluded | | illustration |\n"+fence+"\n\n"+
				"# Real matrix\n\n"+
				"| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"+
				"| real1 | Payment | a | uncovered | | plan |\n"+
				"| real2 | Auth | a | uncovered | | plan |\n")
		res := run(t, ws, scratch)
		if res.MatrixRows != 2 || res.UncoveredRows != 2 {
			t.Fatalf("a fenced example table must be ignored and the real one parsed (want 2/2): %+v", res)
		}
	})

	// Round-2 bypasses: each was an EXECUTED false-green (or, for the
	// convention cases, a false RED) against the round-1 hardening.

	t.Run("bypass_prose_line_inside_the_table", func(t *testing.T) {
		// A heading between two row groups ended the scan and dropped every
		// row below it — uncovered rows included — with no error at all.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n"+
			"## Payment refunds\n"+
			"| hidden | Payment refund | a | uncovered | | plan |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a stray line inside the table must be an error, not a silent truncation: %+v", res)
		}
		if !strings.Contains(res.LogTail, "AFTER the table ended") {
			t.Fatalf("log_tail must name the dropped rows, got %q", res.LogTail)
		}
	})

	t.Run("bypass_indented_code_block_table", func(t *testing.T) {
		// A 4-space-indented table renders as <pre> — the operator sees no
		// table at all — but was parsed as the matrix.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel,
			"<!-- e2e-coverage-matrix: v1 -->\n\nExample:\n\n"+
				"    | ID | Feature | Family | Status | Tests | Notes |\n"+
				"    |---|---|---|---|---|---|\n"+
				"    | fake | Payment | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || res.MatrixRows != 0 {
			t.Fatalf("an indented (code-rendered) table must not be read as the matrix: %+v", res)
		}
	})

	t.Run("bypass_mixed_fence_flavours", func(t *testing.T) {
		// A tilde fence inside a backtick fence toggled one shared boolean
		// back to "outside", leaking the block's table as the matrix.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		fence := strings.Repeat("`", 3)
		writeFile(t, ws, matrixRel,
			"<!-- e2e-coverage-matrix: v1 -->\n\n"+fence+"\nprose\n~~~\n"+
				"| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"+
				"| leaked | Payment | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n"+fence+"\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || res.MatrixRows != 0 {
			t.Fatalf("a table inside a fenced block must stay invisible whatever the fence flavour: %+v", res)
		}
	})

	t.Run("bypass_short_name_in_path_form", func(t *testing.T) {
		// The >=4-char / test-ish guard was only applied to bare names, so
		// `x (test_x.go)` substring-matched almost any file.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/thing_test.go", "package e2e\n\nvar x = 1\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | x (e2e/thing_test.go) | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a 1-char name in path form must not resolve: %+v", res)
		}
	})

	t.Run("bypass_citation_escaping_the_workspace", func(t *testing.T) {
		// `../`, an absolute path, and a symlink out of the tree all passed
		// os.path.isfile.
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "outside_test.go"), []byte("package x\nfunc TestOut(t *testing.T) {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{
			"TestOut (../" + filepath.Base(outside) + "/outside_test.go)",
			"TestOut (" + filepath.Join(outside, "outside_test.go") + ")",
		} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+ref+" | |\n")
			res := run(t, ws, scratch)
			if res.MatrixOK {
				t.Fatalf("a citation outside the workspace must not resolve (%q): %+v", ref, res)
			}
		}
	})

	t.Run("dead_reference_in_a_non_covered_row", func(t *testing.T) {
		// A unit-only row citing its unit suite skipped the claims check
		// entirely, so a stale path lived on (the real matrix carried one).
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | unit-only | pkg/x/gone_test.go | pure function |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK || !strings.Contains(res.LogTail, "DEAD REFERENCE") {
			t.Fatalf("a dead citation must be reported whatever the row's status: %+v", res)
		}
	})

	t.Run("every_citation_must_resolve_not_just_one", func(t *testing.T) {
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | TestReal (e2e/real_test.go), TestGhost (e2e/ghost_test.go) | |\n")
		res := run(t, ws, scratch)
		if res.MatrixOK {
			t.Fatalf("a dead citation beside a live one must still be reported: %+v", res)
		}
	})

	t.Run("root_level_test_directories_are_accepted", func(t *testing.T) {
		// FALSE POSITIVE regression: the round-1 regex required a slash on
		// BOTH sides, so Rust `tests/`, pytest `tests/`, RSpec `spec/` and
		// Jest `__tests__/` — all at the repo ROOT — were rejected, which
		// would make this gate refuse the legitimate matrix of most
		// non-Go repos.
		for _, tc := range []struct{ path, body, name string }{
			{"tests/integration.rs", "#[test]\nfn test_charge_is_idempotent() {}\n", "test_charge_is_idempotent"},
			{"tests/scenarios.py", "def test_charge_is_idempotent():\n    pass\n", "test_charge_is_idempotent"},
			{"__tests__/component.js", "it('renders the charge form', () => {});\n", "renders the charge form"},
			{"spec/user.rb", "describe 'User' do\n  it_behaves_like 'a payer'\nend\n", "it_behaves_like"},
		} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, tc.path, tc.body)
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+tc.path+" | |\n")
			if res := run(t, ws, scratch); !res.MatrixOK {
				t.Fatalf("a root-level %s citation must resolve: %+v", tc.path, res)
			}
		}
	})

	t.Run("whitespace_target_is_not_a_scope", func(t *testing.T) {
		// `--var target=" "` (stray space, template rendering to blank) used
		// to read as a scoped run in the compute gate's `target != ''`,
		// waiving the zero-uncovered requirement of a whole-app run.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+"| a.b | Thing | a | uncovered | | plan |\n")
		if res := runTarget(t, ws, scratch, " "); res.Scoped {
			t.Fatalf("a whitespace-only target must not count as a scope: %+v", res)
		}
		if res := runTarget(t, ws, scratch, ""); res.Scoped {
			t.Fatalf("an empty target must not count as a scope: %+v", res)
		}
		if res := runTarget(t, ws, scratch, "the cli family"); !res.Scoped {
			t.Fatalf("a real target must count as a scope: %+v", res)
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
