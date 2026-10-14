package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// autoWorktreeWorkflow is a one-tool-node workflow declaring the IR default
// `worktree: auto`, the mode whose in-place rule these tests pin.
func autoWorktreeWorkflow(name string) *ir.Workflow {
	return &ir.Workflow{
		Name:  name,
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "auto",
	}
}

// TestEngineRun_SubbotChildWorksInTheParentsTree: a subbot child — a run a
// parent's subbot node spawned, handed the parent's effective workdir — keeps
// working in that tree under `worktree: auto`, with no sandbox in sight. A
// worktree of its own would park its commits on a branch the parent has to
// chase instead of leaving them where the parent judges its result.
func TestEngineRun_SubbotChildWorksInTheParentsTree(t *testing.T) {
	s := tmpStore(t)
	repo, _ := initBareishRepo(t)

	eng := New(autoWorktreeWorkflow("child-in-parent-tree"), s, newStubExecutor(),
		WithWorkDir(repo),
		WithParentRunID("run-parent"),
		WithParentNodeID("spawn_child"),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-subbot-child"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run for a subbot child: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Worktree {
		t.Errorf("run.Worktree = true, want false (a subbot child works in its parent's tree)")
	}
	if r.WorkDir != repo {
		t.Errorf("run.WorkDir = %q, want the parent's tree %q", r.WorkDir, repo)
	}
	// The worktree home is created by setupWorktree and outlives the
	// per-run directory finalization removes, so its absence says no
	// worktree was ever set up for this run.
	wtHome := filepath.Join(s.Root(), "worktrees")
	if _, err := os.Stat(wtHome); err == nil {
		t.Errorf("a subbot child set up a worktree under %s", wtHome)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v (want IsNotExist)", wtHome, err)
	}
	// The isolated-worktree baseline belongs to a run that owns its tree.
	// A child borrowing its parent's must not claim it: resume and the
	// review gate reconstruct a finalization from the pair.
	if r.RepoRoot != "" || r.BaseCommit != "" {
		t.Errorf("run baseline = {RepoRoot:%q BaseCommit:%q}, want both empty for an in-place child", r.RepoRoot, r.BaseCommit)
	}
}

// TestEngineRun_TopLevelAutoRunKeepsItsOwnWorktree: the other direction. A
// root run carries no parent linkage, so `worktree: auto` still gives it the
// isolated worktree that is the mode's whole point.
func TestEngineRun_TopLevelAutoRunKeepsItsOwnWorktree(t *testing.T) {
	s := tmpStore(t)
	repo, originalTip := initBareishRepo(t)

	eng := New(autoWorktreeWorkflow("top-level-auto"), s, newStubExecutor(),
		WithWorkDir(repo),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-top-level-auto"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run for a top-level auto run: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !r.Worktree {
		t.Fatalf("run.Worktree = false, want true (a root `worktree: auto` run is isolated)")
	}
	if want := filepath.Join(s.Root(), "worktrees", runID); r.WorkDir != want {
		t.Errorf("run.WorkDir = %q, want its own worktree %q", r.WorkDir, want)
	}
	if r.RepoRoot != repo {
		t.Errorf("run.RepoRoot = %q, want %q", r.RepoRoot, repo)
	}
	if r.BaseCommit != originalTip {
		t.Errorf("run.BaseCommit = %q, want the repo tip %q", r.BaseCommit, originalTip)
	}
}

