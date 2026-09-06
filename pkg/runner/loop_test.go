package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// NOTE: The runner's main loop wraps NATS deliveries + a Mongo store +
// the runtime engine. Full coverage of processOne / heartbeat /
// executeRun requires a NATS test broker + Mongo container + a
// recordable executor stub. The tests below cover the standalone bits
// that don't need that scaffolding:
//
//   - logDeliveryErr — log-on-error semantics
//   - New() input validation
//   - Shutdown — no-op when no in-flight run
//   - materializeOAuthCredentials — file permission + path semantics
//   - loadWorkflow — IR decode/compile error paths

// ---------------------------------------------------------------------------
// logDeliveryErr
// ---------------------------------------------------------------------------

func TestLogDeliveryErr_NoOpOnNilError(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	logDeliveryErr(logger, "ack", "run-x", nil)
	if buf.Len() != 0 {
		t.Errorf("expected no log output on nil err, got %q", buf.String())
	}
}

func TestLogDeliveryErr_LogsOnError(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelDebug, &buf)
	logDeliveryErr(logger, "nak-shutdown", "run-x", errors.New("boom"))
	out := buf.String()
	if !strings.Contains(out, "run-x") || !strings.Contains(out, "boom") || !strings.Contains(out, "nak-shutdown") {
		t.Errorf("expected runID + op + err in log; got %q", out)
	}
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingNATS(t *testing.T) {
	_, err := New(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "NATS connection") {
		t.Errorf("expected NATS-required err, got %v", err)
	}
}

// Skipped: New() validates Store after NATS, but constructing a
// non-nil *natsq.Conn for the negative-Store path would require a live
// broker — covered indirectly by integration tests once the embedded
// NATS test server lands (tracked alongside the queue/nats tests).

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestShutdown_NoInFlight_NoOp(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	// current is nil; Shutdown returns nil without panicking.
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
	// …and the pod stops advertising itself as available. The early
	// `current == nil` return must not skip this: a runner that leaves
	// idle still leaves.
	if !r.Health().Draining {
		t.Error("Shutdown did not flip readiness — the pod still reads as taking work")
	}
}

// TestShutdown_CompleteMode_LetsRunFinish is the lame-duck proof: on
// SIGTERM the complete-mode runner must NOT cancel its in-flight run — it
// waits for the run to finish on its own. The run's delivery is never
// touched on this happy path, so a nil delivery is safe here.
func TestShutdown_CompleteMode_LetsRunFinish(t *testing.T) {
	var cancelled atomic.Bool
	done := make(chan struct{})
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), DrainMode: DrainModeComplete, DrainTimeout: time.Hour}}
	r.current = &inFlight{
		runID:    "run-1",
		cancelFn: func(error) { cancelled.Store(true) },
		done:     done,
	}

	// Shutdown must block until the run finishes (done closes), NOT return
	// eagerly and NOT cancel the run.
	shutReturned := make(chan struct{})
	go func() { _ = r.Shutdown(context.Background()); close(shutReturned) }()

	select {
	case <-shutReturned:
		t.Fatal("Shutdown returned before the in-flight run finished (lame-duck must wait)")
	case <-time.After(50 * time.Millisecond):
	}
	if cancelled.Load() {
		t.Fatal("lame-duck Shutdown cancelled the in-flight run — it must let it finish")
	}
	// Readiness flips at the START of the drain, not at its end: the
	// lame-duck can last hours and an operator must see which pods are on
	// their way out for that whole time.
	if h := r.Health(); !h.Draining || !h.Busy {
		t.Errorf("mid-drain Health = %+v, want draining with the run still in flight", h)
	}

	// Run finishes naturally → Shutdown returns, still never cancelling.
	close(done)
	select {
	case <-shutReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after the run finished")
	}
	if cancelled.Load() {
		t.Fatal("Shutdown cancelled the run after it had already finished")
	}
}

// ---------------------------------------------------------------------------
// materializeOAuthCredentials
// ---------------------------------------------------------------------------

func TestMaterializeOAuthCredentials_ClaudeCode_FilePermissions(t *testing.T) {
	dir, fname, err := materializeOAuthCredentials(string(secrets.OAuthKindClaudeCode), []byte(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(dir)

	if fname != ".credentials.json" {
		t.Errorf("expected .credentials.json, got %q", fname)
	}

	// Dir must be 0700 so a sibling local user can't read OAuth tokens.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", info.Mode().Perm())
	}

	// File must be 0600.
	finfo, err := os.Stat(filepath.Join(dir, fname))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if finfo.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", finfo.Mode().Perm())
	}

	// Content must round-trip.
	got, err := os.ReadFile(filepath.Join(dir, fname))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != `{"k":"v"}` {
		t.Errorf("file content = %q, want %q", got, `{"k":"v"}`)
	}
}

func TestMaterializeOAuthCredentials_Codex_DifferentFilename(t *testing.T) {
	dir, fname, err := materializeOAuthCredentials(string(secrets.OAuthKindCodex), []byte("payload"))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(dir)
	if fname != "auth.json" {
		t.Errorf("expected auth.json, got %q", fname)
	}
}

