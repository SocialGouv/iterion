// Package runner implements the cloud-mode iterion runner pod. It
// pulls RunMessages from the NATS JetStream queue, claims a
// distributed lease, hydrates the workflow IR, and executes runs
// against the Mongo+S3 store.
//
// One runner pod handles one in-flight run at a time
// (MaxAckPending=1 on the JetStream consumer); horizontal scale
// comes from spawning more pods (KEDA scales on lag — see plan §F
// T-36 runner-keda-scaledobject.yaml).
//
// Cloud-ready plan §F (T-27, T-28, T-29).
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const tracerName = "github.com/SocialGouv/iterion/pkg/runner"

// Config is the runner bootstrap.
type Config struct {
	NATS              *natsq.Conn
	Store             store.RunStore
	RunnerID          string
	WorkDir           string        // base directory for per-run workspaces
	HeartbeatInterval time.Duration // how often to refresh the NATS KV lease
	PendingPoll       time.Duration // how often to refresh nats_pending_messages (0 = 15s)
	FetchWait         time.Duration // long-poll wait per fetch
	Logger            *iterlog.Logger
	// Metrics, when non-nil, receives counters/gauges updates from the
	// runner loop (in-flight runs, durations, heartbeat errors, NATS
	// queue depth, LLM token usage). Nil-safe: passing nil disables
	// metrics emission without changing the loop's behaviour, useful
	// for unit tests and the local-mode dev runner.
	Metrics *metrics.Registry

	// RunSecrets + Sealer carry the BYOK / OAuth bundle the
	// publisher pre-resolved. Both nil → runner falls back to env
	// vars at the LLM call site (Phase A/B compatibility). Phase C.
	RunSecrets secrets.RunSecretsStore
	Sealer     secrets.Sealer
}

// Runner is the long-running consumer loop.
type Runner struct {
	cfg      Config
	consumer *natsq.Consumer
	cancel   context.CancelFunc

	mu      sync.Mutex
	current *inFlight // non-nil while a run is being processed
}

type inFlight struct {
	runID    string
	delivery *natsq.Delivery
	cancelFn context.CancelFunc
	// done is closed once processOne has Ack'd or Nak'd the delivery.
	// Shutdown selects on it to avoid double-acting on a delivery
	// processOne already finalised.
	done chan struct{}
}

// New builds a runner from the supplied dependencies and creates the
// JetStream consumer. The actual loop starts via Run.
func New(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.NATS == nil {
		return nil, fmt.Errorf("runner: NATS connection is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("runner: Store is required")
	}
	if cfg.RunnerID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = fmt.Sprintf("runner-%d", time.Now().UnixNano())
		}
		cfg.RunnerID = host
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 20 * time.Second
	}
	if cfg.PendingPoll == 0 {
		cfg.PendingPoll = 15 * time.Second
	}
	if cfg.FetchWait == 0 {
		cfg.FetchWait = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.New(iterlog.LevelInfo, os.Stderr)
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}

	cons, err := cfg.NATS.NewConsumer(ctx)
	if err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, consumer: cons}, nil
}

