package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedRevision reads a revision's object id out of a fixture repository.
func pinnedRevision(t *testing.T, dir, rev string) string {
	t.Helper()
	// NoAutoMaintenance, not internal/gittest: that package imports this one.
	cmd := exec.Command("git", NoAutoMaintenance("rev-parse", rev)...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// pinnedCommit writes a file and commits it, returning the new commit.
func pinnedCommit(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", name)
	mustRun(t, dir, "commit", "-q", "-m", "pinned "+content)
	return pinnedRevision(t, dir, "HEAD")
}

// TestMaterializePinTakesTheCommitTheOperatorRead is the install-time half of
// a pinned install: the commit decides, and the branch the commit happens to
// be on at this instant decides nothing.
//
// Asserted in BOTH directions, because a materialization that always took the
// branch tip would pass a one-direction test on a repository whose tip is
// still the pin: the pin is read while the branch has ALREADY moved past it,
// and the newer pin is then read while the branch sits where it is.
func TestMaterializePinTakesTheCommitTheOperatorRead(t *testing.T) {
	repo := gitRepo(t)
	first := pinnedCommit(t, repo, "pinned.txt", "first")
	second := pinnedCommit(t, repo, "pinned.txt", "second")
	if first == second {
		t.Fatalf("the fixture made one commit, not two: %s", first)
	}

	atFirst := filepath.Join(t.TempDir(), "tree")
	if err := MaterializePin(context.Background(), repo, first, atFirst); err != nil {
		t.Fatalf("MaterializePin(%s) = %v, want nil", first, err)
	}
	if got := readPinnedFile(t, atFirst); got != "first" {
		t.Errorf("the tree at %s holds %q, want %q — the pin was read, not the branch tip (which is %s)", first, got, "first", second)
	}
	// A materialization, not a working repository: nothing left behind can
	// move what an operator installed.
	if _, err := os.Stat(filepath.Join(atFirst, ".git")); !os.IsNotExist(err) {
		t.Errorf("stat .git in the materialized tree = %v, want it absent", err)
	}

	atSecond := filepath.Join(t.TempDir(), "tree")
	if err := MaterializePin(context.Background(), repo, second, atSecond); err != nil {
		t.Fatalf("MaterializePin(%s) = %v, want nil", second, err)
	}
	if got := readPinnedFile(t, atSecond); got != "second" {
		t.Errorf("the tree at %s holds %q, want %q", second, got, "second")
	}
}

func readPinnedFile(t *testing.T, tree string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(tree, "pinned.txt"))
	if err != nil {
		t.Fatalf("read the materialized file: %v", err)
	}
	return string(body)
}

// TestMaterializePinRefusesARefThatCanMove pins the refusal by name: a branch
// and a tag are pointers somebody else can move, so they are not an answer to
// "which bytes were reviewed".
func TestMaterializePinRefusesARefThatCanMove(t *testing.T) {
	repo := gitRepo(t)
	head := pinnedRevision(t, repo, "HEAD")
	mustRun(t, repo, "tag", "v1.0.0")

	for _, ref := range []string{"main", "v1.0.0", "HEAD", "", strings.ToUpper(head), head[:12]} {
		dest := filepath.Join(t.TempDir(), "tree")
		err := MaterializePin(context.Background(), repo, ref, dest)
		if err == nil {
			t.Errorf("MaterializePin(ref=%q) = nil, want a refusal", ref)
			continue
		}
		if !strings.Contains(err.Error(), "not a commit pin") {
			t.Errorf("MaterializePin(ref=%q) = %v, want it refused as not a commit pin", ref, err)
		}
		// Refused BEFORE git is asked anything: a half-initialised
		// destination would read as an install that nearly worked.
		if entries, statErr := os.ReadDir(dest); statErr == nil && len(entries) > 0 {
			t.Errorf("MaterializePin(ref=%q) left %d entries in %s, want nothing written", ref, len(entries), dest)
		}
	}
	// The pin itself still works, so the table above refuses shapes rather
	// than refusing everything.
	dest := filepath.Join(t.TempDir(), "tree")
	if err := MaterializePin(context.Background(), repo, head, dest); err != nil {
		t.Fatalf("MaterializePin(%s) = %v, want nil", head, err)
	}
}

// TestMaterializePinRefusesASourceGitCannotServe keeps the transport gate on
// the local-path lane: `a::b` is a remote helper to git wherever it appears,
// and a directory that is not a repository has no commit to read.
func TestMaterializePinRefusesASourceGitCannotServe(t *testing.T) {
	repo := gitRepo(t)
	head := pinnedRevision(t, repo, "HEAD")

	plain := t.TempDir()
	helper := filepath.Join(t.TempDir(), "ext::sh -c touch")
	if err := os.MkdirAll(helper, 0o755); err != nil {
		t.Skipf("this filesystem refuses a directory named like a remote helper: %v", err)
	}

	cases := []struct {
		name, source, want string
	}{
		{"not a repository", plain, "not a git repository"},
		{"remote helper in a path", helper, "unsupported transport"},
		{"file scheme", "file://" + repo, "unsupported transport"},
		{"empty", "", "is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "tree")
			err := MaterializePin(context.Background(), tc.source, head, dest)
			if err == nil {
				t.Fatalf("MaterializePin(source=%q) = nil, want a refusal", tc.source)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MaterializePin(source=%q) = %v, want it to say %q", tc.source, err, tc.want)
			}
		})
	}
}

// TestMaterializePinRefusesADestinationThatAlreadyHoldsSomething keeps a
// caller from pointing this function at a live directory.
//
// The contract is "a tree this call owns": git would otherwise initialise a
// repository in a catalog root or a checkout and check the pin out over it,
// which is a deletion nobody asked for dressed as an install.
func TestMaterializePinRefusesADestinationThatAlreadyHoldsSomething(t *testing.T) {
	repo := gitRepo(t)
	head := pinnedRevision(t, repo, "HEAD")

	dest := t.TempDir()
	keep := filepath.Join(dest, "already-here.txt")
	if err := os.WriteFile(keep, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := MaterializePin(context.Background(), repo, head, dest)
	if err == nil {
		t.Fatalf("MaterializePin into a non-empty directory = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("MaterializePin into a non-empty directory = %v, want it refused for holding entries", err)
	}
	if body, readErr := os.ReadFile(keep); readErr != nil || string(body) != "mine\n" {
		t.Fatalf("the file that was already there = %q (err %v), want it untouched", body, readErr)
	}
}
