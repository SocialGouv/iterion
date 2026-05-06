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
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Config is the runner bootstrap.
type Config struct {
	NATS              *natsq.Conn
	Store             store.RunStore
	RunnerID          string
	WorkDir           string        // base directory for per-run workspaces
	HeartbeatInterval time.Duration // how often to refresh the NATS KV lease
	FetchWait         time.Duration // long-poll wait per fetch
	Logger            *iterlog.Logger
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

	// Inherit the publisher's trace so OTel spans created by the
	// engine appear under the originating editor span (plan §F T-41).
	traced := delivery.PropagateTraceTo(parent)
	runCtx, runCancel := context.WithCancel(traced)
	defer runCancel()

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
		_ = delivery.Ack()
		return
	}

	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		if errors.Is(err, natsq.ErrLockHeld) {
			logger.Warn("runner: lock held for %s — naking for sibling", msg.RunID)
			_ = delivery.Nak()
			return
		}
		logger.Error("runner: lock %s: %v", msg.RunID, err)
		_ = delivery.Nak()
		return
	}
	defer func() { _ = lock.Unlock() }()

	// Heartbeat goroutine: refresh the NATS lease while we own it.
	// On refresh failure the heartbeat cancels runCtx so engine.Run
	// unwinds via handleContextDoneWithCheckpoint — better to lose
	// progress than to let the lease expire while the engine is still
	// writing to Mongo (which would invite split-brain when JetStream
	// redelivers to a sibling pod).
	hbDone := make(chan struct{})
	go r.heartbeat(runCtx, runCancel, lock, hbDone)
	defer func() { <-hbDone }()

	if err := r.executeRun(runCtx, msg); err != nil {
		// Distinguish transient (resumable) vs terminal failures.
		// runtime.ErrRunPaused / ErrRunCancelled are not "the
		// delivery failed" — they're successful checkpoint writes
		// and we ack accordingly.
		if errors.Is(err, runtime.ErrRunPaused) || errors.Is(err, runtime.ErrRunCancelled) {
			logger.Info("runner: run %s checkpointed (%v)", msg.RunID, err)
			_ = delivery.Ack()
			return
		}
		// Other errors → nak so JetStream redelivers up to MaxDeliver.
		logger.Error("runner: run %s execution failed: %v", msg.RunID, err)
		_ = delivery.Nak()
		return
	}

	logger.Info("runner: run %s completed", msg.RunID)
	_ = delivery.Ack()
}

// heartbeat refreshes the NATS KV lease so a long-running run keeps
// holding the lock past the 60s default TTL. Returns when ctx is
// cancelled (run finished). On refresh failure the heartbeat triggers
// runCancel so the engine unwinds proactively before the lease expires
// — without that signal, the lease would silently lapse and JetStream
// would redeliver to a sibling pod, two writers ending up on the same
// run state. Better to abort cleanly and re-deliver the original
// message via Nak.
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelFunc, lock store.RunLock, done chan<- struct{}) {
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
				r.cfg.Logger.Error("runner: heartbeat refresh failed: %v — cancelling run to avoid split-brain", err)
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

	executor, err := buildExecutor(ctx, msg, wf, r.cfg.Store, r.cfg.Logger, r.cfg.WorkDir)
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

	if msg.Resume != nil {
		return engine.Resume(ctx, msg.RunID, msg.Resume.Answers)
	}
	return engine.Run(ctx, msg.RunID, msg.Vars)
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
func buildExecutor(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow, st store.RunStore, logger *iterlog.Logger, storeDir string) (runtime.NodeExecutor, error) {
	emitter, ok := st.(model.EventEmitter)
	if !ok {
		return nil, fmt.Errorf("runner: store does not satisfy model.EventEmitter")
	}
	vars := stringifyVars(msg.Vars)
	return runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: wf,
		Vars:     vars,
		Store:    emitter,
		RunID:    msg.RunID,
		Logger:   logger,
		StoreDir: storeDir,
	})
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