// Run drains the queue until ctx is cancelled. Each iteration fetches
// one message, processes it synchronously, and acks (or naks/terms
// on failure). Returns ctx.Err() when shut down cleanly.
func (r *Runner) Run(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	defer cancel()

	r.cfg.Logger.Info("runner: started, runnerID=%s workdir=%s", r.cfg.RunnerID, r.cfg.WorkDir)

	// NATS queue depth gauge: every PendingPoll the runner samples the
	// JetStream consumer info and publishes the Pending count to the
	// Prometheus registry. KEDA scales on the same value via the
	// nats-jetstream scaler — this gauge gives operators a parallel
	// signal in their own dashboards without competing with the scaler.
	if r.cfg.Metrics != nil {
		go r.pollPending(loopCtx)
	}

	for {
		select {
		case <-loopCtx.Done():
			r.cfg.Logger.Info("runner: ctx done — exiting loop")
			return loopCtx.Err()
		default:
		}

		delivery, err := r.consumer.Fetch(loopCtx, r.cfg.FetchWait)
		if err != nil {
			if errors.Is(err, natsq.ErrNoMessage) {
				continue // long-poll elapsed; loop back
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			r.cfg.Logger.Warn("runner: fetch error: %v (backing off)", err)
			select {
			case <-time.After(2 * time.Second):
			case <-loopCtx.Done():
				return loopCtx.Err()
			}
			continue
		}

		r.processOne(loopCtx, delivery)
	}
}

// Shutdown signals the loop to stop fetching new messages and waits
// for the in-flight run (if any) to be cancelled, then republishes
// its delivery so a sibling pod can pick it up. Plan §F T-28.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	cur := r.current
	r.mu.Unlock()

	if r.cancel != nil {
		r.cancel()
	}
	if cur == nil {
		return nil
	}
	// Cancel the in-flight context so engine.Run unwinds via
	// handleContextDoneWithCheckpoint (preserving the checkpoint),
	// and extend the ack window while we wait for it to finish.
	cur.cancelFn()
	_ = cur.delivery.InProgress()
	select {
	case <-cur.done:
		// processOne already Ack'd (paused/cancelled checkpoint) or
		// Nak'd (transient failure) the delivery. Nothing more to do.
		r.cfg.Logger.Info("runner: in-flight run %s drained during shutdown", cur.runID)
	case <-ctx.Done():
		// Grace period expired before the engine finished checkpointing.
		// Best-effort Nak so JetStream redelivers to a sibling pod.
		_ = cur.delivery.Nak()
		r.cfg.Logger.Warn("runner: shutdown grace expired for run %s — naking for redelivery", cur.runID)
	}
	return nil
}

