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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/notify"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usagecap"
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
		// Cancelled is terminal for a REDELIVERED launch message,
		// checkpoint or not: auto-resuming here turned any lost ack of
		// an operator cancel into a resurrection loop (run 019f8ba3
		// came back three times, incl. via plain JetStream redelivery
		// with the runner up, and after every pod roll). The checkpoint
		// stays on the run doc — an explicit resume (msg.Resume set, or
		// the resume API) is the only way to continue. A shutdown-drain
		// whose nak beat the checkpoint write lands here too and now
		// waits for that explicit resume instead of self-restarting.
		if msg.Resume == nil {
			return preconditionOutcome{
				finalStatus: "cancelled",
				op:          "ack-already-cancelled",
				action:      actionAck,
				level:       logInfo,
				logFmt:      "runner: run %s is cancelled — dropping redelivery (explicit resume required to continue)",
				logArgs:     []any{msg.RunID},
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
func classifyExecResult(execErr error, runID string) execOutcome {
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
	// Budget FIRST: an interruption that also carries a spent budget must
	// not be auto-resumed. errors.Is short-circuits, so the order of these
	// two is the precedence — pinned by a table case.
	// Budget exceeded is a RESUMABLE checkpoint, not a transient blip:
	// the engine saved a failed_resumable checkpoint (failBudgetExceeded).
	// Ack it — do NOT Nak for auto-redelivery. Auto-resuming a
	// budget-exceeded run is worse than useless: the same message carries
	// the same (already-spent) budget, so a duration cap re-fails
	// instantly, and each redelivery re-provisions a FRESH pod whose
	// recordRunGitMeta overwrites the first attempt's good git metadata
	// with base==head — silently destroying the run's exported commits
	// (observed live: run 019f8e08 lost 40 in-pod doc commits this way).
	// The operator resumes MANUALLY with a raised cap
	// (`iterion resume --max-duration/--max-cost-usd`), the documented
	// "budget exceeded → raise the cap + resume" recovery.
	//
	// Matched by sentinel AND by RuntimeError code: the engine has two budget
	// exits (the branch scheduler wraps the sentinel, the per-node pre-check
	// historically did not), and a budget death that misses this carve-out is
	// naked into exactly the redelivery loop described above — observed live
	// on 2026-08-04 (run 019fcc30-b9be: six ~40s resume/refail turns at a 96%
	// duration hard limit, each re-provisioning a sandbox). The code match
	// also covers a mixed-version deploy where an older engine produced the
	// unwrapped shape.
	if errors.Is(execErr, runtime.ErrBudgetExceeded) || runtimeCodeOf(execErr) == runtime.ErrCodeBudgetExceeded {
		return execOutcome{
			finalStatus: "budget_exceeded",
			op:          "ack-budget-exceeded",
			action:      actionAck,
			level:       logWarn,
			logFmt:      "runner: run %s hit a budget cap — failed_resumable, NOT auto-resuming (resume manually with a raised cap): %v",
			logArgs:     []any{runID, execErr},
		}
	}
	// Infrastructure interruption (runner drain / lost heartbeat): the
	// engine already wrote failed_resumable (via the ErrRunInterrupted
	// cancel cause). Nak so JetStream redelivers and the reconciliation
	// auto-resumes from the checkpoint — no manual intervention.
	if errors.Is(execErr, runtime.ErrRunInterrupted) {
		return execOutcome{
			finalStatus: "interrupted",
			op:          "nak-interrupted",
			action:      actionNak,
			level:       logWarn,
			logFmt:      "runner: run %s interrupted (resumable) — naking for auto-resume (%v)",
			logArgs:     []any{runID, execErr},
		}
	}
	// Operator cancel: terminal cancelled, acked (redelivery drops it).
	if errors.Is(execErr, runtime.ErrRunCancelled) {
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
	// SchemaMismatchDelay is the redelivery delay applied when this runner
	// rejects a delivery whose schema version it does not recognise (mixed
	// fleet during a rolling schema bump). Must be long enough that the
	// MaxDeliver budget stretches over the duration of a rolling restart —
	// an immediate Nak burns it in seconds (issue #481). 0 →
	// natsq.SchemaMismatchNakDelay.
	SchemaMismatchDelay time.Duration
	// DrainMode governs SIGTERM handling. "complete" (default, the
	// lame-duck posture): stop fetching new runs but let the in-flight run
	// finish naturally before exiting — a rolling deploy interrupts
	// nothing. "interrupt": cancel the in-flight run immediately, checkpoint
	// it, and let it auto-resume on a healthy pod. Either way an interrupted
	// run is promoted to failed_resumable + redelivered (never stranded).
	DrainMode string
	// DrainTimeout is the lame-duck ceiling: the longest Shutdown waits for
	// the in-flight run before capping it (cancel → checkpoint → auto-resume
	// on another pod). It is the internal bound; the k8s
	// terminationGracePeriodSeconds is the hard external one and must be set
	// >= DrainTimeout + margin for the cap to checkpoint cleanly. 0 → 8h.
	DrainTimeout time.Duration
	Logger       *iterlog.Logger
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

	// UsageCapSource answers the operator's ceiling on the LLM
	// subscription's own usage windows (pkg/usagecap) — consulted per
	// evaluation, so a DB-backed source (usagecap.Resolver) makes a
	// runtime cap change effective on live runs within its TTL. Wrap a
	// fixed policy in usagecap.StaticPolicy for the env-only shape. nil
	// caps nothing, which leaves runs bounded only by the provider's
	// wall — the historical behaviour.
	UsageCapSource usagecap.PolicySource
	// UsageCaps is where readings of those windows are shared across
	// replicas, so a pod can park a run before spending anything on
	// rediscovering a ceiling another pod already hit. nil disables the
	// pre-flight; the in-run guard still applies.
	UsageCaps usagecap.Store

	// OrgUsage, when non-nil, receives each run's accumulated LLM
	// cost/tokens into the org's monthly bucket at the end of every
	// execution attempt (the billing source of truth — Prometheus
	// counters above stay tenant-unlabelled). nil → no org metering.
	OrgUsage orgusage.Counter

	// CredPool, when non-nil, receives the spend of a run served by a
	// lending contributor's pooled subscription, closing its lease and
	// freeing the donor's concurrency slot. nil → runs never draw on a
	// pool, or the deployment has none.
	CredPool *credpool.Broker

	// Events, when non-nil, receives a run-outcome trigger.Event
	// (run.finished/failed/cancelled/paused) after every execution
	// attempt — the runner-side twin of runview.emitRunOutcome, so
	// server-side consumers (the usernotify dispatcher, trigger
	// chaining) see runner-pod runs too, not only in-process ones.
	// Lossy fan-out by design; the notification sweep is the safety
	// net. nil → no event publishing.
	Events eventbus.Bus

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

	// Probe state (see health.go). lastTick is the unix-nano stamp of the
	// consume loop's most recent iteration — what tells a wedged loop from
	// an idle one, and (non-zero) that the loop started at all. draining is
	// set at the top of Shutdown.
	draining atomic.Bool
	lastTick atomic.Int64

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

	// steer maps an in-flight run to its live-steering state (override
	// channel + command-id dedup cache) so the per-run NATS subscriber
	// can push bump_loop / raise_budget into the engine. See steer.go.
	steerMu sync.Mutex
	steer   map[string]*runSteerState
	// steerAckFn / steerAckTimeout are test seams for the steering
	// transport (default: NATS PublishSteerAck / 4s engine wait).
	steerAckFn      func(runID, commandID string, body []byte) error
	steerAckTimeout time.Duration

	// ssrfPinUnavailableOnce demotes the expected, permanent "hosts file not
	// writable" SSRF IP-pin condition (non-root runner + kubelet-managed
	// /etc/hosts) to a single info log for the runner's lifetime, instead of a
	// per-clone warn that trains operators to ignore warns. See prepareRepoWorkspace.
	ssrfPinUnavailableOnce sync.Once

	// sandboxRuns maps an in-flight run to its live sandbox Run handle
	// (registered by the engine's sandbox-run observer) so the mid-run
	// credential refreshers can write rotated tokens THROUGH into the
	// sandbox — the k8s workspace is a tar COPY, so a host-side rewrite
	// alone never reaches it. See sandbox_registry.go.
	sandboxRunsMu sync.Mutex
	sandboxRuns   map[string]sandbox.Run
}

type inFlight struct {
	runID    string
	delivery *natsq.Delivery
	// cancelFn cancels the run context WITH A CAUSE (context.CancelCause).
	// The cause is the single source of the shutdown-vs-operator decision:
	// runtime.ErrRunInterrupted (runner drain / lost heartbeat) → the engine
	// writes failed_resumable and the run auto-resumes; runtime.ErrRunCancelled
	// (operator cancel subject) → terminal cancelled. This replaces inferring
	// intent from the loop ctx, which would misclassify an operator cancel
	// arriving during a (up-to-DrainTimeout-long) lame-duck drain and
	// resurrect a deliberately-cancelled run.
	cancelFn context.CancelCauseFunc
	// done is closed once processOne has Ack'd or Nak'd the delivery.
	// Shutdown selects on it to avoid double-acting on a delivery
	// processOne already finalised.
	done chan struct{}
}

const (
	// DrainModeComplete (default) is the lame-duck posture: on SIGTERM the
	// runner stops fetching new runs but lets the in-flight one finish
	// before exiting, so a rolling deploy interrupts nothing.
	DrainModeComplete = "complete"
	// DrainModeInterrupt cancels the in-flight run immediately on SIGTERM,
	// checkpoints it, and lets it auto-resume on a healthy pod — the fast
	// path for an urgent (e.g. security) deploy.
	DrainModeInterrupt = "interrupt"
	// DefaultDrainTimeout is the lame-duck ceiling when unset.
	DefaultDrainTimeout = 8 * time.Hour
	// interruptCheckpointGrace bounds how long Shutdown waits, after
	// capping a lame-duck run at the ceiling, for the engine's checkpoint +
	// status promotion to land before best-effort naking.
	interruptCheckpointGrace = 30 * time.Second
)

// DrainTimeout reports the lame-duck ceiling so the entrypoint can size the
// Shutdown context to it (single source of truth for the bound).
func (r *Runner) DrainTimeout() time.Duration { return r.cfg.DrainTimeout }

// DrainMode reports the resolved drain mode.
func (r *Runner) DrainMode() string { return r.cfg.DrainMode }

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
	if cfg.DrainMode == "" {
		cfg.DrainMode = DrainModeComplete
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.NewFromEnv(os.Stderr)
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

	r.tick() // the probes' "started" signal (health.go)
	r.cfg.Logger.Info("runner: started, runnerID=%s workdir=%s", r.cfg.RunnerID, r.cfg.WorkDir)

	// NATS queue depth gauge: every PendingPoll the runner samples the
	// JetStream consumer info and publishes the Pending count to the
	// Prometheus registry. KEDA scales on the same value via the
	// nats-jetstream scaler — this gauge gives operators a parallel
	// signal in their own dashboards without competing with the scaler.
	if r.cfg.Metrics != nil {
		errtrack.Go("runner.pollPending", func() { r.pollPending(loopCtx) })
	}
	// K8s sandbox reaper (ADR-070): at boot + on a ticker, a healthy
	// runner force-deletes the orphaned sandbox pod + both Secrets +
	// NetworkPolicy of any run no longer held by a NATS lease. This is
	// the managed-cloud counterpart to the runview.Service reaper, which
	// never fires in the cloud (lock-less store + no runview.Service) —
	// closing the OOM-with-surviving-pod plaintext-credential leak that
	// the ownerReference cascade misses (the pod UID survives a container
	// restart). No-op when not in-cluster / no NATS. See reaper.go.
	errtrack.Go("runner.sandboxReaper", func() { r.runSandboxReaper(loopCtx) })
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
		// Liveness stamp: proof the loop is still cycling. An idle loop
		// ticks every FetchWait; a busy one stops ticking, which the probe
		// reads as "busy", not "wedged" (see health.go).
		r.tick()
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

// Shutdown signals the loop to stop fetching new messages and then, per
// DrainMode, either lets the in-flight run finish (lame-duck, default) or
// cancels it immediately for a checkpoint-resume. An interrupted run is
// promoted to failed_resumable and redelivered by processOne, so a rolling
// deploy never strands a run. `ctx` carries the drain ceiling
// (DrainTimeout); k8s terminationGracePeriodSeconds is the hard external
// bound. Plan §F T-28.
//
// Narrow benign race: a delivery Fetched in the instant before SIGTERM may
// not have published r.current yet, so Shutdown sees nil and returns
// without engaging the drain machinery for it. The run still executes
// safely on its decoupled runCtx (the process stays alive because Run
// blocks in processOne) — only the DrainTimeout ceiling is skipped;
// terminationGracePeriodSeconds remains the hard bound and the orphan
// sweeper recovers anything the SIGKILL leaves `running`. No state is lost.
func (r *Runner) Shutdown(ctx context.Context) error {
	// Flip readiness first: a pod that may sit in a lame-duck drain for
	// hours must not look like a fresh one to an operator or a rollout.
	r.draining.Store(true)
	r.mu.Lock()
	cur := r.current
	cancel := r.cancel
	r.mu.Unlock()

	// Stop fetching new deliveries. The in-flight run's context is
	// decoupled from the loop context (see processOne), so this does NOT
	// cancel the running run — Shutdown owns that decision below.
	if cancel != nil {
		cancel()
	}
	if cur == nil {
		return nil
	}

	if r.cfg.DrainMode == DrainModeInterrupt {
		// Cancel the in-flight run now and wait only long enough for
		// processOne to checkpoint + nak; it auto-resumes on a healthy pod
		// from the redelivery. The wait is the SHORT checkpoint grace, not
		// the caller's drain ceiling: this mode exists to leave quickly, and
		// an engine that does not unwind promptly (a delegate ignoring its
		// ctx, a stuck subprocess) would otherwise hold the pod for the full
		// ceiling — the opposite of what the operator asked for. A checkpoint
		// that misses the window is recovered by the orphan sweeper.
		capCtx, capCancel := context.WithTimeout(context.Background(), interruptCheckpointGrace)
		defer capCancel()
		r.cancelAndAwaitCheckpoint(cur, capCtx)
		return nil
	}

	// Lame-duck (complete): let the in-flight run finish. The loop is
	// single-threaded and blocked inside processOne, and the run's
	// heartbeat keeps its NATS lease + ack window alive, so the pod stays
	// up (until the k8s grace ceiling) while the run runs to completion —
	// interrupting nothing.
	r.cfg.Logger.Info("runner: draining — letting in-flight run %s finish (lame-duck, ceiling %s)", cur.runID, r.cfg.DrainTimeout)
	select {
	case <-cur.done:
		r.cfg.Logger.Info("runner: in-flight run %s finished during drain", cur.runID)
	case <-ctx.Done():
		// Ceiling hit (or k8s grace about to SIGKILL): cap the run within a
		// short bounded window so it checkpoints + auto-resumes elsewhere.
		r.cfg.Logger.Warn("runner: drain ceiling reached for run %s — capping for checkpoint resume", cur.runID)
		capCtx, cancel := context.WithTimeout(context.Background(), interruptCheckpointGrace)
		defer cancel()
		r.cancelAndAwaitCheckpoint(cur, capCtx)
	}
	return nil
}

// cancelAndAwaitCheckpoint cancels the in-flight run so the engine unwinds
// via handleContextDoneWithCheckpoint (preserving the checkpoint), extends
// the ack window, and waits for processOne to finalise (promote to
// failed_resumable + nak). If waitCtx expires first it best-effort naks so
// JetStream redelivers to a sibling. Shared by the interrupt drain and the
// lame-duck ceiling cap.
func (r *Runner) cancelAndAwaitCheckpoint(cur *inFlight, waitCtx context.Context) {
	// Cancel WITH the interrupted cause so the engine writes failed_resumable
	// (auto-resume) rather than terminal cancelled — the shutdown-vs-operator
	// decision lives entirely in this cause.
	cur.cancelFn(runtime.ErrRunInterrupted)
	logDeliveryErr(r.cfg.Logger, "in-progress-shutdown", cur.runID, cur.delivery.InProgress())
	select {
	case <-cur.done:
		r.cfg.Logger.Info("runner: in-flight run %s interrupted + checkpointed for resume", cur.runID)
	case <-waitCtx.Done():
		logDeliveryErr(r.cfg.Logger, "nak-shutdown-grace", cur.runID, cur.delivery.Nak())
		r.cfg.Logger.Warn("runner: drain grace expired for run %s — naking for redelivery", cur.runID)
	}
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
	// Decouple the run's cancellation from the loop context: a SIGTERM
	// cancels loopCtx to stop fetching, but the in-flight run must NOT die
	// with it — Shutdown drives run cancellation explicitly per drain mode
	// (lame-duck lets it finish; interrupt cancels it via cur.cancelFn).
	// WithoutCancel keeps the OTel span/trace values while severing the
	// parent-chain cancel. WithCancelCause lets each cancel site stamp WHY
	// (drain/heartbeat → ErrRunInterrupted → resumable; operator subject →
	// ErrRunCancelled → terminal), which the engine reads via context.Cause.
	runCtx, runCancel := context.WithCancelCause(context.WithoutCancel(spanCtx))
	defer runCancel(nil)
	defer func() {
		span.SetAttributes(attribute.String("iterion.run.status", finalStatus))
		if finalStatus == "failed" || finalStatus == "lock_held" {
			span.SetStatus(codes.Error, finalStatus)
		}
		span.End()
	}()

	done := make(chan struct{})
	inflight := &inFlight{runID: msg.RunID, delivery: delivery, cancelFn: runCancel, done: done}
	r.mu.Lock()
	r.current = inflight
	r.mu.Unlock()
	// Registered LAST, so it runs FIRST on the way out. Beyond the close
	// ordering below, that position bounds the liveness probe's blind
	// spot: between clearing current and the loop's next tick the runner
	// reads "idle with an old tick", so this must not drift later in the
	// defer chain (health.go).
	defer func() {
		// Close before nilling: a Shutdown that captured the cur
		// pointer must always observe the channel close.
		close(done)
		r.mu.Lock()
		r.current = nil
		r.mu.Unlock()
	}()

	// Subscribe to the cancel subject for this run. A POST cancel on
	// the API publishes on iterion.cancel.<run_id>; we react by cancelling
	// runCtx with the OPERATOR cause, so the engine writes terminal
	// cancelled (never resurrected), distinct from a shutdown-drain cancel.
	if _, err := r.cfg.NATS.SubscribeCancel(runCtx, msg.RunID, func() { runCancel(runtime.ErrRunCancelled) }); err != nil {
		logger.Warn("runner: subscribe cancel %s: %v (continuing without)", msg.RunID, err)
	}

	// Live steering: register the override channel BEFORE the engine is
	// built (the engine picks it up at construction via
	// steerChannelFor), then subscribe the per-run steer subject. Same
	// lifecycle as the cancel subscription — torn down with runCtx.
	r.registerSteerChannel(runCtx, msg.RunID)
	defer r.unregisterSteerChannel(msg.RunID)
	if _, err := r.cfg.NATS.SubscribeSteer(runCtx, msg.RunID, func(body []byte, cmdID string) {
		r.handleSteerDelivery(msg.RunID, body, cmdID)
	}); err != nil {
		logger.Warn("runner: subscribe steer %s: %v (steering disabled for this run)", msg.RunID, err)
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

	// Heartbeat goroutine: refresh the NATS lease while we own it. On
	// refresh failure it cancels runCtx WITH the interrupted cause so the
	// engine unwinds to failed_resumable — better to lose progress than to
	// let the lease expire while the engine is still writing to Mongo (which
	// would invite split-brain when JetStream redelivers to a sibling pod).
	// The cause makes the redelivery auto-resume without manual intervention.
	hbDone := make(chan struct{})
	errtrack.Go("runner.heartbeat", func() { r.heartbeat(runCtx, runCancel, lock, delivery, hbDone) })
	// Cancel runCtx *before* waiting on hbDone, otherwise we deadlock:
	// heartbeat only exits on ctx.Done(), and the outer `defer runCancel`
	// at function entry is LIFO-last so it would run after this defer.
	// nil cause: the run has already returned terminally here, so this is
	// teardown — the engine never reads the cause. Idempotent panic net.
	defer func() {
		runCancel(nil)
		<-hbDone
	}()

	var usage *metricsEmitter
	err := r.executeRun(runCtx, msg, &usage)
	// Stop the heartbeat before finalizing (Ack/Nak) the delivery. The
	// heartbeat issues periodic InProgress() on this same delivery to
	// hold the JetStream ack deadline open; draining it here guarantees
	// no InProgress() lands after the terminal Ack/Nak below (which would
	// otherwise log a spurious already-acked error). A second drain in
	// the defer above is a no-op on the closed channel.
	runCancel(nil)
	<-hbDone

	// Run-outcome side effects (completion webhook + run.<outcome> event →
	// push notifications, chained triggers) fire ONLY when this delivery
	// reaches a FINAL disposition: an ack, or a park (usage-window retry /
	// DLQ). A plain Nak with redeliveries remaining means the platform
	// itself is about to auto-resume the run — firing there pushed one
	// "run failed" episode per redelivery (the episode key deliberately
	// folds updated_at so a LATER real re-failure notifies again), i.e. up
	// to MaxDeliver notifications for a single deterministic failure
	// within a minute. A non-operator interruption (runner drain / lost
	// heartbeat, ErrRunInterrupted) is excluded for the same reason on its
	// own path: the engine wrote failed_resumable and the redelivery
	// auto-resumes, so a fire here would ping on every deploy.
	outcome := classifyExecResult(err, msg.RunID)
	fireOutcome := func() {
		if !errors.Is(err, runtime.ErrRunInterrupted) {
			r.fireCompletionNotifier(msg)
			r.fireOutcomeEvent(msg, err)
		}
	}

	// The park branches below (usage-window / DLQ) are final by
	// construction and fire through the closure's interruption guard
	// alone; the plain dispatch path at the bottom consults
	// outcomeSideEffectsFire.

	// Checked BEFORE the DLQ park so a usage-window failure on the FINAL
	// delivery still arms a retry instead of being parked: the window is
	// exactly the condition that makes every prior delivery useless, so
	// reaching delivery 8 is evidence for retrying later, not against it.
	// Close the credential-pool lease unless JetStream is about to hand the
	// SAME sealed bundle to another pod — the only case where a later
	// attempt still runs on this lease and can report against it. A parked
	// delivery (usage-window retry, DLQ) is final for this bundle: an armed
	// retry re-publishes and acquires afresh, and a DLQ'd run never comes
	// back. Reporting `interim` there would strand the donor's slot and
	// committed allowance until the 12h lease TTL.
	if handled, retryStatus := r.parkUsageLimitRetry(runCtx, err, delivery, msg, logger); handled {
		r.recordPoolSpend(msg, usage, err, false)
		fireOutcome()
		finalStatus = retryStatus
		return
	}

	if handled, dlqStatus := r.parkOnDLQOnFinalDelivery(err, delivery, msg, logger); handled {
		r.recordPoolSpend(msg, usage, err, false)
		fireOutcome()
		finalStatus = dlqStatus
		return
	}

	// Interim only when JetStream will really redeliver. A Nak does not
	// imply one: ErrRunInterrupted (drain / lost heartbeat) is exempt from
	// the DLQ park above, so on its LAST permitted delivery it Naks into
	// nothing — and a lease left open there strands the donor's slot and
	// committed allowance until the 12h TTL.
	redeliverable := r.cfg.NATS != nil && delivery.NumDelivered() < r.cfg.NATS.MaxDeliver()
	r.recordPoolSpend(msg, usage, err, outcome.action == actionNak && redeliverable)

	if outcomeSideEffectsFire(err, outcome.action) {
		fireOutcome()
	}
	logAt(logger, outcome.level, outcome.logFmt, outcome.logArgs...)
	finalStatus = outcome.finalStatus
	dispatchTerminal(logger, delivery, outcome.action, outcome.op, msg.RunID)
}

// outcomeSideEffectsFire reports whether a delivery ending on the plain
// dispatch path (no park) is a FINAL disposition that must fire the
// run-outcome side effects (completion webhook + run.<outcome> event). A
// Nak is not final — JetStream redelivers and the run auto-resumes, so
// user-facing "run failed" episodes must wait for a disposition that
// actually settles the run. Named (rather than inlined) so the
// err → fires mapping is pinned by a table test next to
// TestClassifyExecResult.
func outcomeSideEffectsFire(execErr error, action deliveryAction) bool {
	return !errors.Is(execErr, runtime.ErrRunInterrupted) && action != actionNak
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

// fireOutcomeEvent publishes the run-outcome trigger.Event onto the events
// bus — the runner-side twin of runview.emitRunOutcome, sharing the same
// trigger.BuildRunOutcome authority. Fired after the execution attempt with
// the checkpoint already persisted, so a human-input pause carries its
// pending interaction_id. Best-effort: a publish failure is logged, never
// fails the delivery (the usernotify sweep reconciles missed episodes).
func (r *Runner) fireOutcomeEvent(msg *queue.RunMessage, execErr error) {
	if r.cfg.Events == nil {
		return
	}
	ectx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	ev := trigger.BuildRunOutcome(ectx, r.cfg.Store, msg.RunID, execErr)
	if err := r.cfg.Events.Publish(ectx, ev); err != nil {
		r.cfg.Logger.Warn("runner: publish %s trigger event for run %s: %v", ev.Kind, msg.RunID, err)
	}
}

// executeRun hydrates the IR from the message, builds the runtime
// engine + Claw executor, then dispatches to Run or Resume based on
// the message shape.
//
// The return is named so the deferred spend accounting can see how the
// attempt ended: a credential pool must know whether it was a provider
// quota window or a rejected credential that stopped the run, not merely
// how much it cost.
// usageOut, when non-nil, receives the run's metrics emitter as soon as it
// exists so the CALLER can report pool spend once it knows the delivery's
// real disposition. The pool report cannot live in this function's defer:
// whether an attempt is the last one — parked on the DLQ rather than
// redelivered — is decided above, after this returns.
func (r *Runner) executeRun(ctx context.Context, msg *queue.RunMessage, usageOut **metricsEmitter) (execErr error) {
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
	if cleanup != nil {
		defer cleanup()
	}
	if credErr != nil {
		// A run whose publisher sealed credentials the runner cannot open
		// MUST NOT proceed without them: everything downstream fails on
		// symptoms that never name this cause (a credential-less clone, an
		// unauthenticated LLM call). Returning routes through the ordinary
		// nak/redelivery path — the sealed bundle deliberately survives
		// non-terminal outcomes precisely so a redelivery can re-fetch it.
		// A run with no bundle at all (SecretsRef == "") still proceeds:
		// injectCredentials returns nil for that legitimate case.
		return fmt.Errorf("runner: credentials inject %s: %w", msg.RunID, credErr)
	}

	// Usage cap, pre-flight. Placed after the credentials (which name the
	// subscription this run would draw on) and before the clone and the
	// container: at this point the run has cost nothing yet, so a capped
	// run parks for free instead of paying a workspace and one LLM call to
	// rediscover a ceiling another pod already measured.
	if capErr := r.usageCapPreflight(ctx, wf, msg, r.cfg.Logger); capErr != nil {
		return capErr
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
			errtrack.Go("runner.refreshFileSecrets", func() {
				r.refreshFileSecretsLoop(refreshCtx, msg.TenantID, creds.GenericRefs, fileSecrets)
			})
		}
	}
	// Same rotation problem for the token GIT uses. Independent of the file
	// secrets above: a repo-targeted run has a forge token even when the
	// workflow declares no `as: file` secret at all, and its push is the last
	// thing the run does — hours after the credential was minted.
	if msg.RepoURL != "" && workDir != "" {
		if ref := r.gitCredentialSecretRef(ctx); ref != "" {
			gitCredCtx, stopGitCred := context.WithCancel(ctx)
			defer stopGitCred()
			errtrack.Go("runner.refreshGitCredentials", func() {
				r.refreshGitCredentialsLoop(gitCredCtx, msg.TenantID, ref, msg.RunID, workDir, msg.RepoURL)
			})
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
		runLogger = r.cfg.Logger.WithWriter(io.MultiWriter(r.cfg.Logger.Writer(), w))
	}

	// DSL-declared supervisors run on the pod alongside the engine —
	// this path builds its engine directly (no runview.Launch), so it
	// wires the hub + coordinators itself, exactly like the CLI. The hub
	// is created BEFORE the executor so it also rides the backend-hook
	// seam (the only one carrying assistant_text / tool events). The
	// run-level override travels on the queue (msg.Supervisors); the
	// pod's ITERION_SUPERVISORS is the layer below.
	var superviseHub *supervise.EventHub
	if len(wf.Supervisors) > 0 && supervise.DeclaredEnabledOrWarn(msg.Supervisors, len(wf.Supervisors), runLogger) {
		superviseHub = supervise.NewEventHub()
	}
	var hookObservers []func(store.Event)
	if superviseHub != nil {
		hookObservers = []func(store.Event){superviseHub.Publish}
	}
	executor, usage, err := r.buildExecutor(ctx, msg, wf, runLogger, hookObservers)
	if err != nil {
		return err
	}
	if usageOut != nil {
		*usageOut = usage
	}
	// Charge the run's spend whatever the outcome — paused, cancelled and
	// failed attempts incurred real LLM spend. The credential pool's half is
	// reported by the caller instead, which alone knows whether this
	// delivery is the last one.
	defer func() { r.recordOrgSpend(msg, usage) }()

	engineOpts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithWorkflowHash(msg.WorkflowHash),
		runtime.WithWorkDir(workDir),
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
		// The launch-time decision, carried on the wire (schema v7). Without
		// it the pod resolves the guard from the workflow and its own (empty)
		// environment, so an operator's `--loop-budget-guard off` on a bot
		// that declares nothing would run guarded anyway.
		runtime.WithLoopBudgetGuard(msg.LoopBudgetGuard),
		// Recovery recipes. Every other host wires these (pkg/cli/run.go,
		// pkg/runview, pkg/dispatcher); the cloud runner did not, so
		// recovery.Classify was never called on the one surface that runs
		// unattended — no transient-network backoff, no compaction retry,
		// and every terminal failure reported as EXECUTION_FAILED whatever
		// its cause. Cloud runs got strictly less recovery than a local
		// `iterion run` of the same bot.
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
	}
	// Sandbox-run observer: registers the live sandbox Run so the mid-run
	// credential refreshers can write rotated tokens THROUGH into the
	// container (forfait credentials + the k8s workspace's git credential
	// copy — see sandbox_registry.go), and starts the sandboxed
	// file-secret refresh loop when the run carries refreshable file
	// secrets (a `sandbox: auto` run mounts them as a launch-time
	// snapshot — docker bind-mount / k8s Secret — so a long run would
	// otherwise push/comment with a dead token, #99).
	sbObsCtx, stopSbObs := context.WithCancel(ctx)
	defer stopSbObs()
	defer r.unregisterSandboxRun(msg.RunID)
	engineOpts = append(engineOpts, runtime.WithSandboxRunObserver(
		r.sandboxRunObserver(sbObsCtx, msg.RunID, msg.TenantID, r.sandboxFileSecretRefs(ctx, wf))))
	// Bundle resources: a bot-qualified run attaches its bundle so the
	// engine mirrors skills/ into <workspace>/.claude/skills AND
	// provisions the bot's devbox.json (host devbox provisioning — the
	// runner pod is the isolation boundary, no sandbox starts here),
	// exactly like a local `iterion run bots/<bot>` does. Best-effort:
	// an unresolvable bot id or a loose .bot just skips the bundle with
	// a warning — the run proceeds without skills or devbox tools.
	if msg.BotID != "" && len(r.cfg.BotsPaths) > 0 {
		if mainFile, rerr := botregistry.ResolveBotPath(msg.BotID, r.cfg.BotsPaths); rerr == nil {
			if b, berr := bundle.OpenDir(filepath.Dir(mainFile)); berr == nil {
				engineOpts = append(engineOpts, runtime.WithBundle(b))
			} else {
				r.cfg.Logger.Warn("runner: bot %q bundle open: %v (skills not mirrored, devbox tools not provisioned)", msg.BotID, berr)
			}
		} else {
			r.cfg.Logger.Warn("runner: bot %q not resolvable in %v (skills not mirrored, devbox tools not provisioned)", msg.BotID, r.cfg.BotsPaths)
		}
	}
	// Plugin/library skills the LAUNCHING instance resolved for us. This pod's
	// iterion home is ephemeral and empty, so local resolution would silently
	// find nothing but the compiled-in builtins; passing the payload (even
	// empty) makes it authoritative and suppresses that dead local lookup.
	if msg.Contributions != nil {
		engineOpts = append(engineOpts, runtime.WithContributions(contributionsFromWire(msg.Contributions)))
	}
	if msg.Resume != nil && msg.Resume.Force {
		// Force-resume must be applied at engine construction so the
		// hash-mismatch guard in pkg/runtime/resume.go reads the flag.
		// This was previously dropped on the floor.
		engineOpts = append(engineOpts, runtime.WithForceResume(true))
	}
	// Live steering: hand the engine the override channel processOne
	// registered for this run, so bump_loop / raise_budget commands
	// arriving on iterion.steer.<run_id> reach the execution loop.
	if steerCh := r.steerChannelFor(msg.RunID); steerCh != nil {
		engineOpts = append(engineOpts, runtime.WithOverrideChannel(steerCh))
	}
	// The engine's own events (chiefly node_recovery) carry classifications
	// nothing else reports. Observed through the engine's dedicated seam
	// rather than a store decorator: a decorator's method set is the
	// INTERFACE's, which would hide the concrete store's optional
	// capabilities (RunFilesStore, InteractionAnswerCAS, …) from the
	// type-assertion probes inside the engine — silently, since each one
	// degrades rather than errors.
	engineOpts = append(engineOpts, runtime.WithEventObserver(usage.observe))
	if superviseHub != nil {
		engineOpts = append(engineOpts, runtime.WithEventObserver(superviseHub.Publish))
		stopSup := supervise.StartDeclared(ctx, superviseHub, &supervise.StoreInjector{Store: r.cfg.Store},
			msg.RunID, supervise.SpecsFromWorkflow(wf, runLogger), runLogger)
		defer stopSup()
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
		// On an export-based sandbox (kubernetes) the clone at workDir is a
		// COPY streamed back from the pod — hold it against the pod-side
		// HEAD the engine captured, so a lost export can never read as a
		// clean "no commits" (run 01a02a4b).
		integ := engine.SandboxWorkspaceIntegrity()
		r.recordRunGitMeta(ctx, msg, workDir, gitBase, integ)
		// Bank a SUCCESSFUL repo-targeted run to the forge before the
		// clone is wiped: the worktree-finalization path never fires
		// here, so without this push a finished run's commits exist
		// nowhere the server can reach (runs merge: "nothing to merge").
		if runErr == nil {
			r.bankRepoWorkspace(ctx, msg, workDir, gitBase, integ)
		}
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
func (r *Runner) buildExecutor(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow, logger *iterlog.Logger, hookObservers []func(store.Event)) (runtime.NodeExecutor, *metricsEmitter, error) {
	spec, usage, err := r.executorSpec(ctx, msg, wf, logger, hookObservers)
	if err != nil {
		return nil, nil, err
	}
	exec, err := runview.BuildExecutor(spec)
	if err != nil {
		return nil, nil, err
	}
	return exec, usage, nil
}

// executorSpec assembles the pod's ExecutorSpec. Split from
// buildExecutor so the wiring is assertable: a binder missing here is a
// capability that silently dies on every cloud run (the supervisor-steer
// and operator-chat inbox was exactly that).
func (r *Runner) executorSpec(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow, logger *iterlog.Logger, hookObservers []func(store.Event)) (runview.ExecutorSpec, *metricsEmitter, error) {
	var zero runview.ExecutorSpec
	emitter, ok := r.cfg.Store.(model.EventEmitter)
	if !ok {
		return zero, nil, fmt.Errorf("runner: store does not satisfy model.EventEmitter")
	}
	// Wrap the emitter so LLM step + delegate events update the
	// iterion_llm_tokens_total / iterion_llm_cost_usd_total counters
	// (and the per-run totals) as they are written to Mongo. Wrapping
	// at the runner boundary keeps pkg/backend/model free of any
	// metrics dependency.
	usage := newMetricsEmitter(emitter, r.cfg.Metrics)
	vars, err := stringifyVars(msg.Vars)
	if err != nil {
		return zero, nil, err
	}
	spec := runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: wf,
		Vars:     vars,
		Store:    usage,
		RunID:    msg.RunID,
		// Backend-hook events (assistant_text, tool_*, llm_*) fire only
		// this seam — the declared-supervisor hub rides it.
		EventObservers: hookObservers,
		// The run-scoped logger (teed into the RunLogStore) — the executor
		// emits the `[node#iter/backend]` agent-stream lines the studio's
		// per-node Logs tab filters on; on the raw pod logger they would
		// reach stdout but never the persisted run.log.
		Logger:   logger,
		StoreDir: r.cfg.WorkDir,
		BotID:    msg.BotID,
		// The launch-time decision, carried on the wire. Without it the pod
		// would resolve auto-memory from the workflow and its own (empty)
		// environment, so an operator's `--auto-memory off` on a bot whose
		// DSL says `on` would run with memory on — the knob failing open.
		AutoMemory:  msg.AutoMemory,
		MemoryStore: r.cfg.MemoryStore,
		// The operator's subscription ceiling, published to the shared
		// store as this run measures it — the pod is where the provider's
		// telemetry is observable, and the only place it can be captured.
		UsageGuard: r.usageGuardFor(ctx, msg, logger),
		// The operator's launch-time model/backend pins, replayed from the
		// wire. Before this, the cloud path persisted them display-only:
		// the studio showed an override the delegates never honoured.
		ModelOverrides: modelOverridesFromMsg(msg.ModelOverrides),
		// Inbox/AsyncAsk drain the run's queued messages into the agent's
		// live turn — supervisor steering and operator chat both ride
		// them. Every other launch surface binds these; without them the
		// pod's supervisors evaluate and inject but nothing ever delivers
		// (and ask_user_async has no store to post to). Publish stays nil
		// on cloud: the Mongo change-stream surfaces the transitions.
		Inbox:    &model.StoreInboxBinder{Store: r.cfg.Store},
		AsyncAsk: &model.StoreAsyncAskBinder{Store: r.cfg.Store},
	}
	return spec, usage, nil
}

// stringifyVars converts the wire payload's free-form vars into the
// string-typed map the executor expects. Non-string scalars are
// formatted with %v; nested structures (maps, slices, structs, …) are
// JSON-encoded so the downstream template engine can still see them.
// A nested value JSON cannot encode surfaces as an error — never
// silently degraded to Go %v syntax.
func stringifyVars(in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
			out[k] = ""
		default:
			switch reflect.ValueOf(v).Kind() {
			case reflect.Bool,
				reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
				reflect.Float32, reflect.Float64,
				reflect.Complex64, reflect.Complex128,
				reflect.String:
				out[k] = fmt.Sprintf("%v", t)
			default:
				b, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("runner: JSON-encode var %q (%T): %w", k, v, err)
				}
				out[k] = string(b)
			}
		}
	}
	return out, nil
}

// modelOverridesFromMsg folds the wire pins into the executor's override
// set — the runner-side twin of runview's launch-entry fold, so a cloud
// run resolves per-node models exactly like a local launch with the same
// flags.
func modelOverridesFromMsg(entries []queue.ModelOverride) model.ModelOverrides {
	var o model.ModelOverrides
	for _, e := range entries {
		if e.Backend != "" {
			o.SetBackend(e.Selector, e.Backend)
		}
		if e.Model != "" {
			o.SetModel(e.Selector, e.Model)
		}
		if e.Provider != "" {
			o.SetProvider(e.Selector, e.Provider)
		}
	}
	return o
}
