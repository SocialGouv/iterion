package runview

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResumeProbeWorkDir_FollowsTheEnginesResumePrecedence pins the directory a
// RESUMED run's fallback screen resolves its repository from. A resume
// re-resolves the sandbox, and the engine resolves it against what the run
// RECORDED: r.RepoRoot, else the root derived from r.WorkDir, else the engine's
// own directory for a run that recorded neither (runtime.seedRepoRootForResume,
// and the sandbox re-bootstrap beside it). An executor predicting from the
// process cwd instead screens the operator's codex crossing against whatever
// directory `iterion resume` happened to be typed in.
//
// Mutation: return r.WorkDir unconditionally — the first sub-case reddens.
func TestResumeProbeWorkDir_FollowsTheEnginesResumePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *store.Run
		want string
	}{
		{"the recorded repo root wins over the run's worktree",
			&store.Run{RepoRoot: "/repo", WorkDir: "/repo/.iterion/worktrees/run-1"}, "/repo"},
		{"no recorded root: the run's own workspace answers",
			&store.Run{WorkDir: "/srv/project"}, "/srv/project"},
		{"a run that recorded neither leaves the probe on the engine's own directory",
			&store.Run{}, ""},
		{"no record at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResumeProbeWorkDir(tc.run); got != tc.want {
				t.Errorf("ResumeProbeWorkDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both resume surfaces answer with that one precedence, and the studio's
// in-process resume is the one whose executor ALSO carries a workspace: its
// WorkDir is the run's recorded workspace (Claw's file-tool root), which is
// not a repository root and need not still resolve back to one. A per-run
// worktree that was pruned or finalized, or a run document read on a replica
// that never held it, resolves to whatever repository encloses its path —
// while the resumed engine roots the run at r.RepoRoot, which sat on the same
// record.
//
// Mutation: drop SandboxProbeWorkDir from resumeExecutorSpec — this reddens.
func TestStudioResumeProbesTheRecordedRepositoryNotThePrunedWorkspace(t *testing.T) {
	repo := gittest.SourceRepo(t)
	worktree := filepath.Join(t.TempDir(), "run-wt")
	gittest.Run(t, repo, "worktree", "add", "--quiet", worktree)
	run := &store.Run{ID: "run-1", RepoRoot: repo, WorkDir: worktree}
	svc := &Service{workDir: repo}

	// While the worktree is on disk, both readings agree — which is why the
	// divergence is invisible until it is not.
	spec := svc.resumeExecutorSpec(nil, run, iterlog.Nop(), "")
	if got := runtime.EngineRepoRoot(spec.sandboxProbeRoot()); got != repo {
		t.Fatalf("precondition: a live worktree must resolve its own repository, got %q want %q", got, repo)
	}

	gittest.RemoveWorktree(t, repo, worktree)
	spec = svc.resumeExecutorSpec(nil, run, iterlog.Nop(), "")
	if got := runtime.EngineRepoRoot(spec.sandboxProbeRoot()); got != run.RepoRoot {
		t.Errorf("the studio resume's screen would probe %q; the resumed engine roots this run at the recorded "+
			"%q — the screen must judge the repository the run executes in", got, run.RepoRoot)
	}
	// The blast radius stays where it was: answering the probe does not move
	// Claw's file tools off the run's recorded workspace.
	if spec.WorkDir != worktree {
		t.Errorf("ExecutorSpec.WorkDir = %q, want the run's recorded workspace %q — it re-roots Claw's file "+
			"tools (resolveExecutorWorkspace)", spec.WorkDir, worktree)
	}
}