// processOne validates, locks, executes a single delivery. The
// per-run context inherits from the runner's loop context so
// shutdown unwinds cleanly via handleContextDoneWithCheckpoint
// (preserving the checkpoint for resume).
func (r *Runner) processOne(parent context.Context, delivery *natsq.Delivery) {
	msg, err := delivery.Decode()
	if err != nil {
		r.cfg.Logger.Error("runner: decode delivery: %v", err)
		_ = delivery.Term() // unrecoverable — bad payload
		return
	}

	logger := r.cfg.Logger
	logger.Info("runner: processing run %s (workflow=%s)", msg.RunID, msg.WorkflowName)

	// runs_active{status=running}: incremented as soon as the runner
	// commits to executing this delivery (post-decode), decremented in
	// the deferred block below regardless of outcome. run_duration_seconds
	// is observed once with the final terminal status so percentile
	// dashboards stay clean even when a run nak's mid-flight.
	start := time.Now()
	finalStatus := "failed"
	if r.cfg.Metrics != nil {
		r.cfg.Metrics.RunsActive.WithLabelValues("running").Inc()
		defer func() {
			r.cfg.Metrics.RunsActive.WithLabelValues("running").Dec()
			r.cfg.Metrics.RunDurationSeconds.WithLabelValues(finalStatus).Observe(time.Since(start).Seconds())
		}()
	}

	// Inherit the publisher's trace so OTel spans created by the
	// engine appear under the originating editor span (plan §F T-41).
	traced := delivery.PropagateTraceTo(parent)
	// Stamp tenant + owner from the message into ctx so every
	// downstream Mongo write picks them up and every Mongo read
	// stays scoped to the run's tenant. The runner trusts the
	// publisher to have set these from a verified Identity; we
	// re-validate against the loaded run doc below.
	traced = store.WithIdentity(traced, msg.TenantID, msg.OwnerID)
	// Root span for the runner-side execution. Per-node spans created
	// inside engine.Run hang off this one, so a single trace covers
	// API → queue → runner → node graph. The span ends in the deferred
	// block below; finalStatus is set at every exit path.
	spanCtx, span := otel.Tracer(tracerName).Start(traced, "iterion.runner.process_one",
		trace.WithAttributes(
			attribute.String("iterion.run_id", msg.RunID),
			attribute.String("iterion.workflow_name", msg.WorkflowName),
			attribute.String("iterion.workflow_hash", msg.WorkflowHash),
			attribute.String("iterion.tenant_id", msg.TenantID),
		),
	)
	runCtx, runCancel := context.WithCancel(spanCtx)
	defer runCancel()
	defer func() {
		span.SetAttributes(attribute.String("iterion.run.status", finalStatus))
		if finalStatus == "failed" || finalStatus == "lock_held" {
			span.SetStatus(codes.Error, finalStatus)
		}
		span.End()
	}()

	done := make(chan struct{})
	r.mu.Lock()
	r.current = &inFlight{runID: msg.RunID, delivery: delivery, cancelFn: runCancel, done: done}
	r.mu.Unlock()
	defer func() {
		// Close before nilling: a Shutdown that captured the cur
		// pointer must always observe the channel close.
		close(done)
		r.mu.Lock()
		r.current = nil
		r.mu.Unlock()
	}()

	// Subscribe to the cancel subject for this run. A POST cancel on
	// the API publishes on iterion.cancel.<run_id>; we react by
	// cancelling runCtx, which unwinds engine.Run via
	// handleContextDoneWithCheckpoint.
	if _, err := r.cfg.NATS.SubscribeCancel(runCtx, msg.RunID, runCancel); err != nil {
		logger.Warn("runner: subscribe cancel %s: %v (continuing without)", msg.RunID, err)
	}

	// Cooperative cancel check: if the server flipped the run to
	// cancelled before we picked it up (T-32 cancel-queued path),
	// ack the JetStream delivery without doing any work — we do
	// not lock, do not touch the engine, and the queue position
	// for sibling runs collapses immediately. The server's cancel
	// handler is responsible for flipping the Mongo doc; the
	// JetStream message becomes a no-op signal.
	preRun, preErr := r.cfg.Store.LoadRun(runCtx, msg.RunID)
	if preErr == nil && preRun != nil && preRun.Status == store.RunStatusCancelled {
		logger.Info("runner: run %s already cancelled — skipping", msg.RunID)
		finalStatus = "cancelled"
		_ = delivery.Ack()
		return
	}
	// Verify the message's tenant matches the persisted document.
	// A mismatch implies either a corrupted publish (publisher
	// stamped the wrong tenant) or a malicious / replayed message.
	// Either way the run is unsafe to execute under either tenant's
	// scope; term the delivery so it doesn't redeliver.
	if preErr == nil && preRun != nil && preRun.TenantID != msg.TenantID {
		logger.Error("runner: tenant mismatch for run %s (msg=%q stored=%q) — terming", msg.RunID, msg.TenantID, preRun.TenantID)
		finalStatus = "tenant_mismatch"
		_ = delivery.Term()
		return
	}

	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		if errors.Is(err, natsq.ErrLockHeld) {
			logger.Warn("runner: lock held for %s — naking for sibling", msg.RunID)
			finalStatus = "lock_held"
			_ = delivery.Nak()
			return
		}
		logger.Error("runner: lock %s: %v", msg.RunID, err)
		_ = delivery.Nak()
		return
	}
	defer func() {
		// Surface release errors at warn level so a stuck KV entry
		// (network partition, permissions) shows up in the runner
		// logs instead of being silently dropped — without this, an
		// expired-but-not-deleted lease blocks siblings for the full
		// LockTTL window with no operator visibility.
		if err := lock.Unlock(); err != nil {
			logger.Warn("runner: lock release for %s: %v", msg.RunID, err)
		}
	}()

	// Heartbeat goroutine: refresh the NATS lease while we own it.
	// On refresh failure the heartbeat cancels runCtx so engine.Run
	// unwinds via handleContextDoneWithCheckpoint — better to lose
	// progress than to let the lease expire while the engine is still
	// writing to Mongo (which would invite split-brain when JetStream
	// redelivers to a sibling pod). hbFailed flips to true before the
	// cancel so processOne can distinguish heartbeat-induced cancel
	// from a legitimate user cancel and Nak instead of Ack — without
	// that, JetStream considers the run done and no sibling picks it
	// up automatically.
	var hbFailed atomic.Bool
	hbDone := make(chan struct{})
	go r.heartbeat(runCtx, runCancel, lock, hbDone, &hbFailed)
	// Cancel runCtx *before* waiting on hbDone, otherwise we deadlock:
	// heartbeat only exits on ctx.Done(), and the outer `defer
	// runCancel()` at function entry is LIFO-last so it would run
	// after this defer. Calling runCancel() here is idempotent.
	defer func() {
		runCancel()
		<-hbDone
	}()

	if err := r.executeRun(runCtx, msg); err != nil {
		// Distinguish transient (resumable) vs terminal failures.
		// runtime.ErrRunPaused / ErrRunCancelled are not "the
		// delivery failed" — they're successful checkpoint writes
		// and we ack accordingly.
		if errors.Is(err, runtime.ErrRunPaused) {
			logger.Info("runner: run %s checkpointed (%v)", msg.RunID, err)
			finalStatus = "paused"
			_ = delivery.Ack()
			return
		}
		if errors.Is(err, runtime.ErrRunCancelled) {
			// Heartbeat-induced cancel: the lease is gone, so a sibling
			// runner is free to pick this up via JetStream redelivery.
			// Nak (not Ack) so the message stays queued instead of
			// being marked done and forcing manual user intervention.
			if hbFailed.Load() {
				logger.Warn("runner: run %s heartbeat lost — naking for sibling redelivery", msg.RunID)
				finalStatus = "lock_held"
				_ = delivery.Nak()
				return
			}
			logger.Info("runner: run %s checkpointed (%v)", msg.RunID, err)
			finalStatus = "cancelled"
			_ = delivery.Ack()
			return
		}
		// Other errors → nak so JetStream redelivers up to MaxDeliver.
		logger.Error("runner: run %s execution failed: %v", msg.RunID, err)
		_ = delivery.Nak()
		return
	}

	logger.Info("runner: run %s completed", msg.RunID)
	finalStatus = "finished"
	_ = delivery.Ack()
}

