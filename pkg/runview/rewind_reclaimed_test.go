package runview

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRewindRestoresEarlyRefusalAtItsOriginalBaseline(t *testing.T) {
	for _, scope := range []RestoreScope{RestoreScopeFull, RestoreScopeNone} {
		t.Run(string(scope), func(t *testing.T) {
			const src = "compute guard:\n  expr:\n    stop: \"true\"\nfail refuse:\n  code: MISCONFIGURED\nworkflow w:\n  entry: guard\n  guard -> refuse\n"
			svc, st, runID := seedRun(t, src, &store.Checkpoint{NodeID: "refuse", Outputs: map[string]map[string]any{"guard": {"stop": true}}}, store.RunStatusFailed)
			repo := t.TempDir()
			base := gittest.InitRepo(t, repo)
			git(t, repo, "update-ref", "refs/iterion/runs/"+runID+"/pristine-worktree", base)
			git(t, repo, "update-ref", store.NodePreSnapshotRef(runID, "guard", 0), base)
			writeFile(t, repo, "unrelated.txt", "main moved\n")
			git(t, repo, "add", "unrelated.txt")
			git(t, repo, "commit", "-m", "later main")
			tip := git(t, repo, "rev-parse", "HEAD")
			ctx := context.Background()
			run, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			run.Worktree, run.WorktreeReclaimed = true, true
			run.RepoRoot, run.BaseCommit = repo, base
			run.WorkDir = filepath.Join(st.Root(), "worktrees", runID)
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Rewind(ctx, RewindSpec{RunID: runID, NodeID: "guard", RestoreScope: scope}); err != nil {
				t.Fatal(err)
			}
			current, err := st.LoadRun(ctx, runID)
			if err != nil || current.WorktreeReclaimed || current.Checkpoint.NodeID != "guard" {
				t.Fatalf("rewind state: run=%+v err=%v", current, err)
			}
			if got := git(t, current.WorkDir, "rev-parse", "HEAD"); got != base {
				t.Fatalf("restored %s, want original %s", got, base)
			}
			if got := git(t, repo, "rev-parse", "HEAD"); got != tip {
				t.Fatalf("operator checkout changed: %s, want %s", got, tip)
			}
		})
	}
}