// TestEngineRun_AParentsWorktreeIsolatesTheWholeFamily: the escape hatch for a
// family that must stay off the operator's checkout is `worktree: auto` on the
// PARENT. The parent takes the worktree, hands it to its child, and the
// child's commit is what the parent's finalization branches.
func TestEngineRun_AParentsWorktreeIsolatesTheWholeFamily(t *testing.T) {
	s := tmpStore(t)
	repo, originalTip := initBareishRepo(t)

	childExec := &workDirProbeExecutor{stubExecutor: newStubExecutor()}
	var childCommit string
	// The node commits where the CHILD's engine put it, and reports a git
	// refusal as a node failure: it runs off the test goroutine, where a
	// *testing.T failure would not end the test.
	childExec.on("start", func(map[string]any) (map[string]any, error) {
		dir := childExec.workDir
		if err := os.WriteFile(filepath.Join(dir, "child-output.txt"), []byte("produced by the child\n"), 0o600); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"add", "child-output.txt"}, {"commit", "-q", "-m", "child: produce the artefact"}, {"rev-parse", "HEAD"}} {
			out, err := gittest.Try(dir, args...)
			if err != nil {
				return nil, fmt.Errorf("git %v in %s: %w: %s", args, dir, err, out)
			}
			childCommit = out
		}
		return map[string]any{}, nil
	})
	var handed string
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		handed = req.WorkDir
		// The option set every host runner builds for a child.
		child := New(autoWorktreeWorkflow("child"), s, childExec,
			WithWorkDir(req.WorkDir),
			WithParentRunID(req.ParentRunID),
			WithParentNodeID(req.NodeID),
			WithLogger(log.New(log.LevelWarn, os.Stderr)),
		)
		return map[string]any{}, child.Run(ctx, "run-family-child", nil)
	}
	parentWf := &ir.Workflow{
		Name: "family-parent", Entry: "run_child", Worktree: "auto",
		Nodes: map[string]ir.Node{
			"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot"},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "run_child", To: "done"}},
	}
	parent := New(parentWf, s, newStubExecutor(),
		WithWorkDir(repo),
		WithSubbotRunner(runner),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)
	if err := parent.Run(context.Background(), "run-family-parent", nil); err != nil {
		t.Fatalf("parent engine.Run: %v", err)
	}

	wantWorktree := filepath.Join(s.Root(), "worktrees", "run-family-parent")
	if handed != wantWorktree {
		t.Fatalf("the parent handed %q to its child, want its own worktree %q", handed, wantWorktree)
	}
	child, err := s.LoadRun(context.Background(), "run-family-child")
	if err != nil {
		t.Fatalf("load child run: %v", err)
	}
	if child.Worktree || child.WorkDir != wantWorktree {
		t.Errorf("child = {Worktree:%v WorkDir:%q}, want it in place in the parent's worktree %q", child.Worktree, child.WorkDir, wantWorktree)
	}
	p, err := s.LoadRun(context.Background(), "run-family-parent")
	if err != nil {
		t.Fatalf("load parent run: %v", err)
	}
	if p.FinalCommit != childCommit {
		t.Errorf("the parent's final commit = %q, want the child's commit %q", p.FinalCommit, childCommit)
	}
	if p.FinalBranch == "" {
		t.Errorf("the parent branched nothing: the child's commit is reachable from the reflog only")
	}
	// Nothing reached the operator's checkout: the branch is the handover.
	if head := gittest.Run(t, repo, "rev-parse", "HEAD"); head != originalTip {
		t.Errorf("the repo HEAD moved to %q, want the untouched tip %q", head, originalTip)
	}
}

// TestEverySubbotRunnerHandsTheChildItsWorktreeFacts: the in-place rule reads
// parentNodeID and the node's `isolated:` assertion, and only the runner that
// turns a SubbotRequest into a child engine can supply them. A runner that
// passes the request's parent RUN id and forgets its NODE id would silently
// hand its local children a worktree of their own again; one that forgets the
// isolation would lend the parent's tree to children the author certified
// write elsewhere, and queue their parallel fan-out on that one tree. The
// class is discovered from the source, not listed, so a new runner joins it
// by existing.
func TestEverySubbotRunnerHandsTheChildItsWorktreeFacts(t *testing.T) {
	const builds = "runtime.WithParentRunID(req.ParentRunID)"
	owed := map[string]string{
		"runtime.WithParentNodeID(req.NodeID)":    "its children would take a worktree of their own",
		"runtime.WithIsolatedChild(req.Isolated)": "an `isolated:` child would work in the parent's tree, and its siblings would queue on that tree's resource gate",
	}
	runners := map[string][]string{}
	// `..` is pkg/: every SubbotRunner implementation lives under it.
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(src), builds) {
			return nil
		}
		missing := []string{}
		for marker := range owed {
			if !strings.Contains(string(src), marker) {
				missing = append(missing, marker)
			}
		}
		runners[path] = missing
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/: %v", err)
	}
	// Four launchers run a child .bot: the CLI, the dispatcher, the studio
	// and the cloud runner. Fewer means the discovery stopped working, and a
	// test that finds nothing proves nothing.
	if len(runners) < 4 {
		t.Fatalf("found %d subbot runners (%v), want at least the four launchers — the marker %q no longer discovers them", len(runners), runners, builds)
	}
	for path, missing := range runners {
		for _, marker := range missing {
			t.Errorf("%s builds a child engine from a SubbotRequest without %s: %s", path, marker, owed[marker])
		}
	}
}

