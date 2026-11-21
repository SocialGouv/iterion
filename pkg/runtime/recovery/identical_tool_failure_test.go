package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests drive the identical-repeat verdict (#3420) through what a run
// really crosses: a compiled .bot, the production executor, the production
// dispatcher and recipes, a real shell, and a filesystem store reopened from
// its directory for every engine life. Nothing is hand-built — a witness that
// constructs the TOOL_FAILED_PERMANENT error itself passes with no producer.
//
// Each command appends a line to executions.log in its working directory
// before it fails, so the number of executions is read off the disk, not off
// a counter the test shares with the engine.

// scenarioLimit is the limit the scenarios of this file are written for, and
// newToolRun pins it: the shipped default can move without them. The tests
// about the DEFAULT, or about another limit, set the variable themselves.
const scenarioLimit = 2

// firstLife is how many times a failing node runs in a run's first engine
// life under the production recipes: once, then the one in-process retry an
// EXECUTION_FAILED gets. Every later life — a redelivery, a resume — runs it
// once: the retry is spent, and the count of it rides the checkpoint.
const firstLife = 2

// toolBot is a one-node workflow around command, run by the shell on the host.
func toolBot(command string) string {
	return "tool gate:\n  command: " + `"` + command + `"` + `

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`
}

// exit3Bot is the issue's command: the same stderr and the same status, every time.
var exit3Bot = toolBot("echo run >> executions.log; echo x >&2; exit 3")

// toolRun is one run's durable half — its workflow, its store directory and
// the directory its command runs in — across the engine lives that execute it.
type toolRun struct {
	t        *testing.T
	wf       *ir.Workflow
	ws       string
	storeDir string
	runID    string
}

func newToolRun(t *testing.T, bot string) *toolRun {
	t.Helper()
	// The code under test gets a home and a temp dir the test owns.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, strconv.Itoa(scenarioLimit))
	pr := parsertest.Parse("identical.bot", bot)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return &toolRun{t: t, wf: cr.Workflow, ws: t.TempDir(), storeDir: t.TempDir(), runID: "run-identical"}
}

// life is one engine process over the run: a store opened from the directory,
// an executor and an engine of its own. Nothing in memory survives from one
// life to the next, as on a second replica. A nil dispatch is the engine
// `iterion resume` builds, which wires none.
func (r *toolRun) life(dispatch runtime.RecoveryDispatch, drive func(eng *runtime.Engine) error) error {
	r.t.Helper()
	st, err := store.New(r.storeDir)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	exec := model.NewClawExecutor(nil, r.wf, model.WithWorkDir(r.ws))
	defer func() { _ = exec.Close() }()
	opts := []runtime.EngineOption{runtime.WithWorkDir(r.ws), runtime.WithSandboxOverride("none")}
	if dispatch != nil {
		opts = append(opts, runtime.WithRecoveryDispatch(dispatch))
	}
	return drive(runtime.New(r.wf, st, exec, opts...))
}

func (r *toolRun) launch(dispatch runtime.RecoveryDispatch) error {
	r.t.Helper()
	return r.life(dispatch, func(eng *runtime.Engine) error { return eng.Run(r.t.Context(), r.runID, nil) })
}

func (r *toolRun) resume(dispatch runtime.RecoveryDispatch) error {
	r.t.Helper()
	return r.life(dispatch, func(eng *runtime.Engine) error { return eng.Resume(r.t.Context(), r.runID, nil) })
}

// untilVerdict launches the run, then resumes it on a fresh engine — what a
// redelivery does — for as long as it ends EXECUTION_FAILED. It returns how
// many redeliveries that took, and the error the last life ended on.
func (r *toolRun) untilVerdict(dispatch runtime.RecoveryDispatch) (redeliveries int, err error) {
	r.t.Helper()
	err = r.launch(dispatch)
	for err != nil && codeOf(r.t, err) == runtime.ErrCodeExecutionFailed {
		if redeliveries == 16 {
			r.t.Fatalf("the run is still %s after %d redeliveries", runtime.ErrCodeExecutionFailed, redeliveries)
		}
		redeliveries++
		err = r.resume(dispatch)
	}
	return redeliveries, err
}

// executions is how many times the command ran, read off its own log.
func (r *toolRun) executions() int {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.ws, "executions.log"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		r.t.Fatalf("read the execution log: %v", err)
	}
	return strings.Count(string(raw), "\n")
}

