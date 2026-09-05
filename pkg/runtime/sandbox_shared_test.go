package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
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
	if err := os.MkdirAll(filepath.Join(workDir, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".claude", "commands", "child-cmd.md"), []byte("# child command\n"), 0o644); err != nil {
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
	if got := string(fake.written[".claude/commands/child-cmd.md"]); got != "# child command\n" {
		t.Fatalf("the child's plugin command was not written through: %q", got)
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

// bindOnly is a parent sandbox that bind-mounts the workspace: its method
// set has NO write-through seam, so the code reads it as bind-mount.
type bindOnly struct{ r *sharedFakeRun }

func (b bindOnly) Driver() string { return "fake-bind" }
func (b bindOnly) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	return b.r.Command(ctx, cmd, opts)
}
func (b bindOnly) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return b.r.Exec(ctx, cmd, opts)
}
func (b bindOnly) Cleanup(ctx context.Context) error { return b.r.Cleanup(ctx) }

func sharedTestEngine(t *testing.T, wf *ir.Workflow, workDir string, share *SharedSandbox, extra ...EngineOption) (*Engine, *sandboxCapturingExecutor, store.RunStore) {
	t.Helper()
	st := tmpStore(t)
	exec := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	opts := append([]EngineOption{WithWorkDir(workDir), WithSandboxOverride("none"), WithParentRunID("run-parent"), WithSharedSandbox(share)}, extra...)
	return New(wf, st, exec, opts...), exec, st
}

// TestStartSandboxShared_ExplicitNoneIsHonouredOrRefused: a child's own
// `sandbox: none` is the operator's choice — honoured under a bind-mount
// parent (host and container share the tree), refused in words under a
// copy-based one (the child's work would land on the host, outside the
// tree the parent judges). Never silently overridden either way.
func TestStartSandboxShared_ExplicitNoneIsHonouredOrRefused(t *testing.T) {
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}, Sandbox: &ir.SandboxSpec{Mode: "none"}}
	ctx := context.Background()

	t.Run("bind-mount parent: honoured", func(t *testing.T) {
		fake := &sharedFakeRun{}
		e, exec, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: bindOnly{fake}, WorkspaceFolder: "/workspace"})
		if _, err := st.CreateRun(ctx, "run-none-bind", "child", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := e.startSandbox(ctx, "run-none-bind", e.workDir, "", nil); err != nil {
			t.Fatalf("startSandbox: %v", err)
		}
		if exec.sandbox != nil {
			t.Fatalf("the child's explicit sandbox: none was overridden by the parent's sandbox (%v)", exec.sandbox)
		}
		evs, _ := st.LoadEvents(ctx, "run-none-bind")
		var saidSo bool
		for _, ev := range evs {
			if ev.Type == store.EventSandboxShared && ev.Data["adopted"] == false {
				saidSo = true
			}
		}
		if !saidSo {
			t.Fatal("honouring the declaration must be said in the events (sandbox_shared adopted=false)")
		}
	})
	t.Run("copy-based parent: refused, typed", func(t *testing.T) {
		e, _, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"})
		if _, err := st.CreateRun(ctx, "run-none-copy", "child", nil); err != nil {
			t.Fatal(err)
		}
		_, err := e.startSandbox(ctx, "run-none-copy", e.workDir, "", nil)
		if err == nil || !strings.Contains(err.Error(), "copy-based") {
			t.Fatalf("err = %v, want the typed refusal naming the copy-based parent", err)
		}
	})
}

