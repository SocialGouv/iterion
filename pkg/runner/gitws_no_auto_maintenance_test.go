package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The runner's git commands must leave NOTHING running. Since git 2.48 a
// `git fetch` ends by detaching `git maintenance run --auto`: that process
// calls setsid() and closes stdin/stdout/stderr — exactly what os/exec waits
// on — so runGitOutEnv returns while it is still holding
// `.git/objects/maintenance.lock` and writing under `.git/objects`. The
// per-run clone at `<WorkDir>/repos/<RunID>` is removed when the run returns
// and again at the next attempt, so that removal would race a process the
// runner has no handle on, and gitOpTimeout would bound nothing that escaped
// it (#828 item 1; the mechanism is measured in #821).
//
// The oracle is the argv git receives, recorded by a PATH shim that delegates
// to the real binary: what iterion controls is the config it passes, and a
// run on git < 2.48 would show nothing either way. The two bank-family fetch
// paths are driven for real; the third (prepareRepoWorkspace) needs a public
// forge host, and rides the same single chokepoint — runGitOutEnv is the only
// place this package builds a git subprocess.
func TestRunnerGitRunsWithoutAutoMaintenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the recording shim is a POSIX shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	r, msg, work, _, _ := bankFixture(t)
	branch := "iterion/run-" + msg.RunID
	gitOut(t, work, "commit", "--allow-empty", "-m", "earlier attempt")
	oldHead := gitOut(t, work, "rev-parse", "HEAD")
	gitOut(t, work, "push", "origin", "HEAD:refs/heads/"+branch)
	gitOut(t, work, "commit", "--allow-empty", "-m", "finished attempt")
	head := gitOut(t, work, "rev-parse", "HEAD")

	// The shim goes on PATH only now, so the log holds the runner's own
	// invocations and not the fixture's.
	shimDir := t.TempDir()
	argvLog := filepath.Join(shimDir, "argv.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argvLog + "'\nexec '" + realGit + "' \"$@\"\n"
	if werr := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); werr != nil {
		t.Fatal(werr)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx := context.Background()
	if !r.bankSupersedes(ctx, msg, work, "", branch, oldHead, head) {
		t.Fatalf("precondition: the finished head contains the earlier one, so the bank must be allowed")
	}
	r.preserveSupersededChain(ctx, msg, work, "", branch, oldHead, head)

	recorded, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the shim recorded nothing — the runner's git did not go through PATH: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	fetches := 0
	for _, argv := range lines {
		// The subcommand as a token, never a substring of the whole line: a
		// count that only matches once the config prefix is there would be
		// vouched for by the very fix it is meant to guard.
		if slices.Contains(strings.Fields(argv), "fetch") {
			fetches++
		}
		if !strings.Contains(argv, "-c maintenance.auto=false") || !strings.Contains(argv, "-c gc.auto=0") {
			t.Errorf("git invocation may spawn a detached maintenance process that outlives the command: %q", argv)
		}
	}
	// One per exercised path: fewer means the assertion above vouched for
	// invocations that never ran.
	if fetches < 2 {
		t.Fatalf("expected both bank-family fetches, recorded %d in %q", fetches, lines)
	}
}
