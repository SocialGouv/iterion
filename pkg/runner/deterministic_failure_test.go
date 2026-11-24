package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Measured 2026-09-06 on run 01a07804 (the Billy 1.6.0 dogfood): a
// `compute` node whose expression the runner's evaluator could not satisfy
// failed, the delivery naked, JetStream redelivered, dispositionForStatus
// synthesised a resume — seven times in ten minutes, each one a fresh pod,
// a fresh clone and a fresh sandbox, all reaching the identical verdict. A
// compute node runs no LLM and no shell and its inputs come from a
// checkpoint that does not move: nothing between two attempts could differ.
func TestClassifyExecResult_DeterministicNodeFailureAcks(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"compute expression", &runtime.RuntimeError{
			Code:    store.FailureExpressionFailed,
			NodeID:  "delivery_reserve",
			Message: `compute "delivery_reserve": field "minutes" expression "max(a, b, c)": expr: max() takes 2 arguments, got 3`,
		}},
		// A tool node's permanent failure has no row here on purpose: a
		// hand-built TOOL_FAILED_PERMANENT passes whether or not anything
		// produces the code. Its witness is the engine's own error —
		// TestARealIdenticalToolFailureIsAckedNotRedelivered below.
		// The conversation rides the checkpoint and the in-node recipe
		// already compacted twice: no attempt after this one is smaller.
		{"context length exceeded", &runtime.RuntimeError{
			Code: store.FailureContextLengthExceeded, NodeID: "campaign",
			Message: "compaction did not reduce context enough to fit the model window",
		}},
		{"wrapped expression failure", fmt.Errorf("engine: %w", &runtime.RuntimeError{
			Code: store.FailureExpressionFailed, Message: "boom",
		})},
		// Measured 2026-09-07 on run 01a07db7: four pods for a schema the
		// serving backend could not read. The declaration rides the IR.
		{"a declared schema the backend cannot read", &runtime.RuntimeError{
			Code: store.FailureSchemaUnusable, NodeID: "voter_v2",
			Message: "parse ExplicitSchema: json: cannot unmarshal array into Go struct field InputSchema.properties.verdicts.type of type string",
		}},
		// Measured 2026-09-07 on run 01a07da6: eight pods in 77 seconds
		// for a model the ChatGPT backend does not serve to this image.
		{"a model the provider will not serve", &runtime.RuntimeError{
			Code: store.FailureModelUnavailable, NodeID: "m_astra",
			Message: `backend "claw" failed: openai: API error 400: {"detail":"The 'gpt-6-astra' model requires a newer version of Codex."}`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := classifyExecResult(c.err, "run-1")
			if out.action != actionAck {
				t.Errorf("action = %v, want actionAck — a NAK redelivers a failure that can only repeat", out.action)
			}
			if out.finalStatus != "deterministic_failure" {
				t.Errorf("finalStatus = %q, want deterministic_failure", out.finalStatus)
			}
		})
	}

	// The classes an automatic resume CAN cure keep naking — this carve-out
	// must not swallow the redelivery path the fleet runs on.
	for _, c := range []struct {
		name string
		err  error
	}{
		{"transient backend fault", &runtime.RuntimeError{Code: store.FailureExecutionFailed, Message: "upstream 502"}},
		{"unclassified error", errors.New("boom")},
		// An agent's output that missed its schema is NOT deterministic:
		// the next sample may conform, so the redelivery keeps its job.
		{"schema validation on an agent output", &runtime.RuntimeError{
			Code: store.FailureSchemaValidation, NodeID: "plan", Message: "output does not match schema",
		}},
		// Nor is a refused credential: every claim re-materialises the
		// sealed OAuth blob and refreshes it, so the EFFECTIVE token on the
		// next attempt can differ from the one that was refused.
		{"auth failed", &runtime.RuntimeError{Code: store.FailureAuthFailed, Message: "401 invalid api key"}},
	} {
		t.Run(c.name+" still naks", func(t *testing.T) {
			if out := classifyExecResult(c.err, "run-1"); out.action != actionNak {
				t.Errorf("action = %v, want actionNak", out.action)
			}
		})
	}
}

// The belt: a delivery that lands on a run already parked with a
// deterministic code must not synthesise a resume either. This is the arm
// that actually produced the seven attempts — classifyExecResult naked, and
// THIS turned every redelivery back into a run.
func TestDispositionForStatus_DeterministicCodeIsNotAutoResumed(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-1"}
	run := &store.Run{
		ID:          "run-1",
		Status:      store.RunStatusFailedResumable,
		FailureCode: store.FailureExpressionFailed,
	}
	out := dispositionForStatus(msg, run)
	if out.proceed {
		t.Error("proceed = true — the runner re-ran a deterministic failure against an unchanged checkpoint")
	}
	if msg.Resume != nil {
		t.Error("a resume was synthesised for a deterministic engine code")
	}
	if out.action != actionAck {
		t.Errorf("action = %v, want actionAck (the run stays failed_resumable for an operator)", out.action)
	}
}

