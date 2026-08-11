package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo initialises a fresh repo in t.TempDir() with a single committed
// file ("a.txt") so each test can mutate from a known baseline. The git
// CLI is required on PATH; tests are skipped otherwise so CI without git
// (rare but possible in stripped containers) doesn't fail with a confusing
// exec error.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git not on PATH — required in CI; skipping here would silently drop the whole pkg/git suite")
		}
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRun(t, dir, "init", "-q", "-b", "main")
	mustRun(t, dir, "config", "user.email", "test@example.com")
	mustRun(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "a.txt")
	mustRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// gitTestEnv is the environment every helper in this suite runs git under.
//
// The suite builds throwaway repositories and configures them with
// `git config`, then asserts on what git reports back. That only holds if the
// repository's own config is authoritative — and it is not, by default:
// GIT_AUTHOR_* / GIT_COMMITTER_* OVERRIDE it, and the operator's global config
// answers for anything the repo leaves unset.
//
// Both leak in production. A sandboxed run with `host_state: none` injects the
// four identity variables into the container (runtime.applyGitIdentityEnv), so
// `TestLogAllowsTabsInUserControlledFields` — which sets a tab-bearing author
// locally and expects to read it back — instead saw the run's own bot identity
// and failed. Vetty held four dependency PRs on that failure alone
// (SocialGouv/iterion #392/#395/#397/#399), reporting a red build for a bump
// that had broken nothing.
// Every GIT_* variable is dropped rather than a chosen few. A denylist here is
// just a list of the leaks already paid for: the identity quartet was the one
// that held the PRs, but GIT_INDEX_FILE — which git sets in every hook
// environment and under `git rebase --exec` / `git bisect run` — makes the
// helpers stage into the ambient index, and GIT_DIR, GIT_WORK_TREE,
// GIT_OBJECT_DIRECTORY and GIT_NAMESPACE each redirect something else. These
// helpers need nothing from the caller's git environment, so they inherit none
// of it.
//
// Scope: this covers what the HELPERS do. The package's own readers (Status,
// Log, RevParseHead) run git with the caller's environment, so an ambient
// GIT_INDEX_FILE still reaches them and the suite still fails under one —
// differently, and for a reason that lives in the production code rather than
// here. Every function in this package takes an explicit dir, which makes that
// inheritance look accidental; deciding it is a separate change.
func gitTestEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	// Neutralise the operator's own ~/.gitconfig too: a global `commit.gpgsign`
	// or `init.defaultBranch` would otherwise reach these repositories.
	return append(out, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

func mustRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGitTestEnvIsHermetic pins the property the whole suite rests on: inside
// these tests the repository's own config decides who authored a commit, not
// whatever the surrounding process was started with.
//
// Without it the suite reports on the environment it happens to run in. That is
// not theoretical — it is how four dependency PRs came to be held on a red
// build none of them caused.
func TestGitTestEnvIsHermetic(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "ambient[bot]")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "ambient[bot]")
	t.Setenv("GIT_COMMITTER_EMAIL", "ambient@example.invalid")
	// And the index: git sets GIT_INDEX_FILE in every hook environment and
	// under `git rebase --exec` / `git bisect run`. Inherited, it does not
	// just skew an assertion — the suite fails outright and leaves a stray
	// index at the ambient path, staging into whatever invoked it.
	//
	// The scrub covers every GIT_* variable, but this test only pins the ones
	// the HELPERS own. GIT_DIR and the object-store family also redirect the
	// package's production readers, which inherit the caller's environment by
	// design; whether they should is a separate question from this suite's
	// hermeticity.
	ambientIndex := filepath.Join(t.TempDir(), "ambient-index")
	t.Setenv("GIT_INDEX_FILE", ambientIndex)

	dir := gitRepo(t)
	mustRun(t, dir, "config", "user.name", "Local Author")
	mustRun(t, dir, "config", "user.email", "local@example.com")
	sha := commit(t, dir, "hermetic.txt", "x\n", "hermetic")

	entries, err := Log(dir, "", sha)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	for _, e := range entries {
		if e.SHA != sha {
			continue
		}
		if e.Author != "Local Author" {
			t.Fatalf("author: got %q, want the repo's own config — the ambient environment is deciding what this suite asserts on", e.Author)
		}
		if _, err := os.Stat(ambientIndex); err == nil {
			t.Errorf("the suite wrote to the ambient GIT_INDEX_FILE (%s) — it is staging into whatever git invoked it", ambientIndex)
		}
		return
	}
	t.Fatalf("commit %s not found in %+v", sha, entries)
}