func TestMaterializeOAuthCredentials_UnknownKindRejected(t *testing.T) {
	_, _, err := materializeOAuthCredentials("unknown-kind", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "unknown oauth kind") {
		t.Errorf("expected unknown-kind err, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// loadWorkflow
// ---------------------------------------------------------------------------

func TestLoadWorkflow_RejectsEmptyIR(t *testing.T) {
	_, err := loadWorkflow(context.Background(), &queue.RunMessage{RunID: "r", WorkflowName: "wf"}, nil)
	if err == nil || !strings.Contains(err.Error(), "neither IRCompiled nor IRRef") {
		t.Errorf("expected neither-IRCompiled-nor-IRRef err, got %v", err)
	}
}

func TestLoadWorkflow_RejectsMalformedIR(t *testing.T) {
	_, err := loadWorkflow(context.Background(), &queue.RunMessage{
		RunID:        "r",
		WorkflowName: "wf",
		IRCompiled:   []byte(`{not valid json`),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "decode IR") {
		t.Errorf("expected decode IR err, got %v", err)
	}
}

// fakeIRBlobs is a store.IRBlobStore that serves preloaded IR bytes so the
// runner's out-of-band fetch path (IRRef fallback, T-42) is exercisable
// without S3/Mongo.
type fakeIRBlobs struct {
	blobs map[string][]byte
	err   error
}

func (f *fakeIRBlobs) GetIRBlob(_ context.Context, key string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.blobs[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeIRBlobs) PutIRBlob(_ context.Context, runID string, body []byte) (string, error) {
	key := "ir/" + runID + ".json"
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	f.blobs[key] = body
	return key, nil
}

func (f *fakeIRBlobs) IRBlobBackend() string { return "s3" }

// TestLoadWorkflow_FetchesIRRef is the runner half of T-42: an oversized
// workflow arrives with an empty IRCompiled and an IRRef, and the runner
// hydrates it by fetching the blob back through the IRBlobStore seam.
func TestLoadWorkflow_FetchesIRRef(t *testing.T) {
	const src = "workflow main:\n  entry: a\n  a -> done\nagent a:\n  backend: \"claw\"\n  model: \"openai/gpt-5.4-mini\"\n  system: sys\nprompt sys:\n  hi\n"
	pr := parser.Parse("main.bot", src)
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}
	blobs := &fakeIRBlobs{blobs: map[string][]byte{"ir/run-big.json": body}}
	msg := &queue.RunMessage{
		RunID:        "run-big",
		WorkflowName: "main",
		IRRef:        &queue.IRRef{StorageKey: "ir/run-big.json", Backend: queue.IRBackendS3},
	}
	wf, err := loadWorkflow(context.Background(), msg, blobs)
	if err != nil {
		t.Fatalf("loadWorkflow: %v", err)
	}
	if wf == nil || wf.Name != "main" {
		t.Fatalf("expected workflow main, got %+v", wf)
	}
}

func TestLoadWorkflow_IRRefWithoutSeamFails(t *testing.T) {
	msg := &queue.RunMessage{
		RunID:        "run-big",
		WorkflowName: "main",
		IRRef:        &queue.IRRef{StorageKey: "ir/run-big.json", Backend: queue.IRBackendS3},
	}
	_, err := loadWorkflow(context.Background(), msg, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot fetch IR blobs") {
		t.Fatalf("expected cannot-fetch err, got %v", err)
	}
}

func TestLoadWorkflow_IRRefFetchErrorPropagates(t *testing.T) {
	msg := &queue.RunMessage{
		RunID:        "run-big",
		WorkflowName: "main",
		IRRef:        &queue.IRRef{StorageKey: "ir/missing.json", Backend: queue.IRBackendS3},
	}
	_, err := loadWorkflow(context.Background(), msg, &fakeIRBlobs{})
	if err == nil || !strings.Contains(err.Error(), "fetch out-of-band IR") {
		t.Fatalf("expected fetch err, got %v", err)
	}
}

// TestLoadWorkflow_ResolvesPluginMCPServers guards the cloud-run MCP path.
// The runner hydrates a pre-compiled AST via loadWorkflow (ir.Compile only,
// which does NOT merge plugin/project MCP servers) and must then call
// mcp.PrepareWorkflow, exactly as executeRun does after the workspace is
// known. Without that call every plugin-contributed server (firecrawl,
// repo-falcon) is silently absent from the catalog, so `mcp.<server>.*`
// resolves to zero tools — the bug that made the firecrawl plugin inert in
// cloud runs while the studio/CLI paths (which compile via runview.compileWith)
// worked. The env assertion additionally locks that the plugin's resolved env
// (FIRECRAWL_API_URL) survives the catalog → ir.MCPServer → manager round-trip
// so self-host routing isn't dropped at the ir.MCPServer bottleneck.
func TestLoadWorkflow_ResolvesPluginMCPServers(t *testing.T) {
	t.Setenv("ITERION_PLUGINS_ENABLE", "firecrawl")
	t.Setenv("ITERION_PLUGIN_FIRECRAWL_API_URL", "http://iterion-firecrawl:3002")

	const src = `prompt sys:
  Use the tools.
prompt usr:
  go.
schema out:
  x: string
agent a:
  backend: "claw"
  model: "openai/gpt-5.4-mini"
  system: sys
  user: usr
  output: out
  tools: [mcp.firecrawl.*]
  mcp:
    servers: [firecrawl]
  session: fresh
workflow main:
  entry: a
  a -> done
`
	pr := parser.Parse("firecrawl.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}

	// Faithful mirror of the runner: hydrate the pre-compiled AST, then
	// resolve the MCP catalog against the workspace (executeRun's sequence).
	wf, err := loadWorkflow(context.Background(), &queue.RunMessage{
		RunID:        "r",
		WorkflowName: "main",
		IRCompiled:   body,
	}, nil)
	if err != nil {
		t.Fatalf("loadWorkflow: %v", err)
	}
	// Precondition: ir.Compile alone must NOT resolve plugin servers — else
	// this test would pass even if the runner dropped the PrepareWorkflow call.
	if _, ok := wf.ResolvedMCPServers["firecrawl"]; ok {
		t.Fatal("precondition failed: ir.Compile resolved the firecrawl plugin server on its own — test no longer guards the runner's PrepareWorkflow call")
	}

	if err := mcp.PrepareWorkflow(wf, t.TempDir()); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	fc, ok := wf.ResolvedMCPServers["firecrawl"]
	if !ok {
		t.Fatalf("firecrawl absent from ResolvedMCPServers after PrepareWorkflow — plugin MCP server not merged (%d servers resolved)", len(wf.ResolvedMCPServers))
	}
	if got := fc.Env["FIRECRAWL_API_URL"]; got != "http://iterion-firecrawl:3002" {
		t.Errorf("FIRECRAWL_API_URL lost in ir.MCPServer round-trip: got %q, want the self-host URL", got)
	}
}

// ---------------------------------------------------------------------------
// classifyExecResult
// ---------------------------------------------------------------------------

// TestClassifyExecResult pins the Ack/Nak matrix for engine outcomes:
// checkpoint writes (paused / operator-paused / operator-cancelled) Ack; an
// infrastructure interruption (ErrRunInterrupted — runner drain / lost
// heartbeat, already written failed_resumable by the engine) Naks for
// auto-resume; a generic failure Naks. errors.Is semantics mean wrapped
// sentinels classify identically. The shutdown-vs-operator distinction now
// rides the returned error (the engine's cancel-cause branch), not runner
// flags — so an operator cancel arriving during a drain still Acks.
func TestClassifyExecResult(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantAction deliveryAction
	}{
		{"success acks finished", nil, "finished", actionAck},
		{"paused acks", runtime.ErrRunPaused, "paused", actionAck},
		{"wrapped paused acks", fmt.Errorf("engine: %w", runtime.ErrRunPaused), "paused", actionAck},
		{"operator pause acks", runtime.ErrRunPausedOperator, "paused_operator", actionAck},
		// Operator cancel stays terminal cancelled and Acks (redelivery
		// drops it) — regardless of any concurrent drain.
		{"operator cancel acks", runtime.ErrRunCancelled, "cancelled", actionAck},
		// Infra interruption: engine already wrote failed_resumable; Nak so
		// the redelivery auto-resumes.
		{"interrupted naks for resume", runtime.ErrRunInterrupted, "interrupted", actionNak},
		{"wrapped interrupted naks", fmt.Errorf("%w: at node n1", runtime.ErrRunInterrupted), "interrupted", actionNak},
		{"generic failure naks", errors.New("boom"), "failed", actionNak},
		// An IR this runner cannot load is deterministic for this runner:
		// Ack (the run is failed_resumable + IR_UNLOADABLE), never a
		// redelivery that reaches the same image and the same verdict.
		{"unloadable IR acks (no redelivery)", fmt.Errorf("runner: %w: compile IR: 3 diagnostic(s)", ErrIRUnloadable), "ir_unloadable", actionAck},
		// Budget exceeded is a resumable checkpoint — Ack, never auto-resume
		// (auto-redelivery re-fails on the same spent budget and its fresh-pod
		// recordRunGitMeta clobbers the exported commits; run 019f8e08). It is
		// the one interruption-shaped outcome that must NOT come back.
		{"budget exceeded acks (no auto-resume)", runtime.ErrBudgetExceeded, "budget_exceeded", actionAck},
		{"wrapped budget exceeded acks", fmt.Errorf("%w: duration (7201/7200)", runtime.ErrBudgetExceeded), "budget_exceeded", actionAck},
		// The engine's per-node budget pre-check historically produced a bare
		// RuntimeError (code only, no sentinel Cause) — and that shape naked
		// into the exact redelivery loop this carve-out forbids (run
		// 019fcc30-b9be: six ~40s resume/refail turns at a 96% duration hard
		// limit). The code match is what catches it, including from an older
		// engine in a mixed-version deploy.
		{"bare runtime-error budget code acks", &runtime.RuntimeError{
			Code: runtime.ErrCodeBudgetExceeded, Message: "budget hard limit reached: duration at 96%",
		}, "budget_exceeded", actionAck},
		// Precedence, pinned rather than left to the order of the ifs: if an
		// error ever carries BOTH, budget must win. Interrupted Naks for
		// auto-resume, and auto-resuming a spent budget re-fails instantly on
		// the restored accounting while a fresh pod's git snapshot overwrites
		// the first attempt's. errors.Is short-circuits, so this is one
		// reordering away from silently regressing.
		{"budget beats interrupted", errors.Join(runtime.ErrRunInterrupted, runtime.ErrBudgetExceeded), "budget_exceeded", actionAck},
		// A sandbox setup phase that hit its own bound: the engine wrote
		// failed_resumable + SANDBOX_SETUP_TIMEOUT; Nak — DELAYED — so a
		// fresh pod retries once the infrastructure has had a moment. Its
		// own status label, not "interrupted": the DLQ park on the last
		// delivery must still apply to it.
		{"sandbox phase timeout naks for a fresh pod, after a delay", fmt.Errorf("runtime: sandbox: %w",
			errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, errors.New("in-pod tar extract: signal: killed"))),
			"sandbox_setup_timeout", actionNakDelayed},
		// The resume arms' sandbox-start park returns the same sentinels the
		// status carries (runtime.parkResumeSandboxFailure): an operator
		// cancel during sandbox start acks, a drain naks exempt from the DLQ
		// park. A bare "runtime: sandbox: docker start: context canceled"
		// would classify as a generic failure — an operator cancel burning a
		// delivery, a drain parked on the DLQ.
		{"resume-arm cancel during sandbox start acks", fmt.Errorf("%w: sandbox start: %v", runtime.ErrRunCancelled, errors.New("docker start: context canceled")),
			"cancelled", actionAck},
		{"resume-arm drain during sandbox start naks", fmt.Errorf("%w: sandbox start: %v", runtime.ErrRunInterrupted, errors.New("kubectl exec: signal: killed")),
			"interrupted", actionNak},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := classifyExecResult(c.err, "run-1")
			if out.finalStatus != c.wantStatus {
				t.Errorf("finalStatus = %q, want %q", out.finalStatus, c.wantStatus)
			}
			if out.action != c.wantAction {
				t.Errorf("action = %v, want %v", out.action, c.wantAction)
			}
		})
	}
}

// A sandbox setup timeout is re-offered with sandboxSetupTimeoutNakDelay,
// through NakWithDelay — a bare Nak re-offers within seconds, and a copy
// that always stalls then burns the delivery budget as back-to-back pods
// (8 × the phase budget before the DLQ park, #669's measured shape).
func TestClassifyExecResult_SandboxSetupTimeoutIsReofferedAfterADelay(t *testing.T) {
	err := fmt.Errorf("runtime: sandbox: %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, errors.New("in-pod tar extract: signal: killed")))
	out := classifyExecResult(err, "run-1")
	if out.delay != sandboxSetupTimeoutNakDelay || out.delay <= 0 {
		t.Fatalf("delay = %s, want %s — an immediate re-offer is 8 back-to-back pods", out.delay, sandboxSetupTimeoutNakDelay)
	}
	d := &fakeDelivery{delivered: 3}
	dispatchExecOutcome(iterlog.Nop(), d, out, "run-1")
	if len(d.nakDelays) != 1 || d.nakDelays[0] != sandboxSetupTimeoutNakDelay || d.naks != 0 || d.terms != 0 || d.acks != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one NakWithDelay(%s)", d, sandboxSetupTimeoutNakDelay)
	}
}

// A pod that never got placed is re-offered too, after long enough for a
// cluster autoscaler to add a node (the measured scale-up was ~2 min).
// Deliberately a delayed NAK and not an interruption: the DLQ park on the
// last permitted delivery still applies, so a fleet that stays full ends
// parked and announced rather than naking into nothing.
func TestClassifyExecResult_SandboxCapacityIsReofferedAfterADelay(t *testing.T) {
	err := fmt.Errorf("runtime: sandbox: %w",
		errors.Join(sandbox.ErrCapacity, errors.New("0/12 nodes are available: 11 Insufficient cpu")))
	out := classifyExecResult(err, "run-1")
	if out.finalStatus != "sandbox_capacity" {
		t.Fatalf("finalStatus = %q, want sandbox_capacity", out.finalStatus)
	}
	if out.delay != sandboxCapacityNakDelay || out.delay <= 0 {
		t.Fatalf("delay = %s, want %s — an immediate re-offer re-hits a cluster that has not grown yet", out.delay, sandboxCapacityNakDelay)
	}
	if bankableStatus(out.finalStatus) {
		t.Fatal("a pod that never started has nothing to bank")
	}
	d := &fakeDelivery{delivered: 3}
	dispatchExecOutcome(iterlog.Nop(), d, out, "run-1")
	if len(d.nakDelays) != 1 || d.nakDelays[0] != sandboxCapacityNakDelay || d.naks != 0 || d.terms != 0 || d.acks != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one NakWithDelay(%s)", d, sandboxCapacityNakDelay)
	}
}

