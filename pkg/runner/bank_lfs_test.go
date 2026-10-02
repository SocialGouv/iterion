package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// lfsFixture is a clone whose tree carries one LFS-tracked path, with the
// driver configured but not required (the binary may be absent: git then
// stores the raw bytes and says so — detection reads the attribute and the
// configured driver, not the binary).
func lfsFixture(t *testing.T, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	gitOut(t, work, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	gitOut(t, work, "config", "filter.lfs.required", "false")
	if err := os.WriteFile(filepath.Join(work, "modèle.bin"), []byte("the run's artefact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "add", "-A")
	gitOut(t, work, "commit", "-qm", "the run's work")
}

// fakeGitLFS plants a git-lfs that records its argv and answers with rc, and
// puts its directory first on PATH.
func fakeGitLFS(t *testing.T, rc int) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexit " + itoa(rc) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git-lfs"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// recordGitArgv plants a git shim that logs every invocation's argv and
// execs the real git, and returns the log's path.
func recordGitArgv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// The bank's push runs no hook, and git-lfs uploads its objects from the
// pre-push hook: hookless, a clone with git-lfs installed pushes the
// pointers, exits 0, and the objects never reach the forge — a fresh clone of
// the banked branch fails its checkout (measured with git-lfs 3.4.1). The
// bank refuses by name instead: the objects are pushed explicitly first, and
// when that fails the bank names it.
func TestBankRefusesWhenTheLFSUploadFails(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	log := fakeGitLFS(t, 1)

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if _, err := os.Stat(log); err != nil {
		t.Fatal("the explicit upload was never attempted")
	}
	if branch, err := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); err == nil {
		t.Fatalf("the bank pushed a branch (%s) whose objects could not be uploaded", branch)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError == "" || !strings.Contains(run.FinalBranchError, "Git LFS") {
		t.Fatalf("FinalBranchError = %q, want a refusal naming Git LFS", run.FinalBranchError)
	}
}

// The explicit upload is the whole point of the gesture: git-lfs cannot
// resolve a `<sha>:<refspec>` argument — that push exits 0 having uploaded
// nothing (measured with git-lfs 3.4.1). A recorded fake of the driver and a
// recorded git pin the command the bank issues: the bare sha, with
// lfs.allowincompletepush — a config VALUE the run can write in its own
// clone — pinned off, and the bank goes on to push the branch.
func TestBankUploadsLFSObjectsForTheBareShaAndThenPushes(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	head := gitOut(t, work, "rev-parse", "HEAD")
	_ = fakeGitLFS(t, 0)
	gitLog := recordGitArgv(t)

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	gitRecorded, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatalf("git was never run through the recorder: %v", err)
	}
	var upload string
	for _, line := range strings.Split(strings.TrimSpace(string(gitRecorded)), "\n") {
		if strings.Contains(line, " lfs push origin ") {
			upload = line
		}
	}
	if upload == "" {
		t.Fatalf("the bank never issued the explicit LFS upload:\n%s", gitRecorded)
	}
	// HasSuffix, not Contains: a refspec (`<sha>:refs/heads/…`) carries the
	// sha too, and git-lfs cannot resolve it — the push must be the bare
	// sha and nothing after it.
	if !strings.Contains(upload, "-c lfs.allowincompletepush=false") || !strings.HasSuffix(upload, "push origin "+head) {
		t.Fatalf("the LFS upload argv %q is not %q", upload, "…push origin "+head)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError != "" {
		t.Fatalf("unexpected FinalBranchError: %q", run.FinalBranchError)
	}
	if branchHead := gitOut(t, origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); branchHead != head {
		t.Fatalf("banked branch at %s, want the run's head %s", branchHead, head)
	}
}

// Where git-lfs exists, the upload is verified where it matters: the remote
// holds the object a fresh clone would smudge. On the shipped code the
// hookless push pushed the pointer alone — exit 0, this directory empty.
func TestBankUploadReachesTheRemote(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not on PATH")
	}
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	head := gitOut(t, work, "rev-parse", "HEAD")

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if branchHead := gitOut(t, origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); branchHead != head {
		t.Fatalf("banked branch at %s, want the run's head %s", branchHead, head)
	}
	objects := filepath.Join(origin, "lfs", "objects")
	found := 0
	werr := filepath.Walk(objects, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Size() > 0 {
			found++
		}
		return nil
	})
	if werr != nil || found == 0 {
		t.Fatal("the bank pushed the pointer and never the object: a fresh clone of the branch cannot check out")
	}
}

// A config VALUE the run can write in its own clone —
// lfs.allowincompletepush=true — makes the upload exit 0 with objects
// missing (measured with git-lfs 3.4.1: pointer for one path, object
// deleted, remote holds 1 of 2). The bank pins it off for its one upload:
// the pointer push stays impossible.
func TestBankRefusesWhenAllowIncompletePushTriesToDefeatTheUpload(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not on PATH")
	}
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	// A second LFS path whose object the clone will NOT hold.
	if err := os.WriteFile(filepath.Join(work, "other.bin"), []byte("second artefact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "add", "-A")
	gitOut(t, work, "commit", "-qm", "second pointer")
	// The committed blob is the LFS POINTER; its oid names the object the
	// clean filter stored under .git/lfs/objects.
	pointer := gitOut(t, work, "cat-file", "blob", "HEAD:other.bin")
	oid := ""
	for _, line := range strings.Split(pointer, "\n") {
		if o, ok := strings.CutPrefix(strings.TrimSpace(line), "oid sha256:"); ok {
			oid = strings.TrimSpace(o)
		}
	}
	if len(oid) != 64 {
		t.Fatalf("unexpected pointer content: %q", pointer)
	}
	if err := os.Remove(filepath.Join(work, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)); err != nil {
		t.Fatalf("remove the object: %v", err)
	}
	gitOut(t, work, "config", "lfs.allowincompletepush", "true")

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if branch, err := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); err == nil {
		t.Fatalf("the bank pushed %s with an LFS object missing (allowincompletepush defeated the refusal), FinalBranchError=%q",
			branch, loadRun(t, r, msg.RunID).FinalBranchError)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError == "" || !strings.Contains(run.FinalBranchError, "Git LFS") {
		t.Fatalf("FinalBranchError = %q, want a refusal naming Git LFS", run.FinalBranchError)
	}
}

// A config read that FAILS is not an absence: a bank that cannot tell
// whether its push carries Git LFS pointers is refused by name, never
// silent.
func TestBankRefusesWhenTheLFSConfigReadFails(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	gitOut(t, work, "commit", "--allow-empty", "-m", "touch the tree so a bank is due")

	// A git whose `config --get-regexp` fails with something other than
	// exit 1 (no match): a real error, not an absence.
	shimDir := t.TempDir()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	shim := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = \"--get-regexp\" ]; then\n    echo 'shim: forced failure' >&2\n    exit 42\n  fi\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if branch, err := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); err == nil {
		t.Fatalf("the bank pushed a branch (%s) without knowing whether its tree carries Git LFS pointers", branch)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError == "" || !strings.Contains(run.FinalBranchError, "cannot read the git config") {
		t.Fatalf("FinalBranchError = %q, want a refusal naming the unreadable config", run.FinalBranchError)
	}
}

func TestBankPushesWhenNothingInTheTreeIsLFSTracked(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	gitOut(t, work, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	gitOut(t, work, "commit", "--allow-empty", "-m", "the filter, no tracked path")
	head := gitOut(t, work, "rev-parse", "HEAD")

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	branchHead, branchErr := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID)
	if branchErr != nil || branchHead != head {
		debug := loadRun(t, r, msg.RunID)
		t.Fatalf("banked branch at %q (%v), want the run's head %s (FinalBranchError=%q FinalBranch=%q)",
			branchHead, branchErr, head, debug.FinalBranchError, debug.FinalBranch)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError != "" {
		t.Fatalf("unexpected FinalBranchError: %q", run.FinalBranchError)
	}
}
