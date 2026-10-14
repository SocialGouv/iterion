package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// committingExecutor stands in for the backend a child's node runs on: it
// receives its cwd through the same workDirSetter seam real executors do
// (ClawExecutor.SetWorkDir) and commits a file there, so the test reads the
// tree a node actually writes into instead of asking the engine where it
// thinks it is.
// The engine pushes the workdir and executes nodes on its own goroutines, so
// every field this fixture shares with the test goroutine is taken under mu.
type committingExecutor struct {
	mu      sync.Mutex
	workDir string
	// commitIn names the node whose execution commits, and the per-node
	// directory record the assertions read.
	commitIn string
	dirs     map[string]string
	// commitErr carries what the commit refused: Execute runs off the test
	// goroutine, where a *testing.T failure would not end the test.
	commitErr error
	// extra, when set, runs in the commitIn node before the commit — what a
	// child's mission writes besides the artefact it commits.
	extra func(dir string) error
}

func (e *committingExecutor) SetWorkDir(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workDir = dir
}

func (e *committingExecutor) nodeDir(nodeID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dirs[nodeID]
}

func (e *committingExecutor) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitErr
}

func (e *committingExecutor) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	e.mu.Lock()
	dir := e.workDir
	e.dirs[node.NodeID()] = dir
	commits := node.NodeID() == e.commitIn
	extra := e.extra
	e.mu.Unlock()
	if !commits {
		return map[string]any{}, nil
	}
	if extra != nil {
		if err := extra(dir); err != nil {
			e.mu.Lock()
			e.commitErr = err
			e.mu.Unlock()
			return nil, err
		}
	}
	err := commitArtefact(dir)
	if err != nil {
		e.mu.Lock()
		e.commitErr = err
		e.mu.Unlock()
		return nil, err
	}
	return map[string]any{"value": "committed"}, nil
}

