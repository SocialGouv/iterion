// Package metrics centralises the Prometheus metrics exposed by the
// cloud-mode iterion server and runner pods. A single registry is
// shared across both binaries so the Helm chart's PodMonitor scrapes
// both endpoints with the same metric names — operators tune
// alerts on iterion_* without caring whether the value comes from
// a server pod or a runner pod.
//
// Plan §F (T-40).
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Registry is the shared metric registry. Tests reset it between
// runs via NewForTesting; production code calls New() once at boot
// and reuses the same registry across the server + runner stacks of
// metrics defined here.
type Registry struct {
	reg *prometheus.Registry

	// --- Server-side metrics (plan §F T-40) ----------------------
	RunsCreatedTotal      *prometheus.CounterVec   // by status
	RunsActive            *prometheus.GaugeVec     // by status
	RunDurationSeconds    *prometheus.HistogramVec // by status
	WSConnections         prometheus.Gauge
	MongoChangeStreamLagS prometheus.Gauge

	// --- Runner-side metrics -------------------------------------
	NATSPendingMessages    prometheus.Gauge
	NATSStalled            prometheus.Gauge
	WorkspaceCloneDuration prometheus.Histogram
	LLMTokensTotal         *prometheus.CounterVec // backend, model, direction
	LLMCostUSDTotal        *prometheus.CounterVec // backend, model
	// DelegateIdleDeadlockTotal counts delegate sessions classified as
	// blocked on an orchestration tool (TaskOutput/Monitor) with no
	// background work to wait on, by outcome: recovered in place, or
	// aborted for a node restart. The number an admission decision about a
	// served model family can rest on.
	DelegateIdleDeadlockTotal *prometheus.CounterVec // backend, model, outcome (recovered|aborted)
	// DelegateBackgroundTotal counts steps of the lifecycle that keeps a
	// delegate session open for the background work it launched.
	DelegateBackgroundTotal *prometheus.CounterVec // backend, phase (waiting|settled|finalizing|abandoned)
	RunnerHeartbeatErrors   prometheus.Counter
	RunnerAdmissionRejected *prometheus.CounterVec // reason (schema|future_epoch)
	RolloutEpochRegression  *prometheus.CounterVec // component (server|runner)

	// --- Control-plane metrics ------------------------------------
	// Deliberately NO tenant labels anywhere (cardinality discipline);
	// per-org accounting lives in the Mongo orgusage counters.
	WebhookDeliveriesTotal  *prometheus.CounterVec // provider, status
	WebhookThrottledTotal   *prometheus.CounterVec // provider, reason (rate_limited|quota_exceeded|backlog_full)
	AuthLoginsTotal         *prometheus.CounterVec // result (success|invalid|locked|password_change_required|error)
	AuthPasswordResetsTotal *prometheus.CounterVec // step (requested|confirmed)
	LaunchDeniedTotal       *prometheus.CounterVec // reason (org_suspended|monthly_run_quota_exceeded|…)
	RunsOrphanRecovered     prometheus.Counter
	// OrphanSweepErrors counts sweep-pass steps that could not do their
	// job (scan failed, lease state unknown, CAS flip failed), by stage.
	// Flat at 0 with RunsOrphanRecovered also flat is health; a growing
	// error count is the sweeper silently disarmed — the state a
	// success-only counter cannot distinguish from "nothing to do".
	OrphanSweepErrors *prometheus.CounterVec // stage (scan|lease|flip)
	DLQDepth          prometheus.Gauge
	DLQDepthByPool    *GaugeSnapshot // pool; registry-known non-disabled pools
	// Provider usage-window retries. The pair is what makes the wait
	// auditable: scheduled counts runs parked for a later reset, resumed
	// counts the ones a sweeper actually brought back. A growing gap means
	// runs are being armed and never resumed — the failure mode this whole
	// path exists to avoid, and one that is otherwise silent because a
	// parked run just sits there as failed_resumable.
	RunsUsageWindowBlocked prometheus.Counter
	RunsRetryScheduled     prometheus.Counter
	// RunsRetryResumed counts the sweeper's passes on a due run by result
	// (enqueued|abandoned|failed|slot_wait|window_wait|release_deferred), and
	// the frozen-program annotations counted beside a pass's own result:
	// frozen_readmitted (a park's frozen program republished — a capacity
	// park's at once, a usage-window park's after its bot's current program
	// was refused for a changed source), frozen_stale (a record that no
	// longer binds its run, ignored), frozen_unavailable (a record whose
	// envelope is gone or corrupt, ignored).
	RunsRetryResumed *prometheus.CounterVec
	RunsRetryPending prometheus.Gauge
	// RunsRetrySweeps is what makes the three above readable. Every one of
	// them sits at 0 on a healthy idle deployment — and also on a deployment
	// where the sweeper never started, because a registered-but-never-Set
	// gauge reports 0 either way. A rising sweep count is the only thing that
	// tells those two apart, and it is the difference between "no run is
	// waiting" and "every waiting run is stranded".
	RunsRetrySweeps prometheus.Counter
	// RunsRetryProbes counts the not-yet-due retries a pass pulled forward
	// so the runner's usage preflight could answer early, BY what the probe
	// then found. It is the cost of the pull-forward probe made visible —
	// each pull is a pod — and the label is what makes it a cost/benefit
	// reading rather than a cost one: see RetryProbePulled.
	RunsRetryProbes *prometheus.CounterVec // result (pulled|still_blocked|stood_down)

	// --- Platform bot overrides (#2354) --------------------------
	// A stored platform override outranks the baked catalog at every
	// launch surface, forever: a bundle pushed once keeps serving after a
	// later release bakes a NEWER one for the same slug. The server
	// reported that on the admin inventory and in one resolve-time Warn,
	// and both are passive — a human has to look, or a run has to launch.
	// These three are the poll that looks on its own.
	//
	// Only the PLATFORM tier is measured: the bot-source store offers no
	// cross-tenant listing, so a per-team sweep would be one Mongo list
	// per team per pass. The team tier keeps the inventory field and the
	// resolve-time Warn.
	BotOverrideShadowsNewer *prometheus.GaugeVec // slug
	// BotShadowCheckUnavailable is what keeps the gauge above readable.
	// Every slug sits at 0 both on a deployment whose overrides are all
	// current AND on one whose catalog could not be read at all — a check
	// that could not run must never read clean, so an unreadable leg
	// raises this and HOLDS the per-slug values rather than zeroing them.
	BotShadowCheckUnavailable prometheus.Gauge
	// BotOverrideUnorderableVersion is the class the comparison cannot
	// judge: Manifest.version is free-form, so a suffixed pin
	// (0.9.2-codex-claw.3) orders against nothing, and a manifest is
	// OPTIONAL on both sides — a bundle pushed without one, or a baked
	// loose .bot, declares no version to compare. None of those is ever
	// flagged as shadowing, and none of them may read as a clean row
	// either: the override outranks the tier below it exactly as a dated
	// pin does, and the question is simply unanswered. INFO only — no
	// alert rule reads it. A deliberate pin is a legitimate operator
	// choice, and paging on one iterion cannot even order would be a page
	// nobody can clear.
	BotOverrideUnorderableVersion *prometheus.GaugeVec // slug

	// --- Shared gateway capacity (ADR-167 T2) ----------------------
	// GatewayAliveRuns is, per gateway host, the alive subbot trees whose
	// runs hold its capacity subject `gw:<host>` (a tree is one place,
	// ADR-167 §5), all teams together — the reading an operator sizes the
	// gateway's bound on. The label is the host, not a tenant: the gateways
	// with alive runs are few, and a host with none has no series (a whole
	// pass replaces the vector).
	GatewayAliveRuns *GaugeSnapshot // host
	// GatewayAliveRunsUnavailable keeps the gauge above readable: a host
	// absent there reads "no alive run" both on a quiet gateway and on a
	// replica whose read failed, so a failed or timed-out read raises this
	// and HOLDS the per-host values. A SampledGauge: only the process that
	// samples (the server) publishes it, so a runner pod sharing the
	// registry never reports an availability nobody measured.
	GatewayAliveRunsUnavailable *SampledGauge

	// --- Capacity transit (ADR-167 T4a) -----------------------------
	// CapacityReassertTotal counts the runner's re-assertions of the places
	// a delivery's admission reserved, by stage (delivery: at pickup, under
	// the run's lock; pre_engine: before the engine loads a delivery whose
	// clone outlasted a quarter of the hold; claim: at the engine's claim),
	// outcome (held, missing: the place is gone and the run parks, error:
	// the ledger did not answer, expired: the hold lapsed before the claim,
	// legacy: a message from a publisher older than the guard, resumed
	// unguarded, unheld_out_of_transit: a delivery that resumes a run holding
	// nothing — neither alive nor in its transit — while the run's stamp names
	// a subject its message holds no place for — counted per such subject, the
	// delivery proceeding)
	// and kind (key|gateway). legacy must read 0 before the per-gateway bound
	// moves to enforce.
	CapacityReassertTotal *prometheus.CounterVec // stage, outcome, kind
	// CapacityTransitEndTotal counts the transits a delivery ended itself
	// because it resumed nothing (restored: the continuation put back;
	// claimed_or_moved: the run had moved on and nothing was written).
	CapacityTransitEndTotal *prometheus.CounterVec // outcome
	// CapacityLaunchReassertTotal counts the launch's re-assertion of its
	// admission right before publishing (held; reserved_again: the entry was
	// gone and a new reservation took a place; parked: none was free;
	// error: the ledger did not answer and the launch failed).
	CapacityLaunchReassertTotal *prometheus.CounterVec // outcome
	// CapacityFrozenProgramTotal counts what a runner's park did with the
	// program of the delivery it parked, by source — capacity (a lost
	// capacity place) or usage_window (a provider usage window) — and
	// outcome: written (a started run, parked with its program frozen for
	// the retry sweeper's re-admission), not_started (nothing ran, nothing
	// frozen: the re-admission resumes the bot's current program),
	// freeze_failed and hash_mismatch (a started run parked without its
	// program — its re-admission resumes the current program, refused if it
	// changed). written and not_started count a park once it lands; a failed
	// freeze is counted when it fails, a park the store then refused
	// included.
	CapacityFrozenProgramTotal *prometheus.CounterVec // source, outcome
}