// TestSubbotNode_HandsTheRunnerTheNodesIsolatedAssertion: the four runners can
// only forward what the request carries, and the subbot node is the one place
// that reads `isolated:` off the IR into it. A request built without the flag
// would read as "not isolated" at every runner — the default that lends the
// parent's tree — with nothing else in the chain able to tell.
func TestSubbotNode_HandsTheRunnerTheNodesIsolatedAssertion(t *testing.T) {
	for _, isolated := range []bool{true, false} {
		t.Run(fmt.Sprintf("isolated=%v", isolated), func(t *testing.T) {
			var got *SubbotRequest
			runner := func(_ context.Context, req SubbotRequest) (map[string]any, error) {
				got = &req
				return map[string]any{}, nil
			}
			wf := &ir.Workflow{
				Name: "parent", Entry: "run_child",
				Nodes: map[string]ir.Node{
					"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot", Isolated: isolated},
					"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				},
				Edges: []*ir.Edge{{From: "run_child", To: "done"}},
			}
			s := tmpStore(t)
			eng := New(wf, s, newStubExecutor(), WithSubbotRunner(runner), WithWorkDir(t.TempDir()))
			if err := eng.Run(context.Background(), fmt.Sprintf("run-isolated-%v", isolated), nil); err != nil {
				t.Fatalf("parent engine.Run: %v", err)
			}
			if got == nil {
				t.Fatal("the subbot runner was never called")
			}
			if got.Isolated != isolated {
				t.Errorf("SubbotRequest.Isolated = %v, want the node's own %v", got.Isolated, isolated)
			}
		})
	}
}