// commitArtefact writes and commits the child's artefact in dir — what a real
// backend does when the child's mission is "produce this and commit it".
func commitArtefact(dir string) error {
	path := filepath.Join(dir, "child-output.txt")
	if err := os.WriteFile(path, []byte("produced by the child\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	for _, args := range [][]string{{"add", "child-output.txt"}, {"commit", "-q", "-m", childCommitSubject}} {
		if out, err := gittest.Try(dir, args...); err != nil {
			return fmt.Errorf("git %v in %s: %w: %s", args, dir, err, out)
		}
	}
	return nil
}

// childCommitSubject is the subject the child's node commits under, read back
// from the parent's checkout.
const childCommitSubject = "child: produce the artefact"

// TestLocalSubbotChildCommitsIntoTheParentsTree drives the real local subbot
// path — `iterion run` on a host with no sandbox — with a parent run working
// in a git checkout and a `worktree: auto` child. The child's commit must land
// in the tree the parent works in: that is where the parent reads its result,
// and a worktree of its own would leave it on a branch nobody looks at.
func TestLocalSubbotChildCommitsIntoTheParentsTree(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	// The host's own escape hatch for the capture cost is a documented
	// operator switch (ITERION_WORKSPACE_TRACK=off): using it must not
	// redden a gate about where a child works.
	t.Setenv("ITERION_WORKSPACE_TRACK", "")
	repo := gittest.SourceRepo(t)
	t.Chdir(repo)

	// The .bot files live outside the checkout: the run's workspace is the
	// repository, and the sources are not part of what the child produces.
	botDir := t.TempDir()
	rootPath := filepath.Join(botDir, "root.bot")
	writeFile(t, rootPath, `schema child_result:
  value: string

subbot produce:
  source: "child.bot"
  output: child_result

workflow root:
  worktree: none
  entry: produce
  produce -> done
`)
	writeFile(t, filepath.Join(botDir, "child.bot"), `schema child_result:
  value: string

tool commit_it:
  command: "true"
  output: child_result

workflow child:
  worktree: auto
  entry: commit_it
  commit_it -> done
`)

	storeDir := filepath.Join(t.TempDir(), "store")
	exec := &committingExecutor{commitIn: "commit_it", dirs: map[string]string{}}
	if err := RunRun(context.Background(), RunOptions{
		File:     rootPath,
		StoreDir: storeDir,
		RunID:    "run-parent-tree",
		Executor: exec,
	}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
		t.Fatalf("RunRun: %v", err)
	}

	if got := exec.nodeDir("commit_it"); got != repo {
		t.Errorf("the child's node ran in %q, want the parent's tree %q", got, repo)
	}
	// The parent's own checkout is what the operator and the parent's next
	// node read: the child's commit and its file must be there.
	if err := exec.failure(); err != nil {
		t.Fatalf("the child's node could not commit: %v", err)
	}
	if subject := gittest.Run(t, repo, "log", "-1", "--format=%s"); subject != childCommitSubject {
		t.Errorf("HEAD subject in the parent's tree = %q, want the child's commit %q", subject, childCommitSubject)
	}
	if _, err := os.Stat(filepath.Join(repo, "child-output.txt")); err != nil {
		t.Errorf("stat child-output.txt in the parent's tree: %v (want the child's file)", err)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	childIDs, err := s.ListChildRuns(context.Background(), "run-parent-tree")
	if err != nil {
		t.Fatalf("ListChildRuns: %v", err)
	}
	if len(childIDs) != 1 {
		t.Fatalf("children of the parent = %v, want exactly one", childIDs)
	}
	child, err := s.LoadRun(context.Background(), childIDs[0])
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if child.Status != store.RunStatusFinished {
		t.Errorf("child status = %q, want %q", child.Status, store.RunStatusFinished)
	}
	if child.Worktree {
		t.Errorf("child.Worktree = true, want false (it works in its parent's tree)")
	}
	if child.WorkDir != repo {
		t.Errorf("child.WorkDir = %q, want the parent's tree %q", child.WorkDir, repo)
	}
	// setupWorktree creates the worktree home and finalization removes only
	// the per-run directory inside it, so its absence says no worktree was
	// ever set up under this store.
	wtHome := filepath.Join(storeDir, "worktrees")
	if _, err := os.Stat(wtHome); err == nil {
		t.Errorf("a worktree was set up under %s, want none", wtHome)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v (want IsNotExist)", wtHome, err)
	}
	// Working in the parent's tree, the child marks its node boundaries
	// through workspace versioning instead of the per-node git refs a
	// worktree run writes — the anchor a human gate inside the child shows
	// and `iterion rewind` restores. No tracker on the child's engine and it
	// captures nothing.
	//
	// Both readers need the (pre, post) PAIR to resolve — nodeChangesFromTracker
	// skips a node missing either half, and the rewind looks up the pre
	// boundary — so the pair is what this asserts, not the presence of some
	// snapshot: a boundary path that stopped writing the pre marker would
	// leave the snapshots directory populated and the readers blind.
	tr := workspacetrack.NewNative(storeDir)
	for _, phase := range []string{workspacetrack.PhasePre, workspacetrack.PhasePost} {
		label := workspacetrack.Label(phase, "commit_it", 0)
		if _, ok := tr.Resolve(child.ID, label); !ok {
			t.Errorf("the child's boundary %q does not resolve: a gate inside the child shows no diff and `iterion rewind` has nothing to restore to", label)
		}
	}
}

// TestLocalSubbotChildBorrowsTheParentsClaudeResources pins what working in a
// LENT tree costs — the paragraph docs/groups-iteration-subbots.md states. For
// the child's whole pass that tree's `.claude/{skills,commands,agents,
// settings.json}` are ITS mirror, and its exit puts the parent's back: the
// parent's own files return byte for byte, and anything else written under
// those paths while the child ran is replaced along with the mirror. What the
// child produces OUTSIDE them is its result and is left alone — the contrast
// is the contract. A sweep made marker-driven (removing only what the mirror
// wrote, the doctrine `pruneWorkspaceMirror` already applies to a checkout it
// does not own) updates this test and that paragraph together.
func TestLocalSubbotChildBorrowsTheParentsClaudeResources(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	t.Setenv("ITERION_WORKSPACE_TRACK", "")
	repo := gittest.SourceRepo(t)
	t.Chdir(repo)

	// What the operator already has in the checkout before the run.
	operatorSkill := filepath.Join(repo, ".claude", "skills", "operator-skill", "SKILL.md")
	const operatorBody = "the operator's own skill\n"
	if err := os.MkdirAll(filepath.Dir(operatorSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorSkill, []byte(operatorBody), 0o600); err != nil {
		t.Fatal(err)
	}

	botDir := t.TempDir()
	rootPath := filepath.Join(botDir, "root.bot")
	writeFile(t, rootPath, `schema child_result:
  value: string

subbot produce:
  source: "child.bot"
  output: child_result

workflow root:
  worktree: none
  entry: produce
  produce -> done
`)
	writeFile(t, filepath.Join(botDir, "child.bot"), `schema child_result:
  value: string

tool commit_it:
  command: "true"
  output: child_result

workflow child:
  worktree: auto
  entry: commit_it
  commit_it -> done
`)

	// Written WHILE the child's node runs: its own mission's skill, and the
	// command file a parallel operator session would be editing.
	underMirror := []string{
		filepath.Join(repo, ".claude", "commands", "written-during-the-child.md"),
		filepath.Join(repo, ".claude", "skills", "child-skill", "SKILL.md"),
	}
	exec := &committingExecutor{commitIn: "commit_it", dirs: map[string]string{}, extra: func(string) error {
		for _, p := range underMirror {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte("written while the child ran\n"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}}

	storeDir := filepath.Join(t.TempDir(), "store")
	if err := RunRun(context.Background(), RunOptions{
		File:     rootPath,
		StoreDir: storeDir,
		RunID:    "run-claude-borrow",
		Executor: exec,
	}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
		t.Fatalf("RunRun: %v", err)
	}
	if err := exec.failure(); err != nil {
		t.Fatalf("the child's node failed: %v", err)
	}

	body, err := os.ReadFile(operatorSkill)
	if err != nil {
		t.Fatalf("the parent's own skill did not come back: %v", err)
	}
	if string(body) != operatorBody {
		t.Errorf("the parent's skill reads %q after the child's pass, want its own %q", body, operatorBody)
	}
	for _, p := range underMirror {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived the child's pass: the borrow is restorative, and the doc paragraph says what it replaces", p)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s: %v (want IsNotExist)", p, err)
		}
	}
	// Outside `.claude/`, the child's production is untouched: that is the
	// result the parent reads, and the whole point of the lent tree.
	if _, err := os.Stat(filepath.Join(repo, "child-output.txt")); err != nil {
		t.Errorf("stat the child's artefact in the parent's tree: %v (the borrow must not touch what the child produced)", err)
	}
	if subject := gittest.Run(t, repo, "log", "-1", "--format=%s"); subject != childCommitSubject {
		t.Errorf("HEAD subject in the parent's tree = %q, want the child's commit %q", subject, childCommitSubject)
	}
}
