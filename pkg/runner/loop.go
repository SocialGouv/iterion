// Package runner implements the cloud-mode iterion runner pod. It
// pulls RunMessages from the NATS JetStream queue, claims a
// distributed lease, hydrates the workflow IR, and executes runs
// against the Mongo+S3 store.
//
// One runner pod handles one in-flight run at a time (its fetch loop
// is sequential; the shared consumer's MaxAckPending caps fleet-wide
// in-flight deliveries); horizontal scale
// comes from spawning more pods (KEDA scales on lag — see plan §F
// T-36 runner-keda-scaledobject.yaml).
//
// Cloud-ready plan §F (T-27, T-28, T-29).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/notify"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
)

const tracerName = "github.com/SocialGouv/iterion/pkg/runner"

// logDeliveryErr surfaces failures from delivery state transitions
// (Ack / Nak / Term) that the caller can't propagate. A missed Term
// leaves a malformed or forged message looping in the queue; a missed
// Nak leaves a transient failure stuck until ack-wait; a missed Ack
// can cause a successful run to redeliver. Without surfacing these,
// the operator has no breadcrumb to chase.
func logDeliveryErr(logger *iterlog.Logger, op, runID string, err error) {
	if err == nil {
		return
	}
	logger.Warn("runner: %s for %s: %v", op, runID, err)
}

// ackTerminal performs delivery.Ack and surfaces the error via
// logDeliveryErr. The triple `logDeliveryErr(logger, op, runID,
// delivery.Ack())` recurs on every ack-and-return path in processOne;
// this helper is the single point that pairs the action with its
// breadcrumb.
func ackTerminal(logger *iterlog.Logger, delivery *natsq.Delivery, op, runID string) {
	logDeliveryErr(logger, op, runID, delivery.Ack())
}

// nakTerminal performs delivery.Nak and surfaces the error via
// logDeliveryErr. Use for transient failures where JetStream
// redelivery is the safety net (lock held, store transient, heartbeat
// loss, generic engine failure).
func nakTerminal(logger *iterlog.Logger, delivery *natsq.Delivery, op, runID string) {
	logDeliveryErr(logger, op, runID, delivery.Nak())
}

// termTerminal performs delivery.Term and surfaces the error via
// logDeliveryErr. Use for poisoned/forged messages whose redelivery
// would loop forever (decode failure, run-not-found, tenant
// mismatch, DLQ-parked).
func termTerminal(logger *iterlog.Logger, delivery *natsq.Delivery, op, runID string) {
	logDeliveryErr(logger, op, runID, delivery.Term())
}

// deliveryAction selects which JetStream state transition (Ack / Nak
// / Term) a terminal outcome warrants.
type deliveryAction int

const (
	actionAck deliveryAction = iota
	actionNak
	actionTerm
)

// logLevel mirrors the three log channels processOne uses for its
// terminal messages so a returned outcome carries enough metadata for
// the caller to log without the helper itself touching a logger.
type logLevel int

const (
	logInfo logLevel = iota
	logWarn
	logError
)

// preconditionOutcome describes the result of the pre-execution
// gauntlet (decode + pre-lock LoadRun + tenant validation). When
// proceed is true the caller continues to lock + execute with the
// loaded preRun; otherwise action + finalStatus + op tell the caller
// which terminal transition to perform on the delivery.
type preconditionOutcome struct {
	proceed     bool
	preRun      *store.Run
	finalStatus string
	op          string // for logDeliveryErr
	action      deliveryAction
	level       logLevel
	logFmt      string
	logArgs     []any
}

// execOutcome describes the result of classifying engine.Run's
// error (or success). The caller takes action based on it. The
// terminal-DLQ branch is intentionally NOT modelled here because it
// has side effects (PublishDLQ + UpdateRunStatusIf); the caller
// inspects the trigger conditions before invoking classifyExecResult.
type execOutcome struct {
	finalStatus string
	op          string
	action      deliveryAction
	level       logLevel
	logFmt      string
	logArgs     []any
}

// resolveDeliveryPreconditions runs the pre-lock store gauntlet:
// LoadRun (with its own short detached timeout context so a runner
// shutdown can't terminate a live delivery) and the status switch
// (which may mutate msg.Resume to convert a redelivered launch into
// a resume). It performs no Ack/Nak/Term and installs no defer — the
// caller acts on the returned outcome.
//
// When outcome.proceed is true the caller continues with outcome.preRun
// (the loaded run document, guaranteed non-nil). Otherwise the caller
// logs outcome.{level,logFmt,logArgs} and invokes the corresponding
// {ack,nak,term}Terminal with outcome.{op, finalStatus} on the
// delivery before returning.
//
// Tenant validation is intentionally OUT of this helper: the failed-
// Term log message for a tenant mismatch is a security-shaped alarm
// (different message + ERROR level) that processOne raises inline so
// the helper stays focused on the routine status machinery.
func (r *Runner) resolveDeliveryPreconditions(msg *queue.RunMessage) preconditionOutcome {
	// Detach from runCtx: it descends from r.cancel(), so a Shutdown
	// firing between SubscribeCancel and this LoadRun would yield
	// context.Canceled and Term a live delivery. The detached ctx
	// still carries tenant identity for the store filter.
	loadCtx, loadCancel := context.WithTimeout(
		store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID),
		5*time.Second)
	preRun, preErr := r.cfg.Store.LoadRun(loadCtx, msg.RunID)
	loadCancel()

	// Transient store errors (timeout) get Nak'd so the message
	// redelivers to a healthier runner; only persistent NotFound /
	// forged-message shapes warrant a Term.
	if preErr != nil && (errors.Is(preErr, context.DeadlineExceeded) || errors.Is(preErr, context.Canceled)) {
		return preconditionOutcome{
			finalStatus: "store_load_transient",
			op:          "nak-store-load-transient",
			action:      actionNak,
			level:       logWarn,
			logFmt:      "runner: pre-lock LoadRun %s transient: %v — naking",
			logArgs:     []any{msg.RunID, preErr},
		}
	}
	// A message whose run document we can't load is unsafe to execute:
	// either the publisher's SaveRun never landed (orphan publish), the
	// run was deleted out from under us, or the message is forged /
	// replayed against a runID that never belonged to this control plane.
	// In every case, terming the delivery is the conservative call —
	// re-delivery would just hit the same NotFound on the next runner.
	if preErr != nil || preRun == nil {
		return preconditionOutcome{
			finalStatus: "store_load_failed",
			op:          "term-store-load-failed",
			action:      actionTerm,
			level:       logError,
			logFmt:      "runner: run %s not found in store (err=%v) — terming",
			logArgs:     []any{msg.RunID, preErr},
		}
	}
	// Redelivered launch messages can arrive after the first attempt
	// already persisted resumable state (failed_resumable,
	// paused_operator, or cancellation-with-checkpoint during shutdown).
	// Re-running them through Engine.Run would be a poison loop because
	// runResolveDoc refuses to restart non-queued statuses; convert the
	// in-memory dispatch to Resume so JetStream redelivery actually uses
	// the checkpoint it exists to protect. A pre-pickup user-cancelled run
	// has no checkpoint and remains a stale delivery to ack/drop.
	switch preRun.Status {
	case store.RunStatusCancelled:
		if preRun.Checkpoint == nil {
			return preconditionOutcome{
				finalStatus: "cancelled",
				op:          "ack-already-cancelled",
				action:      actionAck,
				level:       logInfo,
				logFmt:      "runner: run %s already cancelled — skipping",
				logArgs:     []any{msg.RunID},
			}
		}
		if msg.Resume == nil {
			// We mutate msg.Resume here so the executor takes the
			// resume path; caller still uses outcome.preRun unchanged.
			msg.Resume = &queue.ResumeSpec{}
			return preconditionOutcome{
				proceed: true,
				preRun:  preRun,
				level:   logInfo,
				logFmt:  "runner: run %s redelivered after cancellation checkpoint — resuming",
				logArgs: []any{msg.RunID},
			}
		}
	case store.RunStatusFailedResumable, store.RunStatusPausedOperator:
		if msg.Resume == nil {
			msg.Resume = &queue.ResumeSpec{}
			return preconditionOutcome{
				proceed: true,
				preRun:  preRun,
				level:   logInfo,
				logFmt:  "runner: run %s redelivered in status %s — resuming",
				logArgs: []any{msg.RunID, preRun.Status},
			}
		}
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusPausedWaitingHuman:
		return preconditionOutcome{
			finalStatus: string(preRun.Status),
			op:          "ack-stale-status",
			action:      actionAck,
			level:       logInfo,
			logFmt:      "runner: run %s already in status %s — dropping stale delivery",
			logArgs:     []any{msg.RunID, preRun.Status},
		}
	}
	return preconditionOutcome{proceed: true, preRun: preRun}
}

// classifyExecResult turns engine.Run's (success-or-error) outcome
// into a terminal delivery decision. Pure: no I/O, no defer, no log
// call — the caller logs outcome.{level,logFmt,logArgs} and invokes
// the matching {ack,nak,term}Terminal.
//
// The DLQ branch (NumDelivered >= MaxDeliver) is NOT handled here
// because it has side effects (PublishDLQ + UpdateRunStatusIf). The
// caller checks that trigger inline BEFORE delegating the remaining
// generic-error case to this helper.
func classifyExecResult(execErr error, hbFailed bool, parentErr error, runID string) execOutcome {
	if execErr == nil {
		return execOutcome{
			finalStatus: "finished",
			op:          "ack-finished",
			action:      actionAck,
			level:       logInfo,
			logFmt:      "runner: run %s completed",
			logArgs:     []any{runID},
		}
	}
	// Distinguish transient (resumable) vs terminal failures.
	// runtime.ErrRunPaused / ErrRunCancelled are not "the
	// delivery failed" — they're successful checkpoint writes
	// and we ack accordingly.
	if errors.Is(execErr, runtime.ErrRunPaused) {
		return execOutcome{
			finalStatus: "paused",
			op:          "ack-paused",
			action:      actionAck,
			level:       logInfo,
			logFmt:      "runner: run %s checkpointed (%v)",
			logArgs:     []any{runID, execErr},
		}
	}
	if errors.Is(execErr, runtime.ErrRunPausedOperator) {
		return execOutcome{
			finalStatus: "paused_operator",
			op:          "ack-paused-operator",
			action:      actionAck,
			level:       logInfo,
			logFmt:      "runner: run %s operator-paused (%v)",
			logArgs:     []any{runID, execErr},
		}
	}
	if errors.Is(execErr, runtime.ErrRunCancelled) {
		// Heartbeat-induced cancel: the lease is gone, so a sibling
		// runner is free to pick this up via JetStream redelivery.
		// Nak (not Ack) so the message stays queued instead of
		// being marked done and forcing manual user intervention.
		if hbFailed {
			return execOutcome{
				finalStatus: "lock_held",
				op:          "nak-heartbeat-lost",
				action:      actionNak,
				level:       logWarn,
				logFmt:      "runner: run %s heartbeat lost — naking for sibling redelivery",
				logArgs:     []any{runID},
			}
		}
		if parentErr != nil {
			return execOutcome{
				finalStatus: "shutdown",
				op:          "nak-shutdown-cancelled",
				action:      actionNak,
				level:       logWarn,
				logFmt:      "runner: run %s interrupted by runner shutdown — naking for checkpoint resume",
				logArgs:     []any{runID},
			}
		}
		return execOutcome{
			finalStatus: "cancelled",
			op:          "ack-cancelled",
			action:      actionAck,
			level:       logInfo,
			logFmt:      "runner: run %s checkpointed (%v)",
			logArgs:     []any{runID, execErr},
		}
	}
	// Generic error → caller checks DLQ trigger before falling back to
	// the plain-nak outcome below.
	return execOutcome{
		finalStatus: "failed",
		op:          "nak-exec-failed",
		action:      actionNak,
		level:       logError,
		logFmt:      "runner: run %s execution failed: %v",
		logArgs:     []any{runID, execErr},
	}
}

// logAt routes a pre-formatted log triple (level, fmt, args) to the
// matching Logger channel. Used by processOne to drain the log
// metadata carried in preconditionOutcome / execOutcome.
func logAt(logger *iterlog.Logger, level logLevel, format string, args ...any) {
	if format == "" {
		return
	}
	switch level {
	case logInfo:
		logger.Info(format, args...)
	case logWarn:
		logger.Warn(format, args...)
	case logError:
		logger.Error(format, args...)
	}
}