// doc is the run document as the store holds it now, read through a fresh handle.
func (r *toolRun) doc() *store.Run {
	r.t.Helper()
	st, err := store.New(r.storeDir)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	run, err := st.LoadRun(r.t.Context(), r.runID)
	if err != nil {
		r.t.Fatalf("load run: %v", err)
	}
	return run
}

func (r *toolRun) events(kind store.EventType) []*store.Event {
	r.t.Helper()
	st, err := store.New(r.storeDir)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	all, err := st.LoadEvents(r.t.Context(), r.runID)
	if err != nil {
		r.t.Fatalf("load events: %v", err)
	}
	var out []*store.Event
	for _, e := range all {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

// streak is the gate node's entry of the checkpoint's failure streaks.
func (r *toolRun) streak() (store.NodeFailureStreak, bool) {
	r.t.Helper()
	cp := r.doc().Checkpoint
	if cp == nil {
		return store.NodeFailureStreak{}, false
	}
	s, ok := cp.NodeFailureStreaks["gate"]
	return s, ok
}

// production is the dispatcher every host wires — the real classification and
// the real recipes — minus the waits between two attempts, which decide
// nothing here.
func production() runtime.RecoveryDispatch {
	real := recovery.Dispatch(recovery.DefaultRecipes())
	return func(ctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		action, code := real(ctx, err, prior)
		action.Delay = 0
		return action, code
	}
}

func codeOf(t *testing.T, err error) runtime.ErrorCode {
	t.Helper()
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) {
		t.Fatalf("the engine returned no RuntimeError: %v", err)
	}
	return rt.Code
}

// The issue's command, `echo x >&2; exit 3`, under the DEFAULT limit: the run
// ends TOOL_FAILED_PERMANENT at the execution the default names, after the
// redeliveries that count leaves — none at 2, the first execution and its one
// in-process retry — and says so where an operator reads. Every number here
// derives from the constant.
func TestAnIdenticalToolFailureIsPermanentAtTheDefaultLimit(t *testing.T) {
	r := newToolRun(t, exit3Bot)
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "") // unset: the default
	limit := runtime.DefaultNodeMaxIdenticalFailures
	redeliveries, err := r.untilVerdict(production())
	if err == nil {
		t.Fatal("scenario broken: the command exits 3 and the run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code = %s, want %s — a command that failed identically %d times is still read as a fault a redelivery can outlast", got, runtime.ErrCodeToolFailedPermanent, limit)
	}
	if got := r.executions(); got != limit {
		t.Errorf("the command ran %d time(s), want exactly %d (the default limit)", got, limit)
	}
	if want := max(0, limit-firstLife); redeliveries != want {
		t.Errorf("%d redelivery(ies) before the verdict, want %d: a first life runs the node %d times, each redelivery once", redeliveries, want, firstLife)
	}

	// The fact travels in the chain, for a reader that asks how the verdict
	// was reached.
	var repeat *runtime.IdenticalToolFailure
	if !errors.As(err, &repeat) {
		t.Errorf("the error carries no IdenticalToolFailure: %v", err)
	} else if repeat.Count != limit || repeat.Limit != limit || repeat.NodeID != "gate" {
		t.Errorf("the chain says %d identical failure(s) of %q at a limit of %d, want %d of gate at %d", repeat.Count, repeat.NodeID, repeat.Limit, limit, limit)
	}

	// The run stays resumable, on the permanent code, and its error ends
	// with what to do.
	doc := r.doc()
	if doc.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want %s — an operator's resume must still be possible", doc.Status, store.RunStatusFailedResumable)
	}
	if doc.FailureCode != store.FailureToolFailedPermanent {
		t.Errorf("failure_code = %s, want %s", doc.FailureCode, store.FailureToolFailedPermanent)
	}
	for _, want := range []string{fmt.Sprintf("%d times in a row", limit), "re-runs this node once", "iterion resume --run-id " + r.runID, "outside the bot", runtime.EnvNodeMaxIdenticalFailures} {
		if !strings.Contains(doc.Error, want) {
			t.Errorf("the run's error does not say %q:\n%s", want, doc.Error)
		}
	}

	// The node's event names the promotion, the count and the code it replaces.
	var promoted *store.Event
	for _, e := range r.events(store.EventNodeRecovery) {
		if e.Data["strategy"] == "identical_tool_failure" {
			promoted = e
		}
	}
	if promoted == nil {
		t.Fatal("no node_recovery event says the failure was promoted — the timeline shows a permanent code and nothing explaining it")
	}
	if promoted.NodeID != "gate" || promoted.Data["code"] != string(runtime.ErrCodeToolFailedPermanent) ||
		promoted.Data["promoted_from"] != string(runtime.ErrCodeExecutionFailed) {
		t.Errorf("the promotion event is %s %+v, want node gate, code %s, promoted_from %s",
			promoted.NodeID, promoted.Data, runtime.ErrCodeToolFailedPermanent, runtime.ErrCodeExecutionFailed)
	}
	for _, key := range []string{"identical_failures", "identical_failure_limit"} {
		if got := fmt.Sprint(promoted.Data[key]); got != strconv.Itoa(limit) {
			t.Errorf("%s = %s, want %d", key, got, limit)
		}
	}
	if reason, _ := promoted.Data["reason"].(string); !strings.Contains(reason, "not redelivered") {
		t.Errorf("the event's reason does not say the run is not redelivered: %q", reason)
	}
	failed := r.events(store.EventRunFailed)
	if len(failed) != redeliveries+1 {
		t.Fatalf("%d run_failed event(s), want %d (one per engine life)", len(failed), redeliveries+1)
	}
	if hint, _ := failed[len(failed)-1].Data["hint"].(string); !strings.Contains(hint, "resume") {
		t.Errorf("run_failed carries no remedy naming the resume: %+v", failed[len(failed)-1].Data)
	}

	// The checkpoint holds the streak as a digest: no stderr, no command.
	streak, ok := r.streak()
	if !ok || streak.Count != limit {
		t.Fatalf("checkpoint streak = %+v (present=%v), want a count of %d", streak, ok, limit)
	}
	if !strings.HasPrefix(streak.Fingerprint, "sha256:") || len(streak.Fingerprint) != len("sha256:")+64 {
		t.Errorf("fingerprint %q is not a sha256 digest — the checkpoint must not hold the failure's text", streak.Fingerprint)
	}
}

