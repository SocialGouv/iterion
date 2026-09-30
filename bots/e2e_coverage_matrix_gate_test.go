package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
		// Commit whatever the test set up before verify_run runs. The
		// net_dirty gate (issue #799) refuses a dirty tree; the campaign
		// contract already commits its work before the deterministic gate,
		// so the test represents the SAME state a real pass would present.
		commitFixture(t, ws)
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
		gittest.Run(t, ws, "init", "-q")
		return ws
	}

	greenVerify := func(t *testing.T, scratch string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	verifyScript := func(t *testing.T, scratch, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(body), 0o755); err != nil {
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

	// Round-3 regressions. All four were FALSE POSITIVES the round-2
	// hardening introduced or left: the gate had narrowed to "citations that
	// look like Go test function names", which is exactly this repo's shape
	// and almost nobody else's.

	t.Run("description_style_citations_resolve", func(t *testing.T) {
		// The natural citation shape of RSpec / Jest / Cypress / pytest-BDD
		// is a DESCRIPTION plus its file. Requiring the NAME to contain
		// test/spec/should rejected all of them, even though the FILE
		// already proves it is a test.
		for _, tc := range []struct{ path, body, cite string }{
			{"spec/charge_spec.rb", "describe 'Charge' do\n  it 'charge_is_idempotent' do\n  end\nend\n", "charge_is_idempotent (spec/charge_spec.rb)"},
			{"__tests__/form.js", "it('renders the form', () => {});\n", "renders the form (__tests__/form.js)"},
			{"features/checkout.feature", "Scenario: the cart empties after payment\n", "the cart empties after payment (features/checkout.feature)"},
		} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, tc.path, tc.body)
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+tc.cite+" | |\n")
			res := run(t, ws, scratch)
			// The feature-file case is expected to fail the test-file regex;
			// the two real test files must resolve.
			if strings.HasSuffix(tc.path, ".feature") {
				continue
			}
			if !res.MatrixOK {
				t.Fatalf("a description-style citation of %s must resolve: %+v", tc.path, res)
			}
		}
	})

	t.Run("parametric_names_are_not_split_on_their_commas", func(t *testing.T) {
		// The Tests cell is comma-separated, but a parametric name carries
		// its own commas — pytest's test_x[a=1, b=2], a Jest description
		// with a comma. Splitting naively turned one live citation into a
		// live one plus a bogus fragment, which "every citation must
		// resolve" then reported as an orphan.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, "tests/test_charge.py", "def test_charge_amount_100_usd():\n    pass\n# test_charge[amount=100, currency=usd]\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | test_charge[amount=100, currency=usd] (tests/test_charge.py) | |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("a parametric citation must not be split on its own commas: %+v", res)
		}
	})

	t.Run("prose_in_a_non_covered_tests_cell_is_not_a_dead_reference", func(t *testing.T) {
		// Checking every non-covered row's Tests cell caught the stale path
		// it was written for, but also flagged legitimate operator prose.
		ws, scratch := gitWorkspace(t), t.TempDir()
		greenVerify(t, scratch)
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | excluded | N/A | needs a live cluster |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("operator prose in a non-covered row's Tests cell must not read as a dead reference: %+v", res)
		}
	})

	t.Run("testish_looking_source_files_are_not_test_files", func(t *testing.T) {
		// A case-insensitive `test.` claimed latest.tsx, contest.py,
		// pretest.js — so citing an ordinary source file resolved a
		// coverage claim.
		for _, p := range []string{"frontend/latest.tsx", "cmd/contest.py", "web/pretest.js"} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, p, "export const LatestVersion = 1;\n")
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+p+" | |\n")
			if res := run(t, ws, scratch); res.MatrixOK {
				t.Fatalf("%s is not a test file and must not resolve a claim: %+v", p, res)
			}
		}
		// ...while the CamelCase conventions it exists for still do.
		for _, p := range []string{"src/ChargeTest.java", "src/ChargeSpec.cs"} {
			ws, scratch := gitWorkspace(t), t.TempDir()
			greenVerify(t, scratch)
			writeFile(t, ws, p, "class Charge { void testChargeIsIdempotent() {} }\n")
			writeFile(t, ws, matrixRel, header+
				"| a.b | Thing | a | covered-deterministic | "+p+" | |\n")
			if res := run(t, ws, scratch); !res.MatrixOK {
				t.Fatalf("%s IS a test file by its convention and must resolve: %+v", p, res)
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

	// ---------------------------------------------------------------
	// #1598 — a citation resolves by EXISTENCE, but the claim is
	// EXECUTION: a test the suite itself reported skipped this pass
	// satisfies the grep while the feature stays unexercised.
	// ---------------------------------------------------------------

	t.Run("cited_test_the_suite_reported_skipped_is_red", func(t *testing.T) {
		for _, tc := range []struct{ name, path, body, cite, scriptOut string }{
			{"go", "e2e/flaky_test.go", "package e2e\n\nfunc TestFlaky(t *testing.T) {}\n",
				"TestFlaky (e2e/flaky_test.go)", "--- SKIP: TestFlaky (0.00s)"},
			// A go SUBTEST report names the parent with a slash: the boundary
			// match must still see it — the separator is never an identifier
			// character.
			{"go subtest", "e2e/flaky_test.go", "package e2e\n\nfunc TestFlaky(t *testing.T) {}\n",
				"TestFlaky (e2e/flaky_test.go)", "--- SKIP: TestFlaky/case_a (0.00s)"},
			{"pytest", "tests/test_flaky.py", "def test_flaky():\n    pass\n",
				"test_flaky (tests/test_flaky.py)", "tests/test_flaky.py::test_flaky SKIPPED [ 50%]"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ws, scratch := gitWorkspace(t), t.TempDir()
				verifyScript(t, scratch, "#!/bin/sh\necho '"+tc.scriptOut+"'\nexit 0\n")
				writeFile(t, ws, tc.path, tc.body)
				writeFile(t, ws, matrixRel, header+
					"| a.b | Thing | a | covered-deterministic | "+tc.cite+" | |\n")
				res := run(t, ws, scratch)
				if res.MatrixOK || !strings.Contains(res.LogTail, "UNEXECUTED CLAIM") {
					t.Fatalf("a covered row resting on a test the suite reported skipped must be red: %+v", res)
				}
				if !res.Passed {
					t.Fatalf("the suite verdict stays independent — the run WAS green, the claim is the lie: %+v", res)
				}
			})
		}
	})

	t.Run("a_skip_report_for_a_name_prefixed_test_is_not_the_cited_one", func(t *testing.T) {
		// The skip report must name the CITED test on identifier boundaries:
		// TestFlakyBackend / test_flaky_2 are DIFFERENT tests, and their skip
		// says nothing about whether TestFlaky / test_flaky ran.
		for _, tc := range []struct{ name, path, body, cite, scriptOut string }{
			{"go", "e2e/flaky_test.go",
				"package e2e\n\nfunc TestFlaky(t *testing.T) {}\nfunc TestFlakyBackend(t *testing.T) {}\n",
				"TestFlaky (e2e/flaky_test.go)", "--- SKIP: TestFlakyBackend (0.00s)"},
			{"pytest", "tests/test_flaky.py",
				"def test_flaky():\n    pass\ndef test_flaky_2():\n    pass\n",
				"test_flaky (tests/test_flaky.py)", "test_flaky_2 SKIPPED [ 50%]"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ws, scratch := gitWorkspace(t), t.TempDir()
				verifyScript(t, scratch, "#!/bin/sh\necho '"+tc.scriptOut+"'\nexit 0\n")
				writeFile(t, ws, tc.path, tc.body)
				writeFile(t, ws, matrixRel, header+
					"| a.b | Thing | a | covered-deterministic | "+tc.cite+" | |\n")
				res := run(t, ws, scratch)
				if !res.MatrixOK {
					t.Fatalf("a skip report for %q must not redden the citation of the prefix it shares: %+v", tc.scriptOut, res)
				}
			})
		}
	})

	t.Run("a_passing_cited_test_is_not_an_unexecuted_claim", func(t *testing.T) {
		// Negative evidence only: a verbose green run names its tests with
		// PASS markers, and a summary line carrying the word skipped names
		// no test — neither may trip the detection, or every loud suite
		// reddens.
		ws, scratch := gitWorkspace(t), t.TempDir()
		verifyScript(t, scratch, "#!/bin/sh\necho '=== RUN   TestReal'\necho '--- PASS: TestReal (0.00s)'\necho '5 passed, 1 skipped in 0.10s'\nexit 0\n")
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("a green run reporting the cited test PASSED must stay green: %+v", res)
		}
	})

	t.Run("skip_evidence_in_a_fellow_row_does_not_acquit_or_accuse", func(t *testing.T) {
		// The skip report must name the CITED test: another test's skip is
		// not this row's business (the runner ran what the row claims).
		ws, scratch := gitWorkspace(t), t.TempDir()
		verifyScript(t, scratch, "#!/bin/sh\necho '--- PASS: TestReal (0.00s)'\necho '--- SKIP: TestOther (0.00s)'\nexit 0\n")
		writeFile(t, ws, "e2e/real_test.go", "package e2e\n\nfunc TestReal(t *testing.T) {}\nfunc TestOther(t *testing.T) {}\n")
		writeFile(t, ws, matrixRel, header+
			"| a.b | Thing | a | covered-deterministic | TestReal (e2e/real_test.go) | |\n")
		res := run(t, ws, scratch)
		if !res.MatrixOK {
			t.Fatalf("a skip report for a DIFFERENT test must not redden this row: %+v", res)
		}
	})
}