// dispatchTerminal performs the JetStream state transition selected
// by `action` and surfaces any error via logDeliveryErr.
func dispatchTerminal(logger *iterlog.Logger, delivery *natsq.Delivery, action deliveryAction, op, runID string) {
	switch action {
	case actionAck:
		ackTerminal(logger, delivery, op, runID)
	case actionNak:
		nakTerminal(logger, delivery, op, runID)
	case actionTerm:
		termTerminal(logger, delivery, op, runID)
	}
}

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
	// MemoryStore, when non-nil, backs the agents' workspace-memory
	// tools with a shared store (the cloud Mongo store) instead of the
	// pod's ephemeral filesystem. nil → local filesystem memory.
	MemoryStore knowledge.MemoryStore
	// Metrics, when non-nil, receives counters/gauges updates from the
	// runner loop (in-flight runs, durations, heartbeat errors, NATS
	// queue depth, LLM token usage). Nil-safe: passing nil disables
	// metrics emission without changing the loop's behaviour, useful
	// for unit tests and the local-mode dev runner.
	Metrics *metrics.Registry

	// RunSecrets + Sealer carry the BYOK / OAuth bundle the
	// publisher pre-resolved. Both nil → runner falls back to env
	// vars at the LLM call site.
	RunSecrets secrets.RunSecretsStore
	Sealer     secrets.Sealer

	// GenericSecrets, when non-nil, is the tenant generic-secret store
	// (same Mongo DB, same Sealer). It powers the mid-run refresh of
	// materialised file secrets: the bundle's value is a launch-time
	// snapshot, so a short-TTL credential (1h GitHub App installation
	// token) dies under a long run — the server-side refresh worker
	// keeps the STORE record fresh, and the runner re-reads it via
	// RunBundle.GenericSecretRefs. nil → no refresh (snapshot only).
	GenericSecrets secrets.GenericSecretStore

	// OrgUsage, when non-nil, receives each run's accumulated LLM
	// cost/tokens into the org's monthly bucket at the end of every
	// execution attempt (the billing source of truth — Prometheus
	// counters above stay tenant-unlabelled). nil → no org metering.
	OrgUsage orgusage.Counter

	// BotsPaths is where bot bundles are resolved from (the image ships
	// the catalog at /opt/iterion/bots via ITERION_BOTS_PATH). A run
	// carrying a BotID gets its bundle wired into the engine so the
	// bundle's skills/ are mirrored into <workspace>/.claude/skills —
	// without it, system prompts referencing `.claude/skills/<x>.md`
	// point at nothing in cloud runs. Empty → no bundle resolution.
	BotsPaths []string

	// SandboxDefault / SandboxHostState carry the operator's
	// ITERION_SANDBOX_DEFAULT / ITERION_SANDBOX_HOST_STATE (cfg.Sandbox.*) into
	// the engine. Without this the cloud runner read them (config/env.go) then
	// dropped them: a bot's `sandbox: auto` on the kubernetes driver hard-errored
	// on host_state=auto (no host fs to bind) even with
	// ITERION_SANDBOX_HOST_STATE=none set, because the runner never wired the
	// value the way pkg/cli/run.go does for `iterion run`.
	SandboxDefault   string
	SandboxHostState string

	// SandboxOverride carries ITERION_SANDBOX_OVERRIDE (cfg.Sandbox.Override)
	// into the engine at CLI-override strength, where "none" beats a
	// workflow's inline `sandbox:` block. Set to "none" on runners that are
	// themselves the isolation boundary (a k8s runner pod shipping its own
	// toolchain): a bot's sandbox block — written for local runs — must not
	// spawn a sibling sandbox pod there. SandboxDefault cannot express this
	// (a workflow block outranks the default tier).
	SandboxOverride string
}

// Runner is the long-running consumer loop.
type Runner struct {
	cfg      Config
	consumer *natsq.Consumer

	// completionNotifier POSTs a run-completion webhook when a run
	// carrying a callback URL reaches a terminal state. Built in New;
	// no-op unless the run requested a callback.
	completionNotifier *notify.Notifier

	mu      sync.Mutex
	current *inFlight          // non-nil while a run is being processed; guarded by mu
	cancel  context.CancelFunc // loop-context canceller installed by Run; guarded by mu

	// logWriters maps an in-flight run to its RunLogStore batching
	// writer so the store's LogPositionFn hook (logWriterTotal) can
	// stamp Event.LogOffset. See runlog_writer.go.
	logWritersMu sync.Mutex
	logWriters   map[string]*runLogWriter

	// runEngines maps an in-flight run to its Engine so the store's
	// ActiveDurationFn hook (activeDurationTotal) can stamp
	// Event.ActiveMs with the run's monotonic SharedBudget elapsed —
	// the cloud twin of the runview Service wiring. Guarded by its own
	// mutex (the engine is registered slightly later than the log
	// writer, in processOne after construction). See runlog_writer.go.
	runEnginesMu sync.Mutex
	runEngines   map[string]*runtime.Engine

	// ssrfPinUnavailableOnce demotes the expected, permanent "hosts file not
	// writable" SSRF IP-pin condition (non-root runner + kubelet-managed
	// /etc/hosts) to a single info log for the runner's lifetime, instead of a
	// per-clone warn that trains operators to ignore warns. See prepareRepoWorkspace.
	ssrfPinUnavailableOnce sync.Once
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
	// Run-completion webhook notifier. ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE=1
	// relaxes the SSRF guard for self-hosted deployments whose callback
	// receiver lives on a private network alongside the runner; off by
	// default (cloud runners must not gateway into a private network).
	allowPrivate := os.Getenv("ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE") == "1"
	// ITERION_COMPLETION_WEBHOOK_SECRET, when set, HMAC-signs every
	// outbound payload (X-Iterion-Signature) so receivers can
	// authenticate the delivery. Empty = unsigned.
	secret := os.Getenv("ITERION_COMPLETION_WEBHOOK_SECRET")
	notifier := notify.New(cfg.Logger, 0,
		notify.WithAllowPrivate(allowPrivate),
		notify.WithSigningSecret(secret))
	return &Runner{cfg: cfg, consumer: cons, completionNotifier: notifier}, nil
}

