package server

// The assistant's two git readers (runAuthoringGit, assistantDependencyGit)
// hand their callers git's answer, and every caller reads a VALUE out of it:
// a toplevel path, a SHA, a NUL-separated path list, the index path a lock
// file is then created at, `git version` under Sscanf. git writes warnings,
// hints and advice on stderr whenever the environment gives it a reason to —
// a config file it cannot read, an ambiguous refname, a repository it had to
// repair — and these two endpoints are HTTP-reachable, so the environment of
// the server process decides. The shim is the real git with one stderr line
// in front of its answer: the production path, the real binary, one term
// changed.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botlock"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// talkativeGit puts a git in front of the real one that writes a warning on
// stderr before delegating, and answers with git's own exit status.
func talkativeGit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX git wrapper")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf \"warning: unable to access '/nonexistent/git/attributes': Permission denied\\n\" >&2\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The attested-commit path sits behind a production gate that refuses git
// older than 2.46; on such a machine the gate, not the stderr handling,
// answers first — the wrapper witnesses above carry the class there.
func gitSatisfiesTheAttestedCommitGate(t *testing.T) {
	t.Helper()
	out, err := exec.Command("git", gitlib.NoAutoMaintenance("version")...).Output()
	if err != nil {
		t.Skipf("git version unreadable: %v", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "git version %d.%d", &major, &minor); err != nil || major < 2 || (major == 2 && minor < 46) {
		t.Skipf("this machine's git (%s) is under the 2.46 attested-commit gate, which answers before the stderr handling these tests judge", strings.TrimSpace(string(out)))
	}
}

// The attested authoring commit reads `git version` through Sscanf and the
// index path to take its lock on: a line in front of either turns a usable
// Git into one the server refuses as too old, or sends the lock to a path
// made of git's prose.
func TestCommitAttestedAuthoringFiles_AGitWarningOnStderrDoesNotRefuseTheCommit(t *testing.T) {
	gitSatisfiesTheAttestedCommitGate(t)
	root, commit := attestedGitFixture(t)
	originalPath := os.Getenv("PATH")
	talkativeGit(t)

	head, err := commit()
	if err != nil || strings.TrimSpace(head) == "" {
		t.Fatalf("a git warning on stderr refused an attested authoring commit: head=%q err=%v", head, err)
	}

	// The effect, read from the repository with the shim back down.
	t.Setenv("PATH", originalPath)
	changed := gitTestRead(t, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if !sameGitPathSet(splitGitLines(changed), []string{"selected"}) {
		t.Fatalf("committed paths = %q, want the attested file", changed)
	}
}

// The dependency update resolves the project's Git root, its HEAD and its
// staged set the same way, behind an HTTP handler: a line in front of the
// toplevel makes the server answer that the project is not where it is.
func TestAssistantDependencyBotsUpdate_AGitWarningOnStderrDoesNotRefuseTheUpdate(t *testing.T) {
	gitSatisfiesTheAttestedCommitGate(t)
	s, targetRef := assistantDependencyFixture(t)
	talkativeGit(t)

	rec := assistantDependencyCall(t, s, assistantDependencyBotsUpdateRequest{
		Name: "shared-planner", Ref: targetRef, Message: "chore(bots): pin shared planner",
	})
	if rec.Code != 200 {
		t.Fatalf("a git warning on stderr refused the dependency update: %d %s", rec.Code, rec.Body.String())
	}
	lock, err := botlock.Load(s.cfg.WorkDir)
	if err != nil || lock.Dependencies["shared-planner"].Ref != targetRef {
		t.Fatalf("bots.lock does not carry the updated ref: lock=%#v err=%v", lock, err)
	}
}

// The wrappers behind the handlers, judged where no gate stands between
// them and the witness: the answer is the SHA on stdout, and one warning
// line on stderr must not become part of it.
func TestRunAuthoringGit_AGitWarningOnStderrDoesNotReachTheAnswer(t *testing.T) {
	root := t.TempDir()
	writeGitTestFile(t, filepath.Join(root, "tracked"), "baseline\n", 0o644)
	initAuthoringGit(t, root)
	talkativeGit(t)

	out, err := runAuthoringGit(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("a git warning on stderr refused rev-parse: %v", err)
	}
	if sha := regexp.MustCompile(`^[0-9a-f]{40}$`); !sha.MatchString(strings.TrimSpace(out)) {
		t.Fatalf("the answer carries git's prose: %q, want a bare 40-hex SHA", strings.TrimSpace(out))
	}
}

func TestAssistantDependencyGit_AGitWarningOnStderrDoesNotReachTheAnswer(t *testing.T) {
	root := t.TempDir()
	writeGitTestFile(t, filepath.Join(root, "tracked"), "baseline\n", 0o644)
	initAuthoringGit(t, root)
	talkativeGit(t)

	out, err := assistantDependencyGit(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("a git warning on stderr refused rev-parse: %v", err)
	}
	if sha := regexp.MustCompile(`^[0-9a-f]{40}$`); !sha.MatchString(strings.TrimSpace(out)) {
		t.Fatalf("the answer carries git's prose: %q, want a bare 40-hex SHA", strings.TrimSpace(out))
	}
}
