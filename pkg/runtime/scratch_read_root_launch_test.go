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
// scratch read root the engine attaches (#2650) and the vars map the
// nodes resolve their templates from. The vars matter because they carry
// the SAME ${PROJECT_SCRATCH_DIR} the bot is told to write under — the
// two values must name one directory.
type scratchPushSpy struct {
	scratchDirs []string
	vars        []map[string]any
	workDirs    []string
	repoRoots   []string
}

func (x *scratchPushSpy) Execute(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (x *scratchPushSpy) SetWorkDir(d string)    { x.workDirs = append(x.workDirs, d) }
func (x *scratchPushSpy) SetRepoRoot(r string)   { x.repoRoots = append(x.repoRoots, r) }
func (x *scratchPushSpy) SetScratchDir(d string) { x.scratchDirs = append(x.scratchDirs, d) }
func (x *scratchPushSpy) SetVars(v map[string]any) {
	copied := make(map[string]any, len(v))
	for k, val := range v {
		copied[k] = val
	}
	x.vars = append(x.vars, copied)
}

func (x *scratchPushSpy) lastScratch(t *testing.T) string {
	t.Helper()
	if len(x.scratchDirs) == 0 {
		t.Fatal("the engine never pushed a scratch read root — claw's read tools keep refusing the run's own briefs (#2650)")
	}
	return x.scratchDirs[len(x.scratchDirs)-1]
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

	pushed := spy.lastScratch(t)
	if want := memory.WorkspaceScratchDir(repo); pushed != want {
		t.Errorf("pushed read root = %q, want the repo-root-keyed %q", pushed, want)
	}
	// The value the NODES were given, from the same push: one directory,
	// not two.
	if len(spy.vars) == 0 {
		t.Fatal("the engine never pushed the resolved vars")
	}
	nodeView, _ := spy.vars[len(spy.vars)-1]["run_dir"].(string)
	if nodeView != pushed {
		t.Errorf("${PROJECT_SCRATCH_DIR} resolved to %q for the nodes, but the read root pushed is %q", nodeView, pushed)
	}

	// End to end: the bot writes a brief where its var points, and claw's
	// read tools, scoped on the workspace plus the PUSHED root, read it.
	briefDir := filepath.Join(nodeView, "rgaa-audit", runID, "briefs")
	if err := os.MkdirAll(briefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(briefDir, "1.1.md")
	if err := os.WriteFile(brief, []byte("# 1.1 Images\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := tool.NewReadScope(r.WorkDir)
	scope.AddExtraRoot(pushed)
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
				case "SetScratchDir", "SetVars":
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
	for _, pushed := range []string{"SetScratchDir", "SetVars"} {
		if got := sites[pushed]; len(got) != 1 || got[0] != "resume.go:pushExecutorVars" {
			t.Errorf("%s is called from %v; want exactly one site, resume.go:pushExecutorVars", pushed, got)
		}
	}
}