// Outcomes and stages of CapacityReassertTotal and its siblings.
const (
	CapacityStageDelivery  = "delivery"
	CapacityStagePreEngine = "pre_engine"
	CapacityStageClaim     = "claim"

	CapacityHeld    = "held"
	CapacityMissing = "missing"
	CapacityError   = "error"
	CapacityExpired = "expired"
	CapacityLegacy  = "legacy"
	// CapacityUnheldOutOfTransit is a delivery stage's count, per subject, of
	// a resume of a run holding nothing on a subject its message does not
	// hold (runner countUnheldOutOfTransit).
	CapacityUnheldOutOfTransit = "unheld_out_of_transit"

	CapacityKindKey     = "key"
	CapacityKindGateway = "gateway"
	// CapacityKindNone is the kind of a count that names no subject (legacy).
	CapacityKindNone = "none"

	CapacityTransitRestored       = "restored"
	CapacityTransitClaimedOrMoved = "claimed_or_moved"

	CapacityLaunchHeld          = "held"
	CapacityLaunchReservedAgain = "reserved_again"
	CapacityLaunchParked        = "parked"
	CapacityLaunchError         = "error"

	FrozenProgramWritten      = "written"
	FrozenProgramNotStarted   = "not_started"
	FrozenProgramFreezeFailed = "freeze_failed"
	FrozenProgramHashMismatch = "hash_mismatch"

	// The sources of CapacityFrozenProgramTotal: the values of
	// store.FrozenProgramSourceCapacity and store.FrozenProgramSourceUsageWindow,
	// restated so this package stays a leaf (a runner test pins them equal).
	FrozenProgramSourceCapacity    = "capacity"
	FrozenProgramSourceUsageWindow = "usage_window"
)

