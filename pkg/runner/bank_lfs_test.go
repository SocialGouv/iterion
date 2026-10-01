package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The bank's push runs no hook, and git-lfs uploads its objects from the
// pre-push hook: hookless, a clone with git-lfs installed pushes the
// pointers, exits 0, and the objects never reach the forge — a fresh clone of
// the banked branch fails its checkout (measured with git-lfs 3.4.1). The
// bank refuses by name instead: the objects are pushed explicitly first, and
// when that fails the bank names it.
func TestBankRefusesWhenTheTreeCarriesLFSPathsItCannotUpload(t *testing.T) {
	r, msg, work, origin, base := bankFixture(t)
	if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A filter the clone carries: git-lfs installed is what turns a push into
	// pointers. The name of the driver is all the detection reads.
	gitOut(t, work, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	gitOut(t, work, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	// The driver's binary is absent here: with the filter not required, git
	// stores the raw bytes and says so. Detection reads the attribute and the
	// configured driver, not the binary.
	gitOut(t, work, "config", "filter.lfs.required", "false")
	if err := os.WriteFile(filepath.Join(work, "model.bin"), []byte("the run's artefact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "add", "-A")
	gitOut(t, work, "commit", "-qm", "the run's work")

	r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

	if branch, err := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID); err == nil {
		t.Fatalf("the bank pushed a branch (%s) whose tree the clone cannot upload the objects for", branch)
	}
	run := loadRun(t, r, msg.RunID)
	if run.FinalBranchError == "" || !strings.Contains(run.FinalBranchError, "Git LFS") {
		t.Fatalf("FinalBranchError = %q, want a refusal naming Git LFS", run.FinalBranchError)
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