// The delayed redelivery lands on the run's timeline, naming the phase
// timeout, the delay and the attempt's rank — the only trace between two
// attempts of why the run sits failed_resumable.
func TestRecordRedeliveryDeferred_PutsThePhaseTimeoutOnTheTimeline(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-deferred"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	execErr := fmt.Errorf("runtime: sandbox: %w", errors.Join(sandbox.ErrPhaseTimeout, errors.New("workspace copy stalled")))
	out := classifyExecResult(execErr, id)

	r.recordRedeliveryDeferred(msg, out, execErr, 3, 8)

	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var ev *store.Event
	for _, e := range events {
		if e.Type == store.EventRunRedeliveryDeferred {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no run_redelivery_deferred on the timeline — between two attempts the operator sees nothing")
	}
	if got, _ := ev.Data["reason"].(string); got != "sandbox_setup_timeout" {
		t.Fatalf("reason = %q, want sandbox_setup_timeout", got)
	}
	if got, _ := ev.Data["delay_seconds"].(float64); int(got) != int(sandboxSetupTimeoutNakDelay/time.Second) {
		t.Fatalf("delay_seconds = %v, want %d", ev.Data["delay_seconds"], int(sandboxSetupTimeoutNakDelay/time.Second))
	}
	if got, _ := ev.Data["error"].(string); !strings.Contains(got, "workspace copy stalled") {
		t.Fatalf("error = %q, want the engine's cause", got)
	}
	if d, _ := ev.Data["delivery"].(float64); int(d) != 3 {
		t.Fatalf("delivery = %v, want 3", ev.Data["delivery"])
	}
}

// TestOutcomeSideEffectsFire pins which engine outcomes fire the run-outcome
// side effects (completion webhook + run.<outcome> event) on the plain
// dispatch path. The action is derived through classifyExecResult, so this
// also catches an Ack/Nak flip that would silently change fire behaviour:
// finals (finished / paused / cancelled / budget) fire once; a Nak with
// redeliveries remaining (generic failure, infra interruption) fires
// NOTHING — firing there pushed one "run failed" notification episode per
// redelivery, up to MaxDeliver for a single deterministic failure.
func TestOutcomeSideEffectsFire(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantFires bool
	}{
		{"finished fires", nil, true},
		{"paused fires", runtime.ErrRunPaused, true},
		// A DELAYED nak is still a nak: the run comes back on its own, so
		// firing here would push one "run failed" episode per stalled
		// sandbox attempt.
		{"sandbox setup timeout (delayed nak) does not fire", fmt.Errorf("runtime: sandbox: %w",
			errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, errors.New("copy stalled"))), false},
		{"operator pause fires", runtime.ErrRunPausedOperator, true},
		{"operator cancel fires", runtime.ErrRunCancelled, true},
		{"budget exceeded fires (acked, no auto-resume)", runtime.ErrBudgetExceeded, true},
		{"generic failure naks — no fire before the final disposition", errors.New("boom"), false},
		{"wrapped generic failure naks — no fire", fmt.Errorf("engine: %w", errors.New("boom")), false},
		{"interrupted naks — no fire", runtime.ErrRunInterrupted, false},
		{"wrapped interrupted naks — no fire", fmt.Errorf("%w: at node n1", runtime.ErrRunInterrupted), false},
		{"sandbox phase timeout naks — no fire", fmt.Errorf("runtime: sandbox: %w", sandbox.ErrPhaseTimeout), false},
		{"sandbox capacity (delayed nak) does not fire", fmt.Errorf("runtime: sandbox: %w",
			errors.Join(sandbox.ErrCapacity, errors.New("11 Insufficient cpu"))), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := classifyExecResult(c.err, "run-1")
			if got := outcomeSideEffectsFire(c.err, out.action); got != c.wantFires {
				t.Errorf("outcomeSideEffectsFire(%v, %v) = %v, want %v", c.err, out.action, got, c.wantFires)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveDeliveryPreconditions
// ---------------------------------------------------------------------------

// TestResolveDeliveryPreconditions pins the pre-lock status gauntlet on a
// real (filesystem) store: unknown runs Term; stale terminal deliveries
// Ack; redelivered launches against a resumable status are converted to
// resumes IN PLACE (msg.Resume mutated) so JetStream redelivery uses the
// checkpoint it exists to protect.
func TestResolveDeliveryPreconditions(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	save := func(id string, status store.RunStatus, cp *store.Checkpoint) {
		t.Helper()
		if err := st.SaveRun(ctx, &store.Run{ID: id, WorkflowName: "wf", Status: status, Checkpoint: cp}); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	save("run-running", store.RunStatusRunning, nil)
	save("run-cancelled-nocp", store.RunStatusCancelled, nil)
	save("run-cancelled-cp", store.RunStatusCancelled, &store.Checkpoint{NodeID: "n1"})
	save("run-failres", store.RunStatusFailedResumable, &store.Checkpoint{NodeID: "n1"})
	save("run-pausedop", store.RunStatusPausedOperator, &store.Checkpoint{NodeID: "n1"})
	save("run-finished", store.RunStatusFinished, nil)
	// A PR-closed cancel writes the reason (wrapped by CancelRunWithReason)
	// into run.Error. The runner admission MUST detect it and drop the
	// redelivery even when it carries an explicit resume — see the new
	// case rows below (#663).
	saveErr := func(id string, status store.RunStatus, cp *store.Checkpoint, errText string) {
		t.Helper()
		if err := st.SaveRun(ctx, &store.Run{ID: id, WorkflowName: "wf", Status: status, Checkpoint: cp, Error: errText}); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	// A document cancelled before EndReason existed: the message, wrapped
	// as CancelRunWithReason wraps it ("(was <status>: <prior>)"), is the
	// only carrier — which is what store.EndedBecausePRClosed's migration
	// half reads.
	saveErr("run-cancel-pr-closed", store.RunStatusCancelled, &store.Checkpoint{NodeID: "n1"},
		store.RunEndReasonPRClosed.Message()+" (was failed_resumable: node \"campaign\": rate_limited)")
	// An operator cancel of a QUEUED resume: every cloud resume CASes the
	// doc to queued before publishing, so `cancelled` at admission always
	// post-dates the publish — whatever the error shape.
	saveErr("run-cancelled-op", store.RunStatusCancelled, &store.Checkpoint{NodeID: "n1"}, "cancelled")
	saveErr("run-cancelled-interrupted", store.RunStatusCancelled, &store.Checkpoint{NodeID: "n1"}, "cancelled (was running: runtime: run interrupted)")

	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}

	cases := []struct {
		name        string
		runID       string
		resume      *queue.ResumeSpec
		wantProceed bool
		wantAction  deliveryAction
		wantStatus  string
		wantResume  bool // msg.Resume non-nil after the call
	}{
		{"missing run terms", "run-ghost", nil, false, actionTerm, "store_load_failed", false},
		{"running proceeds as launch", "run-running", nil, true, 0, "", false},
		{"pre-pickup cancel acks", "run-cancelled-nocp", nil, false, actionAck, "cancelled", false},
		// Cancelled is terminal for redelivery even WITH a checkpoint:
		// auto-resume here resurrected operator-cancelled runs (live:
		// 019f8ba3, three times). Only an explicit resume proceeds.
		{"cancel checkpoint stays cancelled", "run-cancelled-cp", nil, false, actionAck, "cancelled", false},
		// A resume message on a cancelled doc: the cancel post-dates the
		// publish (every cloud resume CASes to queued first), so it is an
		// operator's decision the message must not override.
		{"cancel checkpoint with resume set still drops", "run-cancelled-cp", &queue.ResumeSpec{}, false, actionAck, "cancelled", true},
		{"operator cancel of a queued resume drops", "run-cancelled-op", &queue.ResumeSpec{}, false, actionAck, "cancelled", true},
		{"interrupted-shaped cancel with resume set drops", "run-cancelled-interrupted", &queue.ResumeSpec{}, false, actionAck, "cancelled", true},
		{"empty-reason cancel with resume set drops", "run-cancelled-cp", &queue.ResumeSpec{}, false, actionAck, "cancelled", true},
		// #663: a PR-closed cancel is terminal for EVERY redelivery,
		// including one that carries msg.Resume set (from the retry
		// sweeper's SubmitResume or a Nak of the previous attempt).
		// Without this the run 01a06885 dogfood on #646 re-launched a
		// review 2s after the PR merged, burning provider quota.
		{"cancel PR-closed drops resume redelivery", "run-cancel-pr-closed", &queue.ResumeSpec{}, false, actionAck, "cancelled", true},
		{"cancel PR-closed drops bare redelivery too", "run-cancel-pr-closed", nil, false, actionAck, "cancelled", false},
		{"failed_resumable converts to resume", "run-failres", nil, true, 0, "", true},
		{"paused_operator converts to resume", "run-pausedop", nil, true, 0, "", true},
		{"explicit resume passes through", "run-failres", &queue.ResumeSpec{}, true, 0, "", true},
		{"finished acks stale delivery", "run-finished", nil, false, actionAck, "finished", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := &queue.RunMessage{RunID: c.runID, TenantID: "team-1", Resume: c.resume}
			out := r.resolveDeliveryPreconditions(msg)
			if out.proceed != c.wantProceed {
				t.Fatalf("proceed = %v, want %v (outcome %+v)", out.proceed, c.wantProceed, out)
			}
			if out.proceed && out.preRun == nil {
				t.Fatal("proceed=true with nil preRun")
			}
			if !out.proceed {
				if out.action != c.wantAction {
					t.Errorf("action = %v, want %v", out.action, c.wantAction)
				}
				if out.finalStatus != c.wantStatus {
					t.Errorf("finalStatus = %q, want %q", out.finalStatus, c.wantStatus)
				}
			}
			if (msg.Resume != nil) != c.wantResume {
				t.Errorf("msg.Resume non-nil = %v, want %v", msg.Resume != nil, c.wantResume)
			}
		})
	}
}

// #714: a stale LAUNCH redelivery on a run that lived a second life must
// not restart it from node 1. The sequence is the live one: the run is
// cancelled with its checkpoint kept, an operator resumes it (the
// publisher CASes the doc back to `queued`, refreshing QueuedAt, and
// publishes a RESUME message), and JetStream redelivers the ORIGINAL
// LAUNCH message first. That message carries no Resume and meets a
// `queued` doc, so the admission gauntlet used to wave it through:
// runResolveDoc flips queued → running and Engine.Run starts at the entry
// node, re-spending the tokens the checkpoint exists to save and
// re-posting whatever the first pass already posted — while the real
// RESUME message then finds a `running` doc.
//
// QueuedAt is the fence: every transition into `queued` refreshes it, so
// a delivery published BEFORE it belongs to an earlier attempt.
func TestResolveDeliveryPreconditions_StaleLaunchOnRequeuedRun(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}

	// Life 1: launched, executed, checkpointed, then cancelled — the
	// checkpoint survives the cancel by contract.
	const runID = "run-requeued"
	if err := st.SaveRun(ctx, &store.Run{ID: runID, WorkflowName: "wf", Status: store.RunStatusRunning}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "implement"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusCancelled, "operator cancelled"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	launchedAt := time.Now().UTC().Add(-time.Hour)

	// Life 2: the operator's resume claims the doc back to `queued`,
	// which refreshes QueuedAt — the durable attempt marker.
	changed, err := st.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusCancelled})
	if err != nil || !changed {
		t.Fatalf("resume claim = (%t, %v), want (true, nil)", changed, err)
	}
	requeued, err := st.LoadRun(ctx, runID)
	if err != nil || requeued.QueuedAt == nil {
		t.Fatalf("LoadRun after requeue = (%+v, %v), want a QueuedAt marker", requeued, err)
	}
	if requeued.Checkpoint == nil {
		t.Fatal("the requeue dropped the checkpoint — the premise of this test no longer holds")
	}

	t.Run("stale launch redelivery is dropped", func(t *testing.T) {
		msg := &queue.RunMessage{
			RunID:          runID,
			TenantID:       "team-1",
			PublishedAtRFC: launchedAt.Format(time.RFC3339Nano),
		}
		out := r.resolveDeliveryPreconditions(msg)
		if out.proceed && msg.Resume == nil {
			t.Fatal("a launch message published before the current attempt proceeded as a LAUNCH — Engine.Run restarts it from the entry node with the checkpoint on the doc ignored")
		}
		if out.proceed {
			t.Fatalf("the stale delivery proceeded (as a resume) — the run's own resume message is already in flight, so this one must be dropped: %+v", out)
		}
		if out.action != actionAck {
			t.Errorf("action = %v, want actionAck (a stale delivery is dropped, not redelivered)", out.action)
		}
		if out.finalStatus != "stale_attempt" {
			t.Errorf("finalStatus = %q, want stale_attempt", out.finalStatus)
		}
	})

	t.Run("the attempt's own resume message proceeds", func(t *testing.T) {
		msg := &queue.RunMessage{
			RunID:          runID,
			TenantID:       "team-1",
			Resume:         &queue.ResumeSpec{},
			PublishedAtRFC: requeued.QueuedAt.Add(time.Millisecond).Format(time.RFC3339Nano),
		}
		out := r.resolveDeliveryPreconditions(msg)
		if !out.proceed {
			t.Fatalf("the resume publication of the CURRENT attempt was refused: %+v", out)
		}
		if msg.Resume == nil {
			t.Fatal("msg.Resume was cleared")
		}
	})

	// A message with no published_at (a legacy publication, or a store
	// that never stamped one) cannot be told apart by identity. The
	// checkpoint on the doc is then the evidence: a queued doc that
	// carries one has already executed, so the delivery resumes rather
	// than restarting.
	t.Run("undatable launch honours the checkpoint", func(t *testing.T) {
		msg := &queue.RunMessage{RunID: runID, TenantID: "team-1"}
		out := r.resolveDeliveryPreconditions(msg)
		if !out.proceed {
			t.Fatalf("an undatable delivery on a resumable queued doc was dropped: %+v", out)
		}
		if msg.Resume == nil {
			t.Fatal("no resume synthesised — Engine.Run would restart the run from its entry node, ignoring the checkpoint")
		}
	})

	// The fence must not swallow a FIRST attempt: a freshly queued doc
	// with no checkpoint is exactly what the queued arm exists to admit.
	t.Run("a first launch still proceeds as a launch", func(t *testing.T) {
		const freshID = "run-fresh"
		if _, err := st.CreateQueuedRun(ctx, freshID, "wf", "", "", nil); err != nil {
			t.Fatalf("CreateQueuedRun: %v", err)
		}
		msg := &queue.RunMessage{
			RunID:          freshID,
			TenantID:       "team-1",
			PublishedAtRFC: time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		}
		out := r.resolveDeliveryPreconditions(msg)
		if !out.proceed {
			t.Fatalf("a first launch was refused: %+v", out)
		}
		if msg.Resume != nil {
			t.Fatal("a first launch was converted to a resume — it has no checkpoint to resume from")
		}
	})
}

// ---------------------------------------------------------------------------
// injectCredentials / deleteRunSecrets
// ---------------------------------------------------------------------------

const testSealerKeyB64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func testSealer(t *testing.T) secrets.Sealer {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealerFromBase64(testSealerKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

func TestInjectCredentials_NoRefIsPassthrough(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	ctx := context.Background()
	got, cleanup, err := r.injectCredentials(ctx, &queue.RunMessage{RunID: "run-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup != nil {
		t.Error("expected nil cleanup when no SecretsRef")
	}
	if got != ctx {
		t.Error("expected the original ctx back unchanged")
	}
	if _, ok := secrets.CredentialsFromContext(got); ok {
		t.Error("no credentials should be stamped without a SecretsRef")
	}
}

func TestInjectCredentials_RefWithoutStoresFails(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	_, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", SecretsRef: "ref-1"})
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("expected not-wired error, got %v", err)
	}
	if cleanup != nil {
		t.Error("expected nil cleanup on wiring error")
	}
}

func TestInjectCredentials_UnknownRefFails(t *testing.T) {
	r := &Runner{cfg: Config{
		Logger:     iterlog.Nop(),
		RunSecrets: secrets.NewMemoryRunSecretsStore(),
		Sealer:     testSealer(t),
	}}
	_, _, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", SecretsRef: "ref-missing"})
	if err == nil || !strings.Contains(err.Error(), "fetch run_secrets") {
		t.Fatalf("expected fetch error, got %v", err)
	}
}

// TestInjectCredentials_TenantMismatchFails pins the exact-match tenant
// binding: a sealed bundle stamped for another tenant (or a legacy
// tenant-less record) must never be served to this run.
func TestInjectCredentials_TenantMismatchFails(t *testing.T) {
	sealer := testSealer(t)
	rs := secrets.NewMemoryRunSecretsStore()
	sealed, err := secrets.SealRunBundle(sealer, "run-1", secrets.RunBundle{GenericSecrets: map[string]string{"x": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, recTenant := range []string{"team-other", ""} {
		if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: recTenant, RunID: "run-1", SealedBundle: sealed}); err != nil {
			t.Fatal(err)
		}
		r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs, Sealer: sealer}}
		_, _, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"})
		if err == nil || !strings.Contains(err.Error(), "tenant mismatch") {
			t.Fatalf("rec tenant %q: expected tenant-mismatch error, got %v", recTenant, err)
		}
	}
}

// TestInjectCredentials_HappyPathAndCleanup covers the full unseal →
// ctx-stamp → OAuth-materialise cycle and the local-hygiene cleanup:
// plaintext keys are wiped in memory and the materialised OAuth dirs
// removed, while the PERSISTENT sealed bundle stays in the store (the
// redelivery contract — deletion is deleteRunSecrets' job, on
// terminal-clean outcomes only).
func TestInjectCredentials_HappyPathAndCleanup(t *testing.T) {
	sealer := testSealer(t)
	rs := secrets.NewMemoryRunSecretsStore()
	bundle := secrets.RunBundle{
		APIKeys:           map[secrets.Provider]string{"anthropic": "sk-live-1"},
		GenericSecrets:    map[string]string{"forge_token": "tok-1"},
		GenericSecretRefs: map[string]string{"forge_token": "gsid-1"},
		// codex kind: materialised as auth.json, and skipped by the
		// anthropic-only refresher so the test spawns no goroutine.
		OAuthCredentials: map[string][]byte{string(secrets.OAuthKindCodex): []byte(`{"tokens":{}}`)},
		ForgeAppBotLogin: "app[bot]",
	}
	sealed, err := secrets.SealRunBundle(sealer, "run-1", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "team-a", RunID: "run-1", SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}

	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs, Sealer: sealer}}
	ctx, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"})
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected a cleanup func")
	}

	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		t.Fatal("credentials not stamped into ctx")
	}
	if got := creds.APIKey("anthropic"); got != "sk-live-1" {
		t.Errorf("APIKey = %q, want sk-live-1", got)
	}
	if got := creds.GenericSecret("forge_token"); got != "tok-1" {
		t.Errorf("GenericSecret = %q, want tok-1", got)
	}
	if got := creds.GenericRefs["forge_token"]; got != "gsid-1" {
		t.Errorf("GenericRefs = %q, want gsid-1", got)
	}
	if creds.ForgeAppBotLogin != "app[bot]" {
		t.Errorf("ForgeAppBotLogin = %q, want app[bot]", creds.ForgeAppBotLogin)
	}
	dir := creds.OAuthCredentialFiles["codex"]
	if dir == "" {
		t.Fatal("codex OAuth dir not materialised")
	}
	fi, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode = %v, want 0600", fi.Mode().Perm())
	}

	cleanup()
	if got := creds.APIKey("anthropic"); got != "" {
		t.Errorf("API key survived cleanup: %q", got)
	}
	if got := creds.GenericSecret("forge_token"); got != "" {
		t.Errorf("generic secret survived cleanup: %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("OAuth temp dir survived cleanup: %s", dir)
	}
	// The persistent sealed bundle must survive cleanup (redelivery contract).
	if _, err := rs.Get(context.Background(), "ref-1"); err != nil {
		t.Errorf("sealed bundle deleted by cleanup: %v", err)
	}
}