func TestStatusEmptyClean(t *testing.T) {
	dir := gitRepo(t)
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %+v", files)
	}
}

func TestStatusModifiedAddedDeletedUntracked(t *testing.T) {
	dir := gitRepo(t)
	// Modify a.txt
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// New untracked file
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add another file then delete it after committing.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "c.txt")
	mustRun(t, dir, "commit", "-q", "-m", "add c")
	if err := os.Remove(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}

	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != "M" {
		t.Errorf("a.txt: want M, got %q", got["a.txt"])
	}
	if got["b.txt"] != "??" {
		t.Errorf("b.txt: want ??, got %q", got["b.txt"])
	}
	if got["c.txt"] != "D" {
		t.Errorf("c.txt: want D, got %q", got["c.txt"])
	}
}

func TestStatusRename(t *testing.T) {
	dir := gitRepo(t)
	mustRun(t, dir, "mv", "a.txt", "renamed.txt")
	mustRun(t, dir, "add", "-A")
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %+v", files)
	}
	f := files[0]
	if f.Status != "R" {
		t.Errorf("status: want R, got %q", f.Status)
	}
	if f.Path != "renamed.txt" {
		t.Errorf("path: want renamed.txt, got %q", f.Path)
	}
	if f.OldPath != "a.txt" {
		t.Errorf("oldpath: want a.txt, got %q", f.OldPath)
	}
}

func TestStatusNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := Status(dir)
	if err != ErrNotGitRepo {
		t.Fatalf("want ErrNotGitRepo, got %v", err)
	}
}

func TestDiffModified(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "a.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before == nil || *d.Before != "hello\n" {
		t.Errorf("before: %v", d.Before)
	}
	if d.After == nil || *d.After != "changed\n" {
		t.Errorf("after: %v", d.After)
	}
	if d.Binary {
		t.Errorf("Binary should be false for text diff")
	}
}

func TestDiffUntracked(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "fresh.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before != nil {
		t.Errorf("before should be nil for untracked, got %q", *d.Before)
	}
	if d.After == nil || *d.After != "brand new\n" {
		t.Errorf("after: %v", d.After)
	}
}

func TestDiffDeleted(t *testing.T) {
	dir := gitRepo(t)
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "a.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before == nil || *d.Before != "hello\n" {
		t.Errorf("before: %v", d.Before)
	}
	if d.After != nil {
		t.Errorf("after should be nil for deleted, got %q", *d.After)
	}
}

func TestDiffBinary(t *testing.T) {
	dir := gitRepo(t)
	// A NUL byte is the canonical "binary" signal git itself uses.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "blob.bin")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.Binary {
		t.Errorf("Binary should be true")
	}
	if d.Before != nil || d.After != nil {
		t.Errorf("Before/After should be nil for binary, got %v / %v", d.Before, d.After)
	}
}

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"a.txt", false},
		{"sub/a.txt", false},
		{"", true},
		{"/etc/passwd", true},
		{"../escape", true},
		{"sub/../../escape", true},
		{`C:\Windows\system.ini`, true},
		{`sub\file.txt`, true},
		{"with\x00nul", true},
	}
	for _, c := range cases {
		err := ValidateRelPath(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateRelPath(%q): wantErr=%v, got %v", c.in, c.wantErr, err)
		}
	}
}

func TestDiffWorktreeSymlinkReadsLinkTextNotTarget(t *testing.T) {
	dir := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	p, err := Diff(dir, "link.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if p.After == nil {
		t.Fatal("After is nil")
	}
	if *p.After != outside {
		t.Fatalf("Diff followed symlink target: got %q want link text %q", *p.After, outside)
	}
}