// The timeline has to say why a run stopped coming back: a failed_resumable
// row with no further attempt is otherwise indistinguishable from one whose
// redelivery is still in flight.
func TestRecordRetrySkipped_PutsTheDeterministicVerdictOnTheTimeline(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-deterministic"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}

	r.recordRetrySkipped(msg, store.FailureExpressionFailed,
		`compute "delivery_reserve": expr: max() takes 2 arguments, got 3`, nil, "")

	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var ev *store.Event
	for _, e := range events {
		if e.Type == store.EventRunRetrySkipped {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no run_retry_skipped on the timeline — the run just stops, with nothing saying why")
	}
	if got, _ := ev.Data["reason"].(string); got != "deterministic" {
		t.Errorf("reason = %q, want deterministic", got)
	}
	if got, _ := ev.Data["code"].(string); got != string(store.FailureExpressionFailed) {
		t.Errorf("code = %q, want %s", got, store.FailureExpressionFailed)
	}
	if got, _ := ev.Data["error"].(string); got == "" {
		t.Error("error is empty — the operator cannot tell which expression refused")
	}
}

// identicalToolFailure is what a delivery holds when a tool node's command
// failed on every execution: the error the ENGINE returned — a compiled .bot,
// the production executor, dispatcher and recipes, a real shell — and the
// store the run was written to. The command logs each execution to
// executions.log in workDir.
type identicalToolFailure struct {
	err     error
	st      store.RunStore
	wf      *ir.Workflow
	workDir string
	runID   string
}

// newIdenticalToolFailure is the issue's command: the same stderr and the
// same exit status, every time.
func newIdenticalToolFailure(t *testing.T) *identicalToolFailure {
	t.Helper()
	return newToolFailure(t, "echo x >&2; exit 3")
}

// newToolFailure runs a one-node workflow whose tool node logs the execution,
// then runs command, and returns the failure the engine ends on.
func newToolFailure(t *testing.T, command string) *identicalToolFailure {
	t.Helper()
	// The code under test gets a home and a temp dir the test owns.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	pr := parsertest.Parse("identical.bot", `tool merge:
  command: "echo run >> executions.log; `+command+`"

workflow w:
  worktree: none
  sandbox: none
  entry: merge
  merge -> done
`)
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := &identicalToolFailure{st: st, wf: cr.Workflow, workDir: t.TempDir(), runID: "run-identical"}
	f.err = f.engine(t).Run(t.Context(), f.runID, nil)
	if f.err == nil {
		t.Fatal("scenario broken: the command exits 3 and the run did not fail")
	}
	return f
}

// engine is a fresh engine over the run, wired as executeMessage wires the
// runner's: the production dispatcher and recipes. Only the wait between two
// attempts is removed, which decides nothing here.
func (f *identicalToolFailure) engine(t *testing.T) *runtime.Engine {
	t.Helper()
	exec := model.NewClawExecutor(nil, f.wf, model.WithWorkDir(f.workDir))
	t.Cleanup(func() { _ = exec.Close() })
	production := recovery.Dispatch(recovery.DefaultRecipes())
	return runtime.New(f.wf, f.st, exec, runtime.WithWorkDir(f.workDir), runtime.WithSandboxOverride("none"),
		runtime.WithRecoveryDispatch(func(ctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
			action, code := production(ctx, err, prior)
			action.Delay = 0
			return action, code
		}))
}

func (f *identicalToolFailure) executions(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.workDir, "executions.log"))
	if err != nil {
		t.Fatalf("read the execution log: %v", err)
	}
	return strings.Count(string(raw), "\n")
}

// scenarioLimit is the limit the scenarios below are written for when they
// are not about the limit itself: they pin it, so the shipped default can move
// without them. firstDelivery is how many times a failing node runs in the
// delivery that launches a run — once, then the one in-process retry an
// EXECUTION_FAILED gets; every redelivery runs it once.
const (
	scenarioLimit = 2
	firstDelivery = 2
)

func pinScenarioLimit(t *testing.T) {
	t.Helper()
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, strconv.Itoa(scenarioLimit))
}

// What the limit counts, delivery by delivery. A tool node whose command
// fails the same way on every execution: the delivery that launches the run
// executes it twice (the in-process retry is taken once per node and code),
// every redelivery once, and the runner acks the delivery whose execution is
// the limit's. So a limit of 2 costs no redelivery, 3 costs one, 4 two — and
// 1 not even the retry. The default is read from its constant.
func TestTheLimitCountsExecutionsAcrossDeliveries(t *testing.T) {
	for _, c := range []struct {
		name  string
		limit int
	}{
		{"1", 1},
		{"2", 2},
		{"3", 3},
		{"4", 4},
		{"the default", runtime.DefaultNodeMaxIdenticalFailures},
	} {
		t.Run(c.name, func(t *testing.T) {
			knob := strconv.Itoa(c.limit)
			if c.name == "the default" {
				knob = "" // unset
			}
			t.Setenv(runtime.EnvNodeMaxIdenticalFailures, knob)
			f := newIdenticalToolFailure(t)
			err := f.err
			redeliveries := 0
			var out execOutcome
			for {
				out = classifyExecResult(err, f.runID)
				if out.action == actionAck {
					break
				}
				if out.op != "nak-exec-failed" || redeliveries == 16 {
					t.Fatalf("delivery %d ended %v (op %s): %v", redeliveries+1, out.action, out.op, err)
				}
				// The redelivery: admitted as a resume of the run the engine left.
				redeliveries++
				run, lerr := f.st.LoadRun(t.Context(), f.runID)
				if lerr != nil {
					t.Fatal(lerr)
				}
				msg := &queue.RunMessage{RunID: f.runID}
				if d := dispositionForStatus(msg, run); !d.proceed || msg.Resume == nil {
					t.Fatalf("redelivery %d is not admitted as a resume (proceed=%v, op %s) on a run left %s/%s", redeliveries, d.proceed, d.op, run.Status, run.FailureCode)
				}
				err = f.engine(t).ResumeWithHostInputs(t.Context(), f.runID, nil, nil)
			}
			if got := f.executions(t); got != c.limit {
				t.Errorf("the command ran %d time(s) before the ack, want %d", got, c.limit)
			}
			if want := max(0, c.limit-firstDelivery); redeliveries != want {
				t.Errorf("%d redelivery(ies) before the ack, want %d", redeliveries, want)
			}
			if out.finalStatus != "deterministic_failure" {
				t.Errorf("finalStatus = %q, want deterministic_failure", out.finalStatus)
			}
			// An acked death has no successor attempt: the work the run
			// committed is banked here, and the timeline says why nothing
			// follows.
			if !bankableStatus(out.finalStatus) {
				t.Errorf("finalStatus %q is not banked — an acked run's commits die with the pod", out.finalStatus)
			}
			if !recordsRetrySkipped(out.finalStatus, "") {
				t.Errorf("finalStatus %q records no run_retry_skipped — the run just stops, with nothing saying why", out.finalStatus)
			}
			if code := refusalCode(err); code != store.FailureToolFailedPermanent {
				t.Errorf("the code the timeline would carry = %q, want %s", code, store.FailureToolFailedPermanent)
			}
			if hint := runtime.RemedyOf(err).Hint; !strings.Contains(hint, "resume") {
				t.Errorf("the failure names no remedy for run_retry_skipped to carry: %q", hint)
			}
		})
	}
}

// The belt, on the document the engine really wrote: a delivery that reaches
// the parked run anyway (a re-publish, an older runner's Nak) synthesises no
// resume — and the operator's own resume, published after the flip to queued
// that every cloud resume makes, proceeds and re-executes the node.
func TestARealPermanentToolFailureWaitsForAnOperatorsResume(t *testing.T) {
	pinScenarioLimit(t)
	f := newIdenticalToolFailure(t)
	ctx := t.Context()
	run, err := f.st.LoadRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureToolFailedPermanent {
		t.Errorf("the engine left the run %s/%s, want %s/%s", run.Status, run.FailureCode, store.RunStatusFailedResumable, store.FailureToolFailedPermanent)
	}

	redelivery := &queue.RunMessage{RunID: f.runID}
	out := dispositionForStatus(redelivery, run)
	if out.proceed || redelivery.Resume != nil {
		t.Errorf("a redelivery proceeds (proceed=%v, resume synthesised=%v) on a run parked %s", out.proceed, redelivery.Resume != nil, run.FailureCode)
	}
	if out.action != actionAck || out.op != "ack-deliberate-failure" {
		t.Errorf("redelivery disposition = %v/%s, want an ack (ack-deliberate-failure)", out.action, out.op)
	}

	// The operator resumes: the publisher flips the run to queued, then
	// publishes (cloudpublisher.SubmitResume).
	flip, ok, err := store.AsQueuedFlipper(f.st).FlipToQueued(ctx, f.runID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("the resume's flip to queued: changed=%v err=%v — a run parked on a permanent code cannot be resumed", ok, err)
	}
	resume := &queue.RunMessage{
		RunID:          f.runID,
		Resume:         &queue.ResumeSpec{PriorStatus: store.RunStatusFailedResumable},
		PublishedAtRFC: store.PublishAt(time.Now(), flip.At).Format(time.RFC3339Nano),
	}
	queued, err := f.st.LoadRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := dispositionForStatus(resume, queued); !got.proceed {
		t.Fatalf("the operator's resume is dropped (%s): an explicit resume must still re-queue the run", got.op)
	}
	// And the engine the runner hands it to re-executes the node, once.
	rerr := f.engine(t).ResumeWithHostInputs(ctx, f.runID, nil, nil)
	if got := f.executions(t); got != scenarioLimit+1 {
		t.Errorf("the command ran %d time(s) after the operator's resume, want %d", got, scenarioLimit+1)
	}
	if out := classifyExecResult(rerr, f.runID); out.action != actionAck {
		t.Errorf("the same failure after the resume is %v (op %s), want an ack: %v", out.action, out.op, rerr)
	}
}

// The escape hatch: with the limit at 0 the engine leaves the failure
// EXECUTION_FAILED, and the delivery is naked exactly as before.
func TestAnIdenticalToolFailureIsStillNakedWithTheLimitOff(t *testing.T) {
	t.Setenv(runtime.EnvNodeMaxIdenticalFailures, "0")
	f := newIdenticalToolFailure(t)
	out := classifyExecResult(f.err, f.runID)
	if out.action != actionNak || out.op != "nak-exec-failed" {
		t.Errorf("action = %v (op %s), want the plain nak (nak-exec-failed) — %s=0 must restore the redelivery: %v", out.action, out.op, runtime.EnvNodeMaxIdenticalFailures, f.err)
	}
	run, err := f.st.LoadRun(t.Context(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	redelivery := &queue.RunMessage{RunID: f.runID}
	if got := dispositionForStatus(redelivery, run); !got.proceed || redelivery.Resume == nil {
		t.Errorf("the redelivery does not resume the run (proceed=%v, resume synthesised=%v, op %s)", got.proceed, redelivery.Resume != nil, got.op)
	}
}

// An outage is not a failure that repeats for good, whatever it prints twice
// in two seconds. A REAL curl against a server that answers 503 — on the
// loopback, so the scenario needs no resolver and no network: the run ends
// EXECUTION_FAILED after its two executions, the delivery is naked, and the
// redelivery resumes the run — as it did before the identical-repeat verdict
// existed. No proxy is asked: the failure is curl's own, in curl's own words.
func TestARealOutageSeenByCurlIsStillRedelivered(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl is not on PATH (%v): this witness runs the real command and nothing else stands in for its wording", err)
	}
	pinScenarioLimit(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	f := newToolFailure(t, "curl -sS -f --noproxy '*' "+srv.URL)
	if got := f.executions(t); got != firstDelivery {
		t.Fatalf("curl ran %d time(s) before the engine gave up, want %d", got, firstDelivery)
	}
	if got := asked.Load(); got != firstDelivery {
		t.Fatalf("scenario broken: the server was asked %d time(s), want %d — curl did not reach it", got, firstDelivery)
	}
	if !strings.Contains(f.err.Error(), "curl: (22) The requested URL returned error: 503") {
		t.Fatalf("scenario broken: the failure is not curl's reading of the 503:\n%v", f.err)
	}
	out := classifyExecResult(f.err, f.runID)
	if out.action != actionNak || out.op != "nak-exec-failed" {
		t.Errorf("action = %v (op %s), want the plain nak (nak-exec-failed) — an outage was parked as a failure no wait cures:\n%v", out.action, out.op, f.err)
	}
	run, err := f.st.LoadRun(t.Context(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureExecutionFailed {
		t.Errorf("the engine left the run %s/%s, want %s/%s", run.Status, run.FailureCode, store.RunStatusFailedResumable, store.FailureExecutionFailed)
	}
	// Not counted at all — which is the wording's doing, not a chance
	// difference between two outputs: that would leave a streak of one.
	if streak, counted := run.Checkpoint.NodeFailureStreaks["merge"]; counted {
		t.Errorf("the outage was counted in a streak: %+v", streak)
	}
	redelivery := &queue.RunMessage{RunID: f.runID}
	if got := dispositionForStatus(redelivery, run); !got.proceed || redelivery.Resume == nil {
		t.Errorf("the redelivery does not resume the run (proceed=%v, resume synthesised=%v, op %s)", got.proceed, redelivery.Resume != nil, got.op)
	}
}