// Run drains the queue until ctx is cancelled. Each iteration fetches
// one message, processes it synchronously, and acks (or naks/terms
// on failure). Returns ctx.Err() when shut down cleanly.
func (r *Runner) Run(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	// Publish the loop canceller under mu: Shutdown reads it from another
	// goroutine, so an unsynchronised write here is a data race (and the
	// Go memory model permits Shutdown to observe a stale nil and silently
	// skip cancelling the loop, defeating the graceful drain).
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
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
	// K8s sandbox reaper (ADR-070): at boot + on a ticker, a healthy
	// runner force-deletes the orphaned sandbox pod + both Secrets +
	// NetworkPolicy of any run no longer held by a NATS lease. This is
	// the managed-cloud counterpart to the runview.Service reaper, which
	// never fires in the cloud (lock-less store + no runview.Service) —
	// closing the OOM-with-surviving-pod plaintext-credential leak that
	// the ownerReference cascade misses (the pod UID survives a container
	// restart). No-op when not in-cluster / no NATS. See reaper.go.
	go r.runSandboxReaper(loopCtx)
	// Stamp Event.LogOffset from the per-run log writer on stores that
	// support the hook (mongo + filesystem both do) — the cloud twin of
	// the runview Service wiring, powering per-node log slicing.
	if setter, ok := r.cfg.Store.(logPositionSetter); ok {
		setter.SetLogPositionFn(r.logWriterTotal)
	}
	// Same seam: stamp Event.ActiveMs from the run's monotonic
	// SharedBudget elapsed so the studio active timer excludes OS-suspend.
	if setter, ok := r.cfg.Store.(activeDurationSetter); ok {
		setter.SetActiveDurationFn(r.activeDurationTotal)
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
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cur == nil {
		return nil
	}
	// Cancel the in-flight context so engine.Run unwinds via
	// handleContextDoneWithCheckpoint (preserving the checkpoint),
	// and extend the ack window while we wait for it to finish.
	cur.cancelFn()
	logDeliveryErr(r.cfg.Logger, "in-progress-shutdown", cur.runID, cur.delivery.InProgress())
	select {
	case <-cur.done:
		// processOne already Ack'd (paused/cancelled checkpoint) or
		// Nak'd (transient failure) the delivery. Nothing more to do.
		r.cfg.Logger.Info("runner: in-flight run %s drained during shutdown", cur.runID)
	case <-ctx.Done():
		// Grace period expired before the engine finished checkpointing.
		// Best-effort Nak so JetStream redelivers to a sibling pod.
		logDeliveryErr(r.cfg.Logger, "nak-shutdown-grace", cur.runID, cur.delivery.Nak())
		r.cfg.Logger.Warn("runner: shutdown grace expired for run %s — naking for redelivery", cur.runID)
	}
	return nil
}

// processOne validates, locks, executes a single delivery. The
// per-run context inherits from the runner's loop context so
// shutdown unwinds cleanly via handleContextDoneWithCheckpoint
// (preserving the checkpoint for resume).
func (r *Runner) processOne(parent context.Context, delivery *natsq.Delivery) {
	msg, ok := r.decodeOrTerm(delivery)
	if !ok {
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

	spanCtx, span := r.startProcessSpan(parent, delivery, msg)
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
	// ack the JetStream delivery without doing any work.
	pre := r.resolveDeliveryPreconditions(msg)
	logAt(logger, pre.level, pre.logFmt, pre.logArgs...)
	if !pre.proceed {
		finalStatus = pre.finalStatus
		dispatchTerminal(logger, delivery, pre.action, pre.op, msg.RunID)
		return
	}

	if !r.verifyTenantOrTerm(pre, msg, delivery, logger) {
		finalStatus = "tenant_mismatch"
		return
	}

	lock, lockOK, lockStatus := r.acquireRunLock(runCtx, msg, delivery, logger)
	if !lockOK {
		finalStatus = lockStatus
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
	go r.heartbeat(runCtx, runCancel, lock, delivery, hbDone, &hbFailed)
	// Cancel runCtx *before* waiting on hbDone, otherwise we deadlock:
	// heartbeat only exits on ctx.Done(), and the outer `defer
	// runCancel()` at function entry is LIFO-last so it would run
	// after this defer. Calling runCancel() here is idempotent. Kept as
	// a panic-safety net even though the happy path drains the heartbeat
	// explicitly below.
	defer func() {
		runCancel()
		<-hbDone
	}()

	err := r.executeRun(runCtx, msg)
	// Stop the heartbeat before finalizing (Ack/Nak) the delivery. The
	// heartbeat issues periodic InProgress() on this same delivery to
	// hold the JetStream ack deadline open; draining it here guarantees
	// no InProgress() lands after the terminal Ack/Nak below (which would
	// otherwise log a spurious already-acked error). A second drain in
	// the defer above is a no-op on the closed channel.
	runCancel()
	<-hbDone

	r.fireCompletionNotifier(msg)

	if handled, dlqStatus := r.parkOnDLQOnFinalDelivery(err, delivery, msg, logger); handled {
		finalStatus = dlqStatus
		return
	}

	outcome := classifyExecResult(err, hbFailed.Load(), parent.Err(), msg.RunID)
	logAt(logger, outcome.level, outcome.logFmt, outcome.logArgs...)
	finalStatus = outcome.finalStatus
	dispatchTerminal(logger, delivery, outcome.action, outcome.op, msg.RunID)
}

// decodeOrTerm decodes the delivery payload into a queue.RunMessage. On
// decode failure it Terms the delivery so the malformed message doesn't
// loop in JetStream, surfacing a failed-Term at WARN so the operator
// can purge rather than chase a silent loop. Returns (msg, true) on
// success; (nil, false) when the caller must abandon the delivery.
func (r *Runner) decodeOrTerm(delivery *natsq.Delivery) (*queue.RunMessage, bool) {
	msg, err := delivery.Decode()
	if err != nil {
		r.cfg.Logger.Error("runner: decode delivery: %v", err)
		if termErr := delivery.Term(); termErr != nil {
			// A failed Term leaves the malformed message in the queue
			// where it will be redelivered and fail decode again on
			// every runner — surface it so the operator can purge
			// rather than chase a silent loop.
			r.cfg.Logger.Warn("runner: term after decode failure: %v", termErr)
		}
		return nil, false
	}
	return msg, true
}

// startProcessSpan builds the runner-side OTel root span for this
// delivery. It inherits the publisher's trace (so engine spans hang off
// the originating studio span — plan §F T-41), stamps tenant + owner on
// the context (so every downstream Mongo write picks them up and every
// read stays scoped to the run's tenant — re-validated against the
// loaded run doc below), and starts the iterion.runner.process_one
// span. The span ends in a deferred block in processOne; finalStatus is
// set at every exit path.
func (r *Runner) startProcessSpan(parent context.Context, delivery *natsq.Delivery, msg *queue.RunMessage) (context.Context, trace.Span) {
	// Inherit the publisher's trace so OTel spans created by the
	// engine appear under the originating studio span (plan §F T-41).
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
	return otel.Tracer(tracerName).Start(traced, "iterion.runner.process_one",
		trace.WithAttributes(
			attribute.String("iterion.run_id", msg.RunID),
			attribute.String("iterion.workflow_name", msg.WorkflowName),
			attribute.String("iterion.workflow_hash", msg.WorkflowHash),
			attribute.String("iterion.tenant_id", msg.TenantID),
		),
	)
}

// verifyTenantOrTerm refuses a delivery whose message tenant doesn't
// match the persisted run document. A mismatch implies either a
// corrupted publish (publisher stamped the wrong tenant) or a malicious
// / replayed message; either way the run is unsafe to execute under
// either tenant's scope, so we Term the delivery to keep it from
// redelivering. Kept separate from resolveDeliveryPreconditions so the
// failed-Term log can carry a security-shaped ERROR-level alarm asking
// the operator to purge the JetStream subject manually — the generic
// logDeliveryErr breadcrumb wouldn't surface it. Returns true to proceed,
// false when the caller must abandon the delivery.
func (r *Runner) verifyTenantOrTerm(pre preconditionOutcome, msg *queue.RunMessage, delivery *natsq.Delivery, logger *iterlog.Logger) bool {
	if pre.preRun.TenantID == msg.TenantID {
		return true
	}
	logger.Error("runner: tenant mismatch for run %s (msg=%q stored=%q) — terming", msg.RunID, msg.TenantID, pre.preRun.TenantID)
	if termErr := delivery.Term(); termErr != nil {
		// HIGH-impact: a failed Term on a tenant-mismatched
		// message means a forged / replayed delivery stays in the
		// queue and JetStream will redeliver it, looping forever.
		// Surface loudly so the operator can purge the stream.
		logger.Error("runner: term for %s after tenant mismatch FAILED (%v) — message will redeliver; purge the JetStream subject manually", msg.RunID, termErr)
	}
	return false
}

// acquireRunLock claims the distributed run lock guarding against two
// runners executing the same run. ErrLockHeld means a sibling already
// has it — Nak so the sibling retains exclusive ownership; any other
// lock error is a transient-store shape and also Nak'd. Returns
// (lock, true, "") on success; (nil, false, finalStatus) when the caller
// must abandon the delivery (finalStatus is the metric label).
func (r *Runner) acquireRunLock(runCtx context.Context, msg *queue.RunMessage, delivery *natsq.Delivery, logger *iterlog.Logger) (store.RunLock, bool, string) {
	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		if errors.Is(err, natsq.ErrLockHeld) {
			logger.Warn("runner: lock held for %s — naking for sibling", msg.RunID)
			nakTerminal(logger, delivery, "nak-lock-held", msg.RunID)
			return nil, false, "lock_held"
		}
		logger.Error("runner: lock %s: %v", msg.RunID, err)
		nakTerminal(logger, delivery, "nak-lock-error", msg.RunID)
		return nil, false, "failed"
	}
	return lock, true, ""
}

// fireCompletionNotifier fires the run-completion webhook (no-op unless
// the run carries a callback URL). FireForRun re-reads the persisted run
// and gates on the terminal status via shouldNotify — paused runs (the
// run is not done, just waiting) are filtered there, so no error-type
// guard is needed here. The resume that actually terminates fires it
// then. Mirrors runview.spawnRun's in-process fire (same shouldNotify
// authority) so cloud and local behave identically.
func (r *Runner) fireCompletionNotifier(msg *queue.RunMessage) {
	if r.completionNotifier == nil {
		return
	}
	nctx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	r.completionNotifier.FireForRun(nctx, r.cfg.Store, msg.RunID)
}

// parkOnDLQOnFinalDelivery handles the DLQ branch: a generic engine
// error on the LAST permitted JetStream attempt must park a copy on the
// DLQ and Term instead of Nak — without the bridge, JetStream silently
// drops the message after MaxDeliver and the run is unrecoverable except
// by hand. Handled here (not via classifyExecResult) because it has side
// effects (PublishDLQ + UpdateRunStatusIf) and uses its own context with
// `defer cancel()`. Returns (true, finalStatus) when the caller must
// stop processing the delivery (DLQ dispatch already issued);
// (false, "") otherwise to fall through to classifyExecResult.
func (r *Runner) parkOnDLQOnFinalDelivery(err error, delivery *natsq.Delivery, msg *queue.RunMessage, logger *iterlog.Logger) (bool, string) {
	if err == nil ||
		errors.Is(err, runtime.ErrRunPaused) ||
		errors.Is(err, runtime.ErrRunPausedOperator) ||
		errors.Is(err, runtime.ErrRunCancelled) ||
		r.cfg.NATS == nil || delivery.NumDelivered() < r.cfg.NATS.MaxDeliver() {
		return false, ""
	}
	logger.Error("runner: run %s failed on final delivery %d/%d — parking on DLQ: %v",
		msg.RunID, delivery.NumDelivered(), r.cfg.NATS.MaxDeliver(), err)
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if perr := r.cfg.NATS.PublishDLQ(bg, delivery, err.Error()); perr != nil {
		// DLQ unavailable: keep the JetStream redelivery as
		// the only remaining safety net.
		logger.Error("runner: DLQ park for %s failed: %v — naking instead", msg.RunID, perr)
		nakTerminal(logger, delivery, "nak-dlq-failed", msg.RunID)
		return true, "dlq"
	}
	sctx := store.WithIdentity(bg, msg.TenantID, msg.OwnerID)
	if _, serr := r.cfg.Store.UpdateRunStatusIf(sctx, msg.RunID, store.RunStatusFailedResumable,
		fmt.Sprintf("max deliveries exhausted: %v (parked on DLQ — replay via /api/admin/dlq)", err),
		[]store.RunStatus{store.RunStatusRunning, store.RunStatusQueued}); serr != nil {
		logger.Warn("runner: DLQ status flip for %s: %v", msg.RunID, serr)
	}
	termTerminal(logger, delivery, "term-dlq-parked", msg.RunID)
	return true, "dlq"
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
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelFunc, lock store.RunLock, delivery *natsq.Delivery, done chan<- struct{}, hbFailed *atomic.Bool) {
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
			// Hold the JetStream ack deadline open. AckWait (5m default)
			// is far shorter than many real runs; without a periodic
			// InProgress() the broker redelivers the message to a sibling
			// and, after MaxDeliver attempts, drops it from the queue —
			// destroying the crash-recovery safety net while the run is
			// still healthy and head-of-line-blocking one of the consumer's
			// MaxAckPending slots. Best-effort: a transient miss is retried
			// on the next tick, well inside AckWait.
			if err := delivery.InProgress(); err != nil {
				r.cfg.Logger.Warn("runner: heartbeat InProgress failed: %v", err)
			}
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
	// Honour the publisher's per-run wall-clock budget. Without this,
	// queue.RunMessage.TimeoutSec — wired from `iterion run --timeout`
	// and the studio Launch modal — has no effect in cloud mode: the
	// runner ignores the field and the engine inherits an undeadlined
	// ctx. The DSL budget (max_duration) is still enforced inside the
	// engine; this guard catches the operator-level deadline.
	if msg.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(msg.TimeoutSec)*time.Second)
		defer cancel()
	}

	wf, err := loadWorkflow(ctx, msg, store.AsIRBlobStore(r.cfg.Store))
	if err != nil {
		return err
	}

	// Launch-time budget overrides (studio Launch modal / API `budget`
	// object). Applied BEFORE the cloud ceiling below so a tenant's
	// override can only lower the effective caps, never pierce the
	// platform ceiling.
	if err := applyBudgetOverrides(wf, msg.Budget, r.cfg.Logger); err != nil {
		return err
	}

	// Multitenant safeguard: clamp the workflow budget to the platform's hard
	// ceiling so a tenant's bot — however large its declared budget, and
	// especially an `as X(unbounded)` loop whose fuel falls back to
	// budget.MaxIterations — can never exceed what the platform allows. The
	// tenant cannot raise this; it only ever lowers (or, for an unbudgeted
	// bot, imposes) the limits. See ir.Budget.ClampToCeiling.
	applyCloudBudgetCeiling(wf, r.cfg.Logger)

	// Phase C: fetch + decrypt the per-run sealed credentials bundle when the
	// publisher attached one — BEFORE the repo clone + executor so all three
	// see the credentials in ctx. The result lives only in ctx; the runner
	// process itself stays clean of plaintext keys.
	ctx, cleanup, credErr := r.injectCredentials(ctx, msg)
	if credErr != nil {
		r.cfg.Logger.Warn("runner: credentials inject %s: %v (continuing without)", msg.RunID, credErr)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Workspace: an inbound webhook/repo-bound run carries RepoURL/RepoSHA —
	// clone it into a per-run dir (authed with the bound forge token for a
	// private repo) and point the engine there so ${PROJECT_DIR} is the repo
	// under review. Otherwise use the runner's base WorkDir.
	workDir := r.cfg.WorkDir
	// gitBase is the clone's HEAD before the workflow runs — the baseline the
	// per-run commit/file view is measured against. Captured here (while the
	// clone is on-disk) so recordRunGitMeta can persist the commit/file
	// metadata into the store before the pod's ephemeral workspace is wiped;
	// the server pod, which has no worktree, serves the panels from that.
	gitBase := ""
	if strings.TrimSpace(msg.RepoURL) != "" {
		repoDir, derr := r.prepareRepoWorkspace(ctx, msg)
		if derr != nil {
			return fmt.Errorf("runner: prepare repo workspace for %s: %w", msg.RunID, derr)
		}
		workDir = repoDir
		if head, herr := gitlib.RevParseHead(repoDir); herr == nil {
			gitBase = head
		} else {
			r.cfg.Logger.Warn("runner: run %s: capture git baseline: %v", msg.RunID, herr)
		}
		defer func() { _ = os.RemoveAll(repoDir) }()
	}

	// Resolve the MCP catalog (project .mcp.json + enabled-plugin servers)
	// against the run's workspace. loadWorkflow → ir.Compile only builds the
	// graph; it does NOT merge plugin/project MCP servers — that is
	// PrepareWorkflow's job, and the studio/CLI run paths call it via
	// runview.compileWith. The runner hydrates from a pre-compiled AST and so
	// must call it explicitly here, else wf.ResolvedMCPServers stays empty,
	// buildMCPManager returns a nil manager, and every `mcp.<server>.*`
	// wildcard resolves to zero tools (the firecrawl/repo-falcon plugins were
	// silently inert in cloud runs). Fail loudly on a malformed catalog rather
	// than run a bot missing the tools it declared.
	if err := mcp.PrepareWorkflow(wf, workDir); err != nil {
		return fmt.Errorf("runner: resolve MCP servers for %s: %w", msg.RunID, err)
	}

	// No-sandbox file secrets: a workflow with `as: file` secrets but no
	// sandbox (the noop driver can't mount) needs them materialized as 0600
	// files at their mount paths in the runner pod so the in-pod agent can
	// read them — e.g. review-pr's forge_token for glab. Removed on return.
	fileSecrets, rm, ferr := r.materializeFileSecretsNoSandbox(ctx, wf)
	if rm != nil {
		// Always schedule cleanup, even on error: materialize returns a
		// non-nil remover covering the files it wrote BEFORE failing, so
		// partial 0600 secret files (e.g. forge_token) don't leak on disk.
		defer rm()
	}
	if ferr != nil {
		r.cfg.Logger.Warn("runner: materialize file secrets %s: %v", msg.RunID, ferr)
	}
	// Mid-run refresh of materialised file secrets: the bundle value is a
	// launch-time snapshot and a GitHub App installation token lives 1h, so
	// a long (or redelivered) run would push/comment with a dead credential.
	// The server-side refresh worker keeps the STORE record fresh; re-read
	// it and rewrite the file so `cat` at use time gets a live token.
	if len(fileSecrets) > 0 && r.cfg.GenericSecrets != nil {
		if creds, ok := secrets.CredentialsFromContext(ctx); ok && len(creds.GenericRefs) > 0 {
			refreshCtx, stopRefresh := context.WithCancel(ctx)
			defer stopRefresh()
			go r.refreshFileSecretsLoop(refreshCtx, msg.TenantID, creds.GenericRefs, fileSecrets)
		}
	}

	// Isolate the forge CLI (glab/gh) auth config to a PER-RUN directory so a
	// bot's `glab auth login` / `gh auth login` can never leak its forge
	// identity to a LATER run on the same (reused) runner pod. Without this,
	// glab persists its token in $HOME/.config/glab-cli; a subsequent run
	// whose `glab auth status` reports "ok" (against the stale token) skips
	// re-login and posts under the PREVIOUS run's bot account — a cross-run /
	// cross-tenant forge-identity leak (observed live: a review summary posted
	// under a prior run's identity). The per-run forge_token FILE is already
	// isolated above; this isolates the CLI's own persisted auth. The delegate
	// subprocess inherits these from os.Environ(); no-op for sandboxed runs
	// (fresh container HOME). Safe because each runner pod processes one
	// run at a time (sequential fetch loop).
	if cliDir, derr := os.MkdirTemp("", "iterion-cli-"); derr == nil {
		prevGlab, hadGlab := os.LookupEnv("GLAB_CONFIG_DIR")
		prevGH, hadGH := os.LookupEnv("GH_CONFIG_DIR")
		_ = os.Setenv("GLAB_CONFIG_DIR", filepath.Join(cliDir, "glab"))
		_ = os.Setenv("GH_CONFIG_DIR", filepath.Join(cliDir, "gh"))
		defer func() {
			if hadGlab {
				_ = os.Setenv("GLAB_CONFIG_DIR", prevGlab)
			} else {
				_ = os.Unsetenv("GLAB_CONFIG_DIR")
			}
			if hadGH {
				_ = os.Setenv("GH_CONFIG_DIR", prevGH)
			} else {
				_ = os.Unsetenv("GH_CONFIG_DIR")
			}
			_ = os.RemoveAll(cliDir)
		}()
	} else {
		r.cfg.Logger.Warn("runner: isolate forge CLI config %s: %v", msg.RunID, derr)
	}

	// Per-run log persistence (ADR-053): tee the engine logger into the
	// store's RunLogStore so the server pod can live-stream and replay
	// the run's log without a shared filesystem. The offset seed makes a
	// resumed/redelivered run append after the persisted tail. Flushes
	// ride a background ctx carrying the run's tenant identity — NOT the
	// run ctx — so a cancelled run still flushes its final lines. Built
	// BEFORE the executor so the executor's agent-stream lines (the
	// `[node#iter/backend]` tool/LLM tags the studio's per-node Logs tab
	// filters on) persist too — not only the engine's node-level lines.
	runLogger := r.cfg.Logger
	if ls := store.AsRunLogStore(r.cfg.Store); ls != nil {
		idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
		seed, serr := ls.RunLogSize(idCtx, msg.RunID)
		if serr != nil {
			r.cfg.Logger.Warn("runner: run %s: seed log offset: %v — starting at 0", msg.RunID, serr)
		}
		w := newRunLogWriter(idCtx, ls, msg.RunID, seed, r.cfg.Logger)
		defer func() { _ = w.Close() }()
		r.registerLogWriter(msg.RunID, w)
		defer r.unregisterLogWriter(msg.RunID)
		runLogger = iterlog.New(r.cfg.Logger.Level(), io.MultiWriter(r.cfg.Logger.Writer(), w))
	}

	worktreeAuthoritySince := time.Now().UTC()
	executor, usage, err := r.buildExecutor(ctx, msg, wf, runLogger)
	if err != nil {
		return err
	}
	// Charge the org's monthly usage whatever the outcome — paused,
	// cancelled and failed attempts incurred real LLM spend.
	defer r.recordOrgSpend(msg, usage)

	engineOpts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithWorkflowHash(msg.WorkflowHash),
		runtime.WithWorkDir(workDir),
		runtime.WithWorktreeAuthoritySince(worktreeAuthoritySince),
		// Sandbox defaults from the operator config (ITERION_SANDBOX_DEFAULT /
		// ITERION_SANDBOX_HOST_STATE). pkg/cli/run.go wires these for `iterion
		// run`; the cloud runner must too — else cfg.Sandbox.* is read and
		// dropped and a bot's `sandbox: auto` hard-errors on the kubernetes
		// driver (host_state=auto has no host filesystem to bind).
		runtime.WithSandboxDefault(r.cfg.SandboxDefault),
		runtime.WithSandboxHostStateDefault(r.cfg.SandboxHostState),
		// CLI-strength override (ITERION_SANDBOX_OVERRIDE): "none" on a
		// runner that is itself the isolation boundary beats a bot's inline
		// sandbox block, so the run executes directly in the runner pod.
		runtime.WithSandboxOverride(r.cfg.SandboxOverride),
	}
	// Sandboxed counterpart of the no-sandbox file-secret refresh above: a
	// `sandbox: auto` run mounts file secrets as a launch-time snapshot
	// (docker bind-mount / k8s Secret), so the in-pod loop never starts and
	// a long run would push/comment with a dead token (#99, still open for
	// the common cloud path). Register an observer that, once the sandbox
	// Run is live, drives a refresh loop rewriting the in-container secret
	// when the store record rotates. No-op when the run has no refreshable
	// file secrets or the driver can't refresh.
	if refs := r.sandboxFileSecretRefs(ctx, wf); refs != nil {
		sbRefreshCtx, stopSbRefresh := context.WithCancel(ctx)
		defer stopSbRefresh()
		if obs := r.sandboxSecretRefreshObserver(sbRefreshCtx, msg.TenantID, refs); obs != nil {
			engineOpts = append(engineOpts, runtime.WithSandboxRunObserver(obs))
		}
	}
	// Bundle skills: a bot-qualified run mirrors its bundle's skills/ into
	// <workspace>/.claude/skills exactly like a local `iterion run
	// bots/<bot>` does (the engine's mirrorBundleSkills reads the bundle).
	// Best-effort: an unresolvable bot id or a loose .bot just skips the
	// mirror with a warning — the run proceeds without skills.
	if msg.BotID != "" && len(r.cfg.BotsPaths) > 0 {
		if mainFile, rerr := botregistry.ResolveBotPath(msg.BotID, r.cfg.BotsPaths); rerr == nil {
			if b, berr := bundle.OpenDir(filepath.Dir(mainFile)); berr == nil {
				engineOpts = append(engineOpts, runtime.WithBundle(b))
			} else {
				r.cfg.Logger.Warn("runner: bot %q bundle open: %v (skills not mirrored)", msg.BotID, berr)
			}
		} else {
			r.cfg.Logger.Warn("runner: bot %q not resolvable in %v (skills not mirrored)", msg.BotID, r.cfg.BotsPaths)
		}
	}
	if msg.Resume != nil && msg.Resume.Force {
		// Force-resume must be applied at engine construction so the
		// hash-mismatch guard in pkg/runtime/resume.go reads the flag.
		// This was previously dropped on the floor.
		engineOpts = append(engineOpts, runtime.WithForceResume(true))
	}
	engine := runtime.New(wf, r.cfg.Store, executor, engineOpts...)
	// Publish the engine so the store's Event.ActiveMs stamping reads
	// this run's monotonic active elapsed; drop it when the run returns.
	r.registerRunEngine(msg.RunID, engine)
	defer r.unregisterRunEngine(msg.RunID)

	var runErr error
	if msg.Resume != nil {
		runErr = engine.Resume(ctx, msg.RunID, msg.Resume.Answers)
	} else {
		runErr = engine.Run(ctx, msg.RunID, msg.Vars)
	}

	// Persist the run's git metadata (commits + modified files vs the
	// baseline) BEFORE the deferred `os.RemoveAll(repoDir)` wipes the clone.
	// The server pod has no worktree, so this snapshot is the only source
	// the Commits/Files panels have for a finished cloud run. Best-effort:
	// a recording failure must never change the run's outcome.
	if workDir != r.cfg.WorkDir {
		r.recordRunGitMeta(ctx, msg, workDir, gitBase)
	}

	// Upload any tool-produced artifact files (run reports, SBOMs) from
	// the runner-local scratch dir to the durable read backend (S3), so
	// the server pod's Artifacts panel can serve them for this finished
	// run. Best-effort: a recording failure must never change the run's
	// outcome. No-op for stores that don't need the bridge.
	r.uploadRunFiles(ctx, msg)

	// Delete the sealed credentials bundle only on a terminal-clean
	// outcome (success, or paused-for-resume). On every Nak-for-
	// redelivery path — a transient/generic engine error, or a
	// heartbeat-loss ErrRunCancelled — JetStream redelivers the SAME
	// message with the SAME SecretsRef, so the bundle MUST survive for
	// the retry. Deleting it here was the bug: the redelivered attempt
	// hit ErrRunSecretsNotFound, logged "continuing without", ran
	// credential-less and failed again, turning one transient blip into
	// a guaranteed MaxDeliver drop for every BYOK/OAuth run. Paused runs
	// Ack and resume via a FRESH SecretsRef (cloudpublisher.SubmitResume
	// re-seals — run_secrets.go documents "the runner deletes the record
	// on success"), so the old bundle is safe to drop. The store's 24h
	// TTL is the backstop for the user-cancel / terminal-fail paths we
	// intentionally skip here.
	if cleanup != nil && (runErr == nil || errors.Is(runErr, runtime.ErrRunPaused)) {
		r.deleteRunSecrets(msg)
	}
	return runErr
}

