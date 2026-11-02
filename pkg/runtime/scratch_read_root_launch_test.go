package runtime

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// scratchPushSpy records the run-level pushes, values included: the
// scratch read roots the engine attaches (#2650, narrowed by #2869) and
// the vars map the nodes resolve their templates from. The vars matter
// because they carry the SAME ${PROJECT_SCRATCH_DIR} the bot is told to
// write under — the two values must name one directory.
type scratchPushSpy struct {
	scratchRuns   []string
	scratchShared []string
	vars          []map[string]any
	workDirs      []string
	repoRoots     []string
}

func (x *scratchPushSpy) Execute(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (x *scratchPushSpy) SetWorkDir(d string)  { x.workDirs = append(x.workDirs, d) }
func (x *scratchPushSpy) SetRepoRoot(r string) { x.repoRoots = append(x.repoRoots, r) }
func (x *scratchPushSpy) SetScratchDirs(run, shared string) {
	x.scratchRuns = append(x.scratchRuns, run)
	x.scratchShared = append(x.scratchShared, shared)
}
func (x *scratchPushSpy) SetVars(v map[string]any) {
	copied := make(map[string]any, len(v))
	for k, val := range v {
		copied[k] = val
	}
	x.vars = append(x.vars, copied)
}

func (x *scratchPushSpy) lastScratch(t *testing.T) (run, shared string) {
	t.Helper()
	if len(x.scratchRuns) == 0 {
		t.Fatal("the engine never pushed a scratch read root — claw's read tools keep refusing the run's own briefs (#2650)")
	}
	return x.scratchRuns[len(x.scratchRuns)-1], x.scratchShared[len(x.scratchShared)-1]
}

// TestEngineRun_WorktreeAutoPushesTheScratchRootTheNodesAreGiven is the
// launch-path half of #2650, end to end on a real `worktree: auto` run.
//
// ${PROJECT_SCRATCH_DIR} is keyed on the run's REPO ROOT, which Run()
// settles only after the worktree exists; the read root must therefore be
// pushed from the same settled state, not from an earlier point where
// e.repoRoot is still empty and the value collapses onto the per-run
// worktree path. When the two disagree the bot writes its briefs in one
// directory and claw's read tools accept the other — the refusal the
// ticket exists to remove, with no error anywhere to show it.
func TestEngineRun_WorktreeAutoPushesTheScratchRootTheNodesAreGiven(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	wf := &ir.Workflow{
		Name:  "scratch-read-root",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "done"}},
		// The bot's own view of the scratch: a var expanded by the engine,
		// exactly as a `run_dir: ${PROJECT_SCRATCH_DIR}/...` declaration is.
		Vars: map[string]*ir.Var{
			"run_dir": {Name: "run_dir", Type: ir.VarString, HasDefault: true, Default: "${PROJECT_SCRATCH_DIR}"},
		},
		Worktree: "auto",
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	spy := &scratchPushSpy{}
	eng := New(wf, s, spy, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))

	const runID = "run-scratch-read-root"
	ctx := context.Background()
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	// Fixture invariant: this must be the worktree arm, or the two
	// spellings of the scratch dir coincide and the test proves nothing.
	if !r.Worktree || r.RepoRoot != repo || r.WorkDir == repo {
		t.Fatalf("fixture invariant broken: Worktree=%v RepoRoot=%q WorkDir=%q (want a worktree run rooted at %q)",
			r.Worktree, r.RepoRoot, r.WorkDir, repo)
	}
	if memory.WorkspaceScratchDir(r.WorkDir) == memory.WorkspaceScratchDir(repo) {
		t.Fatalf("fixture invariant broken: the worktree and the repo root encode the same scratch key (%q)", memory.WorkspaceScratchDir(repo))
	}

	pushedRun, pushedShared := spy.lastScratch(t)
	shared := memory.WorkspaceScratchDir(repo)
	// The narrowing (#2869): the FIRST root is the run's own subtree; the
	// shared project scratch rides along because this workflow's run_dir
	// still references ${PROJECT_SCRATCH_DIR}.
	if want := filepath.Join(shared, "runs", runID); pushedRun != want {
		t.Errorf("pushed run read root = %q, want the run's own subtree %q", pushedRun, want)
	}
	if pushedShared != shared {
		t.Errorf("pushed shared read root = %q, want the repo-root-keyed %q (the workflow references it)", pushedShared, shared)
	}
	// The value the NODES were given, from the same push: one directory,
	// not two.
	if len(spy.vars) == 0 {
		t.Fatal("the engine never pushed the resolved vars")
	}
	nodeView, _ := spy.vars[len(spy.vars)-1]["run_dir"].(string)
	if nodeView != pushedShared {
		t.Errorf("${PROJECT_SCRATCH_DIR} resolved to %q for the nodes, but the shared read root pushed is %q", nodeView, pushedShared)
	}

	// End to end: the bot writes a brief where its var points, and claw's
	// read tools, scoped on the workspace plus the PUSHED roots, read it.
	briefDir := filepath.Join(nodeView, "rgaa-audit", runID, "briefs")
	if err := os.MkdirAll(briefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(briefDir, "1.1.md")
	if err := os.WriteFile(brief, []byte("# 1.1 Images\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := tool.NewReadScope(r.WorkDir)
	scope.AddExtraRoot(pushedRun)
	scope.AddExtraRoot(pushedShared)
	reg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	read, err := reg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	out, err := read.Execute(ctx, []byte(`{"path": "`+brief+`"}`))
	if err != nil {
		t.Fatalf("read_file(%q) through the pushed read root: %v", brief, err)
	}
	if out != "# 1.1 Images\n" {
		t.Errorf("read_file returned %q, want the brief", out)
	}
}

// TestScratchRootFollowsTheSandboxDecision characterises the second fact
// the push waits for: a sandboxed run's ${PROJECT_SCRATCH_DIR} is the
// FIXED container path, because the host dir is not mounted under its own
// name in the container. It is why the push cannot precede startSandbox,
// and it is the claim the scratchDirSetter seam makes in words.
func TestScratchRootFollowsTheSandboxDecision(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	repo := t.TempDir()

	host := &Engine{}
	host.workDir, host.repoRoot = repo, repo
	if got, want := host.projectScratchDirVarValue(), memory.WorkspaceScratchDir(repo); got != want {
		t.Errorf("unsandboxed scratch = %q, want the repo-root-keyed %q", got, want)
	}

	sandboxed := &Engine{}
	sandboxed.workDir, sandboxed.repoRoot = repo, repo
	sandboxed.containerWorkspace = "/workspace"
	if got := sandboxed.projectScratchDirVarValue(); got != sandboxScratchContainerPath {
		t.Errorf("sandboxed scratch = %q, want the fixed container path %q", got, sandboxScratchContainerPath)
	}

	// ${RUN_SCRATCH_DIR} (#2869) rides the same resolver: the host value
	// keys on the repo root, the sandboxed one on the fixed container
	// path — both under runs/<family root>, the run's own id here.
	host.runScratchOwner = "run-x"
	if got, want := host.runScratchDirVarValue(), filepath.Join(memory.WorkspaceScratchDir(repo), "runs", "run-x"); got != want {
		t.Errorf("host run scratch = %q, want %q", got, want)
	}
	sandboxed.runScratchOwner = "run-x"
	if got, want := sandboxed.runScratchDirVarValue(), sandboxScratchContainerPath+"/runs/run-x"; got != want {
		t.Errorf("sandboxed run scratch = %q, want %q", got, want)
	}
	// An owner never settled falls back to the shared scratch, not a
	// subtree keyed on nothing.
	unset := &Engine{}
	unset.workDir, unset.repoRoot = repo, repo
	if got := unset.runScratchDirVarValue(); got != memory.WorkspaceScratchDir(repo) {
		t.Errorf("owner-less run scratch = %q, want the shared scratch fallback", got)
	}
}

// TestPushExecutorVarsIsTheOnlyScratchRootPush pins the chokepoint: the
// scratch read root is pushed from one place, the place every path
// reaches once e.repoRoot and the sandbox decision are settled. A second
// push site is how the launch arm and the resume arms came to disagree —
// and how a seventh entry point would silently inherit vars without the
// root.
func TestPushExecutorVarsIsTheOnlyScratchRootPush(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "SetScratchDirs", "SetVars":
					sites[sel.Sel.Name] = append(sites[sel.Sel.Name], name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	// Both run-level values ride the same push, from the same one site:
	// vars, so a resume arm cannot hand a node a stale template value, and
	// the scratch root, so none can hand its agents a root keyed on a fact
	// that was not settled yet.
	for _, pushed := range []string{"SetScratchDirs", "SetVars"} {
		if got := sites[pushed]; len(got) != 1 || got[0] != "resume.go:pushExecutorVars" {
			t.Errorf("%s is called from %v; want exactly one site, resume.go:pushExecutorVars", pushed, got)
		}
	}
}

// TestEngineRun_NarrowedScratchRootWhenTheSharedIsUnreferenced is the
// #2869 witness on the launch path: a workflow whose resolved vars never
// name ${PROJECT_SCRATCH_DIR} gets the run's own subtree as its ONLY
// scratch read root — another run's leftovers under the shared project
// scratch refuse, its own read.
func TestEngineRun_NarrowedScratchRootWhenTheSharedIsUnreferenced(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	wf := &ir.Workflow{
		Name:  "scratch-read-root-narrowed",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "done"}},
		// The bot's state all moved to its own subtree — the migration
		// #2869's run_scratch half names.
		Vars: map[string]*ir.Var{
			"own_dir": {Name: "own_dir", Type: ir.VarString, HasDefault: true, Default: "${RUN_SCRATCH_DIR}"},
		},
		Worktree: "auto",
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	spy := &scratchPushSpy{}
	eng := New(wf, s, spy, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))

	const runID = "run-scratch-narrowed"
	ctx := context.Background()
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	shared := memory.WorkspaceScratchDir(repo)
	wantRun := filepath.Join(shared, "runs", runID)

	pushedRun, pushedShared := spy.lastScratch(t)
	if pushedRun != wantRun {
		t.Errorf("pushed run read root = %q, want %q", pushedRun, wantRun)
	}
	if pushedShared != "" {
		t.Errorf("pushed shared read root = %q, want \"\" — no resolved var references ${PROJECT_SCRATCH_DIR}", pushedShared)
	}
	// ${RUN_SCRATCH_DIR} expanded for the nodes to the same subtree the
	// push attached — one directory, not two.
	if len(spy.vars) == 0 {
		t.Fatal("the engine never pushed the resolved vars")
	}
	nodeView, _ := spy.vars[len(spy.vars)-1]["own_dir"].(string)
	if nodeView != pushedRun {
		t.Errorf("${RUN_SCRATCH_DIR} resolved to %q for the nodes, but the run read root pushed is %q", nodeView, pushedRun)
	}

	// End to end: a brief in the run's own subtree reads; another run's
	// leftover under the shared project scratch refuses.
	ownBrief := filepath.Join(wantRun, "briefs", "1.1.md")
	if err := os.MkdirAll(filepath.Dir(ownBrief), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownBrief, []byte("# Own\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherBrief := filepath.Join(shared, "runs", "run-OTHER", "briefs", "0.9.md")
	if err := os.MkdirAll(filepath.Dir(otherBrief), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherBrief, []byte("# Other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := tool.NewReadScope(r.WorkDir)
	scope.AddExtraRoot(pushedRun)
	reg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	read, err := reg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	out, err := read.Execute(ctx, []byte(`{"path": "`+ownBrief+`"}`))
	if err != nil || out != "# Own\n" {
		t.Fatalf("read_file(%q) under the run subtree = %q, %v", ownBrief, out, err)
	}
	if _, err := read.Execute(ctx, []byte(`{"path": "`+otherBrief+`"}`)); err == nil ||
		!strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("read_file(%q) — another run's leftover — must refuse under the narrowed scope, got %v", otherBrief, err)
	}
}