func TestDeleteRunSecrets(t *testing.T) {
	rs := secrets.NewMemoryRunSecretsStore()
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}

	// Empty ref: no-op, record untouched.
	r.deleteRunSecrets(&queue.RunMessage{RunID: "run-1"})
	if _, err := rs.Get(context.Background(), "ref-1"); err != nil {
		t.Fatalf("record deleted on empty ref: %v", err)
	}
	// Real ref: removed.
	r.deleteRunSecrets(&queue.RunMessage{RunID: "run-1", SecretsRef: "ref-1"})
	if _, err := rs.Get(context.Background(), "ref-1"); !errors.Is(err, secrets.ErrRunSecretsNotFound) {
		t.Fatalf("expected ErrRunSecretsNotFound after delete, got %v", err)
	}
	// Nil store: no panic.
	(&Runner{cfg: Config{Logger: iterlog.Nop()}}).deleteRunSecrets(&queue.RunMessage{RunID: "run-1", SecretsRef: "ref-1"})
}

// ---------------------------------------------------------------------------
// stringifyVars / env parsing / cloud budget ceiling
// ---------------------------------------------------------------------------

// TestStringifyVars pins the wire-vars → executor-vars contract: strings
// pass through, non-string scalars render with %v, and nested structures
// are JSON-encoded so the downstream template engine sees parseable JSON
// (`{"a":1}`), never Go %v syntax (`map[a:1]`). A value JSON cannot
// encode is an error, not a silently mangled string.
func TestStringifyVars(t *testing.T) {
	if got, err := stringifyVars(nil); err != nil || got != nil {
		t.Errorf("stringifyVars(nil) = %v, %v, want nil, nil", got, err)
	}
	if got, err := stringifyVars(map[string]any{}); err != nil || got != nil {
		t.Errorf("stringifyVars(empty) = %v, %v, want nil, nil", got, err)
	}
	got, err := stringifyVars(map[string]any{
		"s": "x",
		"f": float64(2),
		"i": 7,
		"b": true,
		"z": nil,
		"m": map[string]any{"b": float64(2), "a": "one"},
		"l": []any{"x", float64(1), true},
	})
	if err != nil {
		t.Fatalf("stringifyVars: %v", err)
	}
	want := map[string]string{
		"s": "x",
		"f": "2",
		"i": "7",
		"b": "true",
		"z": "",
		"m": `{"a":"one","b":2}`, // JSON (sorted keys), not Go map syntax
		"l": `["x",1,true]`,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("stringifyVars[%q] = %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("len = %d, want %d", len(got), len(want))
	}

	// An unencodable nested value surfaces as an error naming the var —
	// never a silent %v fallback.
	if _, err := stringifyVars(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("stringifyVars(chan var) = nil error, want JSON-encode error")
	} else if !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("stringifyVars(chan var) error = %q, want it to name var \"bad\"", err)
	}
}