// TestE2ECoverageConvergedRequiresAnExecutedSuite pins #1598's delivery
// condition on the CONVERGENCE side: `gate.converged` is the only edge into
// done, so it is the run's green — and it must test `skipped` explicitly.
// verify_run already answers passed=false on a missing script (#1711), which
// blocks convergence transitively; but "the suite ran" is the property the
// unattended-merge decision rests on, and a conjunction that never names it
// is one verify_run refactor away from reading "the gate was skipped" as
// "the gate passed" again — which is exactly how the ticket found it.
//
// The expression is EVALUATED, not grepped (the lesson of
// TestGateFailLogCarriesTheReviewOnASkippedBuild): a gate expression parses
// and validates clean and only runs once something else already went wrong.
func TestE2ECoverageConvergedRequiresAnExecutedSuite(t *testing.T) {
	pr := parseBotUnit("e2e-coverage/main.bot")
	if pr.File == nil {
		t.Fatalf("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile produced no Workflow")
	}
	raw, ok := cr.Workflow.Nodes["gate"]
	if !ok {
		t.Fatalf("no gate node")
	}
	cn, ok := raw.(*ir.ComputeNode)
	if !ok {
		t.Fatalf("gate is %T, want *ir.ComputeNode", raw)
	}
	var converged *expr.AST
	for _, e := range cn.Exprs {
		if e != nil && e.Key == "converged" {
			converged = e.AST
		}
	}
	if converged == nil {
		t.Fatalf("gate carries no converged expression")
	}

	// outputs.<node>.<field>, as the runtime supplies them.
	ctxFor := func(passed, skipped, matrixOK, complete, scoped bool, uncovered int) *expr.Context {
		return &expr.Context{
			Outputs: func(path []string) any {
				if len(path) < 2 {
					return nil
				}
				switch path[0] + "." + path[1] {
				case "verify_run.passed":
					return passed
				case "verify_run.skipped":
					return skipped
				case "verify_run.matrix_ok":
					return matrixOK
				case "verify_run.scoped":
					return scoped
				case "verify_run.uncovered_rows":
					return uncovered
				case "campaign.coverage_complete":
					return complete
				}
				return nil
			},
		}
	}
	eval := func(t *testing.T, passed, skipped, matrixOK, complete, scoped bool, uncovered int) bool {
		t.Helper()
		got, err := converged.Eval(ctxFor(passed, skipped, matrixOK, complete, scoped, uncovered))
		if err != nil {
			t.Fatalf("converged does not evaluate on (passed=%v skipped=%v matrix_ok=%v complete=%v scoped=%v uncovered=%d): %v",
				passed, skipped, matrixOK, complete, scoped, uncovered, err)
		}
		b, ok := got.(bool)
		if !ok {
			t.Fatalf("converged evaluated to %T, want bool", got)
		}
		return b
	}

	t.Run("a fully green executed run converges", func(t *testing.T) {
		if !eval(t, true, false, true, true, false, 0) {
			t.Error("suite green + matrix_ok + complete + no uncovered rows must converge")
		}
	})
	t.Run("a skipped suite never converges, whatever passed says", func(t *testing.T) {
		// THE case: "the gate was skipped" must not read as "the gate
		// passed" — even in a shape where passed came out true.
		if eval(t, true, true, true, true, false, 0) {
			t.Error("converged on skipped=true: a gate that did not run certified delivery")
		}
		if eval(t, false, true, true, true, false, 0) {
			t.Error("converged on a refused (skipped) run")
		}
	})
	t.Run("a red suite or matrix still blocks", func(t *testing.T) {
		if eval(t, false, false, true, true, false, 0) {
			t.Error("converged on a red suite")
		}
		if eval(t, true, false, false, true, false, 0) {
			t.Error("converged on a red matrix")
		}
	})
	t.Run("uncovered rows block a whole-app run", func(t *testing.T) {
		if eval(t, true, false, true, true, false, 2) {
			t.Error("converged with uncovered rows remaining and no scope")
		}
		if !eval(t, true, false, true, true, true, 2) {
			t.Error("a scoped run must be allowed to converge with out-of-scope rows uncovered")
		}
	})
}