// The reasons of WebhookThrottledTotal.
const (
	WebhookThrottledRateLimited   = "rate_limited"
	WebhookThrottledQuotaExceeded = "quota_exceeded"
	WebhookThrottledBacklogFull   = "backlog_full"
)

// webhookProviders are the inbound webhook providers the server accepts
// (pkg/server supportedProviders, which a server test pins equal): the
// provider label of WebhookThrottledTotal.
var webhookProviders = []string{"gitlab", "github", "forgejo", "generic", "connector"}

// launchDeniedReasons are the admission gate's denial tokens (pkg/server's
// deny* constants, which a server test pins equal): the reason label of
// LaunchDeniedTotal.
var launchDeniedReasons = []string{
	"org_suspended",
	"monthly_run_quota_exceeded",
	"monthly_cost_cap_exceeded",
	"concurrency_cap_exceeded",
	"launch_rate_limited",
	"repo_quota_exceeded",
	"no_workspace",
	"member_role_required",
	"fork_team_unreadable",
	"fork_org_unreadable",
	"fork_metering_unavailable",
	"fork_metering_error",
	"fork_author_run_quota_exceeded",
	"fork_author_cost_cap_exceeded",
	"resume_run_unreadable",
	"fork_run_unfunded",
	"fork_funding_unreadable",
	"fork_funding_unjudgeable",
	"fork_lane_pool_refused",
	"maintenance_fence",
}