// TestRunsInParentWorkspace_TheFactsItReads pins the whole decision at its
// choke point: which runs `worktree: auto` must not give a tree of their own.
// Each row is a shape the engine really builds — the run-level tests above and
// in pkg/cli exercise the no-sandbox rows end to end; this table is where the
// adopted-sandbox rows, whose sandbox cannot be stood up on a host, are
// pinned.
func TestRunsInParentWorkspace_TheFactsItReads(t *testing.T) {
	adopted := &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"}
	for _, tc := range []struct {
		name string
		opts []EngineOption
		want bool
		why  string
	}{
		{"a root run", nil, false, "a run with no parent owns its workspace"},
		{"a subbot child", []EngineOption{WithParentRunID("run-parent"), WithParentNodeID("spawn")}, true,
			"its commits must land where the parent reads its result"},
		{"an isolated subbot child", []EngineOption{WithParentRunID("run-parent"), WithParentNodeID("spawn"), WithIsolatedChild(true)}, false,
			"the author certified it writes elsewhere, and its siblings must overlap"},
		{"a shard child (parent run, no spawning node)", []EngineOption{WithParentRunID("run-parent")}, false,
			"a run of its own in a workspace of its own"},
		{"a run that adopted a parent's sandbox", []EngineOption{WithSharedSandbox(adopted)}, true,
			"a worktree of its own is a directory that sandbox never mounts"},
		{"an isolated child in an adopted sandbox", []EngineOption{WithParentRunID("run-parent"), WithParentNodeID("spawn"), WithIsolatedChild(true), WithSharedSandbox(adopted)}, true,
			"that sandbox mounts ONE tree whatever the node declares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(autoWorktreeWorkflow("shape"), tmpStore(t), newStubExecutor(), tc.opts...)
			if got := e.runsInParentWorkspace(); got != tc.want {
				t.Errorf("runsInParentWorkspace() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestEngineRun_NonSubbotChildKeepsItsOwnWorktree: a run that carries the
// parent linkage WITHOUT the spawning node — a shard child, a fork — is a run
// of its own in a workspace of its own, and keeps its isolated worktree. The
// parent linkage alone is not the question; the subbot node is.
func TestEngineRun_NonSubbotChildKeepsItsOwnWorktree(t *testing.T) {
	s := tmpStore(t)
	repo, _ := initBareishRepo(t)

	eng := New(autoWorktreeWorkflow("shard-child"), s, newStubExecutor(),
		WithWorkDir(repo),
		WithParentRunID("run-parent"),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-shard-child"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run for a shard child: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !r.Worktree {
		t.Fatalf("run.Worktree = false, want true (a shard child is isolated like any root run)")
	}
	if want := filepath.Join(s.Root(), "worktrees", runID); r.WorkDir != want {
		t.Errorf("run.WorkDir = %q, want its own worktree %q", r.WorkDir, want)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("run status = %q, want %q", r.Status, store.RunStatusFinished)
	}
}

// TestEngineRun_AnIsolatedSubbotChildKeepsItsOwnWorktree: a subbot child whose
// parent node declares `isolated:` keeps the worktree `worktree: auto` gives
// it. The flag is the author's assertion that the child writes no shared
// workspace — and the engine's own guard admits a parallel fan-out on it, so
// taking the parent's tree away from such a child would both contradict the
// assertion and serialize the fan-out it authorizes.
func TestEngineRun_AnIsolatedSubbotChildKeepsItsOwnWorktree(t *testing.T) {
	s := tmpStore(t)
	repo, originalTip := initBareishRepo(t)

	eng := New(autoWorktreeWorkflow("isolated-child"), s, newStubExecutor(),
		WithWorkDir(repo),
		WithParentRunID("run-parent"),
		WithParentNodeID("spawn_child"),
		WithIsolatedChild(true),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-isolated-child"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run for an isolated subbot child: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !r.Worktree {
		t.Fatalf("run.Worktree = false, want true (an `isolated:` child owns its workspace)")
	}
	if want := filepath.Join(s.Root(), "worktrees", runID); r.WorkDir != want {
		t.Errorf("run.WorkDir = %q, want its own worktree %q", r.WorkDir, want)
	}
	if r.RepoRoot != repo || r.BaseCommit != originalTip {
		t.Errorf("run baseline = {RepoRoot:%q BaseCommit:%q}, want {%q %q} — a run owning its worktree carries the pair its finalization needs", r.RepoRoot, r.BaseCommit, repo, originalTip)
	}
}

// TestEngineRun_IsolatedSubbotSiblingsOverlap is the executable form of the
// promise `isolated:` carries: with max_parallel_branches: N the children run
// CONCURRENTLY. The resource gate that protects a lent workspace is exclusive
// per tree and held for a borrowing child's whole life, so children sharing
// one tree can only take turns — the peak is the witness, and a barrier each
// child waits on is how a serialized implementation shows itself (it cannot
// open it) without a sleep deciding the verdict.
//
// Unlike TestFanOutEach_SubbotParallel_AllowedWhenIsolated, whose runner is a
// counting stub that never builds a child engine, this runner builds the real
// thing with the option set every host runner builds — so what the child's own
// Engine.Run serializes is measured here.
func TestEngineRun_IsolatedSubbotSiblingsOverlap(t *testing.T) {
	const n = 3
	s := tmpStore(t)
	repo, _ := initBareishRepo(t)

	var active, peak int32
	barrier := make(chan struct{})
	var once sync.Once
	childExec := newStubExecutor()
	childExec.on("start", func(map[string]any) (map[string]any, error) {
		cur := atomic.AddInt32(&active, 1)
		for { // lock-free max
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		if cur >= int32(n) {
			once.Do(func() { close(barrier) })
		}
		// Bounded: a serialized implementation must FAIL on the peak, not
		// hang the suite until the package timeout.
		select {
		case <-barrier:
		case <-time.After(10 * time.Second):
		}
		atomic.AddInt32(&active, -1)
		return map[string]any{}, nil
	})

	var mu sync.Mutex
	childIDs := []string{}
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		mu.Lock()
		childID := fmt.Sprintf("run-sibling-%d", len(childIDs))
		childIDs = append(childIDs, childID)
		mu.Unlock()
		child := New(autoWorktreeWorkflow("child"), s, childExec,
			WithWorkDir(req.WorkDir),
			WithParentRunID(req.ParentRunID),
			WithParentNodeID(req.NodeID),
			WithIsolatedChild(req.Isolated),
			WithLogger(log.New(log.LevelWarn, os.Stderr)),
		)
		return map[string]any{}, child.Run(ctx, childID, nil)
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"done": true}, nil
	})

	eng := New(subbotFanOutWorkflow(true, n), s, exec,
		WithSubbotRunner(runner),
		WithWorkDir(repo),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)
	if err := eng.Run(context.Background(), "run-isolated-siblings", nil); err != nil {
		t.Fatalf("an isolated subbot fan_out_each at max_parallel=%d must be allowed: %v", n, err)
	}
	if got := atomic.LoadInt32(&peak); got != int32(n) {
		t.Fatalf("peak child concurrency = %d, want %d — the siblings took turns instead of overlapping", got, n)
	}
	// Each sibling in a tree of its own is WHY they can overlap: the gate
	// they would otherwise queue on is keyed by workspace path.
	mu.Lock()
	ids := append([]string{}, childIDs...)
	mu.Unlock()
	trees := map[string]string{}
	for _, id := range ids {
		r, err := s.LoadRun(context.Background(), id)
		if err != nil {
			t.Fatalf("load child %s: %v", id, err)
		}
		if !r.Worktree || r.WorkDir == repo {
			t.Errorf("child %s = {Worktree:%v WorkDir:%q}, want a worktree of its own (not the parent's tree %q)", id, r.Worktree, r.WorkDir, repo)
		}
		if other, dup := trees[r.WorkDir]; dup {
			t.Errorf("children %s and %s share the workspace %q", other, id, r.WorkDir)
		}
		trees[r.WorkDir] = id
	}
	if len(trees) != n {
		t.Errorf("the fan-out produced %d distinct child workspaces, want %d", len(trees), n)
	}
}

// noneWorktreeWorkflow is autoWorktreeWorkflow's opt-out twin: the child's own
// `.bot` declaring `worktree: none`, which no parent assertion can overrule.
func noneWorktreeWorkflow(name string) *ir.Workflow {
	wf := autoWorktreeWorkflow(name)
	wf.Worktree = "none"
	return wf
}

// TestEngineRun_IsolatedSiblingsQueueWhenAutoCannotGiveEachATree is the
// precondition of the promise the previous test measures: `isolated:` keeps the
// worktree `worktree: auto` GIVES the child, so a child that does not get one
// is back in the tree its parent lent it — and there the workspace resource
// gate, exclusive per tree and held for a borrowing child's whole life, makes
// the siblings take turns however high `max_parallel_branches` is. Every shape
// that leaves worktreeActive false reaches it, and an author who reads only
// "isolated siblings never queue" would mis-size a fan-out in each:
//
//   - the child's own `.bot` declares `worktree: none` (its explicit opt-out
//     wins — an isolated assertion is about the parent's tree, not a request
//     for isolation the child refused);
//   - the workspace is not a git repository;
//   - the workspace has no commits yet (an unborn HEAD anchors no worktree);
//   - the store has no filesystem root (the cloud store);
//   - the child adopts its parent's live sandbox (a `sandbox:` run, a runner
//     pod), which mounts ONE tree whatever the node declares — the only one
//     of the five where `auto` had a worktree to give.
//
// The peak is the witness, and the shared tree is why: each case asserts both,
// so a change that hands these children a tree of their own reddens here and
// in the doc paragraph this pins rather than silently widening the promise.
// A peak tells serialization from a slow dispatch only while every branch is
// in flight: the runner's rendezvous holds each child engine until all n
// branches are, and the test reports the count instead of assuming it.
func TestEngineRun_IsolatedSiblingsQueueWhenAutoCannotGiveEachATree(t *testing.T) {
	const n = 3
	cases := []struct {
		name    string
		childWf func(string) *ir.Workflow
		store   func(*testing.T) store.RunStore
		workDir func(*testing.T) string
		// extra: the options a host runner adds in this shape — for an
		// adopted sandbox, the parent's live handle and the one workspace
		// every delegate command sees through it.
		extra func(workDir string) []EngineOption
	}{
		{
			name:    "the child's own .bot declares worktree: none",
			childWf: noneWorktreeWorkflow,
			store:   tmpStore,
			workDir: func(t *testing.T) string { repo, _ := initBareishRepo(t); return repo },
		},
		{
			name:    "the workspace is not a git repository",
			childWf: autoWorktreeWorkflow,
			store:   tmpStore,
			workDir: func(t *testing.T) string { return t.TempDir() },
		},
		{
			name:    "the workspace has no commits yet",
			childWf: autoWorktreeWorkflow,
			store:   tmpStore,
			workDir: func(t *testing.T) string {
				dir := t.TempDir()
				gittest.Run(t, dir, "init", "-b", "main")
				return dir
			},
		},
		{
			name:    "the store has no filesystem root",
			childWf: autoWorktreeWorkflow,
			store:   func(t *testing.T) store.RunStore { return rootlessStore{RunStore: tmpStore(t)} },
			workDir: func(t *testing.T) string { repo, _ := initBareishRepo(t); return repo },
		},
		{
			// A store WITH a root and a git repo WITH commits: `auto` could
			// have produced a worktree here, and the adopted sandbox is what
			// overrides the declaration.
			name:    "the child adopts its parent's live sandbox",
			childWf: autoWorktreeWorkflow,
			store:   tmpStore,
			workDir: func(t *testing.T) string { repo, _ := initBareishRepo(t); return repo },
			extra: func(workDir string) []EngineOption {
				return []EngineOption{WithSharedSandbox(&SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: workDir})}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.store(t)
			repo := tc.workDir(t)

			var active, peak, dispatched, inFlightShort int32
			barrier := make(chan struct{})
			inFlight := make(chan struct{})
			var once, onceInFlight sync.Once
			childExec := newStubExecutor()
			childExec.on("start", func(map[string]any) (map[string]any, error) {
				cur := atomic.AddInt32(&active, 1)
				for { // lock-free max
					p := atomic.LoadInt32(&peak)
					if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
						break
					}
				}
				if cur >= int32(n) {
					once.Do(func() { close(barrier) })
				}
				// Short on purpose: serialization is what this test EXPECTS,
				// so every child pays this wait. Its job is to hold the
				// overlap window open while the siblings — in flight already,
				// see the runner's rendezvous — reach this handler; one that
				// the lent tree's writer does not hold back closes the
				// barrier instead of waiting.
				select {
				case <-barrier:
				case <-time.After(250 * time.Millisecond):
				}
				atomic.AddInt32(&active, -1)
				return map[string]any{}, nil
			})

			var mu sync.Mutex
			childIDs := []string{}
			runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
				// No child engine starts before every branch is in flight:
				// a peak of 1 only says "they took turns" while they had the
				// chance to overlap. A branch the fan-out never dispatched is
				// reported, not waited out — inFlightShort fails the test.
				if atomic.AddInt32(&dispatched, 1) >= int32(n) {
					onceInFlight.Do(func() { close(inFlight) })
				}
				select {
				case <-inFlight:
				case <-time.After(10 * time.Second):
					atomic.StoreInt32(&inFlightShort, atomic.LoadInt32(&dispatched))
				}
				mu.Lock()
				childID := fmt.Sprintf("run-queued-sibling-%d", len(childIDs))
				childIDs = append(childIDs, childID)
				mu.Unlock()
				opts := []EngineOption{
					WithWorkDir(req.WorkDir),
					WithParentRunID(req.ParentRunID),
					WithParentNodeID(req.NodeID),
					WithIsolatedChild(req.Isolated),
					WithLogger(log.New(log.LevelError, os.Stderr)),
				}
				if tc.extra != nil {
					opts = append(opts, tc.extra(repo)...)
				}
				child := New(tc.childWf("child"), s, childExec, opts...)
				return map[string]any{}, child.Run(ctx, childID, nil)
			}

			exec := newStubExecutor()
			exec.on("entry", func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
			})
			exec.on("collect", func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"done": true}, nil
			})

			eng := New(subbotFanOutWorkflow(true, n), s, exec,
				WithSubbotRunner(runner),
				WithWorkDir(repo),
				WithLogger(log.New(log.LevelError, os.Stderr)),
			)
			if err := eng.Run(context.Background(), "run-queued-siblings", nil); err != nil {
				t.Fatalf("an isolated subbot fan_out_each must still be admitted: %v", err)
			}

			if k := atomic.LoadInt32(&inFlightShort); k != 0 {
				t.Fatalf("the witness did not hold: %d of %d branches in flight — a peak of 1 cannot tell serialization from a dispatch that never happened", k, n)
			}
			if got := atomic.LoadInt32(&peak); got != 1 {
				t.Errorf("peak child concurrency = %d, want 1 — siblings sharing one lent tree take turns on its exclusive writer", got)
			}
			mu.Lock()
			ids := append([]string{}, childIDs...)
			mu.Unlock()
			if len(ids) != n {
				t.Fatalf("the fan-out ran %d children, want %d", len(ids), n)
			}
			for _, id := range ids {
				r, err := s.LoadRun(context.Background(), id)
				if err != nil {
					t.Fatalf("load child %s: %v", id, err)
				}
				if r.Worktree {
					t.Errorf("child %s: run.Worktree = true, want false — `worktree: auto` cannot produce a worktree in this shape", id)
				}
				if r.WorkDir != repo {
					t.Errorf("child %s: run.WorkDir = %q, want the parent's tree %q", id, r.WorkDir, repo)
				}
			}
		})
	}
}

