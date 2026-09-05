package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// cliParentSandboxFake stands in for the parent's live sandbox: commands run
// on the host, counted, so the test can tell whether the child went through
// the parent's handle.
type cliParentSandboxFake struct {
	mu       sync.Mutex
	commands int
	cleanups int
}

func (f *cliParentSandboxFake) Driver() string { return "fake-parent" }
func (f *cliParentSandboxFake) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
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
func (f *cliParentSandboxFake) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := f.Command(ctx, cmd, opts).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		err = nil
	}
	return sandbox.ExecResult{Stdout: out, ExitCode: code}, err
}
func (f *cliParentSandboxFake) Cleanup(context.Context) error {
	f.mu.Lock()
	f.cleanups++
	f.mu.Unlock()
	return nil
}

// TestSubbotRunnerForCLI_RunsTheChildInTheParentSandbox: on the CLI host,
// as on the runner and the studio, a child executes in its parent's sandbox
// and in the parent's effective workdir — never in a tree of its own that
// the parent's sandbox does not mount.
func TestSubbotRunnerForCLI_RunsTheChildInTheParentSandbox(t *testing.T) {
	dir := t.TempDir()
	parentTree := filepath.Join(dir, "parent-tree")
	if err := os.MkdirAll(parentTree, 0o755); err != nil {
		t.Fatal(err)
	}
	child := `schema out:
  ok: bool
tool step:
  command: ` + "`printf '{\"ok\":true}'`" + `
  output: out
workflow child:
  entry: step
  step -> done
`
	if err := os.WriteFile(filepath.Join(dir, "child.bot"), []byte(child), 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	fake := &cliParentSandboxFake{}

	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, logger, RunOptions{NoInteractive: true})
	out, err := runner(context.Background(), runtime.SubbotRequest{
		Source:        "child.bot",
		ParentRunID:   "parent-run",
		NodeID:        "run_child",
		WorkDir:       parentTree,
		ParentSandbox: &runtime.SharedSandbox{Run: fake, WorkspaceFolder: parentTree},
	})
	if err != nil {
		t.Fatalf("subbot runner: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("child output = %v, want the child's own tool output", out)
	}
	if fake.commands == 0 {
		t.Fatal("the child's tool command never went through the parent's sandbox handle")
	}
	if fake.cleanups != 0 {
		t.Fatalf("cleanups = %d, want 0: the child must not tear down the parent's sandbox", fake.cleanups)
	}
	ids, err := s.ListChildRuns(context.Background(), "parent-run")
	if err != nil || len(ids) != 1 {
		t.Fatalf("child runs = %v (%v), want exactly one", ids, err)
	}
	c, err := s.LoadRun(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("load child run: %v", err)
	}
	if c.WorkDir != "" && c.WorkDir != parentTree {
		t.Fatalf("child WorkDir = %q, want the parent's tree %q (no worktree of its own)", c.WorkDir, parentTree)
	}
	evs, _ := s.LoadEvents(context.Background(), c.ID)
	shared, started := 0, 0
	for _, ev := range evs {
		switch ev.Type {
		case store.EventSandboxShared:
			shared++
		case store.EventSandboxStarted:
			started++
		}
	}
	if shared != 1 || started != 0 {
		t.Fatalf("child events: sandbox_shared=%d sandbox_started=%d, want 1/0", shared, started)
	}
}
