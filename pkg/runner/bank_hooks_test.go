package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The per-run clone is the run's tree: its hooks directory and its config are
// the run's to write. A hook there runs inside the runner's own git gestures —
// the bank's push above all, after every verdict the run's bots took — free
// to refuse the bank and strand the run's output. None runs.
func TestBankRunsNoRepositoryHook(t *testing.T) {
	for _, tc := range []struct{ name, hook, body string }{
		{"a pre-push hook refusing the bank", "pre-push", "#!/bin/sh\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, msg, work, origin, base := bankFixture(t)
			gitOut(t, work, "commit", "--allow-empty", "-m", "the run's work")
			head := gitOut(t, work, "rev-parse", "HEAD")
			hooks := filepath.Join(work, ".git", "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hooks, tc.hook), []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}

			r.bankRepoWorkspace(context.Background(), msg, work, base, runtime.WorkspaceIntegrity{}, "finished")

			branchHead, err := gittest.Try(origin, "rev-parse", "refs/heads/iterion/run-"+msg.RunID)
			if err != nil || branchHead != head {
				t.Fatalf("banked branch at %q (%v), want the run's head %s: a hook of the run decided the bank", branchHead, err, head)
			}
			if run := loadRun(t, r, msg.RunID); run.FinalBranchError != "" {
				t.Fatalf("the bank reports a failure a hook of the run caused: %q", run.FinalBranchError)
			}
		})
	}
}
