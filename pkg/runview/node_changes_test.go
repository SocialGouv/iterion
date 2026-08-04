package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// seedNodeChangesRun parks an in-place run and returns the service, its
// tracker and the workspace — the shape every run on a real instance has.
func seedNodeChangesRun(t *testing.T) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(linearBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-node-changes"
	if _, err := st.CreateRun(context.Background(), runID, "linear", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.WorkDir = ws
	run.Worktree = false
	run.Status = store.RunStatusFinished
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, ws, runID
}

// TestNodeChanges_InPlaceRunViaTracker is the operator's real case: every
// run on their instances is worktree=false, so the git boundary refs do
// not exist and only the tracker can answer.
func TestNodeChanges_InPlaceRunViaTracker(t *testing.T) {
	svc, ws, runID := seedNodeChangesRun(t)
	tr := svc.workspaceTracker
	if tr == nil {
		t.Fatal("a filesystem-backed service must wire workspace versioning")
	}

	if err := os.WriteFile(filepath.Join(ws, "before.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePre, "implement", 0)); err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	// The node does its work.
	if err := os.WriteFile(filepath.Join(ws, "before.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "created.md"), []byte("# by implement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePost, "implement", 0)); err != nil {
		t.Fatalf("post capture: %v", err)
	}

	set, err := svc.NodeChanges(context.Background(), runID, "implement", -1)
	if err != nil {
		t.Fatalf("NodeChanges: %v", err)
	}
	if !set.Available {
		t.Fatalf("unavailable: %s", set.Reason)
	}
	if set.Source != "workspace" {
		t.Errorf("Source = %q, want workspace (this run has no git worktree)", set.Source)
	}
	got := map[string]string{}
	for _, f := range set.Files {
		got[f.Path] = f.Status
	}
	if got["before.txt"] != "M" || got["created.md"] != "A" {
		t.Errorf("files = %v, want before.txt=M created.md=A", got)
	}

	// And the diff of one file resolves both sides.
	diff, err := svc.NodeFileDiff(context.Background(), runID, "implement", -1, "before.txt")
	if err != nil {
		t.Fatalf("NodeFileDiff: %v", err)
	}
	if diff.Before == nil || *diff.Before != "v1" || diff.After == nil || *diff.After != "v2" {
		t.Errorf("diff = %+v, want before=v1 after=v2", diff)
	}
}

// TestNodeChanges_SubbotSaysWhyItIsEmpty is the pitfall that would make
// the panel lie: a subbot records an opening boundary and never a
// closing one, so a naive implementation renders "no changes" for the
// node kind most likely to have rewritten the tree.
func TestNodeChanges_SubbotSaysWhyItIsEmpty(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	src := `schema out:
  value: string

agent survey:
  model: "claude-opus-4-7"
  output: out

subbot delegate:
  source: "child.bot"
  output: out

workflow withsub:
  entry: survey
  survey -> delegate
  delegate -> done
`
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-subbot"
	if _, err := st.CreateRun(context.Background(), runID, "withsub", nil); err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.FilePath = botPath
	run.WorkDir = dir
	run.Status = store.RunStatusFinished
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	set, err := svc.NodeChanges(context.Background(), runID, "delegate", -1)
	if err != nil {
		t.Fatalf("NodeChanges: %v", err)
	}
	if set.Available {
		t.Fatal("a subbot records no closing boundary; it cannot be available")
	}
	if !strings.Contains(set.Reason, "subbot") {
		t.Errorf("Reason = %q — it must name the node kind, not read as \"this node changed nothing\"", set.Reason)
	}
}

// TestNodeChanges_UnknownNodeIsNotAnEmptyDiff: an unresolvable node must
// carry a reason too.
func TestNodeChanges_UnknownNodeIsNotAnEmptyDiff(t *testing.T) {
	svc, _, runID := seedNodeChangesRun(t)
	set, err := svc.NodeChanges(context.Background(), runID, "nonexistent", -1)
	if err != nil {
		t.Fatalf("NodeChanges: %v", err)
	}
	if set.Available {
		t.Fatal("expected unavailable for a node with no boundary")
	}
	if set.Reason == "" {
		t.Error("an unavailable result must always carry a reason")
	}
}

// TestNodeChanges_EmptyRangeIsAvailable: a node that read files but wrote
// none has both boundaries and zero changes. That is a legitimate answer
// and must NOT be reported as unavailable — the two mean different things
// to a reviewer.
func TestNodeChanges_EmptyRangeIsAvailable(t *testing.T) {
	svc, ws, runID := seedNodeChangesRun(t)
	tr := svc.workspaceTracker
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePre, "verify", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePost, "verify", 0)); err != nil {
		t.Fatal(err)
	}

	set, err := svc.NodeChanges(context.Background(), runID, "verify", -1)
	if err != nil {
		t.Fatalf("NodeChanges: %v", err)
	}
	if !set.Available {
		t.Fatalf("a read-only node has both boundaries; unavailable=%q is wrong", set.Reason)
	}
	if len(set.Files) != 0 {
		t.Errorf("files = %v, want none", set.Files)
	}
}

// TestNodeChanges_ExplicitIterationDoesNotFallBack is Revi's Rd56b21.
//
// Walking downwards is the semantics of iteration < 0 ("the latest one
// recorded"), but the loop ran unconditionally — and the studio ALWAYS
// sends an explicit iteration. So a loop node midway through its
// iteration 1 (opening boundary written, closing one not yet) silently
// resolved iteration 0 and was reported Available with iteration 0's file
// list, under a caption reading "· iteration 0" that looks like a label
// rather than a correction. explainMissingBoundary's "this node has not
// finished yet" was unreachable for any looped node.
func TestNodeChanges_ExplicitIterationDoesNotFallBack(t *testing.T) {
	svc, ws, runID := seedNodeChangesRun(t)
	tr := svc.workspaceTracker
	ctx := context.Background()

	// Iteration 0 ran to completion.
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePre, "implement", 0)); err != nil {
		t.Fatalf("pre 0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "iter0-only.txt"), []byte("from pass 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePost, "implement", 0)); err != nil {
		t.Fatalf("post 0: %v", err)
	}
	// Iteration 1 has started but not finished: opening boundary only.
	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePre, "implement", 1)); err != nil {
		t.Fatalf("pre 1: %v", err)
	}

	set, err := svc.NodeChanges(ctx, runID, "implement", 1)
	if err != nil {
		t.Fatalf("NodeChanges: %v", err)
	}
	if set.Available {
		names := make([]string, 0, len(set.Files))
		for _, f := range set.Files {
			names = append(names, f.Path)
		}
		t.Fatalf("iteration 1 reported Available with %v (resolved iteration %d) — "+
			"an explicit iteration must resolve THAT iteration or say why it cannot, "+
			"not silently present an earlier pass's diff", names, set.Iteration)
	}
	if set.Reason == "" {
		t.Error("an unavailable range must say why")
	}

	// iteration < 0 keeps its documented meaning: the latest recorded one.
	latest, err := svc.NodeChanges(ctx, runID, "implement", -1)
	if err != nil {
		t.Fatalf("NodeChanges(latest): %v", err)
	}
	if !latest.Available || latest.Iteration != 0 {
		t.Errorf("latest = {available:%v iteration:%d}, want the completed iteration 0",
			latest.Available, latest.Iteration)
	}
}