// Restart is normal: a runner drained between the first execution and its
// retry leaves a checkpoint, and the engine that resumes it — another process,
// another executor, the store read back from disk — meets the same failure a
// second time. The count is the checkpoint's, so that second failure is the
// identical repeat, exactly as if one engine had seen both.
func TestAFreshEngineContinuesTheStreakItsPredecessorLeft(t *testing.T) {
	r := newToolRun(t, exit3Bot)

	// First life: the drain lands while the dispatcher is deciding, so the
	// retry it grants (the real recipe, with its real wait) is cut by the
	// teardown and the run parks interrupted after ONE execution.
	ctx, drain := context.WithCancelCause(t.Context())
	defer drain(nil)
	real := recovery.Dispatch(recovery.DefaultRecipes())
	draining := func(dctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		drain(runtime.ErrRunInterrupted)
		return real(dctx, err, prior)
	}
	err := r.life(draining, func(eng *runtime.Engine) error { return eng.Run(ctx, r.runID, nil) })
	if !errors.Is(err, runtime.ErrRunInterrupted) {
		t.Fatalf("scenario broken: the first life ended with %v, want an interruption", err)
	}
	if got := r.executions(); got != 1 {
		t.Fatalf("scenario broken: the first life ran the command %d time(s), want 1", got)
	}
	if doc := r.doc(); doc.Status != store.RunStatusFailedResumable || doc.FailureCode != store.FailureInterrupted {
		t.Fatalf("scenario broken: the drained run is %s/%s, want %s/%s", doc.Status, doc.FailureCode, store.RunStatusFailedResumable, store.FailureInterrupted)
	}
	if streak, ok := r.streak(); !ok || streak.Count != 1 {
		t.Fatalf("the drained run's checkpoint streak = %+v (present=%v), want a count of 1 — the first failure did not ride the checkpoint", streak, ok)
	}

	// Second life: nothing of the first engine is left but the store.
	err = r.resume(production())
	if err == nil {
		t.Fatal("scenario broken: the resumed run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code after the resume = %s, want %s — the fresh engine counted the failure as a first one: the streak is not durable", got, runtime.ErrCodeToolFailedPermanent)
	}
	if got := r.executions(); got != scenarioLimit {
		t.Errorf("the command ran %d time(s) over both lives, want exactly %d", got, scenarioLimit)
	}
	if doc := r.doc(); doc.FailureCode != store.FailureToolFailedPermanent {
		t.Errorf("failure_code = %s, want %s", doc.FailureCode, store.FailureToolFailedPermanent)
	}
}