// heartbeat refreshes the NATS KV lease so a long-running run keeps
// holding the lock past the 60s default TTL. Returns when ctx is
// cancelled (run finished). On refresh failure the heartbeat sets
// hbFailed and triggers runCancel so the engine unwinds proactively
// before the lease expires — without that signal, the lease would
// silently lapse and JetStream would redeliver to a sibling pod, two
// writers ending up on the same run state. processOne reads hbFailed
// to Nak (not Ack) the delivery so the message stays queued for sibling
// redelivery instead of requiring manual user resume.
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelFunc, lock store.RunLock, done chan<- struct{}, hbFailed *atomic.Bool) {
	defer close(done)
	natsLock, ok := lock.(*natsq.Lock)
	if !ok {
		return // no-op lock or non-NATS provider — nothing to refresh
	}
	t := time.NewTicker(r.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := natsLock.Refresh(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return // run already exiting
				}
				if r.cfg.Metrics != nil {
					r.cfg.Metrics.RunnerHeartbeatErrors.Inc()
				}
				r.cfg.Logger.Error("runner: heartbeat refresh failed: %v — cancelling run to avoid split-brain", err)
				hbFailed.Store(true)
				runCancel()
				return
			}
		}
	}
}

// executeRun hydrates the IR from the message, builds the runtime
// engine + Claw executor, then dispatches to Run or Resume based on
// the message shape.
func (r *Runner) executeRun(ctx context.Context, msg *queue.RunMessage) error {
	wf, err := loadWorkflow(msg)
	if err != nil {
		return err
	}

	executor, err := r.buildExecutor(ctx, msg, wf)
	if err != nil {
		return err
	}

	engineOpts := []runtime.EngineOption{
		runtime.WithLogger(r.cfg.Logger),
		runtime.WithWorkflowHash(msg.WorkflowHash),
		runtime.WithWorkDir(r.cfg.WorkDir),
	}
	if msg.Resume != nil && msg.Resume.Force {
		// Force-resume must be applied at engine construction so the
		// hash-mismatch guard in pkg/runtime/resume.go reads the flag.
		// This was previously dropped on the floor.
		engineOpts = append(engineOpts, runtime.WithForceResume(true))
	}
	engine := runtime.New(wf, r.cfg.Store, executor, engineOpts...)

	// Phase C: fetch + decrypt the per-run sealed credentials bundle
	// when the publisher attached one. The result lives only in ctx —
	// the runner process itself stays clean of plaintext keys.
	ctx, cleanup, credErr := r.injectCredentials(ctx, msg)
	if credErr != nil {
		r.cfg.Logger.Warn("runner: credentials inject %s: %v (continuing without)", msg.RunID, credErr)
	}
	if cleanup != nil {
		defer cleanup()
	}

	if msg.Resume != nil {
		return engine.Resume(ctx, msg.RunID, msg.Resume.Answers)
	}
	return engine.Run(ctx, msg.RunID, msg.Vars)
}