// recordRunGitMeta computes the run's commit/file metadata from the clone
// at workDir and persists it into the store, so the server pod can render
// the Commits/Files panels after this runner pod's workspace is gone (the
// cloud path where the live git inspection has no worktree to read). base
// is the clone HEAD captured before the run; workDir is the on-disk clone.
//
// Best-effort throughout: a non-git workDir, an empty range (no commits),
// or a store without the RunGitMetaStore seam all no-op cleanly. Never
// returns an error — the caller has already decided the run's outcome.
func (r *Runner) recordRunGitMeta(ctx context.Context, msg *queue.RunMessage, workDir, base string) {
	gs := store.AsRunGitMetaStore(r.cfg.Store)
	if gs == nil {
		return
	}
	if base == "" {
		// Baseline capture failed earlier (already warned). Persisting a
		// snapshot with no range would serve a CONFIDENT "no commits" for a
		// run that may well have committed — worse than the panel reporting
		// the metadata unavailable. Skip.
		return
	}
	meta, err := store.BuildRunGitMeta(workDir, base)
	if err != nil {
		if !errors.Is(err, gitlib.ErrNotGitRepo) {
			r.cfg.Logger.Warn("runner: run %s: build git meta: %v", msg.RunID, err)
		}
		return
	}
	// Flush on a background ctx carrying the run's tenant identity — NOT the
	// run ctx — so a cancelled/timed-out run still persists its final view
	// (mirrors the run-log writer's flush-ctx rationale).
	idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	// Capture per-file diff content (before/after) into the snapshot while the
	// clone still exists, so the server pod can serve /files/diff and
	// /commits/{sha}/diff for this run once the worktree is gone. Bounded:
	// small diffs inline, large ones offloaded to the blob backend, anything
	// past the budget dropped (Truncated). Best-effort — the metadata is the
	// contract; diff content is an enrichment.
	store.PopulateRunDiffs(idCtx, msg.RunID, workDir, meta, store.AsRunDiffBlobStore(r.cfg.Store))
	if err := gs.SaveRunGitMeta(idCtx, msg.RunID, meta); err != nil {
		r.cfg.Logger.Warn("runner: run %s: persist git meta: %v", msg.RunID, err)
	}
}

