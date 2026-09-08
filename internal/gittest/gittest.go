// Package gittest is the one place iterion's tests build a `git`
// subprocess, a throwaway repository, or unregister a worktree.
//
// It exists because the two properties a test's git command must have are
// invisible at the call site, and a guard copied per helper drifts:
//
//   - Auto-maintenance must be OFF. Since git 2.48 every writing command
//     (fetch, commit, merge, rebase, am) ends by detaching
//     `git maintenance run --auto`; that child calls setsid() and closes its
//     descriptors, which is exactly what os/exec waits for, so
//     CombinedOutput returns while it still holds
//     `.git/objects/maintenance.lock` and writes under `.git/objects`. A
//     test builds its repository under t.TempDir(), which Go removes the
//     moment the test returns — so the removal races a writer nothing can
//     wait for. On CI's git that has ejected pull requests from the merge
//     queue (issues #821, #828); the host's git 2.43 shows nothing.
//
//   - The environment must be scrubbed and pinned. An inherited GIT_DIR or
//     GIT_COMMON_DIR makes the command answer about another repository —
//     `worktree add` then writes into somebody else's admin dir — and an
//     absent identity makes `git commit` fail on a machine with no global
//     gitconfig, which is every CI runner.
//
// Both are baked into Cmd, so a caller cannot forget either. The guard that
// keeps new call sites coming here is
// pkg/git.TestEveryTestGitCallerDisablesAutoMaintenance.
package gittest

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// testConfig is applied to every command on top of NoAutoMaintenance.
//
// init.defaultBranch is here because Env cuts the global config off: without
// it `git init` (with no -b) lands on git's built-in default and warns, which
// is a fixture's branch name deciding to change under it.
var testConfig = []string{"-c", "init.defaultBranch=main"}

// Cmd builds `git args...` to run in dir, with auto-maintenance refused and
// Env applied. Use it when the caller needs the *exec.Cmd itself (stdin, a
// context, an exit code it asserts on); Run and Try cover the rest.
func Cmd(dir string, args ...string) *exec.Cmd {
	full := gitlib.NoAutoMaintenance(append(slices.Clone(testConfig), args...)...)
	cmd := exec.Command("git", full...) // #nosec G204 -- test-only helper, args come from the test
	cmd.Dir = dir
	cmd.Env = Env()
	return cmd
}

// Env is the environment every git command a test runs gets. It is the scrub
// Cmd applies just above: gitlib.SanitizeEnv drops the redirection variables
// (GIT_DIR, GIT_COMMON_DIR, GIT_INDEX_FILE…) that would make the command
// answer about another repository.
//
// The operator's own config is cut off too (GIT_CONFIG_SYSTEM/GLOBAL →
// os.DevNull) so a `commit.gpgsign = true`, a `core.hooksPath` or an alias in
// ~/.gitconfig cannot decide whether a test passes; the identity variables
// stand in for the config that is then no longer there, which is also the
// state of every CI runner. LC_ALL/LANG pin git's own diagnostics to English
// so a caller may branch on them. Cmd's testConfig restores
// init.defaultBranch, the one setting that cut genuinely removes.
func Env() []string {
	return append(gitlib.SanitizeEnv(os.Environ()),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=iterion test",
		"GIT_AUTHOR_EMAIL=test@iterion.invalid",
		"GIT_COMMITTER_NAME=iterion test",
		"GIT_COMMITTER_EMAIL=test@iterion.invalid",
		"LC_ALL=C",
		"LANG=C",
	)
}

// Run runs `git args...` in dir and returns its trimmed combined output,
// failing the test when git exits non-zero.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := Try(dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// Try is Run for a command the caller expects may fail: it returns git's
// trimmed combined output alongside the error instead of ending the test.
func Try(dir string, args ...string) (string, error) {
	out, err := Cmd(dir, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