// An operator's resume is never refused: it re-executes the node. The streak
// is the node's, not the engine's — an identical failure after the resume is
// one more in the same row, so it is permanent at once, after ONE execution,
// on the engine `iterion resume` builds (which wires no dispatcher). The cause
// fixed, the node succeeds, the run finishes and the streak is gone.
func TestAnOperatorResumeReExecutesAPermanentToolFailure(t *testing.T) {
	r := newToolRun(t, toolBot("echo run >> executions.log; test -f fixed || { echo x >&2; exit 3; }"))
	if got := codeOf(t, r.launch(production())); got != runtime.ErrCodeToolFailedPermanent {
		t.Fatalf("scenario broken: the launch ended %s, want %s", got, runtime.ErrCodeToolFailedPermanent)
	}

	// Resumed with nothing fixed.
	err := r.resume(nil)
	if err == nil {
		t.Fatal("scenario broken: the resumed run did not fail")
	}
	if got := r.executions(); got != scenarioLimit+1 {
		t.Errorf("the command ran %d time(s), want %d — the operator's resume must re-execute the node, once", got, scenarioLimit+1)
	}
	if got := codeOf(t, err); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code after the resume = %s, want %s — the same failure once more in a row", got, runtime.ErrCodeToolFailedPermanent)
	}
	if streak, ok := r.streak(); !ok || streak.Count != scenarioLimit+1 {
		t.Errorf("checkpoint streak = %+v (present=%v), want a count of %d", streak, ok, scenarioLimit+1)
	}

	// Resumed with the cause fixed.
	if werr := os.WriteFile(filepath.Join(r.ws, "fixed"), nil, 0o600); werr != nil {
		t.Fatal(werr)
	}
	if err := r.resume(nil); err != nil {
		t.Fatalf("the run did not finish once the cause was fixed: %v", err)
	}
	if doc := r.doc(); doc.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want %s", doc.Status, store.RunStatusFinished)
	}
	if streak, ok := r.streak(); ok {
		t.Errorf("the streak survived the node's success: %+v", streak)
	}
}

// Output that differs between two executions is not the same failure: the run
// keeps the EXECUTION_FAILED reading it had. The honest limit of a fingerprint.
func TestAToolFailureWhoseOutputVariesStaysExecutionFailed(t *testing.T) {
	// stderr is the number of executions so far: 1, then 2.
	r := newToolRun(t, toolBot("echo run >> executions.log; wc -l < executions.log >&2; exit 3"))
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s — two different outputs were read as one failure", got, runtime.ErrCodeExecutionFailed)
	}
	if got := r.executions(); got != firstLife {
		t.Errorf("the command ran %d time(s), want %d", got, firstLife)
	}
	if streak, ok := r.streak(); !ok || streak.Count != 1 {
		t.Errorf("checkpoint streak = %+v (present=%v), want a count of 1 — the second, different failure starts its own", streak, ok)
	}
}

// A different failure ends the streak, and the new one is counted from one —
// across engines too: the redelivery that meets it again makes the repeat.
func TestADifferentToolFailureStartsItsOwnStreak(t *testing.T) {
	// The first execution prints a, every later one b.
	r := newToolRun(t, toolBot("echo run >> executions.log; if [ $(wc -l < executions.log) -le 1 ]; then echo a >&2; else echo b >&2; fi; exit 3"))
	if got := codeOf(t, r.launch(production())); got != runtime.ErrCodeExecutionFailed {
		t.Fatalf("code after a then b = %s, want %s — b is not a repeat of a", got, runtime.ErrCodeExecutionFailed)
	}
	if streak, ok := r.streak(); !ok || streak.Count != 1 {
		t.Fatalf("checkpoint streak after a then b = %+v (present=%v), want a count of 1", streak, ok)
	}
	// The redelivery: a fresh engine meets b again.
	if got := codeOf(t, r.resume(production())); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code after a second b = %s, want %s", got, runtime.ErrCodeToolFailedPermanent)
	}
	if got := r.executions(); got != firstLife+1 {
		t.Errorf("the command ran %d time(s), want %d", got, firstLife+1)
	}
}