func TestEnvPositiveIntAndFloat(t *testing.T) {
	const key = "ITERION_TEST_ENV_POSITIVE"
	intCases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"", 0, false}, {"5", 5, true}, {"0", 0, false}, {"-3", 0, false}, {"abc", 0, false},
	}
	for _, c := range intCases {
		t.Setenv(key, c.in)
		got, ok := envPositiveInt(key)
		if got != c.want || ok != c.ok {
			t.Errorf("envPositiveInt(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
	floatCases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"", 0, false}, {"2.5", 2.5, true}, {"0", 0, false}, {"-1.5", 0, false}, {"NaN%", 0, false},
	}
	for _, c := range floatCases {
		t.Setenv(key, c.in)
		got, ok := envPositiveFloat(key)
		if got != c.want || ok != c.ok {
			t.Errorf("envPositiveFloat(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func clearCloudCeilingEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ITERION_CLOUD_MAX_ITERATIONS", "ITERION_CLOUD_MAX_TOKENS",
		"ITERION_CLOUD_MAX_PARALLEL_BRANCHES", "ITERION_CLOUD_MAX_COST_USD",
		"ITERION_CLOUD_MAX_DURATION",
	} {
		t.Setenv(k, "")
	}
}

func TestApplyCloudBudgetCeiling(t *testing.T) {
	t.Run("no env is a no-op (nil budget stays nil)", func(t *testing.T) {
		clearCloudCeilingEnv(t)
		wf := &ir.Workflow{}
		applyCloudBudgetCeiling(wf, iterlog.Nop())
		if wf.Budget != nil {
			t.Errorf("Budget = %+v, want nil (no platform ceiling configured)", wf.Budget)
		}
	})
	t.Run("ceiling imposes limits on an unbudgeted workflow", func(t *testing.T) {
		clearCloudCeilingEnv(t)
		t.Setenv("ITERION_CLOUD_MAX_ITERATIONS", "10")
		t.Setenv("ITERION_CLOUD_MAX_DURATION", "2h")
		wf := &ir.Workflow{}
		applyCloudBudgetCeiling(wf, iterlog.Nop())
		if wf.Budget == nil || wf.Budget.MaxIterations != 10 || wf.Budget.MaxDuration != "2h" {
			t.Errorf("Budget = %+v, want iterations 10 + duration 2h imposed", wf.Budget)
		}
	})
	t.Run("lower declared budget is kept", func(t *testing.T) {
		clearCloudCeilingEnv(t)
		t.Setenv("ITERION_CLOUD_MAX_TOKENS", "100000")
		wf := &ir.Workflow{Budget: &ir.Budget{MaxTokens: 500}}
		applyCloudBudgetCeiling(wf, iterlog.Nop())
		if wf.Budget.MaxTokens != 500 {
			t.Errorf("MaxTokens = %d, want the tenant's lower 500 kept", wf.Budget.MaxTokens)
		}
	})
}

// ---------------------------------------------------------------------------
// uploadRunFiles / recordRunGitMeta
// ---------------------------------------------------------------------------

// fakeUploaderStore is a RunStore that additionally exposes the
// RunFilesUploader bridge, recording the call for assertions. The embedded
// interface is nil — only UploadRunFiles may be invoked.
type fakeUploaderStore struct {
	store.RunStore
	n         int
	err       error
	gotRun    string
	gotTenant string
}

func (f *fakeUploaderStore) UploadRunFiles(ctx context.Context, runID string) (int, error) {
	f.gotRun = runID
	f.gotTenant, _ = store.TenantFromContext(ctx)
	return f.n, f.err
}

func TestUploadRunFiles(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-a", OwnerID: "owner-1"}

	t.Run("store without the seam no-ops", func(t *testing.T) {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
		r.uploadRunFiles(context.Background(), msg) // must not panic
	})
	t.Run("uploader seam receives runID under the tenant identity", func(t *testing.T) {
		fake := &fakeUploaderStore{n: 2}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		r.uploadRunFiles(context.Background(), msg)
		if fake.gotRun != "run-1" {
			t.Errorf("uploaded run = %q, want run-1", fake.gotRun)
		}
		if fake.gotTenant != "team-a" {
			t.Errorf("upload ctx tenant = %q, want team-a", fake.gotTenant)
		}
	})
	t.Run("upload failure is non-fatal", func(t *testing.T) {
		fake := &fakeUploaderStore{err: errors.New("s3 down")}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		r.uploadRunFiles(context.Background(), msg) // logged, never panics
	})
}

// fakeGitMetaStore is a RunStore that additionally persists git metadata,
// capturing the snapshot recordRunGitMeta saves.
type fakeGitMetaStore struct {
	store.RunStore
	saved    *store.RunGitMeta
	savedRun string
}

func (f *fakeGitMetaStore) SaveRunGitMeta(_ context.Context, runID string, meta *store.RunGitMeta) error {
	f.savedRun = runID
	f.saved = meta
	return nil
}

func (f *fakeGitMetaStore) LoadRunGitMeta(context.Context, string) (*store.RunGitMeta, error) {
	return nil, nil
}

// gitCommitAll stages everything and commits with a fixed identity.
func gitCommitAll(t *testing.T, r *Runner, dir, msg string) {
	t.Helper()
	ctx := context.Background()
	if err := r.runGit(ctx, dir, "", "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := r.runGit(ctx, dir, "", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", msg); err != nil {
		t.Fatalf("git commit: %v", err)
	}
}

// TestRecordRunGitMeta pins the best-effort git snapshot: no baseline or a
// non-git workdir skip persistence entirely; a real clone with commits past
// the baseline persists the commit + file lists the server pod serves after
// the runner's workspace is wiped.
func TestRecordRunGitMeta(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}

	t.Run("empty baseline skips persistence", func(t *testing.T) {
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		r.recordRunGitMeta(context.Background(), msg, t.TempDir(), "", runtime.WorkspaceIntegrity{})
		if fake.saved != nil {
			t.Errorf("snapshot persisted despite empty baseline: %+v", fake.saved)
		}
	})
	t.Run("non-git workdir skips persistence", func(t *testing.T) {
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		r.recordRunGitMeta(context.Background(), msg, t.TempDir(), "deadbeef", runtime.WorkspaceIntegrity{})
		if fake.saved != nil {
			t.Errorf("snapshot persisted for a non-git dir: %+v", fake.saved)
		}
	})
	t.Run("commits past the baseline are persisted", func(t *testing.T) {
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir := t.TempDir()
		ctx := context.Background()
		if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c1")
		base, err := gitlib.RevParseHead(dir)
		if err != nil {
			t.Fatalf("rev-parse base: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c2")

		r.recordRunGitMeta(ctx, msg, dir, base, runtime.WorkspaceIntegrity{})
		if fake.saved == nil {
			t.Fatal("no snapshot persisted")
		}
		if fake.savedRun != "run-1" {
			t.Errorf("saved run = %q, want run-1", fake.savedRun)
		}
		if fake.saved.BaseCommit != base {
			t.Errorf("BaseCommit = %q, want %q", fake.saved.BaseCommit, base)
		}
		if len(fake.saved.Commits) != 1 {
			t.Fatalf("Commits = %+v, want exactly the one post-baseline commit", fake.saved.Commits)
		}
		foundB := false
		for _, f := range fake.saved.Files {
			if f.Path == "b.txt" {
				foundB = true
			}
		}
		if !foundB {
			t.Errorf("Files = %+v, want b.txt listed", fake.saved.Files)
		}
	})
	t.Run("baseline equal to HEAD persists the empty snapshot", func(t *testing.T) {
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir := t.TempDir()
		ctx := context.Background()
		if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c1")
		head, err := gitlib.RevParseHead(dir)
		if err != nil {
			t.Fatal(err)
		}
		r.recordRunGitMeta(ctx, msg, dir, head, runtime.WorkspaceIntegrity{})
		if fake.saved == nil {
			t.Fatal("no snapshot persisted for a no-commit run")
		}
		if len(fake.saved.Commits) != 0 || len(fake.saved.Files) != 0 {
			t.Errorf("expected empty commit/file lists, got %+v / %+v", fake.saved.Commits, fake.saved.Files)
		}
	})
	t.Run("baseline tree with a diverging pod-side HEAD is NOT recorded", func(t *testing.T) {
		// Export-based sandbox whose pod finished elsewhere: an empty
		// snapshot here would be a confident lie (the run committed, the
		// export lost it) — recordRunGitMeta must skip, not certify zero.
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir := t.TempDir()
		ctx := context.Background()
		if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c1")
		head, err := gitlib.RevParseHead(dir)
		if err != nil {
			t.Fatal(err)
		}

		r.recordRunGitMeta(ctx, msg, dir, head, runtime.WorkspaceIntegrity{
			Applicable: true, PodHead: "feedfacefeedfacefeedfacefeedfacefeedface"})
		if fake.saved != nil {
			t.Errorf("empty snapshot persisted despite the pod-side HEAD disagreeing: %+v", fake.saved)
		}

		r.recordRunGitMeta(ctx, msg, dir, head, runtime.WorkspaceIntegrity{
			Applicable: true, CaptureErr: "pod gone"})
		if fake.saved != nil {
			t.Errorf("empty snapshot persisted despite the pod-side HEAD being unknown: %+v", fake.saved)
		}

		// The pod agreeing (HEAD == baseline) keeps the empty snapshot a
		// recordable FACT — falsified the other way.
		r.recordRunGitMeta(ctx, msg, dir, head, runtime.WorkspaceIntegrity{
			Applicable: true, PodHead: head})
		if fake.saved == nil {
			t.Error("pod-confirmed empty snapshot was not persisted")
		}
	})
}

// A precondition drop must carry the JetStream attempt count and stream
// sequence on its own line; a silent outcome stays silent.
func TestWithDeliveryAttempt(t *testing.T) {
	f, a := withDeliveryAttempt("runner: run %s is cancelled — dropping redelivery (resume=%v)", []any{"run-1", true}, 2, 77)
	if got := fmt.Sprintf(f, a...); got != "runner: run run-1 is cancelled — dropping redelivery (resume=true) (delivery=2 seq=77)" {
		t.Fatalf("rendered %q", got)
	}
	if f, a := withDeliveryAttempt("", nil, 3, 9); f != "" || a != nil {
		t.Fatalf("silent outcome must stay silent, got %q %v", f, a)
	}
}

// The admission gauntlet runs BEFORE the per-run lock. A `running` doc
// must leave it untouched: the lease is the only liveness authority, and
// a write here — with the lease possibly held by a live sibling — would
// mislabel a live run, then Nak away when the lock refuses. Adoption of
// an orphan happens under the lock, never here.
func TestResolveDeliveryPreconditions_RunningIsNotWrittenBeforeTheLock(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.SaveRun(ctx, &store.Run{
		ID: "run-running-prelock", TenantID: "team-1", OwnerID: "u1",
		WorkflowName: "wf", Status: store.RunStatusRunning,
		Checkpoint: &store.Checkpoint{NodeID: "n1"},
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, lockLivenessOverride: true}
	msg := &queue.RunMessage{RunID: "run-running-prelock", TenantID: "team-1", OwnerID: "u1"}
	out := r.resolveDeliveryPreconditions(msg)
	if !out.proceed {
		t.Fatalf("a running doc must proceed to the lock, got %+v", out)
	}
	if msg.Resume != nil {
		t.Fatal("the delivery was converted to a resume BEFORE the lock — the write it implies raced whoever holds the lease")
	}
	got, err := st.LoadRun(ctx, "run-running-prelock")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusRunning {
		t.Fatalf("status = %s: the doc was written BEFORE the lock — a live sibling's run just got mislabelled", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Under-lock adoption of a stale `running` doc
// ---------------------------------------------------------------------------

// fakeDelivery records the JetStream transition a decision takes.
type fakeDelivery struct {
	delivered         int
	acks, naks, terms int
	nakDelays         []time.Duration
}

func (d *fakeDelivery) Ack() error  { d.acks++; return nil }
func (d *fakeDelivery) Nak() error  { d.naks++; return nil }
func (d *fakeDelivery) Term() error { d.terms++; return nil }
func (d *fakeDelivery) NakWithDelay(delay time.Duration) error {
	d.nakDelays = append(d.nakDelays, delay)
	return nil
}
func (d *fakeDelivery) NumDelivered() int { return d.delivered }

// lockHeldStore answers every LockRun with the lease-held signal: a
// sibling pod owns the run.
type lockHeldStore struct{ store.RunStore }

func (lockHeldStore) LockRun(context.Context, string) (store.RunLock, error) {
	return nil, natsq.ErrLockHeld
}

// seedRunningRun persists a `running` doc with a checkpoint and returns
// it as the store reads it (UpdatedAt stamped by the save).
func seedRunningRun(t *testing.T, st store.RunStore, id string) *store.Run {
	t.Helper()
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.SaveRun(ctx, &store.Run{
		ID: id, TenantID: "team-1", OwnerID: "u1", WorkflowName: "wf",
		Status: store.RunStatusRunning, Checkpoint: &store.Checkpoint{NodeID: "n1"},
		// Every real doc carries a last-write time (CreateRun and each
		// status transition stamp it); a zero one is the legacy shape.
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	run, err := st.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return run
}

func loadStatus(t *testing.T, st store.RunStore, id string) *store.Run {
	t.Helper()
	run, err := st.LoadRun(store.WithIdentity(context.Background(), "team-1", "u1"), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return run
}

// A lease held by another pod is a live owner: the delivery naks away and
// NOTHING is written — adoption never runs, because it runs after the
// lock and the lock refused.
func TestAcquireRunLock_LeaseHeldByAnotherDefersWithoutWrite(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-held"
	seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: lockHeldStore{st}, Logger: iterlog.Nop()}, lockLivenessOverride: true}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	d := &fakeDelivery{delivered: 3}

	_, ok, status := r.acquireRunLock(context.Background(), msg, d, iterlog.Nop())

	if ok || status != "lock_held" {
		t.Fatalf("acquireRunLock = (%v, %q), want (false, lock_held)", ok, status)
	}
	if d.naks != 0 || d.acks != 0 || d.terms != 0 || len(d.nakDelays) != 1 || d.nakDelays[0] != natsq.DefaultLockTTL {
		t.Fatalf("delivery transitions = %+v, want one delayed Nak (the sibling keeps the run)", d)
	}
	if got := loadStatus(t, st, id); got.Status != store.RunStatusRunning || got.FailureCode != "" {
		t.Fatalf("doc = %s/%q after a refused lock — a live sibling's run was written to", got.Status, got.FailureCode)
	}
	if msg.Resume != nil {
		t.Fatal("the delivery was converted to a resume although the lock refused")
	}
}

// A `running` doc written more recently than the adoption floor is not
// adopted: the previous holder may be a lapsed-but-alive pod still
// unwinding, whose unconditional terminal write would land on top of the
// adopter. The delivery is re-offered after the floor's remainder — a
// delayed Nak, so the redelivery budget is not burnt inside the floor —
// and the lock is released for it.
func TestAdoptRunningUnderLock_YoungDocIsReofferedAfterTheFloor(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-young"
	doc := seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, lockLivenessOverride: true}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	lock, err := st.LockRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}
	d := &fakeDelivery{delivered: 2}

	out := r.adoptRunningUnderLock(msg, doc, d, doc.UpdatedAt.Add(time.Minute))

	if out.proceed {
		t.Fatalf("a doc written 1m ago was adopted (%+v) — inside the %s floor a lapsed pod may still be unwinding", out, runningAdoptionFloor)
	}
	if out.action != actionNakDelayed {
		t.Fatalf("action = %v, want a delayed Nak", out.action)
	}
	if want := runningAdoptionFloor - time.Minute; out.delay > want || out.delay < want-2*time.Second {
		t.Fatalf("delay = %s, want the floor's remainder ≈ %s", out.delay, want)
	}
	dispatchPrecondition(iterlog.Nop(), d, out, id)
	if len(d.nakDelays) != 1 || d.nakDelays[0] != out.delay || d.naks != 0 {
		t.Fatalf("delivery transitions = %+v, want one NakWithDelay(%s) and no bare Nak (seven bare naks in two minutes is the #669 log)", d, out.delay)
	}
	if got := loadStatus(t, st, id); got.Status != store.RunStatusRunning {
		t.Fatalf("doc = %s, want running left untouched", got.Status)
	}
	if msg.Resume != nil {
		t.Fatal("a deferred delivery must not be converted to a resume")
	}
	// The lock is released on the way out of processOne; the next
	// delivery must be able to take it.
	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	next, err := st.LockRun(context.Background(), id)
	if err != nil {
		t.Fatalf("the lock was not released for the re-offered delivery: %v", err)
	}
	_ = next.Unlock()
}

// A young `running` doc on the LAST permitted delivery: JetStream will not
// re-offer it whatever we answer, so the delivery must not claim a
// re-offer ("re-offered in Ns") that never comes. Nothing is written — the
// floor exists because the previous holder may still be unwinding — the
// message is termed, and the log names the owner of what happens next:
// the orphan sweeper.
func TestAdoptRunningUnderLock_YoungDocOnLastDeliveryTermsAndNamesTheSweeper(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-young-last"
	doc := seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, lockLivenessOverride: true, maxDeliverOverride: 8}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	d := &fakeDelivery{delivered: 8}

	out := r.adoptRunningUnderLock(msg, doc, d, doc.UpdatedAt.Add(time.Minute))

	if out.proceed {
		t.Fatalf("a young doc was adopted on the last delivery (%+v) — the floor still applies", out)
	}
	if out.action == actionNakDelayed {
		t.Fatalf("action = delayed Nak on delivery %d/%d — JetStream will not re-offer it, so a 're-offered in Ns' log line is false and nothing parks or reconciles the doc", d.delivered, r.maxDeliver())
	}
	if out.action != actionTerm {
		t.Fatalf("action = %v, want Term (explicit on the queue side; the sweeper owns the doc)", out.action)
	}
	if !strings.Contains(out.logFmt, "LAST permitted delivery") || !strings.Contains(out.logFmt, "orphan sweeper") {
		t.Fatalf("the log must say this is the last delivery and who reconciles the doc, got %q", out.logFmt)
	}
	dispatchPrecondition(iterlog.Nop(), d, out, id)
	if d.terms != 1 || len(d.nakDelays) != 0 || d.naks != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one Term", d)
	}
	if got := loadStatus(t, st, id); got.Status != store.RunStatusRunning || got.FailureCode != "" {
		t.Fatalf("doc = %s/%q, want running left untouched — a park over a possibly-live writer clobbers or is clobbered", got.Status, got.FailureCode)
	}
	if msg.Resume != nil {
		t.Fatal("the termed delivery was converted to a resume")
	}
}

// A `running` doc older than the floor, under our lock, is an orphan:
// promoted to failed_resumable + PROCESS_ORPHANED with continuation
// redelivery_pending — THIS delivery resumes it next, so nothing that
// acts on `final` (the board dispatcher, the stuck-card watchdog, the
// outcome router) may act in the window — and the delivery is converted
// into a resume so the checkpoint is honoured.
func TestAdoptRunningUnderLock_OldDocIsAdoptedAsRedeliveryPending(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-orphan"
	doc := seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, lockLivenessOverride: true}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	d := &fakeDelivery{delivered: 2}

	out := r.adoptRunningUnderLock(msg, doc, d, doc.UpdatedAt.Add(10*time.Minute))

	if !out.proceed {
		t.Fatalf("an orphan older than the floor was not adopted: %+v", out)
	}
	if msg.Resume == nil {
		t.Fatal("msg.Resume still nil after the promote — Engine.Resume would refuse `running` and burn the delivery (#669 part 2)")
	}
	got := loadStatus(t, st, id)
	if got.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", got.Status)
	}
	if got.FailureCode != store.FailureProcessOrphaned {
		t.Fatalf("FailureCode = %q, want PROCESS_ORPHANED", got.FailureCode)
	}
	if got.ContinuationState != store.ContinuationRedeliveryPending {
		t.Fatalf("ContinuationState = %q, want redelivery_pending — `final` lets three consumers act on a run this very delivery is about to resume", got.ContinuationState)
	}
	if !strings.Contains(out.logFmt, "delivery %d/%d") || !strings.Contains(out.logFmt, "last doc write") {
		t.Fatalf("the promote line must carry the doc age and the delivery count, got %q", out.logFmt)
	}
}

// Without a lease authority (no queue wired, a lock-less store) a held
// lock proves nothing: the doc is left to the engine, unwritten.
func TestAdoptRunningUnderLock_NoLeaseAuthorityLeavesTheDocAlone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-no-authority"
	doc := seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}

	out := r.adoptRunningUnderLock(msg, doc, &fakeDelivery{}, doc.UpdatedAt.Add(time.Hour))

	if !out.proceed || msg.Resume != nil {
		t.Fatalf("no-authority path must proceed unchanged, got %+v (resume=%v)", out, msg.Resume != nil)
	}
	if got := loadStatus(t, st, id); got.Status != store.RunStatusRunning {
		t.Fatalf("doc = %s, want running — a wrong promote under an untestable premise", got.Status)
	}
}