// uploadRunFiles copies the run's tool-produced artifact files from the
// runner-local scratch dir to the durable read backend (the Mongo store's
// S3 bridge). The server pod, which never saw this runner's disk, then
// serves them from the artifact-files panel. Best-effort: a store without
// the RunFilesUploader seam (filesystem dev store) no-ops cleanly, and an
// upload failure is logged, never fatal — the run's outcome is already
// decided. Runs on a background ctx carrying the run's tenant identity so
// a cancelled/timed-out run still flushes what it produced (mirrors
// recordRunGitMeta).
func (r *Runner) uploadRunFiles(_ context.Context, msg *queue.RunMessage) {
	up := store.AsRunFilesUploader(r.cfg.Store)
	if up == nil {
		return
	}
	idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	n, err := up.UploadRunFiles(idCtx, msg.RunID)
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: upload artifact files: %v", msg.RunID, err)
		return
	}
	if n > 0 {
		r.cfg.Logger.Info("runner: run %s: uploaded %d artifact file(s)", msg.RunID, n)
	}
}

// injectCredentials resolves the run's sealed bundle, decrypts it,
// stamps the plaintext into ctx via secrets.WithCredentials, and
// returns a cleanup func that performs LOCAL hygiene (wipes the
// in-memory plaintext keys + removes the OAuth temp dirs) at the call
// site. When no bundle is attached or the runner has no Sealer wired,
// returns the original ctx unchanged.
//
// The cleanup func runs on every executeRun return. Removal of the
// *persistent* sealed bundle from the store is intentionally NOT part
// of cleanup — see deleteRunSecrets, which executeRun invokes only on a
// terminal-clean outcome so a redelivered run can re-fetch its secrets.
//
// OAuth-forfait blobs are materialised in fresh temp directories
// (CLAUDE_CONFIG_DIR / CODEX_HOME-shaped) and wired through
// Credentials.OAuthCredentialFiles so the delegate backends point
// the spawned CLI at them. The cleanup func tears the dirs down on
// every exit path.
// prepareRepoWorkspace clones the run's RepoURL@RepoSHA into a fresh per-run
// directory and returns its path. For a private repo it authenticates the
// HTTPS clone with the bound forge token (forge_token / gitlab_token /
// github_token from the sealed bundle). The default branch is cloned first so
// the review base (typically `main`) is present, then the run's ref is fetched
// and checked out so merge-base diffs resolve.
func (r *Runner) prepareRepoWorkspace(ctx context.Context, msg *queue.RunMessage) (string, error) {
	// RepoURL/RepoSHA arrive from a webhook payload (the generic webhook
	// body is fully attacker-controlled) and flow into git below
	// unmodified. Validate the transport + ref shape BEFORE touching the
	// filesystem or spawning git so a remote-helper URL (`ext::sh -c …`)
	// or a flag-shaped ref (`--upload-pack=…`) can never reach the
	// subprocess. This is the runner's flag/transport-injection boundary,
	// mirroring the bot-install path.
	pinnedIP, err := validateRepoTarget(ctx, msg.RepoURL, msg.RepoSHA)
	if err != nil {
		return "", err
	}
	// SSRF connect-time hardening for the two TOCTOU vectors validateRepoTarget
	// alone can't close (it resolves but git re-resolves at connect time):
	//   (a) DNS rebinding — the clone-guard CONNECT proxy below is the
	//       enforcing layer: git dials through a loopback proxy that
	//       re-resolves the CONNECT host through the same SSRF guard and dials
	//       ONLY the validated IP, so it holds on non-root pods too. The
	//       /etc/hosts pin stays as belt-and-braces (it also covers ssh
	//       remotes) but is best-effort: on a non-root runner the file is
	//       kubelet-owned and unwritable — expected, logged once at info.
	//   (b) HTTP 302 → internal — disabled per git invocation below
	//       (http.followRedirects=false), and the proxy's single-host
	//       allowlist refuses any off-host CONNECT regardless.
	// The pod-level egress NetworkPolicy (block RFC1918/metadata) stays as
	// infra defence-in-depth on top.
	host, hostErr := extractRepoHost(msg.RepoURL)
	if hostErr == nil && pinnedIP != nil {
		if restore, perr := pinHostInHostsFile(runnerHostsFile, host, pinnedIP); perr == nil {
			defer restore()
		} else if pinUnavailable(perr) {
			// Expected & permanent on a non-root runner: /etc/hosts is a
			// kubelet-managed bind-mount owned by root, so the pin can never
			// land here. Log ONCE at info — the clone-guard proxy is the
			// connect-time control.
			r.ssrfPinUnavailableOnce.Do(func() {
				r.cfg.Logger.Info("runner: SSRF IP-pin unavailable on this runner: %s not writable (non-root); the clone-guard proxy is the connect-time control (%v)", runnerHostsFile, perr)
			})
		} else {
			// Unexpected: writable file but the write still failed. Keep warning per-clone.
			r.cfg.Logger.Warn("runner: SSRF IP-pin skipped for %s→%s (%v); the clone-guard proxy is the connect-time control", host, pinnedIP, perr)
		}
	}
	// git honours HTTPS_PROXY for http(s) transports only; ssh remotes keep
	// the pre-check + hosts pin + pod egress policy as their guard.
	var gitEnv []string
	if hostErr == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg.RepoURL)), "https://") {
		endpoint, stopProxy, perr := startCloneGuardProxy(host, !cloneAllowPrivate())
		if perr != nil {
			return "", fmt.Errorf("runner: %w", perr)
		}
		defer stopProxy()
		gitEnv = cloneGuardEnv(endpoint)
	}
	dir := filepath.Join(r.cfg.WorkDir, "repos", msg.RunID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean repo dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("mkdir repo parent: %w", err)
	}

	cloneURL, tok, appBotLogin := msg.RepoURL, "", ""
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		tok = strutil.FirstNonBlank(creds.GenericSecret("forge_token"), creds.GenericSecret("gitlab_token"), creds.GenericSecret("github_token"))
		appBotLogin = creds.ForgeAppBotLogin
		if tok != "" {
			cloneURL = injectGitToken(msg.RepoURL, tok)
		}
	}

	// -c http.followRedirects=false closes SSRF vector (b): a 302 from the
	// validated canonical https host to an internal address must not be
	// auto-followed by git.
	if err := r.runGitEnv(ctx, "", tok, gitEnv, "-c", "http.followRedirects=false", "clone", "--no-tags", "--quiet", cloneURL, dir); err != nil {
		return "", err
	}
	if ref := strings.TrimSpace(msg.RepoSHA); ref != "" {
		if err := r.runGitEnv(ctx, dir, tok, gitEnv, "-c", "http.followRedirects=false", "fetch", "--no-tags", "--quiet", "origin", ref); err != nil {
			return "", err
		}
		if err := r.runGit(ctx, dir, tok, "checkout", "--quiet", "-B", ref, "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	// Cloud sandboxes have no ~/.gitconfig (the host bind-mount is dropped on
	// kubernetes and the runner pod has none of its own), so seed an
	// author/committer in the clone's LOCAL config. It travels into the sandbox
	// with .git, so commit-producing bots (feature-dev's commit_changes, willy,
	// billy, docs-refresh, …) don't fail "Author identity unknown".
	//
	// Prefer the identity that OWNS the push token (resolved from the forge) so
	// a pushed commit is attributed to the real pusher, not a stray account
	// sharing the fallback email. Falls back to a neutral bot identity (never a
	// real person's) when there's no token or resolution fails. Overridable via
	// ITERION_GIT_AUTHOR_NAME / ITERION_GIT_AUTHOR_EMAIL.
	authorName, authorEmail := gitAuthorName(), gitAuthorEmail()
	if tok != "" {
		// A github_app connection's forge_token is an installation token that
		// can't `GET /user`; the publisher threads the App bot login so we
		// resolve its canonical committer via `GET /users/<login>` instead.
		if appBotLogin != "" {
			if n, e, ok := resolveAppBotCommitterIdentity(ctx, msg.RepoURL, appBotLogin, tok); ok {
				authorName, authorEmail = n, e
			}
		} else if n, e, ok := resolveForgeCommitterIdentity(ctx, msg.RepoURL, tok); ok {
			authorName, authorEmail = n, e
		}
	}
	_ = r.runGit(ctx, dir, "", "config", "user.name", authorName)
	_ = r.runGit(ctx, dir, "", "config", "user.email", authorEmail)
	seedRunScratchIgnore(dir)
	r.cfg.Logger.Info("runner: cloned %s@%s for run %s", msg.RepoURL, msg.RepoSHA, msg.RunID)
	return dir, nil
}

// gitAuthorName / gitAuthorEmail are the identity seeded into a cloud clone's
// local git config so an in-sandbox `git commit` has an author even though no
// ~/.gitconfig is mounted. Overridable per-deployment.
func gitAuthorName() string {
	if v := strings.TrimSpace(os.Getenv("ITERION_GIT_AUTHOR_NAME")); v != "" {
		return v
	}
	return "iterion-runner[bot]"
}

func gitAuthorEmail() string {
	if v := strings.TrimSpace(os.Getenv("ITERION_GIT_AUTHOR_EMAIL")); v != "" {
		return v
	}
	// A `.invalid` domain (RFC 2606, reserved, never resolvable) guarantees this
	// fallback maps to NO GitHub account — the commit shows the bot name as
	// plain text, never a stray individual. The default was
	// `iterion@users.noreply.github.com`, which GitHub silently attributed to an
	// unrelated real user "iterion". The push-token identity above is the
	// preferred, attributed path; this only fires token-less.
	return "iterion-runner@bot.iterion.invalid"
}

// seedRunScratchIgnore locally excludes iterion's per-run scratch — the
// .claude/ dir (mirrored skills + claude_code's plan.md) — from the cloned
// repo, so a bot's `git add -A` (which stages new files so the reviewers'
// `git diff HEAD` can see them) doesn't drag that scratch into the review
// diff. Writes .git/info/exclude (local, never committed or pushed);
// best-effort, so a read-only or unusual .git layout simply no-ops.
func seedRunScratchIgnore(dir string) {
	p := filepath.Join(dir, ".git", "info", "exclude")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# iterion per-run scratch (mirrored skills + plan) — not part of the change\n.claude/\n")
}

// validateRepoTarget gates the webhook-sourced clone URL and ref before
// they reach git. It rejects remote-helper transports (`ext::`, `file://`)
// via ValidateCloneSource and flag-shaped refs (leading `-`) via
// ValidateBranchName — the two ways an attacker-controlled RepoURL/RepoSHA
// could turn `git clone`/`git fetch` into arbitrary command execution.
// An empty ref is allowed (the caller only fetches when RepoSHA is non-blank).
//
// After the transport/ref shape passes, the URL's host is run through the
// shared SSRF guard (httpdial.ResolvePublicHost) so a holder of a per-org
// `iwh_` webhook token cannot point the runner at an internal address
// (loopback, RFC1918/ULA, link-local, cloud metadata, cluster aliases) to
// probe the cloud network. Mirrors the completion-webhook guard in pkg/notify.
// On-prem deployments with internal forges set
// ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1 to relax the strict mode.
func validateRepoTarget(ctx context.Context, repoURL, repoSHA string) (net.IP, error) {
	if err := gitlib.ValidateCloneSource(repoURL); err != nil {
		return nil, fmt.Errorf("runner: reject repo url: %w", err)
	}
	if ref := strings.TrimSpace(repoSHA); ref != "" {
		if err := gitlib.ValidateBranchName(ref); err != nil {
			return nil, fmt.Errorf("runner: reject repo ref: %w", err)
		}
	}
	host, err := extractRepoHost(repoURL)
	if err != nil {
		return nil, fmt.Errorf("runner: reject repo url: %w", err)
	}
	allowPrivate := cloneAllowPrivate()
	// First line of defence: refuse non-public hosts before any subprocess
	// spawns, and feed the resolved IP to the /etc/hosts pin. On its own this
	// would be TOCTOU-incomplete (git re-resolves the hostname at connect
	// time); the enforcing layer is the clone-guard CONNECT proxy
	// (startCloneGuardProxy) prepareRepoWorkspace routes https git through,
	// which re-validates and pins the resolved IP at the moment of the dial.
	// ssh remotes are not proxied — for them this pre-check, the hosts pin and
	// the pod egress policy remain the guard.
	ip, err := httpdial.ResolvePublicHost(ctx, host, !allowPrivate)
	if err != nil {
		return nil, fmt.Errorf("runner: repo host %q is not a public address (set ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1 to allow internal forges): %w", host, err)
	}
	return ip, nil
}

// extractRepoHost pulls the host out of a clone URL in the shapes
// ValidateCloneSource permits: `https://host[:port]/...`, `ssh://[user@]host[:port]/...`,
// and scp-like `[user@]host:path`. Returns an error when the host can't be
// determined (defence in depth — ValidateCloneSource has already rejected
// hostless and unsupported-transport forms above).
func extractRepoHost(repoURL string) (string, error) {
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("parse: %w", err)
		}
		host := u.Hostname()
		if host == "" {
			return "", fmt.Errorf("missing host in %q", repoURL)
		}
		return host, nil
	}
	// scp-like: `[user@]host:path`. ValidateCloneSource already requires
	// the colon to come before any slash and the host to be non-empty.
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return "", fmt.Errorf("missing host in %q", repoURL)
	}
	host := s[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if host == "" {
		return "", fmt.Errorf("missing host in %q", repoURL)
	}
	return host, nil
}

