package pluginsource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A fetch must leave NOTHING running. Since git 2.48 every command that writes
// to a repository ends by detaching `git maintenance run --auto`: that process
// calls setsid() and closes its standard descriptors, which is exactly what
// os/exec waits for, so CombinedOutput returns while it still holds
// `.git/objects/maintenance.lock` and writes under `.git/objects`. The cache
// checkout is the caller's to delete, and the detached writer is not waitable —
// it raced `t.TempDir()`'s cleanup into a merge-queue ejection (#821).
//
// The oracle is the argv git receives, not a timing observation: what iterion
// controls is the config it passes, and a run on git < 2.48 would show nothing
// either way.
func TestFetch_RunsGitWithoutAutoMaintenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the recording shim is a POSIX shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	origin := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v: %s", args, gerr, out)
		}
	}
	runGit("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "plugin.yaml"),
		[]byte("name: p\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "seed")
	runGit("tag", "v0.1.0")

	// The shim records the argv and delegates, so the fetch really runs and the
	// recorded arguments are the ones git actually got.
	shimDir := t.TempDir()
	argvLog := filepath.Join(shimDir, "argv.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argvLog + "'\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := &Fetcher{CacheDir: t.TempDir()}
	if _, err := f.Fetch(context.Background(), PluginSource{
		ID: "ps-1", TenantID: "team-a", Name: "p", GitURL: origin, Ref: "v0.1.0", Enabled: true,
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	recorded, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the shim recorded nothing — the fetch did not go through PATH: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	// init, remote add, fetch, checkout: fewer means the assertion below is
	// vouching for invocations that never happened.
	if len(lines) < 4 {
		t.Fatalf("expected the fetch's four git invocations, recorded %d: %q", len(lines), lines)
	}
	for _, argv := range lines {
		if !strings.Contains(argv, "-c maintenance.auto=false") || !strings.Contains(argv, "-c gc.auto=0") {
			t.Errorf("git invocation may spawn a detached maintenance process that outlives the fetch: %q", argv)
		}
	}
}
