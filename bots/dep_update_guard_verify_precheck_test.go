package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDepUpdateGuardVerifyPrechecks covers the two verify-script states that
// are AUTHORSHIP defects rather than red builds: no script at all, and a script
// that runs nothing.
//
// The difference matters. A red build is a fact about the bump; a step the
// agent skipped is not, and reported as a red build the two are
// indistinguishable — the operator reads "this bump breaks the repo" about a
// bump that breaks nothing. Reported as a precheck rejection they loop back
// into verify_build with the reason and self-heal, while the commit path stays
// just as closed.
//
// Both conditions are decidable from the file alone, which is why they are the
// only two here. A third check, rejecting a script whose lines are paths rather
// than commands (iterion#398), was written and removed: judging shell text
// statically produced a false positive on every review round — a leading `cd`,
// a here-doc body, a subdirectory helper, a backslash continuation, an artifact
// the script builds first — and a guard that holds a bump which builds inflicts
// exactly the failure it was added to prevent. What remains of it lives in the
// verify-build skill, as the rule that every line must be a command.
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

	// Revi's Reb56bb. The here-doc scan looks for << anywhere on a line, so a
	// quoted value or an end-of-line comment containing it would open a body
	// that never closes — swallowing the rest of the script, which then reads
	// as running nothing and is held as EMPTY SCRIPT.
	t.Run("a phantom here-doc must not swallow the script", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The << never has a matching terminator line: it sits in a quoted
		// value, not a here-doc. It is carried by a no-op so that the only
		// line which counts as running is the one it would swallow.
		script := "#!/bin/sh\ncd " + ws + "\nexport MSG='a << b'\n./run.sh\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		res := run(t, ws, scratch)
		if res.PrecheckRejected {
			t.Errorf("the commands after it are still commands: %q", res.PrecheckReason)
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