// gitOpTimeout bounds a single runner-side git subprocess. Without it a
// clone/fetch against a wedged remote hangs on the run ctx alone — and a
// run launched without --timeout has NO deadline, so the git subprocess
// pins the runner pod (one in-flight run each) indefinitely while the heartbeat
// keeps the lease alive (the heartbeat proves the pod lives, not that
// the clone progresses). GIT_TERMINAL_PROMPT=0 covers the credential
// prompt, not a stalled TCP transfer. Override with
// ITERION_RUNNER_GIT_TIMEOUT (a Go duration; <= 0 disables).
var gitOpTimeout = defaultGitOpTimeout()

func defaultGitOpTimeout() time.Duration {
	if v := os.Getenv("ITERION_RUNNER_GIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d // <= 0 disables the bound
		}
	}
	return 15 * time.Minute
}

// runGit runs a git subprocess, redacting tok from any error output so an
// authed clone URL never leaks into logs. Each invocation is bounded by
// gitOpTimeout on top of the caller's ctx.
func (r *Runner) runGit(ctx context.Context, dir, tok string, args ...string) error {
	return r.runGitEnv(ctx, dir, tok, nil, args...)
}

// runGitEnv is runGit with extra environment entries appended after the
// baseline (later entries win) — network git ops use it to route through the
// clone-guard proxy via HTTPS_PROXY.
func (r *Runner) runGitEnv(ctx context.Context, dir, tok string, extraEnv []string, args ...string) error {
	if gitOpTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gitOpTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Cancellation must reach git's helper processes (git-remote-https
	// inherits our output pipes — killing only the parent leaves
	// CombinedOutput blocked on the helper's copy), and WaitDelay is the
	// final unblock if a helper still holds them after the group kill.
	hardenGitCancel(cmd)
	cmd.WaitDelay = 10 * time.Second
	// Never prompt for credentials (fail fast instead of hanging), and ignore
	// any host-level git config in the runner image.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail, shown := strings.TrimSpace(string(out)), strings.Join(args, " ")
		if tok != "" {
			detail = strings.ReplaceAll(detail, tok, "***")
			shown = strings.ReplaceAll(shown, tok, "***")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && gitOpTimeout > 0 {
			return fmt.Errorf("git %s: timed out after %s (ITERION_RUNNER_GIT_TIMEOUT bounds each git op): %w: %s", shown, gitOpTimeout, err, detail)
		}
		return fmt.Errorf("git %s: %w: %s", shown, err, detail)
	}
	return nil
}

// injectGitToken rewrites an https clone URL to carry an oauth2 token in its
// userinfo (works for GitLab project/personal access tokens and GitHub PATs).
func injectGitToken(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return rawURL
	}
	u.User = url.UserPassword("oauth2", token)
	return u.String()
}

// materializeFileSecretsNoSandbox writes the workflow's `as: file` secrets to
// 0600 files at their mount paths in the runner pod when the run has no
// sandbox (a sandboxed run mounts them into the container instead). Returns
// the written files keyed by secret name (for the mid-run refresher) and a
// cleanup that removes them; both nil when nothing was written.
func (r *Runner) materializeFileSecretsNoSandbox(ctx context.Context, wf *ir.Workflow) (map[string]string, func(), error) {
	if wf == nil || len(wf.Secrets) == 0 ||
		runtime.WorkflowSandboxActive(wf, r.cfg.SandboxOverride, r.cfg.SandboxDefault) {
		// No secrets, or the run RESOLVES to an active sandbox (which mounts
		// file secrets into the container). The resolved decision — not
		// wf.Sandbox — is what matters: under ITERION_SANDBOX_OVERRIDE=none a
		// bot's sandbox block is neutralized and the run executes in this pod,
		// so its file secrets must be materialized here (run 019f4551's
		// push_auth_probe found no forge_token exactly because this gate used
		// to test the static declaration).
		return nil, nil, nil
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	written := map[string]string{}
	paths := func() []string {
		out := make([]string, 0, len(written))
		for _, p := range written {
			out = append(out, p)
		}
		return out
	}
	for name, s := range wf.Secrets {
		if !s.IsFile() {
			continue
		}
		val := creds.GenericSecret(name)
		if val == "" {
			continue // optional / unresolved → skip; the agent just won't find it
		}
		mp := secrets.ResolveFileMountPath(name, s.MountPath)
		// Confine writes to the secrets mount dir. The default mount path is
		// always under it; a DSL-supplied mount_path is tenant-controlled and
		// this runner pod is NOT sandboxed, so without this guard a crafted
		// mount_path (e.g. /root/.ssh/authorized_keys, /etc/cron.d/x) would
		// write the secret value to an arbitrary host path. The helper also
		// rejects path traversal and non-clean paths.
		if _, ok := secrets.RelativeToSecretFilesMountDir(mp); !ok {
			if r.cfg.Logger != nil {
				r.cfg.Logger.Warn("runner: refusing out-of-tree mount_path %q for file secret %q (must be under %s)", mp, name, secrets.SecretFilesMountDir)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
			return written, removeFilesFunc(paths()), err
		}
		if err := os.WriteFile(mp, []byte(val), 0o600); err != nil {
			return written, removeFilesFunc(paths()), err
		}
		written[name] = mp
	}
	if len(written) == 0 {
		return nil, nil, nil
	}
	return written, removeFilesFunc(paths()), nil
}

// fileSecretRefreshInterval paces the mid-run re-read of materialised file
// secrets from the generic-secret store. Well under the 10-minute cadence of
// the server-side refresh worker, so a rotated credential reaches the file
// within minutes of the store update.
const fileSecretRefreshInterval = 5 * time.Minute

// refreshFileSecretsLoop re-reads each materialised file secret's store
// record on a fixed cadence and rewrites the file when the value changed.
// Tools re-read the file per invocation (`cat /run/iterion/secrets/<name>`),
// so a rotation propagates to every subsequent forge push/comment without
// touching the process environment. Failures are logged and retried next
// tick — the file keeps its last good value; nothing is ever truncated.
func (r *Runner) refreshFileSecretsLoop(ctx context.Context, tenantID string, refs, files map[string]string) {
	tick := time.NewTicker(fileSecretRefreshInterval)
	defer tick.Stop()
	last := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.refreshFileSecretsOnce(ctx, tenantID, refs, files, last)
	}
}

// refreshFileSecretsOnce is one tick of refreshFileSecretsLoop: re-read every
// ref'd file secret and atomically rewrite the ones whose store value moved.
// readFreshSecret re-reads and unseals a generic-secret record by id
// under the tenant scope. Bounded, tenant-scoped; never logs the value.
// Shared by the no-sandbox (refreshFileSecretsOnce) and sandboxed
// (refreshSandboxFileSecretsOnce) mid-run refresh paths.
func (r *Runner) readFreshSecret(ctx context.Context, tenantID, id string) ([]byte, error) {
	rctx, cancel := context.WithTimeout(store.WithTenant(ctx, tenantID), 15*time.Second)
	defer cancel()
	rec, err := r.cfg.GenericSecrets.Get(rctx, id)
	if err != nil {
		return nil, err
	}
	return secrets.OpenGenericSecret(r.cfg.Sealer, rec.ID, rec.SealedSecret)
}

func (r *Runner) refreshFileSecretsOnce(ctx context.Context, tenantID string, refs, files, last map[string]string) {
	for name, path := range files {
		id := refs[name]
		if id == "" {
			continue // snapshot-only secret (no store ref) — nothing to refresh
		}
		val, err := r.readFreshSecret(ctx, tenantID, id)
		if err != nil {
			r.cfg.Logger.Warn("runner: refresh file secret %q (ref %s): %v", name, id, err)
			continue
		}
		if len(val) == 0 || string(val) == last[name] {
			continue
		}
		// Atomic replace so a concurrent `cat` never sees a torn write.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, val, 0o600); err != nil {
			r.cfg.Logger.Warn("runner: refresh file secret %q: write: %v", name, err)
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			r.cfg.Logger.Warn("runner: refresh file secret %q: rename: %v", name, err)
			continue
		}
		last[name] = string(val)
		r.cfg.Logger.Info("runner: refreshed file secret %q from store (rotation picked up)", name)
	}
}