// TestStartSandboxShared_PausingChildRefusedOnCopyBasedParent: a child
// that can park for an operator is resumed outside its parent — in a
// sandbox of its own, a fresh copy — so under a copy-based parent it is
// refused at the door; under a bind-mount parent it runs.
func TestStartSandboxShared_PausingChildRefusedOnCopyBasedParent(t *testing.T) {
	wf := compileBot(t, `
schema answer:
  confirmed: bool

prompt ask_text:
  Confirm.

human gate:
  instructions: ask_text
  output: answer
  interaction: human

workflow child:
  entry: gate
  gate -> done
`).Workflow
	ctx := context.Background()
	t.Run("copy-based: refused", func(t *testing.T) {
		e, _, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"})
		_, _ = st.CreateRun(ctx, "run-gate-copy", "child", nil)
		_, err := e.startSandbox(ctx, "run-gate-copy", e.workDir, "", nil)
		if err == nil || !strings.Contains(err.Error(), "human gate") {
			t.Fatalf("err = %v, want the typed refusal of a pausing child", err)
		}
	})
	t.Run("bind-mount: adopted", func(t *testing.T) {
		fake := &sharedFakeRun{}
		e, exec, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: bindOnly{fake}, WorkspaceFolder: "/workspace"})
		_, _ = st.CreateRun(ctx, "run-gate-bind", "child", nil)
		if _, err := e.startSandbox(ctx, "run-gate-bind", e.workDir, "", nil); err != nil {
			t.Fatalf("startSandbox: %v", err)
		}
		if exec.sandbox == nil {
			t.Fatal("a pausing child under a bind-mount parent must still adopt the parent's sandbox")
		}
	})
}

