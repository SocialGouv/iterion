package runview

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// stubSandboxDriver is a constructible sandbox.Driver for the registry
// seam: the fallback screen's sandbox PROBE only ever calls Name —
// Prepare/Start are the engine's, and no test here reaches them.
type stubSandboxDriver struct{ name string }

func (d stubSandboxDriver) Name() string { return d.name }
func (d stubSandboxDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsHostBindMounts: true}
}
func (d stubSandboxDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return nil, nil
}
func (d stubSandboxDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, nil
}

// driverlessSandboxRegistry mimics a host with no container runtime:
// docker/podman constructors fail the way the real ones do when the
// binary is absent, and only the always-constructible noop remains —
// exactly what registry.Default() yields on such a host. Mirrors the
// pkg/runtime helper of the same shape (driverlessHostRegistry); each
// package owns its copy because test helpers do not cross packages.
func driverlessSandboxRegistry() map[string]sandbox.DriverConstructor {
	fail := func(name string) sandbox.DriverConstructor {
		return func() (sandbox.Driver, error) {
			return nil, &sandbox.ErrUnavailable{Driver: name, Reason: "not installed"}
		}
	}
	return map[string]sandbox.DriverConstructor{
		"docker": fail("docker"),
		"podman": fail("podman"),
		"noop":   noop.Constructor,
	}
}

// buildWithCodexFallback runs BuildExecutor over a one-agent workflow
// carrying `sandbox: auto`, with the operator's `--fallback codex:…`
// stage, against the given driver set, and returns the run's timeline.
func buildWithCodexFallback(t *testing.T, drivers map[string]sandbox.DriverConstructor) []*store.Event {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"

	spec := ExecutorSpec{
		Ctx:   context.Background(),
		Store: st,
		RunID: "run-fallback-codex",
		Workflow: &ir.Workflow{
			Name:    "canary",
			Nodes:   map[string]ir.Node{"work": agent},
			Sandbox: &ir.SandboxSpec{Mode: "auto"},
		},
		// Not a git repo: mode=auto resolves its spec through the default
		// image, the same path the engine takes for such a workspace.
		WorkDir: t.TempDir(),
		// The operator's launch-time ask: survive a quota wall by crossing
		// to the codex CLI.
		RunFallback:    []ir.Fallback{{Backend: "codex", Model: "gpt-5.4"}},
		SandboxDrivers: drivers,
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	evts, err := st.LoadEvents(context.Background(), spec.RunID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evts
}

// A `sandbox: auto` run on a host with no container runtime degrades to
// UNSANDBOXED at engine start — yet its mode still resolves active, and a
// screen that reads the mode alone refuses the operator's codex fallback
// with "cannot run inside the sandbox this run resolves to" on a run that
// has no sandbox at all (#1564). The codex delegate's own guard keys on
// the DRIVER (task.Sandbox.Driver()), so the screen must too: degraded
// host → the stage is taken; a host with a real driver → it is refused.
func TestBuildExecutor_CodexFallbackScreenFollowsTheDriver(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop — CI runs on kubernetes pods (HostCloud prefers kubernetes,noop)
	// The degraded host: no refusal — dispatch would allow the stage too.
	if refusals := refusalEvents(buildWithCodexFallback(t, driverlessSandboxRegistry())); len(refusals) != 0 {
		reason, _ := refusals[0].Data["reason"].(string)
		t.Fatalf("a sandbox: auto run degraded to the host had its codex fallback refused (%q) — "+
			"the run executes on the host, where codex CAN run (#1564)", reason)
	}

	// The same launch on a host with a driver: exactly one refusal, and
	// it names the sandbox as the reason.
	refusals := refusalEvents(buildWithCodexFallback(t, map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return stubSandboxDriver{name: "docker"}, nil },
	}))
	if len(refusals) != 1 {
		t.Fatalf("a genuinely sandboxed run must refuse the codex stage exactly once, got %d", len(refusals))
	}
	reason, _ := refusals[0].Data["reason"].(string)
	if !strings.Contains(reason, "codex") || !strings.Contains(reason, "sandbox") {
		t.Errorf("the refusal must name codex and the sandbox, got %q", reason)
	}
}