// sandboxFileSecretRefs returns the file secrets of wf that carry a store
// ref in the run's credentials — the ones a mid-run refresh can rewrite.
// Nil when the store/creds/refs are absent or no file secret is
// refreshable. Shared shape with the no-sandbox refresher's refs map
// (name → generic-secret id).
func (r *Runner) sandboxFileSecretRefs(ctx context.Context, wf *ir.Workflow) map[string]string {
	if wf == nil || len(wf.Secrets) == 0 || r.cfg.GenericSecrets == nil {
		return nil
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok || len(creds.GenericRefs) == 0 {
		return nil
	}
	refs := map[string]string{}
	for name, s := range wf.Secrets {
		if !s.IsFile() {
			continue
		}
		if id := creds.GenericRefs[name]; id != "" {
			refs[name] = id
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// sandboxSecretRefreshObserver returns a runtime sandbox-run observer
// that starts the sandboxed mid-run file-secret refresh loop once the
// container is live — the sandboxed counterpart to refreshFileSecretsLoop
// (#99 covered only the no-sandbox in-pod path, so a long `sandbox: auto`
// run that pushes/comments after ~1h used a dead token). Returns nil when
// the run has no refreshable file secrets, so the engine hook is a no-op.
//
// refreshCtx must be a context the caller cancels when the run ends (the
// loop exits on it); the observer only spawns the goroutine.
func (r *Runner) sandboxSecretRefreshObserver(refreshCtx context.Context, tenantID string, refs map[string]string) func(sandbox.Run) {
	if len(refs) == 0 {
		return nil
	}
	return func(run sandbox.Run) {
		refresher, ok := run.(sandbox.SecretFileRefresher)
		if !ok {
			r.cfg.Logger.Warn("runner: sandbox driver %q does not support mid-run secret refresh; a long run may push with a stale token", run.Driver())
			return
		}
		go r.refreshSandboxFileSecretsLoop(refreshCtx, tenantID, refs, refresher)
	}
}

// refreshSandboxFileSecretsLoop re-reads each refreshable file secret's
// store record on a fixed cadence and, when the value rotated, hands it
// to the sandbox driver's SecretFileRefresher to propagate into the
// running container. Mirrors refreshFileSecretsLoop; failures are logged
// (never the value) and retried next tick.
func (r *Runner) refreshSandboxFileSecretsLoop(ctx context.Context, tenantID string, refs map[string]string, refresher sandbox.SecretFileRefresher) {
	tick := time.NewTicker(fileSecretRefreshInterval)
	defer tick.Stop()
	last := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.refreshSandboxFileSecretsOnce(ctx, tenantID, refs, refresher, last)
	}
}

// refreshSandboxFileSecretsOnce is one tick of
// refreshSandboxFileSecretsLoop: re-read every ref'd file secret and push
// the ones whose store value moved into the sandbox.
func (r *Runner) refreshSandboxFileSecretsOnce(ctx context.Context, tenantID string, refs map[string]string, refresher sandbox.SecretFileRefresher, last map[string]string) {
	for name, id := range refs {
		val, err := r.readFreshSecret(ctx, tenantID, id)
		if err != nil {
			r.cfg.Logger.Warn("runner: refresh sandboxed file secret %q (ref %s): %v", name, id, err)
			continue
		}
		if len(val) == 0 || string(val) == last[name] {
			continue
		}
		if err := refresher.RefreshSecretFile(ctx, name, val); err != nil {
			r.cfg.Logger.Warn("runner: refresh sandboxed file secret %q: %v", name, err)
			continue
		}
		last[name] = string(val)
		r.cfg.Logger.Info("runner: refreshed sandboxed file secret %q into the container (rotation picked up)", name)
	}
}

func removeFilesFunc(paths []string) func() {
	return func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
}

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
	// Tenant binding: rec.TenantID and msg.TenantID must match exactly.
	// The old code allowed empty rec.TenantID to bypass the check, on
	// the assumption that legacy records predated multitenancy — but
	// once a tenant_id is on the wire (msg.TenantID), a SecretsRef
	// stamped without a tenant could be served to a different tenant
	// that happened to request the same ref. New writes always stamp
	// tenant_id; if you see this error for a legacy ref, backfill its
	// tenant via the migration script before resuming the run.
	if rec.TenantID != msg.TenantID {
		return ctx, nil, fmt.Errorf("run_secrets tenant mismatch (msg=%q sealed=%q)", msg.TenantID, rec.TenantID)
	}
	bundle, err := secrets.OpenRunBundle(r.cfg.Sealer, msg.RunID, rec.SealedBundle)
	if err != nil {
		return ctx, nil, fmt.Errorf("unseal run_secrets %s: %w", msg.SecretsRef, err)
	}

	creds := secrets.Credentials{
		APIKeys: bundle.APIKeys,
		Generic: bundle.GenericSecrets,
		// Per-secret egress narrowing from bot-secret bindings; the guard
		// intersects these with the workflow's declared hosts. Hostnames
		// are not secret, so cleanup below leaves them untouched.
		GenericHosts:         bundle.GenericSecretHosts,
		GenericRefs:          bundle.GenericSecretRefs,
		OAuthCredentialFiles: map[string]string{},
		ForgeAppBotLogin:     bundle.ForgeAppBotLogin,
	}
	tmpDirs := make([]string, 0, len(bundle.OAuthCredentials))
	// cancelRefresh stops the per-run OAuth-forfait token refreshers (set
	// below once the files are materialised). cleanup calls it so the
	// goroutines exit before their temp dirs are removed.
	cancelRefresh := func() {}
	// cleanup performs LOCAL process hygiene only — wiping the decrypted
	// API keys from memory and removing the materialised OAuth temp dirs.
	// It runs on EVERY executeRun return (including Nak-for-redelivery
	// paths) so plaintext never outlives the attempt. Deleting the
	// *persistent* sealed bundle is deliberately NOT done here: it must
	// happen only on a terminal-clean outcome (executeRun calls
	// deleteRunSecrets) so a redelivered run can re-fetch the same
	// SecretsRef instead of silently running credential-less.
	cleanup := func() {
		cancelRefresh()
		for k := range bundle.APIKeys {
			bundle.APIKeys[k] = ""
		}
		for k := range bundle.GenericSecrets {
			bundle.GenericSecrets[k] = ""
		}
		for _, dir := range tmpDirs {
			_ = os.RemoveAll(dir)
		}
	}
	// refreshFiles maps oauth kind → the materialised credential file path,
	// fed to the per-run forfait token refresher so a long run never hits an
	// expired token mid-workflow (see oauth_refresh.go).
	refreshFiles := make(map[string]string, len(bundle.OAuthCredentials))
	for kind, payload := range bundle.OAuthCredentials {
		dir, fname, err := materializeOAuthCredentials(kind, payload)
		if err != nil {
			r.cfg.Logger.Warn("runner: oauth materialise %s for run %s: %v", kind, msg.RunID, err)
			continue
		}
		tmpDirs = append(tmpDirs, dir)
		creds.OAuthCredentialFiles[kind] = dir
		refreshFiles[kind] = filepath.Join(dir, fname)
		r.cfg.Logger.Info("runner: oauth-forfait active run=%s tenant=%s kind=%s file=%s/%s", msg.RunID, msg.TenantID, kind, dir, fname)
	}
	if len(refreshFiles) > 0 {
		stopRefresh := make(chan struct{})
		var once sync.Once
		cancelRefresh = func() { once.Do(func() { close(stopRefresh) }) }
		r.startOAuthRefreshers(stopRefresh, msg.RunID, refreshFiles)
	}
	return secrets.WithCredentials(ctx, creds), cleanup, nil
}

// deleteRunSecrets best-effort removes the persistent sealed bundle for
// this run from the RunSecrets store. executeRun calls it ONLY on a
// terminal-clean outcome (success or paused-for-resume) — never on a
// Nak-for-redelivery path, where the SAME SecretsRef must survive so the
// redelivered attempt can re-fetch its credentials. Detached from the
// (possibly already-cancelled) run context with its own short timeout. A
// failed delete is logged but non-fatal: the store's 24h TTL reaps the
// bundle regardless.
func (r *Runner) deleteRunSecrets(msg *queue.RunMessage) {
	if msg.SecretsRef == "" || r.cfg.RunSecrets == nil {
		return
	}
	ctxDel, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if delErr := r.cfg.RunSecrets.Delete(ctxDel, msg.SecretsRef); delErr != nil {
		r.cfg.Logger.Warn("runner: run_secrets delete for %s (ref=%s): %v", msg.RunID, msg.SecretsRef, delErr)
	}
}

// materializeOAuthCredentials writes the sealed payload to a fresh
// temp dir under the file name the corresponding CLI expects.
//
//   - claude_code → <dir>/.credentials.json (CLAUDE_CONFIG_DIR=<dir>)
//   - codex       → <dir>/auth.json         (CODEX_HOME=<dir>)
//
// The directory is mode 0o700, the file 0o600 so other local users
// (including a sandbox host's UID-shifted writer) cannot read.
func materializeOAuthCredentials(kind string, payload []byte) (dir string, fname string, err error) {
	switch secrets.OAuthKind(kind) {
	case secrets.OAuthKindClaudeCode:
		fname = ".credentials.json"
	case secrets.OAuthKindCodex:
		fname = "auth.json"
	default:
		return "", "", fmt.Errorf("unknown oauth kind %q", kind)
	}
	dir, err = os.MkdirTemp("", "iter-oauth-")
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	full := filepath.Join(dir, fname)
	if err := os.WriteFile(full, payload, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, fname, nil
}

// loadWorkflow decodes the AST for a run and compiles it to IR. The IR
// travels inline on RunMessage.IRCompiled for the vast majority of
// workflows; when it exceeds the NATS max_payload the publisher offloads
// it out-of-band (T-42) and sends an IRRef instead, which the runner
// fetches here via the store's IRBlobStore seam (blobs may be nil for a
// store without the seam — then an IRRef is unrecoverable and fails loudly).
func loadWorkflow(ctx context.Context, msg *queue.RunMessage, blobs store.IRBlobStore) (*ir.Workflow, error) {
	raw := msg.IRCompiled
	if len(raw) == 0 {
		if msg.IRRef == nil || msg.IRRef.StorageKey == "" {
			return nil, fmt.Errorf("runner: RunMessage for run %s carries neither IRCompiled nor IRRef", msg.RunID)
		}
		if blobs == nil {
			return nil, fmt.Errorf("runner: run %s references out-of-band IR (%s:%s) but this store cannot fetch IR blobs", msg.RunID, msg.IRRef.Backend, msg.IRRef.StorageKey)
		}
		fetched, err := blobs.GetIRBlob(ctx, msg.IRRef.StorageKey)
		if err != nil {
			return nil, fmt.Errorf("runner: fetch out-of-band IR %s for run %s: %w", msg.IRRef.StorageKey, msg.RunID, err)
		}
		raw = fetched
	}
	file, err := ast.UnmarshalFile(raw)
	if err != nil {
		return nil, fmt.Errorf("runner: decode IR: %w", err)
	}
	cr := ir.Compile(file)
	if cr.HasErrors() {
		return nil, fmt.Errorf("runner: compile IR: %d diagnostic(s)", len(cr.Diagnostics))
	}
	return cr.Workflow, nil
}

// applyBudgetOverrides folds launch-time budget overrides from the queue
// message into the loaded workflow ("non-zero wins, zero inherits" — same
// contract as the CLI flags and the local launch path). Must run before
// applyCloudBudgetCeiling so the platform ceiling still clamps whatever the
// tenant asked for. A malformed max_duration fails the run loudly rather
// than silently running without the cap the caller asked for.
func applyBudgetOverrides(wf *ir.Workflow, b *queue.BudgetOverrides, logger *iterlog.Logger) error {
	if wf == nil || b == nil {
		return nil
	}
	o := ir.BudgetOverrides{
		MaxCostUSD:          b.MaxCostUSD,
		MaxTokens:           b.MaxTokens,
		MaxDuration:         b.MaxDuration,
		MaxIterations:       b.MaxIterations,
		MaxParallelBranches: b.MaxParallelBranches,
	}
	if o.IsZero() {
		return nil
	}
	if err := o.Validate(); err != nil {
		// The publisher validates at launch; reaching this means a
		// hand-crafted or corrupted message — same treatment as a tenant
		// mismatch: corrupted queue entry, fail the run.
		return fmt.Errorf("runner: launch budget override: %w", err)
	}
	ir.ApplyBudgetOverrides(wf, o)
	logger.Info("runner: launch budget overrides applied (cost=%.2f tokens=%d duration=%q iterations=%d branches=%d)",
		o.MaxCostUSD, o.MaxTokens, o.MaxDuration, o.MaxIterations, o.MaxParallelBranches)
	return nil
}

// applyCloudBudgetCeiling clamps wf.Budget to the platform ceiling read from
// the environment — the cloud-side enforcement of the unbounded-loop fuel
// invariant. Set any of ITERION_CLOUD_MAX_ITERATIONS, ITERION_CLOUD_MAX_TOKENS,
// ITERION_CLOUD_MAX_COST_USD, ITERION_CLOUD_MAX_DURATION,
// ITERION_CLOUD_MAX_PARALLEL_BRANCHES to impose a hard, tenant-unraisable cap.
// No env set → no-op (self-hosted / single-tenant keeps DSL budgets verbatim).
func applyCloudBudgetCeiling(wf *ir.Workflow, logger *iterlog.Logger) {
	if wf == nil {
		return
	}
	ceiling := &ir.Budget{}
	any := false
	if v, ok := envPositiveInt("ITERION_CLOUD_MAX_ITERATIONS"); ok {
		ceiling.MaxIterations, any = v, true
	}
	if v, ok := envPositiveInt("ITERION_CLOUD_MAX_TOKENS"); ok {
		ceiling.MaxTokens, any = v, true
	}
	if v, ok := envPositiveInt("ITERION_CLOUD_MAX_PARALLEL_BRANCHES"); ok {
		ceiling.MaxParallelBranches, any = v, true
	}
	if f, ok := envPositiveFloat("ITERION_CLOUD_MAX_COST_USD"); ok {
		ceiling.MaxCostUSD, any = f, true
	}
	if s := os.Getenv("ITERION_CLOUD_MAX_DURATION"); s != "" {
		ceiling.MaxDuration, any = s, true
	}
	if !any {
		return
	}
	if wf.Budget == nil {
		wf.Budget = &ir.Budget{}
	}
	before := *wf.Budget
	wf.Budget.ClampToCeiling(ceiling)
	if logger != nil && *wf.Budget != before {
		logger.Info("runner: clamped workflow budget to platform ceiling (iterations=%d tokens=%d cost=%.2f dur=%q)",
			wf.Budget.MaxIterations, wf.Budget.MaxTokens, wf.Budget.MaxCostUSD, wf.Budget.MaxDuration)
	}
}

func envPositiveInt(key string) (int, bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func envPositiveFloat(key string) (float64, bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return f, true
}

// buildExecutor reuses runview.BuildExecutor so the runner shares
// exactly the same backend / tool / MCP wiring as the studio server
// and the CLI run path. Vars from the message are forwarded so
// {{vars.X}} expansion works without re-resolving from disk.
//
// The returned metricsEmitter is the same wrapper the executor writes
// through — executeRun reads its RunTotals at the end of the attempt
// to charge the org's monthly usage. Always wrapped (Prometheus
// registry may be nil) so org metering works without metrics.
func (r *Runner) buildExecutor(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow, logger *iterlog.Logger) (runtime.NodeExecutor, *metricsEmitter, error) {
	emitter, ok := r.cfg.Store.(model.EventEmitter)
	if !ok {
		return nil, nil, fmt.Errorf("runner: store does not satisfy model.EventEmitter")
	}
	// Wrap the emitter so LLM step + delegate events update the
	// iterion_llm_tokens_total / iterion_llm_cost_usd_total counters
	// (and the per-run totals) as they are written to Mongo. Wrapping
	// at the runner boundary keeps pkg/backend/model free of any
	// metrics dependency.
	usage := newMetricsEmitter(emitter, r.cfg.Metrics)
	vars := stringifyVars(msg.Vars)
	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: wf,
		Vars:     vars,
		Store:    usage,
		RunID:    msg.RunID,
		// The run-scoped logger (teed into the RunLogStore) — the executor
		// emits the `[node#iter/backend]` agent-stream lines the studio's
		// per-node Logs tab filters on; on the raw pod logger they would
		// reach stdout but never the persisted run.log.
		Logger:      logger,
		StoreDir:    r.cfg.WorkDir,
		BotID:       msg.BotID,
		MemoryStore: r.cfg.MemoryStore,
	})
	if err != nil {
		return nil, nil, err
	}
	return exec, usage, nil
}

// recordOrgSpend charges the run's accumulated LLM consumption to the
// org's monthly usage bucket. Called at the end of every execution
// attempt — paused/cancelled/failed attempts incurred real spend too,
// and a redelivered attempt re-charges only what it re-executed.
// Detached ctx: a Mongo blip must not fail the run path; the miss is
// logged and the Prometheus counters still carry the global totals.
func (r *Runner) recordOrgSpend(msg *queue.RunMessage, usage *metricsEmitter) {
	if r.cfg.OrgUsage == nil || usage == nil || msg.TenantID == "" {
		return
	}
	costUSD, in, out := usage.RunTotals()
	if costUSD <= 0 && in <= 0 && out <= 0 {
		return
	}
	// Charge the same usage key the launch gate metered the run on:
	// the parent org (caps sum across the org's teams — charging the
	// team key instead leaves the org's cost-cap document at zero, so
	// the cap never trips in a multi-team org). OrgID is empty on
	// pre-orgid messages and org-less pre-backfill teams — both were
	// metered on the team key, so fall back to it.
	key := msg.OrgID
	if key == "" {
		key = msg.TenantID
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.cfg.OrgUsage.AddSpend(bg, key, time.Now().UTC(), costUSD, in, out); err != nil {
		r.cfg.Logger.Warn("runner: org spend record for %s (run %s): %v", key, msg.RunID, err)
	}
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
//
// It also accumulates the run's own totals (cost + tokens) so the
// runner can charge the org's monthly usage bucket once at the end of
// the attempt — reg may be nil (no Prometheus) while the run totals
// still accumulate.
type metricsEmitter struct {
	inner model.EventEmitter
	reg   *metrics.Registry

	// modelByNode caches the last model name reported by an
	// llm_request event for a given node, so the subsequent
	// llm_step_finished events can be labelled even though the step
	// payload itself doesn't repeat the model field.
	//
	// priceByModel caches the resolved per-token rates so the
	// cost.EstimateUSD path (which hits claw's live registry — a disk
	// read + JSON parse each call) doesn't fire on every step event.
	// A workflow with 50 steps × 10 parallel branches would otherwise
	// serialise 500 disk hits through the metrics emitter mutex.
	mu           sync.Mutex
	modelByNode  map[string]string
	priceByModel map[string]modelRate

	// Per-run accumulation for org metering. Cost covers claw steps
	// only (delegate backends report tokens without a price table) —
	// a floor, not an exact invoice; documented on orgusage.
	runCostUSD      float64
	runInputTokens  int64
	runOutputTokens int64
}

// modelRate is the per-token cost (USD) for a given model, derived
// once via cost.EstimateUSD and cached. `known` distinguishes
// "table doesn't know this model" (skip the counter) from
// "rates are genuinely zero".
type modelRate struct {
	inputUSDPerToken  float64
	outputUSDPerToken float64
	known             bool
}

func newMetricsEmitter(inner model.EventEmitter, reg *metrics.Registry) *metricsEmitter {
	return &metricsEmitter{
		inner:        inner,
		reg:          reg,
		modelByNode:  make(map[string]string),
		priceByModel: make(map[string]modelRate),
	}
}

// rateFor returns the cached per-token rates for the given model,
// resolving once via cost.EstimateUSD. Called under m.mu.
func (m *metricsEmitter) rateForLocked(modelName string) modelRate {
	if r, ok := m.priceByModel[modelName]; ok {
		return r
	}
	const probe = 1_000_000
	inUSD := cost.EstimateUSD(modelName, probe, 0)
	outUSD := cost.EstimateUSD(modelName, 0, probe)
	r := modelRate{
		inputUSDPerToken:  inUSD / float64(probe),
		outputUSDPerToken: outUSD / float64(probe),
		known:             inUSD > 0 || outUSD > 0,
	}
	m.priceByModel[modelName] = r
	return r
}

func (m *metricsEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	m.observe(evt)
	return m.inner.AppendEvent(ctx, runID, evt)
}

// AppendPlanSnapshot forwards plan-snapshot persistence to the wrapped
// emitter when it implements model.PlanWriter (the Mongo cloud store now
// does). Without this explicit forward the capture hook's plain
// `emitter.(PlanWriter)` assertion runs against THIS wrapper — which,
// lacking the method, would yield nil and silently disable plan capture
// for every cloud run even once the store supports it. When the inner
// store is not a PlanWriter (a store without the seam) this is a benign
// no-op (wrote=false, no error) — identical to today's nil-planSink
// behaviour, and NOT the loud store-write failure path.
func (m *metricsEmitter) AppendPlanSnapshot(ctx context.Context, runID string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error) {
	pw, ok := m.inner.(model.PlanWriter)
	if !ok {
		return snap, false, nil
	}
	return pw.AppendPlanSnapshot(ctx, runID, snap)
}

// WriteTurn forwards per-LLM-turn checkpoint persistence to the wrapped
// emitter when it implements model.TurnWriter (the Mongo cloud store now
// does). Same rationale as AppendPlanSnapshot: without this explicit
// forward the capture hook's `emitter.(TurnWriter)` assertion runs against
// THIS wrapper — which, lacking the method, would yield nil and silently
// disable per-turn capture for every cloud run (breaking the studio
// timeline + fork-from-turn) even once the store supports it. When the
// inner store is not a TurnWriter this is a benign no-op (nil error),
// matching today's nil-turnSink skip behaviour.
func (m *metricsEmitter) WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error {
	tw, ok := m.inner.(model.TurnWriter)
	if !ok {
		return nil
	}
	return tw.WriteTurn(ctx, t)
}

// WriteToolBlob forwards per-tool-call I/O sidecar persistence to the
// wrapped emitter when it implements model.ToolBlobWriter (the Mongo
// cloud store now does). Same rationale as WriteTurn: the capture hook's
// `emitter.(ToolBlobWriter)` assertion runs against THIS wrapper, so
// without the forward large tool outputs would silently fall back to the
// capped inline preview for every cloud run. When the inner store is not
// a ToolBlobWriter this signals "no sidecar" the same way a nil
// blobSink does — persistToolPayload then keeps the capped inline body.
func (m *metricsEmitter) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	bw, ok := m.inner.(model.ToolBlobWriter)
	if !ok {
		return 0, fmt.Errorf("runner: inner store does not persist tool blobs")
	}
	return bw.WriteToolBlob(ctx, runID, toolUseID, kind, body)
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
		const backend = "claw"
		inputT := toFloat(evt.Data["input_tokens"])
		outputT := toFloat(evt.Data["output_tokens"])

		// Single critical section: resolve the per-node model name,
		// accumulate run-level token + cost totals, and compute the
		// per-model cost delta against the cached rate (rateForLocked
		// requires the lock held by its caller). Prometheus writes and
		// the addTokens helper run AFTER the unlock — counter Add is
		// atomic on the vec, addTokens reads only its locals.
		m.mu.Lock()
		modelName := m.modelByNode[evt.NodeID]
		if modelName == "" {
			modelName = "unknown"
		}
		m.runInputTokens += int64(inputT)
		m.runOutputTokens += int64(outputT)
		var costDelta float64
		if modelName != "unknown" {
			rate := m.rateForLocked(modelName)
			if rate.known {
				if c := inputT*rate.inputUSDPerToken + outputT*rate.outputUSDPerToken; c > 0 {
					m.runCostUSD += c
					costDelta = c
				}
			}
		}
		m.mu.Unlock()

		m.addTokens(backend, modelName, "input", evt.Data["input_tokens"])
		m.addTokens(backend, modelName, "output", evt.Data["output_tokens"])
		m.addTokens(backend, modelName, "cache_read", evt.Data["cache_read_tokens"])
		m.addTokens(backend, modelName, "cache_write", evt.Data["cache_write_tokens"])
		// LLMCostUSDTotal: unknown models leave the counter untouched so
		// observers can tell "no data" from "$0" via the absence of
		// samples.
		if costDelta > 0 && m.reg != nil {
			m.reg.LLMCostUSDTotal.WithLabelValues(backend, normalizeModelLabel(modelName)).Add(costDelta)
		}
	case store.EventDelegateFinished:
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		tokensF := toFloat(evt.Data["tokens"])

		// Single critical section: resolve the per-node model name and
		// accumulate the aggregated token count. Prometheus write
		// happens after the unlock via addTokens (counter Add is atomic).
		m.mu.Lock()
		modelName := m.modelByNode[evt.NodeID]
		m.runInputTokens += int64(tokensF)
		m.mu.Unlock()

		// Delegate events report a single aggregated token count;
		// label as input so a sum across directions stays meaningful.
		m.addTokens(backend, modelName, "input", evt.Data["tokens"])
	}
}

// RunTotals snapshots the run's accumulated LLM consumption — what
// the runner charges to the org's monthly usage bucket.
func (m *metricsEmitter) RunTotals() (costUSD float64, inputTokens, outputTokens int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runCostUSD, m.runInputTokens, m.runOutputTokens
}

func (m *metricsEmitter) lookupModel(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelByNode[nodeID]
}

func (m *metricsEmitter) addTokens(backend, modelName, direction string, raw any) {
	n := toFloat(raw)
	if n <= 0 || backend == "" || m.reg == nil {
		return
	}
	m.reg.LLMTokensTotal.WithLabelValues(backend, normalizeModelLabel(modelName), direction).Add(n)
}

// normalizeModelLabel bounds the prometheus `model` label cardinality
// by stripping trailing date-style version suffixes (e.g. "-20260427",
// "-2026-04-27") and truncating overlong identifiers. Without this,
// label values churn every time a provider ships a new dated snapshot,
// growing the time-series set without bound.
func normalizeModelLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	// Strip trailing -<digits[-digits...]> patterns.
	for {
		i := strings.LastIndexByte(s, '-')
		if i < 0 || i == len(s)-1 {
			break
		}
		tail := s[i+1:]
		alldigit := true
		for _, r := range tail {
			if r < '0' || r > '9' {
				alldigit = false
				break
			}
		}
		if !alldigit {
			break
		}
		s = s[:i]
	}
	const maxLen = 64
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// toFloat coerces the JSON-decoded scalar (always float64 in Go's
// encoding/json) to a non-negative float64, returning 0 when the
// value is missing, nil, or not a number.
func toFloat(raw any) float64 {
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
func stringifyVars(in map[string]any) map[string]string {
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
