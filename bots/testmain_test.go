package bots

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The git-exec tests in this package (dep-update-guard, branch-improve
// gate, campaign gate commands, docs-refresh scope…) create throwaway
// repos and commit in them. The operator's global/system git config must
// not leak into those repos: commit.gpgsign=true hangs every test commit
// on a pinentry that has no TTY, which reads as a 600s package timeout
// with no failing test named. CI never sees it (runners sign nothing) —
// only contributors do.
// Blanking both files also drops any safe.directory the SYSTEM config
// supplied, and one test in this package — TestGitignoreKeepsClaudeRuntime
// JunkIgnored — asks git about the REAL checkout rather than a throwaway
// repo. Where that checkout is foreign-owned (a devcontainer or CI
// bind-mount, which is exactly what a system-wide safe.directory exists
// to cover) git would then refuse it for dubious ownership, and the test
// reads the error as "not a git work tree" and SKIPS — coverage lost
// silently, under a misleading reason. So re-supply the exception:
// GIT_CONFIG_KEY_* lands in git's `command` scope, which counts as
// protected configuration, and safe.directory is honoured from there
// (it is ignored from repo-local config by design). Relaxing an
// ownership check for the test process only, never a signing or
// identity setting, keeps the isolation above intact.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Setenv("GIT_CONFIG_COUNT", "1")
	os.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	os.Setenv("GIT_CONFIG_VALUE_0", "*")
	os.Exit(m.Run())
}

// TestPackageGitEnvSurvivesForeignOwner pins the second half of TestMain
// against the first: blanking the config files must not cost the git-exec
// tests their safe.directory exception. GIT_TEST_ASSUME_DIFFERENT_OWNER is
// git's own switch for the ownership check, so this reproduces a
// devcontainer/CI bind-mount without needing to chown anything, and it runs
// against a throwaway repo so the result never depends on how THIS checkout
// happens to be owned. Without the GIT_CONFIG_KEY_0 lines it fails with
// "detected dubious ownership".
func TestPackageGitEnvSurvivesForeignOwner(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	if out, err := exec.Command(git, "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd := exec.Command(git, "-C", repo, "rev-parse", "--is-inside-work-tree")
	cmd.Env = append(os.Environ(), "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git refused a foreign-owned repo under this package's env — the git-exec tests would skip or fail wherever the checkout is bind-mounted: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("rev-parse --is-inside-work-tree = %q, want %q", got, "true")
	}
}
