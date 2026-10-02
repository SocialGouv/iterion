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
// nothing (measured with git-lfs 3.4.1). A recorded fake of the driver pins
// the command the bank issues: the bare sha, nothing else, and the bank goes
// on to push the branch.
func TestBankUploadsLFSObjectsForTheBareShaAndThenPushes(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	lfsFixture(t, work)
	head := gitOut(t, work, "rev-parse", "HEAD")
	log := fakeGitLFS(t, 0)

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("git-lfs was never run: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	upload := calls[len(calls)-1]
	if upload != "push origin "+head {
		t.Fatalf("git-lfs was asked %q, want %q — a refspec it cannot resolve uploads nothing", upload, "push origin "+head)
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