// A command that fails, then succeeds, finishes — the one cheap retry is what
// it is for — and leaves no streak behind.
func TestAToolFailureThenASuccessFinishesAndLeavesNoStreak(t *testing.T) {
	r := newToolRun(t, toolBot("echo run >> executions.log; test $(wc -l < executions.log) -ge 2 || { echo x >&2; exit 3; }"))
	if err := r.launch(production()); err != nil {
		t.Fatalf("the run did not finish on its retry: %v", err)
	}
	if got := r.executions(); got != firstLife {
		t.Errorf("the command ran %d time(s), want %d", got, firstLife)
	}
	doc := r.doc()
	if doc.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want %s", doc.Status, store.RunStatusFinished)
	}
	if doc.Checkpoint != nil && len(doc.Checkpoint.NodeFailureStreaks) != 0 {
		t.Errorf("the streak survived the node's success: %+v", doc.Checkpoint.NodeFailureStreaks)
	}
}

// A network needle keeps its own code and its own backoff: six retries, the
// same text every time, and no streak is ever counted for it.
func TestAToolFailureNamingANetworkOutageKeepsItsBackoff(t *testing.T) {
	r := newToolRun(t, toolBot("echo run >> executions.log; echo 'curl: (7) Failed to connect: Connection refused' >&2; exit 7"))
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeNetworkTransient {
		t.Errorf("code = %s, want %s", got, runtime.ErrCodeNetworkTransient)
	}
	if got := r.executions(); got != 7 {
		t.Errorf("the command ran %d time(s), want 7 (the first execution and the six retries of the network recipe)", got)
	}
	if streak, ok := r.streak(); ok {
		t.Errorf("a network failure was counted in a streak: %+v", streak)
	}
}

// The limit at 0 is the escape hatch: the run behaves exactly as before — the
// catch-all code, two executions — and its checkpoint carries no streak.
func TestTheIdenticalFailureLimitAtZeroRestoresTheTransientReading(t *testing.T) {
	r := newToolRun(t, exit3Bot)
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "0")
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s — the limit is 0 and the failure was promoted anyway", got, runtime.ErrCodeExecutionFailed)
	}
	if got := r.executions(); got != firstLife {
		t.Errorf("the command ran %d time(s), want %d", got, firstLife)
	}
	if doc := r.doc(); doc.Checkpoint == nil || doc.Checkpoint.NodeFailureStreaks != nil {
		t.Errorf("the checkpoint carries a streak with the limit off: %+v", doc.Checkpoint)
	}
	if doc := r.doc(); doc.Checkpoint.NodeAttempts["gate"][string(runtime.ErrCodeExecutionFailed)] != firstLife {
		t.Errorf("node attempts = %+v, want %d under %s", doc.Checkpoint.NodeAttempts, firstLife, runtime.ErrCodeExecutionFailed)
	}
}

// The limit is a count: at 3, the first engine's two identical failures stay
// transient, and the redelivery's third is the repeat.
func TestTheIdenticalFailureLimitIsACount(t *testing.T) {
	r := newToolRun(t, exit3Bot)
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "3")
	if got := codeOf(t, r.launch(production())); got != runtime.ErrCodeExecutionFailed {
		t.Fatalf("code after two identical failures at a limit of 3 = %s, want %s", got, runtime.ErrCodeExecutionFailed)
	}
	if got := codeOf(t, r.resume(production())); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code after the third = %s, want %s", got, runtime.ErrCodeToolFailedPermanent)
	}
	if got := r.executions(); got != 3 {
		t.Errorf("the command ran %d time(s), want 3", got)
	}
}

// A `script:` body runs from a temp file the executor names afresh for every
// execution, and an interpreter quotes that name in its own errors — here the
// script prints it itself ($0), so the scenario holds under any shell. The
// name is the executor's, not the failure's: the two executions are one
// failure.
func TestAScriptFailureQuotingItsTempFileIsStillTheSameFailure(t *testing.T) {
	r := newToolRun(t, `tool gate:
  language: sh
  script: |
    echo run >> executions.log
    echo "$0: line 2: giving up" >&2
    exit 3

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`)
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the script did not fail")
	}
	if !strings.Contains(err.Error(), ".iterion-script-") {
		t.Fatalf("scenario broken: the failure does not quote the script's temp file, so this proves nothing:\n%v", err)
	}
	if got := codeOf(t, err); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code = %s, want %s — the temp file's random name made two executions of one failure look different", got, runtime.ErrCodeToolFailedPermanent)
	}
	if got := r.executions(); got != scenarioLimit {
		t.Errorf("the script ran %d time(s), want %d", got, scenarioLimit)
	}
}