// The pre-lock copy is stale by construction: a peer's terminal write
// that landed between the two reads is honoured through the ordinary
// disposition — resumed now, not deferred on the stale copy's age, and
// with no CAS of our own over its cause.
func TestAdoptRunningUnderLock_PeerMovedTheDocTakesItsDisposition(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-moved"
	stale := seedRunningRun(t, st, id)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.UpdateRunStatusCoded(ctx, id, store.RunStatusFailedResumable, "interrupted at node n1", store.FailureInterrupted); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, lockLivenessOverride: true}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}

	// The stale copy reads young: judged on it, the delivery would be
	// deferred for the floor's remainder while the doc is resumable NOW.
	out := r.adoptRunningUnderLock(msg, stale, &fakeDelivery{}, stale.UpdatedAt.Add(time.Minute))

	if !out.proceed || msg.Resume == nil {
		t.Fatalf("a peer-moved failed_resumable must convert to a resume now, got %+v (resume=%v)", out, msg.Resume != nil)
	}
	if got := loadStatus(t, st, id); got.FailureCode != store.FailureInterrupted {
		t.Fatalf("FailureCode = %q, want the peer's INTERRUPTED kept — the adoption must not overwrite a cause it did not establish", got.FailureCode)
	}
}

// The PR-closed drop is a PROTOCOL between the webhook layer that cancels and
// the runner that admits. Its carrier is the typed EndReason on the run doc,
// not the prose in run.Error: a reworded, translated or truncated message must
// not silently re-arm the redelivery this admission exists to refuse.
func TestResolveDeliveryPreconditions_PRClosedIsReadFromTheTypedEndReason(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	save := func(id string, end store.RunEndReason, errText string) {
		t.Helper()
		if err := st.SaveRun(ctx, &store.Run{
			ID: id, WorkflowName: "wf", Status: store.RunStatusCancelled,
			Checkpoint: &store.Checkpoint{NodeID: "n1"},
			EndReason:  end, Error: errText,
		}); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	// The typed reason with a message that shares nothing with the constant.
	save("run-typed", store.RunEndReasonPRClosed, "la pull request est fermée (was running: node \"campaign\")")
	// A doc written before the field existed: the prose is all there is.
	save("run-legacy", "", store.RunEndReasonPRClosed.Message()+" (was failed_resumable: node \"campaign\": rate_limited)")
	// An operator cancel must NOT be mistaken for one.
	save("run-operator", store.RunEndReasonOperator, "cancelled by user")

	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	for _, c := range []struct {
		name, runID, wantOp string
	}{
		{"the typed reason decides", "run-typed", "ack-pr-closed-cancel"},
		{"a pre-field doc still reads its prose", "run-legacy", "ack-pr-closed-cancel"},
		{"an operator cancel keeps its own line", "run-operator", "ack-cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := &queue.RunMessage{RunID: c.runID, TenantID: "team-1", Resume: &queue.ResumeSpec{}}
			out := r.resolveDeliveryPreconditions(msg)
			if out.proceed {
				t.Fatalf("a cancelled run must never proceed (outcome %+v)", out)
			}
			if out.op != c.wantOp {
				t.Fatalf("op = %q, want %q", out.op, c.wantOp)
			}
		})
	}
}