// The screen's probe reads ONE directory per run: the root
// runtime.EngineRepoRoot derives from the directory the ENGINE is given. That
// choice — the engine's workDir when the surface names it, the executor's
// WorkDir otherwise — belongs to sandboxProbeRoot, and the probe must take its
// argument from there.
//
// This guard reads the CALL rather than a verdict because no verdict can tell
// the two roots apart: every non-empty root resolves an active `sandbox: auto`
// spec (the default image is never empty, and driver selection reads the mode
// alone), so only an empty root could flip the boolean. The day activeness
// depends on the repository's devcontainer is the day this screen must ALREADY
// be reading the run's own repository — and the argument is where that is
// decided.
func TestTheSandboxProbeTakesItsRootFromTheSpecsProbeRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "executor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}

	// EVERY call is judged, not the one the walk happens to reach last: the
	// screen's own probe sits mid-file, so a second consumer added below it
	// would otherwise answer for it and the screen could regress to the cwd
	// under a green gate.
	var args []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RunWillBeSandboxed" {
			return true
		}
		if len(call.Args) < 4 {
			t.Fatalf("RunWillBeSandboxed called with %d args — the repoRoot is the 4th", len(call.Args))
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, call.Args[3]); err != nil {
			t.Fatalf("print the repoRoot argument: %v", err)
		}
		args = append(args, buf.String())
		return true
	})
	if len(args) == 0 {
		t.Fatal("no RunWillBeSandboxed call in executor.go — the fallback screen no longer probes the engine's " +
			"sandbox decision, or this guard is reading the wrong file")
	}
	for i, arg := range args {
		if !strings.Contains(arg, "sandboxProbeRoot()") || strings.Contains(arg, "spec.WorkDir") {
			t.Errorf("call %d of %d resolves its repository root from %s — every RunWillBeSandboxed call here "+
				"must read spec.sandboxProbeRoot(), which answers with the directory the ENGINE receives. "+
				"spec.WorkDir alone is the executor's tool workspace: the runner, the CLI subbot child and the "+
				"studio subbot child leave it empty while handing the engine a per-run clone or the parent's "+
				"worktree, so the screen would judge the process cwd's repository instead of the run's.",
				i+1, len(args), arg)
		}
	}
}

// The probe's root is the repository of the directory the ENGINE is given, and
// that directory is not always the executor's: the cloud runner's per-run
// clone, a subbot child's inherited parent worktree and a resumed run's
// recorded workspace all travel in SandboxProbeWorkDir while WorkDir stays the
// process workspace Claw's file tools resolve against.
func TestSandboxProbeRootIsTheDirectoryTheEngineReceives(t *testing.T) {
	repo := gittest.SourceRepo(t)
	worktree := filepath.Join(t.TempDir(), "run-wt")
	gittest.Run(t, repo, "worktree", "add", "--quiet", worktree)
	t.Cleanup(func() { gittest.RemoveWorktree(t, repo, worktree) })
	toolWorkspace := gittest.SourceRepo(t) // another repository entirely
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for _, tc := range []struct {
		name string
		spec ExecutorSpec
		want string
	}{
		{"the engine's directory wins over the executor's workspace",
			ExecutorSpec{WorkDir: toolWorkspace, SandboxProbeWorkDir: worktree}, worktree},
		{"one directory for both: WorkDir answers",
			ExecutorSpec{WorkDir: worktree}, worktree},
		{"an engine given no directory is rooted at the process cwd, and so is the probe",
			ExecutorSpec{}, cwd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.sandboxProbeRoot(); got != tc.want {
				t.Fatalf("sandboxProbeRoot() = %q, want %q", got, tc.want)
			}
		})
	}

	// The cwd, not "": an engine given no directory fills its own workDir
	// with the cwd and then roots the run at THAT directory's repository.
	// runtime.EngineRepoRoot("") stops at the cwd itself, so an empty answer
	// screens a subdirectory of the repository the run executes in — which
	// every `iterion run` typed below its repository root would be.
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if got, want := runtime.EngineRepoRoot(ExecutorSpec{}.sandboxProbeRoot()), repo; got != want {
		t.Errorf("from %s the screen would probe the repository at %q; an engine given no directory roots this "+
			"run at %q — the codex screen must judge the run's repository, not the directory it was typed in",
			sub, got, want)
	}

	// And the root it resolves to is the engine's own: a run executing in a
	// linked worktree resolves the MAIN repository, which is the directory
	// the engine's sandbox resolution reads its devcontainer from.
	probed := runtime.EngineRepoRoot(ExecutorSpec{WorkDir: toolWorkspace, SandboxProbeWorkDir: worktree}.sandboxProbeRoot())
	if probed != repo {
		t.Errorf("the screen would probe %q, the engine resolves %q", probed, repo)
	}
	if probed == toolWorkspace {
		t.Errorf("the screen probed the executor's tool workspace %q — that is not where the run executes", toolWorkspace)
	}
}
