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

	// Observed live on iterion#394/#395/#411 (2026-08-12): three holds reported
	// "build/tests not green" over an excerpt containing nothing but a list of
	// `ok` lines and a bare `FAIL`. A test runner prints its per-package
	// successes AFTER the package that failed, so a blind tail of the output is
	// systematically the wrong excerpt — it keeps the successes and drops the
	// one line naming what broke. The operator then cannot tell a real
	// regression from an environment failure without a run log nobody kept.
	t.Run("the excerpt keeps the failing line, not just the trailing successes", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		// The shape `go test ./...` produces: the failure first, then far more
		// than the excerpt budget of passing packages after it.
		var b strings.Builder
		b.WriteString("#!/bin/sh\n")
		b.WriteString("echo '--- FAIL: TestAwaitTerminal_LoadRunMissTransient (0.42s)'\n")
		b.WriteString("echo 'FAIL\tgithub.com/SocialGouv/iterion/cmd/iterion\t0.51s'\n")
		for i := 0; i < 400; i++ {
			b.WriteString("echo 'ok  \tgithub.com/SocialGouv/iterion/pkg/filler" +
				strings.Repeat("x", 20) + "\t(cached)'\n")
		}
		b.WriteString("echo FAIL\n")
		b.WriteString("exit 1\n")
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if res.Passed {
			t.Fatal("a script exiting 1 must not pass")
		}
		if !strings.Contains(res.LogTail, "TestAwaitTerminal_LoadRunMissTransient") {
			t.Errorf("the excerpt drops the name of what failed, so the hold cannot be diagnosed from the report; got:\n%s", res.LogTail)
		}
		if !strings.Contains(res.LogTail, "cmd/iterion") {
			t.Errorf("the excerpt drops the failing package; got:\n%s", res.LogTail)
		}
	})

	// Revi's R009763 on the fix above: keeping the LAST matches is the same
	// mistake as keeping the last lines. A cascade — a suite where one broken
	// package fails many subtests — pushes the first failure, the one that
	// usually explains the rest, out of any tail-shaped budget.
	t.Run("under a cascade the excerpt keeps the FIRST failures", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		var b strings.Builder
		b.WriteString("#!/bin/sh\n")
		for i := 0; i < 120; i++ {
			b.WriteString("echo '--- FAIL: TestCascade" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i) + " (0.01s)'\n")
			b.WriteString("echo '    x_test.go:1: boom'\n")
		}
		b.WriteString("exit 1\n")
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if !strings.Contains(res.LogTail, "TestCascade000") {
			t.Errorf("the first failure is the one that explains the cascade, and it was dropped; got:\n%s", res.LogTail)
		}
	})

	// Revi's second question on the same fix. A Go compile failure prints
	// `file.go:12:5: undefined: Baz` — matching no failure keyword — and only
	// then `FAIL pkg [build failed]`. Naming the package without the
	// diagnostic does not tell an operator what broke, which is the whole
	// point of the excerpt. Likewise `--- FAIL: TestX` without the assertion
	// that follows it.
	t.Run("the excerpt carries the diagnostic, not only the verdict line", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		var b strings.Builder
		b.WriteString("#!/bin/sh\n")
		b.WriteString("echo 'pkg/foo/bar.go:12:5: undefined: Baz'\n")
		b.WriteString("echo 'FAIL\tgithub.com/SocialGouv/iterion/pkg/foo [build failed]'\n")
		b.WriteString("echo '--- FAIL: TestThing (0.02s)'\n")
		b.WriteString("echo '    thing_test.go:41: got 3, want 4'\n")
		for i := 0; i < 300; i++ {
			b.WriteString("echo 'ok  \tgithub.com/SocialGouv/iterion/pkg/other\t(cached)'\n")
		}
		b.WriteString("exit 1\n")
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if !strings.Contains(res.LogTail, "undefined: Baz") {
			t.Errorf("a compile diagnostic matches no failure keyword and must still survive; got:\n%s", res.LogTail)
		}
		if !strings.Contains(res.LogTail, "got 3, want 4") {
			t.Errorf("the assertion says WHY the test failed and is never on the --- FAIL line; got:\n%s", res.LogTail)
		}
	})

	// Revi's R253811, the counter-case to the head bias: a script chaining
	// install -> lint -> build -> test can match noisily in an early step that
	// does not fail the run (a resolver complaint, a deprecation banner), and
	// spending the whole budget there buries the step that actually failed.
	// Head-biased must not mean head-only.
	t.Run("a noisy early step does not bury the failure that ended the run", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		var b strings.Builder
		b.WriteString("#!/bin/sh\n")
		for i := 0; i < 200; i++ {
			b.WriteString("echo 'ERROR: pip dependency resolver complaint " + itoa(i) + "'\n")
		}
		b.WriteString("echo '--- FAIL: TestTheRealOne (0.03s)'\n")
		b.WriteString("echo '    real_test.go:9: the assertion that matters'\n")
		b.WriteString("exit 1\n")
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if !strings.Contains(res.LogTail, "TestTheRealOne") {
			t.Errorf("200 non-fatal early matches crowded out the failing step; got:\n%s", res.LogTail)
		}
	})

	// Revi's R20320f, and the sharpest of the round: splitting the budget
	// between a digest and the tail makes the excerpt STRICTLY WORSE than the
	// blind tail it replaced whenever nothing matches — and the keyword list
	// is not exhaustive and never will be. A make/gradle/cmake failure reads
	// "Error 2", which matches nothing here, so the whole ceiling has to fall
	// back to the tail rather than being half-spent on an empty digest.
	t.Run("a failure matching no keyword is not worse off than before", func(t *testing.T) {
		ws, scratch := t.TempDir(), t.TempDir()
		var b strings.Builder
		b.WriteString("#!/bin/sh\n")
		for i := 0; i < 200; i++ {
			b.WriteString("echo 'make[1]: Leaving directory /src/sub" + itoa(i) + "'\n")
		}
		b.WriteString("echo 'the real one: recipe for target build failed at line 5'\n")
		// ~2900 chars of trailing noise: inside a 4000 budget, outside a 2000 one.
		for i := 0; i < 60; i++ {
			b.WriteString("echo 'make[1]: Leaving directory /src/after" + itoa(i) + "'\n")
		}
		b.WriteString("exit 2\n")
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, scratch)
		if !strings.Contains(res.LogTail, "recipe for target build failed") {
			t.Errorf("splitting the budget for a digest that does not exist lost the failure the old blind tail kept; got %d chars:\n%s", len(res.LogTail), res.LogTail)
		}
	})
}

// itoa avoids pulling strconv in for one call site in a fixture generator.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
