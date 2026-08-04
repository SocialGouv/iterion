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
	"github.com/SocialGouv/iterion/pkg/runtime"
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
		{"operator pause fires", runtime.ErrRunPausedOperator, true},
		{"operator cancel fires", runtime.ErrRunCancelled, true},
		{"budget exceeded fires (acked, no auto-resume)", runtime.ErrBudgetExceeded, true},
		{"generic failure naks — no fire before the final disposition", errors.New("boom"), false},
		{"wrapped generic failure naks — no fire", fmt.Errorf("engine: %w", errors.New("boom")), false},
		{"interrupted naks — no fire", runtime.ErrRunInterrupted, false},
		{"wrapped interrupted naks — no fire", fmt.Errorf("%w: at node n1", runtime.ErrRunInterrupted), false},
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
		{"cancel checkpoint explicit resume proceeds", "run-cancelled-cp", &queue.ResumeSpec{}, true, 0, "", true},
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
		r.recordRunGitMeta(context.Background(), msg, t.TempDir(), "")
		if fake.saved != nil {
			t.Errorf("snapshot persisted despite empty baseline: %+v", fake.saved)
		}
	})
	t.Run("non-git workdir skips persistence", func(t *testing.T) {
		fake := &fakeGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		r.recordRunGitMeta(context.Background(), msg, t.TempDir(), "deadbeef")
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

		r.recordRunGitMeta(ctx, msg, dir, base)
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
		r.recordRunGitMeta(ctx, msg, dir, head)
		if fake.saved == nil {
			t.Fatal("no snapshot persisted for a no-commit run")
		}
		if len(fake.saved.Commits) != 0 || len(fake.saved.Files) != 0 {
			t.Errorf("expected empty commit/file lists, got %+v / %+v", fake.saved.Commits, fake.saved.Files)
		}
	})
}