// The dispositions of one pull-forward probe, each counted at the ONE site
// that knows it:
//
//   - RetryProbePulled, at the pull itself (pkg/server.probeRetry): the pod
//     this probe will spend;
//   - RetryProbeStillBlocked, beside the free re-arm
//     (pkg/runner.armUsageWindowRetry): the resume reached the runner and the
//     usage preflight refused again — the window was exactly where the
//     provider said it was;
//   - RetryProbeStoodDown, at the sweeper's stand-down
//     (pkg/server.standDownProbedRetry): the resume never reached the runner
//     (admission deferred, publish refused, pool remapped), so the probe
//     observed nothing at all.
//
// `pulled` minus the other two is what the pass exists for: the probes whose
// run got PAST the preflight, which is the only outcome a probe cannot
// observe from the row — the resume that works simply runs, and the park that
// may follow it hours later is an ordinary new wall. An operator deciding
// whether ITERION_RETRY_PROBE_BUDGET earns its pods reads that difference.
const (
	RetryProbePulled       = "pulled"
	RetryProbeStillBlocked = "still_blocked"
	RetryProbeStoodDown    = "stood_down"
)

// New registers the metrics on a fresh registry. Each call gives a
// fully-isolated registry — convenient for tests, and called once at
// server/runner boot in production.
func New() *Registry {
	reg := prometheus.NewRegistry()
	r := &Registry{reg: reg}

	r.RunsCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_runs_created_total",
		Help: "Total number of runs accepted, broken down by terminal status.",
	}, []string{"status"})
	r.RunsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "iterion_runs_active",
		Help: "Current count of in-flight runs by status (running, queued, paused).",
	}, []string{"status"})
	r.RunDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "iterion_run_duration_seconds",
		Help:    "Wall-clock duration of completed runs, excluding queued + paused intervals.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 14), // 1s … ~16k s
	}, []string{"status"})
	r.WSConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_ws_connections",
		Help: "Number of currently connected run-console WebSocket clients.",
	})
	r.MongoChangeStreamLagS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_mongo_change_stream_lag_seconds",
		Help: "Seconds between event creation and change-stream delivery on the runview subscription.",
	})

	r.NATSPendingMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_nats_pending_messages",
		Help: "Pending JetStream messages on the iterion.queue.runs durable consumer.",
	})
	r.NATSStalled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_nats_stalled",
		Help: "1 when the runs consumer has held pending messages with no claim for the stall window — deliveries are wedged and runs will not start (issue #2890).",
	})
	r.WorkspaceCloneDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "iterion_workspace_clone_duration_seconds",
		Help:    "Time spent cloning the workspace repository before engine start.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms … ~400s
	})
	r.LLMTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_llm_tokens_total",
		Help: "LLM token usage by backend, model and direction (input/output/cache_read/cache_write).",
	}, []string{"backend", "model", "direction"})
	r.LLMCostUSDTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_llm_cost_usd_total",
		Help: "Cumulative LLM cost in USD by backend and model.",
	}, []string{"backend", "model"})
	r.DelegateIdleDeadlockTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_delegate_idle_deadlock_total",
		Help: "Delegate sessions classified as blocked on an orchestration tool (TaskOutput/Monitor) with no background work to wait on, by backend, model and outcome (recovered in place | aborted for retry).",
	}, []string{"backend", "model", "outcome"})
	r.DelegateBackgroundTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_delegate_background_total",
		Help: "Delegate sessions kept open for the background work they launched, by backend and lifecycle phase (waiting | settled | finalizing: iterion asked for the report before the work was done | abandoned: work that never reported back was lost with the session).",
	}, []string{"backend", "phase"})
	r.RunnerHeartbeatErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runner_heartbeat_errors_total",
		Help: "Number of NATS KV lease refresh failures encountered while a run was in flight.",
	})
	r.RunnerAdmissionRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_runner_admission_rejected_total",
		Help: "Run-message admissions rejected before execution, by bounded reason token.",
	}, []string{"reason"})
	r.RolloutEpochRegression = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_rollout_epoch_regression_total",
		Help: "Processes started below the persistent runner-epoch high-water mark.",
	}, []string{"component"})

	r.WebhookDeliveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_webhook_deliveries_total",
		Help: "Inbound webhook deliveries by provider and terminal status (launched/filtered/invalid/duplicate/launch_error).",
	}, []string{"provider", "status"})
	r.WebhookThrottledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_webhook_throttled_total",
		Help: "Inbound webhook deliveries rejected before processing, by provider and reason (rate_limited|quota_exceeded|backlog_full).",
	}, []string{"provider", "reason"})
	r.AuthLoginsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_auth_logins_total",
		Help: "Password login attempts by result.",
	}, []string{"result"})
	r.AuthPasswordResetsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_auth_password_resets_total",
		Help: "Self-service password reset flow progression (requested|confirmed).",
	}, []string{"step"})
	r.LaunchDeniedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_launch_denied_total",
		Help: "Run launches denied by the admission gate, by stable reason token.",
	}, []string{"reason"})
	r.RunsOrphanRecovered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runs_orphan_recovered_total",
		Help: "Stranded queued/running runs the sweeper flipped to failed_resumable.",
	})
	r.OrphanSweepErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_orphan_sweep_errors_total",
		Help: "Orphan-sweeper steps that failed (scan, lease probe, CAS flip). Growth means orphan recovery is degraded.",
	}, []string{"stage"})
	r.RunsUsageWindowBlocked = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runs_usage_window_blocked_total",
		Help: "Runs that failed because the LLM provider's quota window was exhausted.",
	})
	r.RunsRetryScheduled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runs_retry_scheduled_total",
		Help: "Automatic retries armed for a run waiting on a provider quota reset.",
	})
	r.RunsRetryResumed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_runs_retry_resumed_total",
		Help: "Outcomes of the retry sweeper acting on a due run, by result. frozen_readmitted, frozen_stale and frozen_unavailable annotate a park's frozen program (a capacity park's, re-admitted at once; a usage-window park's, re-admitted when its bot's current program is refused for a changed source) and are counted beside the pass's own result (enqueued, failed...), never instead of it.",
	}, []string{"result"})
	r.RunsRetryPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_runs_retry_pending",
		Help: "Runs currently armed for an automatic retry (sampled each sweep).",
	})
	r.RunsRetrySweeps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runs_retry_sweeps_total",
		Help: "Retry-sweeper passes completed. Flat at 0 means the sweeper is not running, which the other retry metrics cannot distinguish from an idle one.",
	})
	r.RunsRetryProbes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_runs_retry_probes_total",
		Help: "Pull-forward probes of a not-yet-due usage-window retry, by what the probe found: pulled (the pod spent), still_blocked (the preflight refused again), stood_down (its resume never reached the runner). pulled minus the other two found the window reopened.",
	}, []string{"result"})
	r.DLQDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_dlq_depth",
		Help: "Messages currently parked on the runs DLQ stream.",
	})
	r.BotOverrideShadowsNewer = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "iterion_bot_override_shadows_newer",
		Help: "1 when the platform bot override of this slug is strictly older than the bundle this deployment would otherwise serve, so the override is holding the newer bake back. Read both versions off `iterion remote admin bots` (bundle_version / shadowed_version); clear it by re-pushing the bundle or dropping the override.",
	}, []string{"slug"})
	r.BotShadowCheckUnavailable = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_bot_shadow_check_unavailable",
		Help: "1 when the override-shadow check could not run (the bot catalog or the platform rows could not be read). The per-slug gauges then hold their last values: a check that could not run must not read as a clean inventory.",
	})
	r.BotOverrideUnorderableVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "iterion_bot_override_unorderable_version",
		Help: "1 when the platform override of this slug and the bundle below it cannot be ordered — a free-form or suffixed version, or a side that declares no version at all — so no staleness verdict is made either way. Informational: no alert rule reads this series.",
	}, []string{"slug"})

	r.GatewayAliveRuns = newGaugeSnapshot(prometheus.GaugeOpts{
		Name: "iterion_gateway_alive_runs",
		Help: "Alive subbot trees holding gw:<host> (ADR-167 §5: a tree is one place), all teams together. Every server replica samples the same listing: aggregate with max over the replicas whose iterion_gateway_alive_runs_unavailable is 0, never sum — a blind replica holds its last values. A host with none has no series.",
	}, "host")
	r.GatewayAliveRunsUnavailable = newSampledGauge(prometheus.GaugeOpts{
		Name: "iterion_gateway_alive_runs_unavailable",
		Help: "1 until a pass succeeds and after any failed or timed-out read; the per-host series of iterion_gateway_alive_runs then hold their last values. Published only by a process that samples them (the server).",
	})
	r.CapacityReassertTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_capacity_reassert_total",
		Help: "Runner re-assertions of the capacity places a delivery's admission reserved (ADR-167 C2), by stage (delivery|pre_engine|claim), outcome (held|missing|error|expired|legacy|unheld_out_of_transit) and kind (key|gateway). legacy counts messages from a publisher older than the guard, resumed unguarded: it must read 0 before the per-gateway bound enforces. unheld_out_of_transit counts, per subject, a delivery that resumes a run holding nothing — neither alive nor in its transit: the hold lapsed or was never stamped, or an operator pause — while the run's stamp names a subject its message holds no place for: nothing re-asserts it, and the delivery proceeds.",
	}, []string{"stage", "outcome", "kind"})
	r.CapacityTransitEndTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_capacity_transit_end_total",
		Help: "Capacity transits a delivery ended itself because it resumed nothing: restored (the continuation put back) or claimed_or_moved (the run had moved on).",
	}, []string{"outcome"})
	r.CapacityLaunchReassertTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_capacity_launch_reassert_total",
		Help: "A launch's re-assertion of its capacity places right before it publishes (ADR-167 C3): held, reserved_again (the entry was gone, a new reservation took a place), parked (none was free) or error (the ledger did not answer, the launch failed).",
	}, []string{"outcome"})
	r.CapacityFrozenProgramTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_capacity_frozen_program_total",
		Help: "What a runner's park did with the program of the delivery it parked (ADR-167 T4a), by source (capacity: a lost capacity place; usage_window: a provider usage window) and outcome: written (a started run's program frozen for its re-admission), not_started (nothing ran, the re-admission resumes the current bot), freeze_failed or hash_mismatch (a started run parked without its program — its re-admission resumes the current program, refused if it changed). written and not_started count a park once it lands; a failed freeze is counted when it fails, a park the store then refused included.",
	}, []string{"source", "outcome"})
	r.DLQDepthByPool = newGaugeSnapshot(prometheus.GaugeOpts{
		Name: "iterion_dlq_depth_by_pool",
		Help: "Messages parked on each registry-known, non-disabled runner pool DLQ.",
	}, "pool")

	reg.MustRegister(
		r.RunsCreatedTotal, r.RunsActive, r.RunDurationSeconds,
		r.WSConnections, r.MongoChangeStreamLagS,
		r.NATSPendingMessages, r.NATSStalled, r.WorkspaceCloneDuration,
		r.LLMTokensTotal, r.LLMCostUSDTotal, r.DelegateIdleDeadlockTotal, r.DelegateBackgroundTotal, r.RunnerHeartbeatErrors,
		r.RunnerAdmissionRejected, r.RolloutEpochRegression,
		r.WebhookDeliveriesTotal, r.WebhookThrottledTotal,
		r.AuthLoginsTotal, r.AuthPasswordResetsTotal,
		r.LaunchDeniedTotal, r.RunsOrphanRecovered, r.OrphanSweepErrors, r.DLQDepth, r.DLQDepthByPool,
		r.BotOverrideShadowsNewer, r.BotShadowCheckUnavailable, r.BotOverrideUnorderableVersion,
		r.GatewayAliveRuns, r.GatewayAliveRunsUnavailable,
		r.RunsUsageWindowBlocked, r.RunsRetryScheduled,
		r.RunsRetryResumed, r.RunsRetryPending, r.RunsRetrySweeps, r.RunsRetryProbes,
		r.CapacityReassertTotal, r.CapacityTransitEndTotal, r.CapacityLaunchReassertTotal,
		r.CapacityFrozenProgramTotal,
	)
	r.createRuleChildren()
	return r
}

