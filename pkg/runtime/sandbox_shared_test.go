package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sharedFakeRun is a live sandbox handle a PARENT run would own: commands
// run on this host (so a tool-only child really executes), every call is
// counted, and the copy-based write-through seam records what a child
// pushed into "the pod".
type sharedFakeRun struct {
	mu       sync.Mutex
	commands int
	cleanups int
	written  map[string][]byte
}

func (f *sharedFakeRun) Driver() string { return "fake-shared" }
func (f *sharedFakeRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	f.mu.Lock()
	f.commands++
	f.mu.Unlock()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = opts.WorkDir
	c.Env = os.Environ()
	for k, v := range opts.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	return c
}
func (f *sharedFakeRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := f.Command(ctx, cmd, opts).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return sandbox.ExecResult{ExitCode: code, Stdout: out}, nil
}
func (f *sharedFakeRun) Cleanup(context.Context) error {
	f.mu.Lock()
	f.cleanups++
	f.mu.Unlock()
	return nil
}
func (f *sharedFakeRun) RefreshWorkspaceFile(_ context.Context, rel string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.written == nil {
		f.written = map[string][]byte{}
	}
	f.written[rel] = append([]byte(nil), value...)
	return nil
}

// sandboxCapturingExecutor is a stub executor that also accepts the live
// sandbox the engine hands it, as ClawExecutor does.
type sandboxCapturingExecutor struct {
	*stubExecutor
	sandbox sandbox.Run
}

func (s *sandboxCapturingExecutor) SetSandbox(run sandbox.Run) { s.sandbox = run }

// TestStartSandboxSharedAdoptsTheParentRun: a child engine handed its
// parent's live sandbox settles on it — the executor routes through the
// parent's handle, ${PROJECT_DIR} remaps to the parent's in-container
// workspace, the child's mirrored skills are written through into a
// copy-based sandbox, the run says so in its events — and never
// prepares, starts or cleans up a sandbox of its own.
func TestStartSandboxSharedAdoptsTheParentRun(t *testing.T) {
	st := tmpStore(t)
	workDir := t.TempDir()
	// A skill the child's bundle mirrored into the host workdir before the
	// sandbox settles — invisible to a copy-based parent pod unless pushed.
	if err := os.MkdirAll(filepath.Join(workDir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".claude", "skills", "child-skill.md"), []byte("# child skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &sharedFakeRun{}
	exec := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}
	e := New(wf, st, exec,
		WithWorkDir(workDir),
		WithSandboxOverride("auto"), // would try to start one of its own — must be ignored
		WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: fake, WorkspaceFolder: "/workspace", SharedStateDir: "/shared"}),
	)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
		t.Fatal(err)
	}
	cleanup, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
	if err != nil {
		t.Fatalf("startSandbox: %v", err)
	}
	if exec.sandbox != fake {
		t.Fatalf("executor sandbox = %v, want the parent's handle", exec.sandbox)
	}
	if !e.sandboxSettled || e.containerWorkspace != "/workspace" {
		t.Fatalf("settled=%v containerWorkspace=%q, want settled on the parent's workspace", e.sandboxSettled, e.containerWorkspace)
	}
	if e.activeShare == nil || e.activeShare.Run != fake {
		t.Fatal("activeShare must carry the shared facts on to grandchildren")
	}
	if got := string(fake.written[".claude/skills/child-skill.md"]); got != "# child skill\n" {
		t.Fatalf("the child's mirrored skill was not written through into the parent's sandbox: %q", got)
	}
	cleanup()
	if fake.cleanups != 0 {
		t.Fatalf("cleanups = %d, want 0: the parent owns the sandbox's lifecycle", fake.cleanups)
	}
	evs, err := st.LoadEvents(ctx, "run-child")
	if err != nil {
		t.Fatal(err)
	}
	var shared, started int
	for _, ev := range evs {
		switch ev.Type {
		case store.EventSandboxShared:
			shared++
			if ev.Data["parent_run"] != "run-parent" || ev.Data["driver"] != "fake-shared" {
				t.Fatalf("sandbox_shared data = %v", ev.Data)
			}
		case store.EventSandboxStarted:
			started++
		}
	}
	if shared != 1 || started != 0 {
		t.Fatalf("events: sandbox_shared=%d sandbox_started=%d, want 1/0 (no sandbox of its own)", shared, started)
	}
}

// TestSubbotNodeHandsTheParentSandboxToTheChild: the request a subbot
// node makes carries the sandbox facts of the run it belongs to — nil when
// the parent has none.
func TestSubbotNodeHandsTheParentSandboxToTheChild(t *testing.T) {
	wf := compileBot(t, `
schema out:
  ok: bool

subbot kid:
  source: "kid.bot"
  output: out

workflow parent:
  entry: kid
  kid -> done
`).Workflow
	for _, tc := range []struct {
		name  string
		share *SharedSandbox
	}{
		{"parent sandboxed", &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"}},
		{"parent unsandboxed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *SharedSandbox
			var called bool
			opts := []EngineOption{
				WithSandboxOverride("none"),
				WithSubbotRunner(func(_ context.Context, req SubbotRequest) (map[string]any, error) {
					called = true
					got = req.ParentSandbox
					return map[string]any{"ok": true}, nil
				}),
			}
			// A sandboxed parent: it executes in a live sandbox (here, one
			// handed to it — the grandchild shape; an own sandbox settles
			// the same facts).
			if tc.share != nil {
				opts = append(opts, WithSharedSandbox(tc.share))
			}
			e := New(wf, tmpStore(t), newStubExecutor(), opts...)
			if err := e.Run(context.Background(), "run-"+tc.name, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !called {
				t.Fatal("the subbot runner was not invoked")
			}
			if got != tc.share {
				t.Fatalf("ParentSandbox = %v, want %v", got, tc.share)
			}
		})
	}
}