// injectCredentials resolves the run's sealed bundle, decrypts it,
// stamps the plaintext into ctx via secrets.WithCredentials, and
// returns a cleanup func that wipes the bundle at the call site.
// When no bundle is attached or the runner has no Sealer wired,
// returns the original ctx unchanged.
func (r *Runner) injectCredentials(ctx context.Context, msg *queue.RunMessage) (context.Context, func(), error) {
	if msg.SecretsRef == "" {
		return ctx, nil, nil
	}
	if r.cfg.RunSecrets == nil || r.cfg.Sealer == nil {
		return ctx, nil, fmt.Errorf("runner: SecretsRef set but RunSecrets/Sealer not wired")
	}
	rec, err := r.cfg.RunSecrets.Get(ctx, msg.SecretsRef)
	if err != nil {
		return ctx, nil, fmt.Errorf("fetch run_secrets %s: %w", msg.SecretsRef, err)
	}
	if rec.TenantID != "" && rec.TenantID != msg.TenantID {
		return ctx, nil, fmt.Errorf("run_secrets tenant mismatch (msg=%q sealed=%q)", msg.TenantID, rec.TenantID)
	}
	bundle, err := secrets.OpenRunBundle(r.cfg.Sealer, msg.RunID, rec.SealedBundle)
	if err != nil {
		return ctx, nil, fmt.Errorf("unseal run_secrets %s: %w", msg.SecretsRef, err)
	}
	creds := secrets.Credentials{
		APIKeys: bundle.APIKeys,
	}
	cleanup := func() {
		// Best-effort delete: a missing record is fine (TTL already
		// got it). The runner deletes on every exit so a redelivered
		// message gets a fresh ref from the publisher.
		ctxDel, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.cfg.RunSecrets.Delete(ctxDel, msg.SecretsRef)
		// Wipe plaintext (best-effort; Go gives no secure-erase
		// guarantee but this reduces window of exposure).
		for k := range bundle.APIKeys {
			bundle.APIKeys[k] = ""
		}
	}
	return secrets.WithCredentials(ctx, creds), cleanup, nil
}

// loadWorkflow decodes the AST embedded in the message and compiles
// it to IR. T-42 will add a fallback for IRRef when IRCompiled is
// absent (oversized IR pulled from S3 or Mongo); the inline case
// covers the vast majority of workflows.
func loadWorkflow(msg *queue.RunMessage) (*ir.Workflow, error) {
	if len(msg.IRCompiled) == 0 {
		// IRRef fallback isn't wired yet — server should always
		// inline the IR for now. Fail loudly so the operator can
		// surface the "IR too large" mode they hit.
		return nil, fmt.Errorf("runner: RunMessage.IRCompiled is empty (IRRef fallback not yet implemented)")
	}
	file, err := ast.UnmarshalFile(msg.IRCompiled)
	if err != nil {
		return nil, fmt.Errorf("runner: decode IR: %w", err)
	}
	cr := ir.Compile(file)
	if cr.HasErrors() {
		return nil, fmt.Errorf("runner: compile IR: %d diagnostic(s)", len(cr.Diagnostics))
	}
	return cr.Workflow, nil
}

// buildExecutor reuses runview.BuildExecutor so the runner shares
// exactly the same backend / tool / MCP wiring as the editor server
// and the CLI run path. Vars from the message are forwarded so
// {{vars.X}} expansion works without re-resolving from disk.
func (r *Runner) buildExecutor(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow) (runtime.NodeExecutor, error) {
	emitter, ok := r.cfg.Store.(model.EventEmitter)
	if !ok {
		return nil, fmt.Errorf("runner: store does not satisfy model.EventEmitter")
	}
	// Wrap the emitter so LLM step + delegate events update the
	// iterion_llm_tokens_total / iterion_llm_cost_usd_total counters
	// as they are written to Mongo. Wrapping at the runner boundary
	// keeps pkg/backend/model free of any metrics dependency.
	if r.cfg.Metrics != nil {
		emitter = newMetricsEmitter(emitter, r.cfg.Metrics)
	}
	vars := stringifyVars(msg.Vars)
	return runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: wf,
		Vars:     vars,
		Store:    emitter,
		RunID:    msg.RunID,
		Logger:   r.cfg.Logger,
		StoreDir: r.cfg.WorkDir,
	})
}