// createRuleChildren creates at 0 every child a chart rule reads through
// increase() or rate() (charts/iterion/templates/prometheus-rule.yaml), on
// every process at its boot. A child born by its first Inc is first scraped
// at 1, and increase() never sees that event: a single freeze failure, a
// single legacy resume per pod, would read 0. An event before a pod's first
// scrape stays invisible all the same.
func (r *Registry) createRuleChildren() {
	for _, reason := range launchDeniedReasons {
		r.LaunchDeniedTotal.WithLabelValues(reason)
	}
	for _, provider := range webhookProviders {
		for _, reason := range []string{WebhookThrottledRateLimited, WebhookThrottledQuotaExceeded, WebhookThrottledBacklogFull} {
			r.WebhookThrottledTotal.WithLabelValues(provider, reason)
		}
	}
	for _, stage := range []string{CapacityStageDelivery, CapacityStagePreEngine, CapacityStageClaim} {
		for _, kind := range []string{CapacityKindKey, CapacityKindGateway} {
			r.CapacityReassertTotal.WithLabelValues(stage, CapacityError, kind)
		}
	}
	r.CapacityReassertTotal.WithLabelValues(CapacityStageDelivery, CapacityLegacy, CapacityKindNone)
	r.CapacityLaunchReassertTotal.WithLabelValues(CapacityLaunchError)
	for _, source := range []string{FrozenProgramSourceCapacity, FrozenProgramSourceUsageWindow} {
		for _, outcome := range []string{FrozenProgramFreezeFailed, FrozenProgramHashMismatch} {
			r.CapacityFrozenProgramTotal.WithLabelValues(source, outcome)
		}
	}
}

