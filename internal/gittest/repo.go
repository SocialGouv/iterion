package gittest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// InitRepo turns dir into a git repository with one commit on `main` and
// returns that commit's SHA.
//
// The identity and the signing opt-out are written into the repository's own
// config, not only into Env: a command iterion itself spawns during the test
// (finalizeWorktree's squash, the runner's bank) does not inherit this
// package's environment, and CI has no global identity to fall back on.
func InitRepo(t testing.TB, dir string) string {
	t.Helper()
	Run(t, dir, "init", "-q", "-b", "main")
	Run(t, dir, "config", "user.name", "iterion test")
	Run(t, dir, "config", "user.email", "test@iterion.invalid")
	Run(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", dir, err)
	}
	Run(t, dir, "add", "README.md")
	Run(t, dir, "commit", "-q", "-m", "init")
	return Run(t, dir, "rev-parse", "HEAD")
}

// SourceRepo returns a throwaway repository under t.TempDir(), ready to be
// the workspace of a `worktree: auto` run.
//
// This is the answer to the other half of the class: a run whose workspace is
// the checkout the tests THEMSELVES run in registers its per-run worktree in
// the developer's own repository, and the registration outlives the checkout
// t.TempDir() deletes (issue #870 — 1 773 dead entries, 2.5 GB, measured).
// A repository the test owns is deleted whole, admin dir included.
func SourceRepo(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	InitRepo(t, dir)
	return dir
}

// RemoveWorktree deletes the linked worktree at path and drops the one
// administrative entry that names it, for a test that genuinely had to run
// against a repository it does not own.
//
// It never calls `git worktree prune`: that sweeps the whole repository and
// removes the registration of ANY worktree missing at that instant — an
// operator's checkout on an unmounted volume among them, whose index (and
// staged work) lives in that entry. Matching on the recorded path is the
// same discipline as pkg/worktreepool's pruneWorktreeRegistration, and for
// the same reason: git disambiguates colliding basenames with a numeric
// suffix, so the entry named after a run id may well be somebody else's.
func RemoveWorktree(t testing.TB, repo, path string) {
	t.Helper()
	// Captured before the removal: EvalSymlinks cannot resolve a path that
	// no longer exists, and a store reached through a symlink would then
	// never match the recorded pointer.
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	resolved = filepath.Clean(resolved)

	// Best effort: the directory may already be gone, which is precisely the
	// case the registration sweep below exists for.
	_, _ = Try(repo, "worktree", "remove", "--force", path)

	common, err := commonDir(repo)
	if err != nil {
		t.Fatalf("locate git common dir of %s: %v", repo, err)
	}
	if pruned, err := pruneRegistration(common, resolved); err != nil {
		t.Fatalf("prune worktree registration for %s: %v", resolved, err)
	} else if !pruned {
		// Nothing named it: `worktree remove` above already took the entry,
		// or the worktree was never registered here. Both are fine.
		return
	}
}

// commonDir returns the absolute path of the repository-wide git directory
// of the repository containing dir — the one that holds `worktrees/`. For a
// linked worktree that is the MAIN repository's .git, which is exactly where
// a per-run worktree gets registered.
func commonDir(dir string) (string, error) {
	out, err := Try(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

// pruneRegistration removes the administrative entry under
// <common>/worktrees whose `gitdir` pointer names resolvedPath, and only
// that one. Reports whether an entry was removed.
func pruneRegistration(common, resolvedPath string) (bool, error) {
	root := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		admin := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, "gitdir")) // #nosec G304 -- under git's own admin dir
		if err != nil {
			continue
		}
		// The pointer names <worktree>/.git; compare the worktree itself.
		if filepath.Clean(filepath.Dir(strings.TrimSpace(string(raw)))) != resolvedPath {
			continue
		}
		if err := os.RemoveAll(admin); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