// TestRefuseResumeOfSharedChild: a child that executed in its parent's
// copy-based sandbox is not resumed on its own — a fresh copy, diverging
// work — but a bind-mount lineage resumes freely, and an engine holding a
// parent handle is never refused.
func TestRefuseResumeOfSharedChild(t *testing.T) {
	ctx := context.Background()
	mk := func(t *testing.T, id string, data map[string]any, share *SharedSandbox) (*Engine, *store.Run) {
		t.Helper()
		st := tmpStore(t)
		opts := []EngineOption{WithSandboxOverride("none")}
		if share != nil {
			opts = append(opts, WithSharedSandbox(share))
		}
		e := New(&ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}, st, newStubExecutor(), opts...)
		pc := store.AsParentedRunCreator(st)
		if pc == nil {
			t.Fatal("the filesystem store must create parented runs")
		}
		r, err := pc.CreateChildRun(ctx, id, "child", "run-parent", nil)
		if err != nil {
			t.Fatal(err)
		}
		if data != nil {
			if err := e.emit(ctx, id, store.EventSandboxShared, "", data); err != nil {
				t.Fatal(err)
			}
		}
		return e, r
	}
	t.Run("copy-based lineage: refused, typed", func(t *testing.T) {
		e, r := mk(t, "run-c1", map[string]any{"adopted": true, "copy_based": true}, nil)
		err := e.refuseResumeOfSharedChild(ctx, r)
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeResumeInvalid || !strings.Contains(rt.Hint, "resume the parent") {
			t.Fatalf("err = %v, want RESUME_INVALID naming the parent", err)
		}
	})
	t.Run("bind-mount lineage: resumes", func(t *testing.T) {
		e, r := mk(t, "run-c2", map[string]any{"adopted": true, "copy_based": false}, nil)
		if err := e.refuseResumeOfSharedChild(ctx, r); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("declaration honoured (not adopted): resumes", func(t *testing.T) {
		e, r := mk(t, "run-c3", map[string]any{"adopted": false}, nil)
		if err := e.refuseResumeOfSharedChild(ctx, r); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("with a parent handle to adopt: resumes", func(t *testing.T) {
		e, r := mk(t, "run-c4", map[string]any{"adopted": true, "copy_based": true}, &SharedSandbox{Run: &sharedFakeRun{}})
		if err := e.refuseResumeOfSharedChild(ctx, r); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	// The guard is wired into both resume paths — a failed child and a
	// parked one — before any sandbox of their own is started.
	t.Run("wired: Resume of a failed_resumable child", func(t *testing.T) {
		wf := compileBot(t, `
schema out:
  ok: bool

compute step:
  output: out
  expr:
    ok: "true"

workflow child:
  entry: step
  step -> done
`).Workflow
		st := tmpStore(t)
		pc := store.AsParentedRunCreator(st)
		if _, err := pc.CreateChildRun(ctx, "run-c5", "child", "run-parent", nil); err != nil {
			t.Fatal(err)
		}
		if err := st.FailRunResumable(ctx, "run-c5", &store.Checkpoint{NodeID: "step"}, "boom", ""); err != nil {
			t.Fatal(err)
		}
		e := New(wf, st, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
		if err := e.emit(ctx, "run-c5", store.EventSandboxShared, "", map[string]any{"adopted": true, "copy_based": true}); err != nil {
			t.Fatal(err)
		}
		err := e.Resume(ctx, "run-c5", nil)
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeResumeInvalid {
			t.Fatalf("Resume err = %v, want RESUME_INVALID from the shared-child guard", err)
		}
		r, _ := st.LoadRun(ctx, "run-c5")
		if r.Status != store.RunStatusFailedResumable {
			t.Fatalf("status = %q, want the run left failed_resumable for its parent to resume", r.Status)
		}
	})
	t.Run("force resume bypasses in words", func(t *testing.T) {
		wf := compileBot(t, `
schema out:
  ok: bool

compute step:
  output: out
  expr:
    ok: "true"

workflow child:
  entry: step
  step -> done
`).Workflow
		st := tmpStore(t)
		pc := store.AsParentedRunCreator(st)
		if _, err := pc.CreateChildRun(ctx, "run-c7", "child", "run-parent", nil); err != nil {
			t.Fatal(err)
		}
		if err := st.FailRunResumable(ctx, "run-c7", &store.Checkpoint{NodeID: "step"}, "boom", ""); err != nil {
			t.Fatal(err)
		}
		e := New(wf, st, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithForceResume(true))
		if err := e.emit(ctx, "run-c7", store.EventSandboxShared, "", map[string]any{"adopted": true, "copy_based": true}); err != nil {
			t.Fatal(err)
		}
		if err := e.Resume(ctx, "run-c7", nil); err != nil {
			t.Fatalf("Resume with --force = %v, want the child resumed in a sandbox of its own", err)
		}
		evs, _ := st.LoadEvents(ctx, "run-c7")
		var said bool
		for _, ev := range evs {
			if ev.Type == store.EventSandboxShared && ev.Data["forced"] == true && ev.Data["adopted"] == false {
				said = true
			}
		}
		if !said {
			t.Fatal("a forced resume outside the parent must be said in the events (sandbox_shared forced=true)")
		}
	})
	t.Run("wired: Resume of a parked child", func(t *testing.T) {
		wf := compileBot(t, `
schema answer:
  confirmed: bool

prompt ask_text:
  Confirm.

human gate:
  instructions: ask_text
  output: answer
  interaction: human

workflow child:
  entry: gate
  gate -> done
`).Workflow
		st := tmpStore(t)
		pc := store.AsParentedRunCreator(st)
		if _, err := pc.CreateChildRun(ctx, "run-c6", "child", "run-parent", nil); err != nil {
			t.Fatal(err)
		}
		e := New(wf, st, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
		if err := e.Run(ctx, "run-c6", nil); !errors.Is(err, ErrRunPaused) {
			t.Fatalf("Run err = %v, want ErrRunPaused", err)
		}
		if err := e.emit(ctx, "run-c6", store.EventSandboxShared, "", map[string]any{"adopted": true, "copy_based": true}); err != nil {
			t.Fatal(err)
		}
		err := e.Resume(ctx, "run-c6", map[string]any{"confirmed": true})
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeResumeInvalid {
			t.Fatalf("Resume err = %v, want RESUME_INVALID from the shared-child guard", err)
		}
		r, _ := st.LoadRun(ctx, "run-c6")
		if r.Status != store.RunStatusPausedWaitingHuman {
			t.Fatalf("status = %q, want the run left parked for its parent to resume", r.Status)
		}
	})
}

// TestSharedChildRunsInPlace: a child in its parent's sandbox works in the
// parent's tree — it never creates a worktree of its own, which the
// parent's sandbox would never mount.
func TestSharedChildRunsInPlace(t *testing.T) {
	workDir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@x"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", workDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workDir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workDir, "commit", "-qm", "seed").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	wf := compileBot(t, `
schema out:
  ok: bool

compute step:
  output: out
  expr:
    ok: "true"

workflow child:
  entry: step
  step -> done
`).Workflow
	e, _, st := sharedTestEngine(t, wf, workDir, &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"})
	if err := e.Run(context.Background(), "run-inplace", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(st.Root(), "worktrees")); len(entries) != 0 {
		t.Fatalf("a shared child created a worktree of its own: %v", entries)
	}
	r, _ := st.LoadRun(context.Background(), "run-inplace")
	if r.WorkDir != "" && r.WorkDir != workDir {
		t.Fatalf("run.WorkDir = %q, want the parent's tree %q", r.WorkDir, workDir)
	}
}

// TestStartSandboxShared_OwnDeclarationIsHonouredOrRefused: every field a
// child declares about its own sandbox — not only `none` — is the
// operator's choice: honoured under a bind-mount parent (a sandbox of its
// own on the same tree), refused typed under a copy-based one, and named
// either way.
func TestStartSandboxShared_OwnDeclarationIsHonouredOrRefused(t *testing.T) {
	ctx := context.Background()
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}, Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "iterion-sandbox-sec"}}

	t.Run("bind-mount parent: honoured, declaration named", func(t *testing.T) {
		fake := &sharedFakeRun{}
		e, exec, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: bindOnly{fake}, WorkspaceFolder: "/workspace"})
		_, _ = st.CreateRun(ctx, "run-own-bind", "child", nil)
		if _, err := e.startSandbox(ctx, "run-own-bind", e.workDir, "", nil); err != nil {
			t.Fatalf("startSandbox: %v", err)
		}
		if exec.sandbox != nil {
			t.Fatalf("the child's own image declaration was overridden by the parent's sandbox (%v)", exec.sandbox)
		}
		evs, _ := st.LoadEvents(ctx, "run-own-bind")
		var named bool
		for _, ev := range evs {
			if ev.Type == store.EventSandboxShared && ev.Data["adopted"] == false {
				if d, ok := ev.Data["declared"].([]any); ok {
					for _, f := range d {
						if f == "image" {
							named = true
						}
					}
				}
			}
		}
		if !named {
			t.Fatalf("the honoured declaration must be named in the event: %+v", evs)
		}
	})
	t.Run("copy-based parent: refused, declaration named", func(t *testing.T) {
		e, _, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"})
		_, _ = st.CreateRun(ctx, "run-own-copy", "child", nil)
		_, err := e.startSandbox(ctx, "run-own-copy", e.workDir, "", nil)
		if err == nil || !strings.Contains(err.Error(), "image") || !strings.Contains(err.Error(), "copy-based") {
			t.Fatalf("err = %v, want the typed refusal naming the declared image and the copy-based parent", err)
		}
	})
	t.Run("node-level network declaration counts", func(t *testing.T) {
		nwf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{
			"step": &ir.AgentNode{Sandbox: &ir.SandboxSpec{Network: &ir.SandboxNetwork{Mode: "allowlist"}}},
		}}
		got := declaredSandboxFields(nwf)
		if len(got) != 1 || got[0] != "step.network" {
			t.Fatalf("declaredSandboxFields = %v, want [step.network]", got)
		}
		e, _, st := sharedTestEngine(t, nwf, t.TempDir(), &SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: "/workspace"})
		_, _ = st.CreateRun(ctx, "run-own-node", "child", nil)
		if _, err := e.startSandbox(ctx, "run-own-node", e.workDir, "", nil); err == nil || !strings.Contains(err.Error(), "step.network") {
			t.Fatalf("err = %v, want the refusal naming the node's network declaration", err)
		}
	})
	t.Run("inherit and auto are not declarations", func(t *testing.T) {
		if got := declaredSandboxFields(&ir.Workflow{Nodes: map[string]ir.Node{}, Sandbox: &ir.SandboxSpec{Mode: "auto", HostState: "auto"}}); len(got) != 0 {
			t.Fatalf("declaredSandboxFields = %v, want none", got)
		}
		if got := declaredSandboxFields(&ir.Workflow{Nodes: map[string]ir.Node{}}); len(got) != 0 {
			t.Fatalf("declaredSandboxFields = %v, want none", got)
		}
	})
}

// TestSharedSandboxCopyBasedClassificationPinsEachDriver: "copy-based" is
// read off the write-through seam the driver implements. This table is
// where a future driver that copies the workspace without a refresher
// turns red before it can lose a child's work in silence.
func TestSharedSandboxCopyBasedClassificationPinsEachDriver(t *testing.T) {
	if !sharedSandboxIsCopyBased((*kubernetes.Run)(nil)) {
		t.Fatal("the kubernetes driver copies the workspace into the pod: copy-based")
	}
	if sharedSandboxIsCopyBased((*docker.Run)(nil)) {
		t.Fatal("the docker driver bind-mounts the workspace: not copy-based")
	}
}