// Handler returns an http.Handler suitable for mounting at /metrics.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{Registry: r.reg})
}

// Mount is an extra route served alongside /metrics on the same
// listener. The runner uses it for its probe endpoints, which the chart
// already points at this port.
type Mount struct {
	Path    string
	Handler http.Handler
}

// StartServer binds a dedicated HTTP listener for /metrics plus any
// extra mounts. Returns the *http.Server so the caller can Shutdown it
// cleanly. addr is host:port (e.g. ":9090"); empty addr disables the
// listener and returns nil, nil.
//
// On listener-bind failure StartServer returns the error
// synchronously so an operator who configured ITERION_METRICS_ADDR
// observes the gap at boot, not in a silent goroutine.
func (r *Registry) StartServer(addr string, logger *iterlog.Logger, mounts ...Mount) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	for _, m := range mounts {
		mux.Handle(m.Path, m.Handler)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: bind %s: %w", addr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if logger != nil {
				logger.Error("metrics: serve %s: %v", addr, err)
			}
		}
	}()
	if logger != nil {
		logger.Info("metrics: serving /metrics on %s", addr)
	}
	return srv, nil
}

// ShutdownTimeout is the bounded wait the StartServer-spawned listener
// gets before it is killed via context. Callers can wrap with their
// own context if they need a different budget.
const ShutdownTimeout = 5 * time.Second

// ShutdownServer is a convenience wrapper around srv.Shutdown that
// applies the package-level timeout.
func ShutdownServer(srv *http.Server) error {
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}
