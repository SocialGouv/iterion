package recovery_test

import (
	"context"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// execClient stands in for a container driver's exec client. A sandboxed tool
// command reaches it wrapped by sandbox.Completion — argv `sh -c <wrapper>
// iterion-tool <cmd...>`, exactly what the docker and kubernetes drivers are
// handed — and it runs that wrapper on the host, so the completion line, the
// settle and the status the executor reads are the production ones. What
// varies is what the CLIENT reports of the command's end:
//
//   - propagates: it exits with the command's status (docker exec; kubectl
//     exec through the helpers, which return kubectl's code);
//   - kubectl: the same, plus kubectl's own stderr line on a non-zero status;
//   - reportsNothing: it exits 0 whatever the command did.
type execClient struct {
	reports string
	dir     string
}

const (
	propagates     = "propagates"
	kubectlLike    = "kubectl"
	reportsNothing = "reports nothing"
)

func (c *execClient) Driver() string { return "test-exec-client" }

func (c *execClient) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *osexec.Cmd {
	var cmd *osexec.Cmd
	if len(argv) >= 4 && argv[0] == "sh" && argv[3] == "iterion-tool" {
		var client string
		switch c.reports {
		case propagates:
			client = `"$@"`
		case kubectlLike:
			client = `"$@"; rc=$?; [ "$rc" -ne 0 ] && echo "command terminated with exit code $rc" >&2; exit $rc`
		case reportsNothing:
			client = `"$@"; exit 0`
		}
		cmd = osexec.CommandContext(ctx, "sh", append([]string{"-c", client, "exec-client"}, argv...)...)
	} else {
		cmd = osexec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	cmd.Dir = c.dir
	cmd.Env = os.Environ()
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = opts.Stdin
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (c *execClient) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := c.Command(ctx, argv, opts).Output()
	return sandbox.ExecResult{Stdout: out}, err
}

func (c *execClient) Cleanup(context.Context) error { return nil }

// sandboxedExecutor is the production executor whose sandbox is the stand-in,
// whatever the engine settles for the run.
type sandboxedExecutor struct {
	*model.ClawExecutor
	run sandbox.Run
}

func (s *sandboxedExecutor) SetSandbox(sandbox.Run) { s.ClawExecutor.SetSandbox(s.run) }

// The identical-repeat verdict is live on the SANDBOXED path, where cloud runs
// execute: a command, a script and a process a signal ended are each counted
// through the real wrapped argv once the exec client propagates the status —
// and a client that reports nothing while the completion line says 3 is not
// counted. That last status is proven, but by the completion line alone: the
// chain carries no exit error of the client's, so the failure stays
// EXECUTION_FAILED and keeps its redeliveries. It fails safe.
func TestASandboxedCommandIsCountedWhenItsClientPropagatesTheStatus(t *testing.T) {
	scriptBot := `tool gate:
  language: sh
  script: |
    echo run >> executions.log
    echo x >&2
    exit 3

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`
	for _, c := range []struct {
		name    string
		reports string
		bot     string
		counted bool
	}{
		{"a command that exits 3", propagates, exit3Bot, true},
		{"the same, through a client that adds its own line", kubectlLike, exit3Bot, true},
		{"a script that exits 3", propagates, scriptBot, true},
		{"a command a signal ended", propagates, toolBot("echo run >> executions.log; kill -9 $$"), true},
		{"a client that reports nothing of the command's exit 3", reportsNothing, exit3Bot, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newToolRun(t, c.bot)
			st, err := store.New(r.storeDir)
			if err != nil {
				t.Fatal(err)
			}
			exec := &sandboxedExecutor{ClawExecutor: model.NewClawExecutor(nil, r.wf, model.WithWorkDir(r.ws)), run: &execClient{reports: c.reports, dir: r.ws}}
			defer func() { _ = exec.Close() }()
			runErr := runtime.New(r.wf, st, exec, runtime.WithWorkDir(r.ws), runtime.WithSandboxOverride("none"),
				runtime.WithRecoveryDispatch(production())).Run(t.Context(), r.runID, nil)
			if runErr == nil {
				t.Fatal("scenario broken: the run did not fail")
			}
			if c.reports == reportsNothing && !strings.Contains(runErr.Error(), "did not report that status") {
				t.Fatalf("scenario broken: the failure is not the settle's own reading of an unreported status:\n%v", runErr)
			}
			want := runtime.ErrCodeExecutionFailed
			if c.counted {
				want = runtime.ErrCodeToolFailedPermanent
			}
			if got := codeOf(t, runErr); got != want {
				t.Errorf("code = %s, want %s:\n%v", got, want, runErr)
			}
			if got := r.executions(); got != scenarioLimit {
				t.Errorf("the command ran %d time(s), want %d", got, scenarioLimit)
			}
			streak, ok := r.streak()
			if c.counted && (!ok || streak.Count != scenarioLimit) {
				t.Errorf("checkpoint streak = %+v (present=%v), want a count of %d", streak, ok, scenarioLimit)
			}
			if !c.counted && ok {
				t.Errorf("an unreported status was counted in a streak: %+v", streak)
			}
		})
	}
}