// TestIsolatedChildsCommitIsReplayableOnlyWhereAutoGivesItATree pins what
// `isolated:` is worth to a child whose PRODUCT is a commit — the remedy the
// replay paragraphs of docs/groups-iteration-subbots.md and docs/resume.md
// offer. The declaration keeps the worktree `worktree: auto` gives the child,
// so where auto can give one the child commits in a tree of its own and a
// re-execution meets a tree without that commit: the child's commits ride a
// storage branch of their own, which only an operator's merge brings into the
// lent tree (finalization leaves it pending — no subbot runner hands a child
// `WithAutoMerge`). Where auto can give none —
// a store with no filesystem root (the cloud store), an adopted sandbox —
// the SAME isolated child commits in the lent tree, the commit outlives the
// attempt, and the re-execution dies on an empty index. A remedy stated
// without that precondition is one a reader applies where it does nothing.
func TestIsolatedChildsCommitIsReplayableOnlyWhereAutoGivesItATree(t *testing.T) {
	cases := []struct {
		name string
		// ownTree: `worktree: auto` really has a worktree to give here.
		ownTree bool
		store   func(*testing.T) store.RunStore
		extra   func(workDir string) []EngineOption
	}{
		{
			name:    "auto can give the child a tree of its own",
			ownTree: true,
			store:   tmpStore,
		},
		{
			name:  "the store has no filesystem root (the cloud store)",
			store: func(t *testing.T) store.RunStore { return rootlessStore{RunStore: tmpStore(t)} },
		},
		{
			name:  "the child adopts its parent's live sandbox",
			store: tmpStore,
			extra: func(workDir string) []EngineOption {
				return []EngineOption{WithSharedSandbox(&SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: workDir})}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.store(t)
			lent, tip0 := initBareishRepo(t)

			var attempt int32
			exec := &workDirProbeExecutor{stubExecutor: newStubExecutor()}
			exec.on("start", func(map[string]any) (map[string]any, error) {
				// The child's product IS a commit, and its work is
				// deterministic: the second execution produces the same
				// artefact, so an index that already carries it is empty.
				dir := exec.workDir
				if err := os.WriteFile(filepath.Join(dir, "artefact.txt"), []byte("the child's product\n"), 0o644); err != nil {
					return nil, err
				}
				n := atomic.LoadInt32(&attempt)
				if out, err := gittest.Try(dir, "add", "-A"); err != nil {
					return nil, fmt.Errorf("attempt %d: git add -A in %s: %v: %s", n, dir, err, out)
				}
				if out, err := gittest.Try(dir, "commit", "-q", "-m", "child: produce the artefact"); err != nil {
					return nil, fmt.Errorf("attempt %d: git commit in %s: %v: %s", n, dir, err, out)
				}
				return map[string]any{}, nil
			})

			// The option set every subbot runner hands a child declared
			// `isolated:` (pkg/cli, pkg/runner, pkg/runview, pkg/dispatcher).
			runChild := func(runID string) (*store.Run, error) {
				atomic.AddInt32(&attempt, 1)
				opts := []EngineOption{
					WithWorkDir(lent),
					WithParentRunID("run-parent"),
					WithParentNodeID("spawn_child"),
					WithIsolatedChild(true),
					WithLogger(log.New(log.LevelError, os.Stderr)),
				}
				if tc.extra != nil {
					opts = append(opts, tc.extra(lent)...)
				}
				eng := New(autoWorktreeWorkflow("isolated-committing-child"), s, exec, opts...)
				err := eng.Run(context.Background(), runID, nil)
				r, loadErr := s.LoadRun(context.Background(), runID)
				if loadErr != nil {
					t.Fatalf("load child run %s: %v", runID, loadErr)
				}
				return r, err
			}

			first, firstErr := runChild("isolated-child-1")
			if firstErr != nil {
				t.Fatalf("the child's first execution must finish: %v", firstErr)
			}
			tip1 := gittest.Run(t, lent, "rev-parse", "HEAD")
			_, replayErr := runChild("isolated-child-2")

			if first.Worktree != tc.ownTree {
				t.Fatalf("run.Worktree = %v, want %v — `isolated:` is worth exactly the worktree `worktree: auto` can give", first.Worktree, tc.ownTree)
			}
			if inLent := first.WorkDir == lent; inLent == tc.ownTree {
				t.Errorf("run.WorkDir = %q (the lent tree is %q): a child with a tree of its own must not work in the lent one, and one without must", first.WorkDir, lent)
			}
			if moved := tip1 != tip0; moved == tc.ownTree {
				t.Errorf("the lent tree's HEAD moved=%v (%s -> %s), want %v — a child's own commits stay on its own branch, an in-place child's land here", moved, tip0, tip1, !tc.ownTree)
			}
			if tc.ownTree {
				if replayErr != nil {
					t.Errorf("the replay of a child that owns its tree must finish (it meets a tree without its commit), got: %v", replayErr)
				}
				// Why the lent tree stays put: finalization banks the
				// child's commits on a storage branch and leaves the merge
				// for an operator (no subbot runner sets auto_merge).
				if first.FinalBranch == "" {
					t.Errorf("run.FinalBranch is empty: the child's commits must be banked on a storage branch of their own")
				}
				if first.MergeStatus != store.MergeStatusPending {
					t.Errorf("run.MergeStatus = %q, want %q — only an operator's merge brings a child's branch into the lent tree", first.MergeStatus, store.MergeStatusPending)
				}
				return
			}
			if replayErr == nil {
				t.Fatalf("the replay of an isolated child working in the lent tree must die on an empty index — the commit of its first execution is still there (HEAD %s)", tip1)
			}
			if !strings.Contains(replayErr.Error(), "nothing to commit") {
				t.Errorf("the replay failed on %v, want git's own \"nothing to commit\"", replayErr)
			}
		})
	}
}