// An outage of what the command talks to prints the same line two seconds
// later and a different one ten minutes later. Named in the words of the
// tool that met it (here curl's, for a host that did not resolve), the
// failure is never counted: the run keeps EXECUTION_FAILED after its one
// retry — the code the queue redelivers — and no streak is written.
func TestACommandThatNamesAnOutageIsNeverCounted(t *testing.T) {
	r := newToolRun(t, toolBot("echo run >> executions.log; echo 'curl: (6) Could not resolve host: api.github.com' >&2; exit 6"))
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if got := codeOf(t, err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s — an outage that repeats word for word was read as a failure no wait cures", got, runtime.ErrCodeExecutionFailed)
	}
	if got := r.executions(); got != firstLife {
		t.Errorf("the command ran %d time(s), want %d", got, firstLife)
	}
	if streak, ok := r.streak(); ok {
		t.Errorf("an outage was counted in a streak: %+v", streak)
	}
}

// A `script:` body whose file cannot be written never ran: there is no exit
// status, and nothing the command printed. Two such attempts read alike —
// the more so once the file's random name is normalised — and are still not
// "the same exit status and output".
func TestAScriptThatNeverRanIsNeverCounted(t *testing.T) {
	r := newToolRun(t, `tool gate:
  language: sh
  script: |
    echo run >> executions.log
    exit 3

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`)
	// The scratch a host script is written to is the temp dir: under a
	// regular file, it can be neither created nor written to.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(blocker, "scratch"))
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if !strings.Contains(err.Error(), "script") || r.executions() != 0 {
		t.Fatalf("scenario broken: the script ran %d time(s), or the failure is not its setup's:\n%v", r.executions(), err)
	}
	if got := codeOf(t, err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s — a command that never ran was read as failing with the same exit status twice", got, runtime.ErrCodeExecutionFailed)
	}
	if got := len(r.events(store.EventNodeRecovery)); got != firstLife {
		t.Errorf("%d node_recovery event(s), want %d (the retry, then the terminal reading)", got, firstLife)
	}
	if streak, ok := r.streak(); ok {
		t.Errorf("a command that never ran was counted in a streak: %+v", streak)
	}
}

// A Verified Action is judged by its postcondition, and its recipe's own end
// still travels in the failure: a recipe that really exited non-zero, on a
// goal that is not met, is a command that ran to an exit status of its own —
// counted like any other, and permanent at the limit.
func TestAnUnmetPostconditionAfterAFailedRecipeIsCounted(t *testing.T) {
	r := newToolRun(t, `tool gate:
  command: "echo run >> executions.log; echo x >&2; exit 3"
  postcondition: "test -f never-written"

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`)
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if !strings.Contains(err.Error(), "postcondition not met") {
		t.Fatalf("scenario broken: the failure is not the unmet postcondition's:\n%v", err)
	}
	if got := codeOf(t, err); got != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("code = %s, want %s — the recipe exited 3 twice on the same output, and its status did not reach the chain", got, runtime.ErrCodeToolFailedPermanent)
	}
	if got := r.executions(); got != scenarioLimit {
		t.Errorf("the recipe ran %d time(s), want %d", got, scenarioLimit)
	}
	if streak, ok := r.streak(); !ok || streak.Count != scenarioLimit {
		t.Errorf("checkpoint streak = %+v (present=%v), want a count of %d", streak, ok, scenarioLimit)
	}
}

// The same goal unmet after a recipe that exited 0 is no process's failure:
// there is no exit status to repeat, the verdict is the oracle's alone, and
// the run keeps the code it had.
func TestAnUnmetPostconditionAfterACleanRecipeIsNotCounted(t *testing.T) {
	r := newToolRun(t, `tool gate:
  command: "echo run >> executions.log; true"
  postcondition: "test -f never-written"

workflow w:
  worktree: none
  sandbox: none
  entry: gate
  gate -> done
`)
	err := r.launch(production())
	if err == nil {
		t.Fatal("scenario broken: the run did not fail")
	}
	if !strings.Contains(err.Error(), "postcondition not met") {
		t.Fatalf("scenario broken: the failure is not the unmet postcondition's:\n%v", err)
	}
	if got := codeOf(t, err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s", got, runtime.ErrCodeExecutionFailed)
	}
	if got := r.executions(); got != firstLife {
		t.Errorf("the recipe ran %d time(s), want %d", got, firstLife)
	}
	if streak, ok := r.streak(); ok {
		t.Errorf("an unmet postcondition after a zero exit was counted in a streak: %+v", streak)
	}
}
