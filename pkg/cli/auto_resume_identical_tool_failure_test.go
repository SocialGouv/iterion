package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// firstRun is how many times a failing node runs in the run `iterion run`
// launches: once, then the one in-process retry an EXECUTION_FAILED gets.
// Every auto-resume runs it once more.
const firstRun = 2

// autoResumedToolRun runs a one-node workflow whose command fails the same
// way every time — a compiled .bot, the production executor, the dispatcher
// `iterion run` wires, a real shell — then hands the engine's error to the
// `--auto-resume` loop itself, with attempts to spend and no wait.
// It returns how many times the command ran, how many resumes the loop
// announced on the timeline, and the loop's verdict.
func autoResumedToolRun(t *testing.T, attempts int) (executions, autoResumes int, err error) {
	t.Helper()
	// The code under test gets a home and a temp dir the test owns.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	pr := parsertest.Parse("identical.bot", `tool gate:
  command: "echo run >> executions.log; echo x >&2; exit 3"

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`)
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	st, serr := store.New(t.TempDir())
	if serr != nil {
		t.Fatalf("store.New: %v", serr)
	}
	ws := t.TempDir()
	exec := model.NewClawExecutor(nil, cr.Workflow, model.WithWorkDir(ws))
	t.Cleanup(func() { _ = exec.Close() })
	production := recovery.Dispatch(recovery.DefaultRecipes())
	eng := runtime.New(cr.Workflow, st, exec, runtime.WithWorkDir(ws), runtime.WithSandboxOverride("none"),
		// The dispatcher RunRun wires, minus the wait between two attempts.
		runtime.WithRecoveryDispatch(func(ctx context.Context, derr error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
			action, code := production(ctx, derr, prior)
			action.Delay = 0
			return action, code
		}))

	const runID = "run-identical"
	ctx := t.Context()
	runErr := eng.Run(ctx, runID, nil)
	if runErr == nil {
		t.Fatal("scenario broken: the command exits 3 and the run did not fail")
	}
	err = autoResumeLoop(ctx, eng, st, runID, autoResumeConfig{
		MaxAttempts: attempts,
		Retry:       retrypolicy.Policy{MaxWait: "1ms"},
	}, runErr, iterlog.Nop())

	raw, rerr := os.ReadFile(filepath.Join(ws, "executions.log"))
	if rerr != nil {
		t.Fatalf("read the execution log: %v", rerr)
	}
	events, lerr := st.LoadEvents(ctx, runID)
	if lerr != nil {
		t.Fatalf("load events: %v", lerr)
	}
	for _, e := range events {
		if e.Type == store.EventRunAutoResumed {
			autoResumes++
		}
	}
	return strings.Count(string(raw), "\n"), autoResumes, err
}

// `iterion run --auto-resume N` on a command that fails identically, under the
// DEFAULT limit: the loop re-drives the run only while its failure is still
// the transient one, and refuses at the execution the default names — at a
// default of 2 that is the engine's own two executions: no resume, nothing
// announced. The run stays parked on the code that says why. The counts
// derive from the constant.
func TestAutoResumeRefusesARealIdenticalToolFailure(t *testing.T) {
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "") // unset: the default
	limit := runtime.DefaultNodeMaxIdenticalFailures
	executions, autoResumes, err := autoResumedToolRun(t, limit+3)
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) {
		t.Fatalf("the loop returned no RuntimeError: %v", err)
	}
	if rt.Code != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code = %s, want %s", rt.Code, runtime.ErrCodeToolFailedPermanent)
	}
	if executions != limit {
		t.Errorf("the command ran %d time(s), want %d — the auto-resume loop re-drove a command past the limit of identical failures", executions, limit)
	}
	if want := max(0, limit-firstRun); autoResumes != want {
		t.Errorf("%d run_auto_resumed event(s), want %d", autoResumes, want)
	}
}

// With the limit off the failure is the transient one it used to be, and the
// loop spends its three attempts on it: one execution per resume.
func TestAutoResumeStillReDrivesAToolFailureWithTheLimitOff(t *testing.T) {
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "0")
	const attempts = 3
	executions, autoResumes, err := autoResumedToolRun(t, attempts)
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) {
		t.Fatalf("the loop returned no RuntimeError: %v", err)
	}
	if rt.Code != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s", rt.Code, runtime.ErrCodeExecutionFailed)
	}
	if executions != firstRun+attempts || autoResumes != attempts {
		t.Errorf("executions = %d, run_auto_resumed = %d, want %d and %d (the first run's executions, then one per auto-resume)", executions, autoResumes, firstRun+attempts, attempts)
	}
}