// pollPending samples the JetStream consumer info on a fixed cadence
// and republishes the Pending count to nats_pending_messages. Exits
// when ctx is cancelled. Errors are logged at debug level — the
// scaler is the source of truth for autoscaling, so a transient miss
// here is observability noise, not a correctness issue.
func (r *Runner) pollPending(ctx context.Context) {
	t := time.NewTicker(r.cfg.PendingPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pending, err := r.consumer.Pending(ctx)
			if err != nil {
				r.cfg.Logger.Debug("runner: pending poll: %v", err)
				continue
			}
			r.cfg.Metrics.NATSPendingMessages.Set(float64(pending))
		}
	}
}

// metricsEmitter wraps a model.EventEmitter and taps llm_step_finished
// / delegate_finished events to keep the LLM token + cost counters
// up-to-date. The forward call to the underlying emitter happens
// regardless of metric outcome so write durability is unaffected.
type metricsEmitter struct {
	inner model.EventEmitter
	reg   *metrics.Registry

	// modelByNode caches the last model name reported by an
	// llm_request event for a given node, so the subsequent
	// llm_step_finished events can be labelled even though the step
	// payload itself doesn't repeat the model field.
	mu          sync.Mutex
	modelByNode map[string]string
}

func newMetricsEmitter(inner model.EventEmitter, reg *metrics.Registry) *metricsEmitter {
	return &metricsEmitter{inner: inner, reg: reg, modelByNode: make(map[string]string)}
}

func (m *metricsEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	m.observe(evt)
	return m.inner.AppendEvent(ctx, runID, evt)
}

func (m *metricsEmitter) observe(evt store.Event) {
	switch evt.Type {
	case store.EventLLMRequest:
		if model, _ := evt.Data["model"].(string); model != "" && evt.NodeID != "" {
			m.mu.Lock()
			m.modelByNode[evt.NodeID] = model
			m.mu.Unlock()
		}
	case store.EventLLMStepFinished:
		modelName := m.lookupModel(evt.NodeID)
		if modelName == "" {
			modelName = "unknown"
		}
		const backend = "claw"
		m.addTokens(backend, modelName, "input", evt.Data["input_tokens"])
		m.addTokens(backend, modelName, "output", evt.Data["output_tokens"])
		m.addTokens(backend, modelName, "cache_read", evt.Data["cache_read_tokens"])
		m.addTokens(backend, modelName, "cache_write", evt.Data["cache_write_tokens"])
	case store.EventDelegateFinished:
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		// Delegate events report a single aggregated token count;
		// label as input so a sum across directions stays meaningful.
		m.addTokens(backend, m.lookupModel(evt.NodeID), "input", evt.Data["tokens"])
	}
}

func (m *metricsEmitter) lookupModel(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelByNode[nodeID]
}

func (m *metricsEmitter) addTokens(backend, modelName, direction string, raw interface{}) {
	n := toFloat(raw)
	if n <= 0 || backend == "" {
		return
	}
	if modelName == "" {
		modelName = "unknown"
	}
	m.reg.LLMTokensTotal.WithLabelValues(backend, modelName, direction).Add(n)
}

// toFloat coerces the JSON-decoded scalar (always float64 in Go's
// encoding/json) to a non-negative float64, returning 0 when the
// value is missing, nil, or not a number.
func toFloat(raw interface{}) float64 {
	switch v := raw.(type) {
	case float64:
		if v < 0 {
			return 0
		}
		return v
	case int:
		if v < 0 {
			return 0
		}
		return float64(v)
	case int64:
		if v < 0 {
			return 0
		}
		return float64(v)
	}
	return 0
}

// stringifyVars converts the wire payload's free-form vars into the
// string-typed map the executor expects. Non-string scalars are
// formatted with %v; nested structures are JSON-encoded so the
// downstream template engine can still see them.
func stringifyVars(in map[string]interface{}) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
			out[k] = ""
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out
}
