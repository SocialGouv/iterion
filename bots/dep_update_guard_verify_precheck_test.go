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

	// Revi's R6d2b2b: POSIX sh executes any WORD containing a slash as a
	// pathname, prefix or no prefix. A listing produced by `git ls-files` or
	// `go list` carries no `./`, and produces the identical symptom — so a
	// guard keyed on the prefix fires or not depending on how the agent
	// happened to spell it.
	t.Run("bare relative paths, no ./ prefix, are rejected too", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "pkg", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, name := range []string{"a_test.go", "b_test.go", "c_test.go", "d_test.go", "e_test.go"} {
			rel := filepath.Join("pkg", "x", name)
			if err := os.WriteFile(filepath.Join(ws, rel), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, rel)
		}
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if !res.PrecheckRejected {
			t.Errorf("whether the guard fires must not depend on how the listing was spelled; got exit %d: %s", res.ExitCode, res.LogTail)
		}
		if res.Passed {
			t.Error("a script of unrunnable paths must not open the commit path")
		}
	})

	// Revi's R510ffe: the loop-back added above asks an agent that already
	// skipped authorship to write SOMETHING. A script that runs nothing exits
	// 0, which the gate reads as a verified build — so the cheapest way to
	// satisfy that pressure would be a green commit certifying no verification
	// at all. This is the floor that makes the loop-back safe to offer.
	for _, tc := range []struct {
		name, body string
	}{
		{"empty", ""},
		{"shebang and comments only", "#!/bin/sh\n# nothing to verify for this bump\n"},
		{"no-ops only", "#!/bin/sh\nset -e\ncd /tmp\nexit 0\n"},
		{"a bare true", "#!/bin/sh\ntrue\n"},
	} {
		t.Run("a vacuous script is rejected: "+tc.name, func(t *testing.T) {
			ws, scratch := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			res := run(t, ws, scratch)
			if res.Passed {
				t.Error("a script that runs nothing must not certify a build — it exits 0 for the same reason a passing one does")
			}
			if !res.PrecheckRejected {
				t.Errorf("it is an authorship defect and must be sent back, got exit %d: %s", res.ExitCode, res.LogTail)
			}
			if !strings.Contains(res.PrecheckReason, "EMPTY SCRIPT") {
				t.Errorf("the reason must name the defect, got %q", res.PrecheckReason)
			}
		})
	}

	// Revi's R78f11d. verify_system step 5 tells the agent, verbatim, that a
	// repo with genuinely no build or test system gets a script echoing that
	// and exiting 0. Counting echo as a no-op rejected the one shape the prompt
	// prescribes — and the rejection then asked for "the repo own build and
	// test commands", advice unfollowable for a repo that has none, so the loop
	// exhausted and held a bump that broke nothing.
	t.Run("the shape verify_system prescribes for a repo with no build system", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		body := "#!/bin/sh\necho 'this repo has no build or test system'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("the guard rejects the script its own prompt asks for: %q", res.PrecheckReason)
		}
		if !res.Passed {
			t.Errorf("it exits 0 and must pass, got exit %d: %s", res.ExitCode, res.LogTail)
		}
	})

	// Revi's R12dc35. The floor above judged a line by its first word, so the
	// most ordinary script shape there is — a no-op leading a chain — counted
	// as running nothing and was sent back twice, then held. That is the exact
	// failure this PR exists to remove, reintroduced by its own guard.
	t.Run("a no-op leading a chained command still counts as running", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "run-tests.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The shape Revi named: a no-op leads, the build follows.
		script := "#!/bin/sh\nset -e\ncd " + ws + " && ./run-tests.sh\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("a chained command is not a vacuous script: %q", res.PrecheckReason)
		}
		if !res.Passed {
			t.Errorf("a green script must pass, got exit %d: %s", res.ExitCode, res.LogTail)
		}
	})

	// Revi's Rac0c6b. The listing check required the path to be an existing
	// FILE, so `go list ./...` output (import paths that resolve to nothing)
	// and directories out of `find -type d` sailed through — while failing at
	// sh exactly like the unexecutable file did. POSIX is sharper: a word
	// containing a slash is never PATH-searched, so anything that is not an
	// executable regular file cannot run.
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"go list import paths", []string{
			"github.com/SocialGouv/iterion/pkg/git",
			"github.com/SocialGouv/iterion/pkg/store",
			"github.com/SocialGouv/iterion/pkg/runtime",
			"github.com/SocialGouv/iterion/pkg/runner",
			"github.com/SocialGouv/iterion/pkg/server",
		}},
		{"directories", []string{"pkg/git/", "pkg/store/", "pkg/runtime/", "pkg/runner/", "pkg/server/"}},
	} {
		t.Run("a listing of "+tc.name+" is rejected", func(t *testing.T) {
			ws, scratch := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(strings.Join(tc.lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			res := run(t, ws, scratch)
			if !res.PrecheckRejected {
				t.Errorf("these fail at sh just like an unexecutable file; got exit %d: %s", res.ExitCode, res.LogTail)
			}
			if res.Passed {
				t.Error("a script of unrunnable words must not open the commit path")
			}
		})
	}

	// Revi's R691cf0. A here-doc body is data. Writing a path list into one is
	// the natural way to feed a runner a set of paths — which is precisely what
	// the NOT COMMANDS rejection tells the agent to do, so scanning the body
	// would reject the remediation the message recommends.
	t.Run("a here-doc body is data, not commands", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "pkg", "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		var body []string
		for _, name := range []string{"w.go", "x.go", "y.go", "z.go", "q.go"} {
			rel := filepath.Join("pkg", "a", name)
			if err := os.WriteFile(filepath.Join(ws, rel), []byte("package a\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			body = append(body, rel)
		}
		if err := os.WriteFile(filepath.Join(ws, "runner.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nset -e\ncd " + ws + "\ncat > list.txt <<'EOF'\n" +
			strings.Join(body, "\n") + "\nEOF\n./runner.sh\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("the paths inside the here-doc are never executed: %q", res.PrecheckReason)
		}
		if !res.Passed {
			t.Errorf("a green script must pass, got exit %d: %s", res.ExitCode, res.LogTail)
		}
	})

	// Revi's Ree852e. A script's cwd moves as it runs, so resolving relative
	// words against the workspace root alone reads a subdirectory's helpers as
	// an unrunnable listing — a hold on a bump that builds, which is the very
	// failure mode being removed.
	t.Run("helpers invoked after a cd into a subdirectory are not a listing", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		sub := filepath.Join(ws, "tools")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		var calls []string
		for _, name := range []string{"build.sh", "test.sh", "lint.sh", "vet.sh", "tidy.sh"} {
			if err := os.WriteFile(filepath.Join(sub, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, "./"+name)
		}
		script := "#!/bin/sh\nset -e\ncd " + sub + "\n" + strings.Join(calls, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("these resolve and execute from the directory the script entered: %q", res.PrecheckReason)
		}
		if !res.Passed {
			t.Errorf("a green script must pass, got exit %d: %s", res.ExitCode, res.LogTail)
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
