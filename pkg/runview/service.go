package runview

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/notify"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/sessionboard"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// LaunchSpec describes a workflow invocation. Mirrors the inputs of
// `iterion run` but framed as data so HTTP handlers (and any future
// programmatic caller) construct it without going through cobra flags.
type LaunchSpec struct {
	FilePath string // absolute .bot path; sandbox check is the caller's job
	// Source carries the .bot source verbatim. Used by cloud-mode
	// callers when the server pod has no local copy of the workflow
	// (the studio SPA uploads the source inline). When non-empty it
	// takes precedence over FilePath for parsing; FilePath is still
	// retained for display and for the runner to recompile against
	// the same logical workflow.
	Source string
	Vars   map[string]string // --var-style overrides
	// Preset is the name of an in-source preset (presets: block) to
	// apply before Vars. Unknown name → launch error. Empty means no
	// preset.
	Preset  string
	RunID   string        // optional explicit ID; auto-generated when empty
	Timeout time.Duration // 0 disables
	// MergeInto controls the worktree-finalization fast-forward target
	// for `worktree: auto` runs. "" or "current" → FF the user's
	// currently-checked-out branch (default); "none" → skip FF;
	// <branch-name> → FF that branch (only honoured when it matches
	// the currently-checked-out branch).
	MergeInto string
	// BranchName overrides the default storage branch
	// `iterion/run/<friendly>` created on the worktree's HEAD.
	BranchName string
	// MergeStrategy selects how the run's commits are landed on the
	// merge target: "squash" (default — collapse into one commit) or
	// "merge" (fast-forward, preserve history). Persisted on run.json
	// so the deferred-merge UI can pre-fill the same choice.
	MergeStrategy store.MergeStrategy
	// AutoMerge captures the launch-time intent. When true, the engine
	// applies MergeStrategy synchronously at end of run; when false the
	// engine creates the storage branch only and leaves merge_status
	// pending for the UI to drive via POST /api/runs/{id}/merge.
	AutoMerge bool
	// AttachmentPromote, when set, is invoked after CreateRun and
	// before the engine starts. It is expected to materialise every
	// attachment declared in `attachments:` into the run-scoped store
	// (typically by promoting uploads from a staging area). Errors
	// abort the launch.
	AttachmentPromote runtime.AttachmentPromoteFunc
	// Backend, when non-empty, overrides the workflow's `default_backend:`
	// for this run. Node-level explicit `backend:` declarations still
	// win — this is a soft default override, not a hard force. Empty
	// preserves the resolver chain. NOTE: the detached runner path
	// (ITERION_RUNS_DETACHED=1) does not yet honor this field; the
	// service layer logs a warning and ignores it there.
	Backend string
	// Compress is the run-level command-output-compression override
	// ("", "on", "ultra", "off") from the studio Launch toggle. ""
	// inherits the workflow/node `compress:` DSL then ITERION_COMPRESS. Highest
	// priority input to rewrite.Resolve. See docs/plugins.md.
	Compress string
	// AutoMemory is the run-level auto-memory (MEMORY.md) override ("", "on",
	// "off") from the studio Launch toggle. "" inherits the workflow/node
	// `auto_memory:` DSL then ITERION_AUTO_MEMORY.
	AutoMemory string
	// LoopBudgetGuard is the run-level back-edge affordability override
	// ("", "on", "off"). "" inherits the workflow `loop_budget_guard:` then
	// ITERION_LOOP_BUDGET_GUARD.
	LoopBudgetGuard string
	// Supervisors is the run-level kill switch for DSL-declared
	// `supervisor NAME:` watchers ("", "on", "off"). "" inherits
	// ITERION_SUPERVISORS; the default is on. See docs/supervisors.md.
	Supervisors string
	// Permission is the run-level tool-permission-gate mode override
	// ("", "off", "ask", "deny") from the studio Launch toggle. ""
	// inherits the workflow/node `permission:` DSL then
	// ITERION_PERMISSION. See docs/permissions.md.
	Permission string
	// ReviewMode is the run-level mono/dual review-topology override
	// ("", "auto", "mono", "dual") from the studio Launch toggle /
	// dispatcher. Only affects bots that declare a review_mode var. ""
	// / "auto" resolves from detected provider credentials at launch;
	// "mono"/"dual" force it. See pkg/reviewtopology.
	ReviewMode string
	// RetryPolicy is the retry contract already RESOLVED across its layers
	// by the launch site (per-run override → launching surface → bot
	// manifest → machine default, clamped by the platform ceiling). It is
	// snapshotted verbatim onto the run document so the runner can read it
	// without knowing schedules or manifests exist. Nil = the consumer
	// applies pkg/retrypolicy's defaults.
	RetryPolicy *store.RunRetryPolicy
	// ModelOverrides are launch-time per-node/-group backend+model overrides
	// (studio Launch dropdowns). Each entry targets nodes by selector (node id,
	// id glob, or kind keyword agent|judge) and wins over the node's DSL
	// backend:/model:. Empty applies nothing. Composes with ReviewMode. See
	// model_override.go.
	ModelOverrides []ModelOverrideEntry
	// RoutingPolicy is the launch-frozen outcome contract (validated
	// and hashed by the HTTP layer); persisted on the run doc and
	// replayed from it on resume.
	RoutingPolicy *store.RoutingPolicy
	// Fallback is the operator's ordered run-level fallback chain (the
	// studio Launch row; a single CLI --fallback becomes a one-stage chain).
	// It applies to agent nodes that declare no `fallbacks:` of their own,
	// and never to judges —
	// a weaker judge still emits a well-formed verdict, so a blanket
	// launch setting must not reach one. Empty = none. See ADR-087.
	Fallback []FallbackEntry
	// Budget carries launch-time budget-cap overrides for the workflow's
	// `budget:` block — the HTTP equivalent of the CLI --max-cost-usd /
	// --max-tokens / --max-duration / --max-iterations /
	// --max-parallel-branches flags. Applied to the compiled workflow after
	// recipe/preset resolution and before the executor snapshots Budget
	// (non-zero field wins, zero inherits — see ir.ApplyBudgetOverrides).
	// The detached path forwards it as the CLI flags; the queued cloud
	// path publishes it on the RunMessage (clamped to the pool grant),
	// persists the raw ask on the run doc, and replays it on resume so
	// unattended auto-retries keep the cap the launch declared.
	Budget *ir.BudgetOverrides
	// ParentRunID, ShardIndex, ShardCount, ShardLabel are set when a
	// parent run dispatches this as a shard child (see Cap. 3 in
	// docs/security-bots-distributed.md). The cloudpublisher copies
	// them onto the persisted Run document AND onto the published
	// RunMessage so the runner pod that picks up the work knows it's
	// part of a sharded set.
	ParentRunID string
	ShardIndex  int
	ShardCount  int
	ShardLabel  string
	// CallbackURL, when set, is an http/https endpoint the engine POSTs
	// a run-completion webhook to when the run terminates (see
	// pkg/notify). Lets a programmatic caller (chat adapter, CI bridge)
	// be told the run finished without polling. Empty for CLI / studio.
	CallbackURL string
	// CallbackToken is an opaque value echoed back verbatim in the
	// completion payload so the receiver can correlate the callback to
	// the originating request (e.g. a chat thread id) without state.
	CallbackToken string
	// CallbackAnswerNode optionally names the node whose latest artifact
	// holds the run's user-facing answer (the "final_answer" field).
	// Empty → the notifier scans all artifact nodes for "final_answer".
	CallbackAnswerNode string
	// RepoURL / RepoRef, when set, propagate onto the published
	// RunMessage so a cloud runner clones the repo before sandboxing.
	// Used by webhook-launched runs (inbound MR review) where the
	// operator has no local checkout.
	RepoURL string
	RepoRef string
	// ProjectPath is the stable forge slug ("group/project") the run
	// targets, persisted on the run so the studio can filter/group runs
	// by repository. Set by inbound-webhook launches; empty otherwise.
	ProjectPath string
	// BotID is the bot bundle name (e.g. "review-pr") this run launches.
	// The cloud publisher uses it to resolve bot-secret bindings during
	// credential sealing. Empty for plain .bot launches.
	BotID string
	// BundleDir, when set, is the launch-materialized directory of a STORED
	// bot bundle (a team-authored bot or a platform override): compile merges
	// its prompts/ into the AST exactly like a baked bundle's, so they
	// participate in IR validation and the workflow hash. Server-owned temp
	// dir, cleaned up by the launch surface after Launch returns; never
	// persisted.
	BundleDir string
	// BotBundle is the stored-bundle ref the cloud publisher stamps on the
	// queue message so the runner rebuilds the SAME bundle from the store
	// (skills/, devbox.json, attachments) instead of attaching the stale
	// baked one. Nil for baked catalog bots and plain .bot launches.
	BotBundle *BotBundleRef
	// KeyOverrides pins a specific BYOK key per LLM provider for this run
	// (provider name → api_key id), overriding the org/user default in
	// secrets.Resolve. Set by webhook launches that carry per-webhook key
	// bindings; empty for normal launches. See docs/byok.md.
	KeyOverrides map[string]string
	// SecretOverrides pins a stored secret per workflow-secret name (name ->
	// secret id) for this run, overriding the org bot-secret binding. Set by
	// webhook launches carrying per-webhook secret bindings. See docs/byok.md.
	SecretOverrides map[string]string

	// --- Dispatcher-convergence fields (ADR-046) ---
	// These four carry the invariants the dispatcher's EngineRunner.Dispatch
	// needs so it can route its execution step through this single launch
	// authority without losing workspace / stall / cap / retry semantics. All
	// are per-launch overrides; empty/nil inherits the service-level default.

	// WorkDir overrides the service-level working directory for this launch
	// (runtime.WithWorkDir). The dispatcher sets it to the per-issue isolated
	// worktree so `${PROJECT_DIR}` in bot var defaults expands to that
	// worktree, not the daemon's cwd. Empty inherits WithWorkDir.
	WorkDir string
	// ExtraObservers are per-launch event observers fired on EVERY run
	// event — both the engine-level events runtime.WithEventObserver sees
	// AND the high-frequency tool_started/tool_called events the backend
	// hook layer emits (which bypass the engine callback) — matching the
	// dispatcher's stall-heartbeat semantics. The dispatcher wires one that
	// advances its last-event watermark for stall detection. Empty adds
	// none. Delivered through TWO disjoint seams — runtime.WithEventObserver
	// for engine events + ExecutorSpec.EventObservers for backend-hook
	// events — so no store wrapper is interposed (a wrapper would shadow the
	// store's optional capabilities against the executor/sandbox type-probes;
	// ADR-046).
	ExtraObservers []func(store.Event)
	// DailyCap, when non-nil, overrides the service-level per-(store, UTC-day)
	// spend-cap guard for this launch (runtime.WithDailyCap). The dispatcher
	// builds it from its SINGLETON SpendStore so every concurrent dispatched
	// run writes the one ledger, serialising on a single mutex. Nil inherits
	// the service's dailyCap.
	DailyCap *runtime.DailyCapGuard
	// SourceRef stamps who originated this run onto the run record
	// (runtime.WithSource); the studio RunHeader links back to it. The
	// dispatcher sets it to the kanban issue that triggered the dispatch. Nil
	// leaves Source unset (CLI / studio / fork launches).
	SourceRef *store.RunSource
	// PipelineTicketID is the native ticket this ROOT launch belongs to.
	// Purely a concurrency-gate hint: a ticket whose last run died sits in
	// the pipeline board's needs-attention lane and RESERVES a slot so
	// nothing takes the place it needs to restart into. Passing the id here
	// makes this launch CONSUME that ticket's own reservation instead of
	// being refused by it — without it, the card would deadlock behind the
	// slot it is holding for itself. Empty for every non-ticket launch.
	PipelineTicketID string
	// OnOutcome, when set, is invoked once with the run's terminal Go error
	// (nil on success, runtime.ErrRunPaused/ErrRunPausedOperator on a pause,
	// the failure error otherwise) just before the run goroutine closes
	// LaunchResult.Done. It is the return-path completion of the four data
	// fields above: a blocking caller (the dispatcher's EngineRunner routing
	// through this launch authority) reads the SAME typed error the direct
	// engine.Run would have returned, so its retry / park / sandbox-backoff
	// logic stays byte-identical. Nil for fire-and-forget CLI / studio /
	// webhook launches. Not honoured on the cloud-queue or detached paths.
	OnOutcome func(error)
}

// ModelOverrideEntry is one launch-time per-node/-group model+backend
// override directive (studio Launch dropdowns). Selector matches a node by
// exact id, id glob ("reviewer_*"), or kind keyword ("agent"|"judge"). Empty
// Backend/Model/Provider fields leave that dimension unchanged.
type ModelOverrideEntry struct {
	Selector string `json:"selector"`
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// FallbackEntry is one stage of the operator's ordered run-level
// fallback chain.
type FallbackEntry struct {
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// toRunFallback folds launch entries into the IR chain. Targetless
// entries stay in place so ApplyRunFallback can preserve stage indexes
// while screening every stage independently.
func toRunFallback(entries []FallbackEntry) []ir.Fallback {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ir.Fallback, 0, len(entries))
	for _, e := range entries {
		out = append(out, ir.Fallback{
			Name:     ir.RunFallbackName,
			Backend:  e.Backend,
			Model:    e.Model,
			Provider: e.Provider,
		})
	}
	return out
}

// toModelOverrides folds the launch entries into the engine's ModelOverrides.
func toModelOverrides(entries []ModelOverrideEntry) model.ModelOverrides {
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

// toRunModelOverrides converts the launch entries into the persisted,
// display-only representation stamped on the run record (so the studio
// Overview can show what a run was launched with). Distinct from
// toModelOverrides, which builds the engine's live override set applied
// to the executor.
func toRunModelOverrides(entries []ModelOverrideEntry) []store.RunModelOverride {
	if len(entries) == 0 {
		return nil
	}
	out := make([]store.RunModelOverride, 0, len(entries))
	for _, e := range entries {
		out = append(out, store.RunModelOverride{
			Selector: e.Selector,
			Backend:  e.Backend,
			Model:    e.Model,
			Provider: e.Provider,
		})
	}
	return out
}

// BotBundleRef identifies a STORED bot bundle (pkg/botsource row) by its
// tenant scope — a team id, or botsource.PlatformTenantID for a
// deployment-wide override — plus slug and the row version resolved at
// launch. The runner fetches the row, VERIFIES the version still matches
// (a push racing the launch fails the run loudly instead of pairing this
// launch's IR with newer resources), and materializes it as the run's
// bundle.
type BotBundleRef struct {
	TenantID string `json:"tenant_id"`
	Slug     string `json:"slug"`
	Version  int    `json:"version"`
}

// ResumeSpec describes a resume request.
type ResumeSpec struct {
	RunID    string
	FilePath string // .bot file (loaded fresh; must match the run's WorkflowHash unless Force)
	// Source mirrors LaunchSpec.Source: cloud-mode callers can supply
	// the .bot contents inline so the server pod does not need to
	// resolve FilePath against a local filesystem.
	Source string
	// BundleDir / BotBundle mirror LaunchSpec's fields: a cloud resume of a
	// stored bot re-resolves the bundle fresh (like credentials are
	// re-sealed), so the compile merge and the runner-side materialization
	// stay consistent with THIS resume's source.
	BundleDir string
	BotBundle *BotBundleRef
	Answers   map[string]any // answers for human nodes; ignored for failed_resumable
	Force     bool           // skip workflow hash check
	Timeout   time.Duration  // 0 disables
	// AutoMemory re-states the run-level auto-memory override ("", "on",
	// "off"). It is not inherited from the original launch: overrides are not
	// persisted on the run, so a resume that said nothing would silently fall
	// back to the workflow's own value — turning memory on for a run the
	// operator had launched hermetically.
	AutoMemory string
	// LoopBudgetGuard re-states the run-level back-edge affordability
	// override ("", "on", "off"), for the same reason AutoMemory does.
	LoopBudgetGuard string
	// Supervisors re-states the run-level supervisors kill switch
	// ("", "on", "off"), for the same reason AutoMemory does.
	Supervisors string
	// Automatic marks a machine-initiated resume — the retry sweeper, or any
	// caller resuming without an operator's request: the resume gate then
	// applies CanAutoResume() instead of CanOperatorResume(). CanAutoResume
	// excludes cancelled (an operator's cancel is a decision automation never
	// overrides), so a cancel landing between a sweeper's claim and its
	// publish is refused instead of being flipped back to queued by the CAS —
	// which clears run.Error, the only PR-closed marker the runner admission
	// reads. Operator surfaces (HTTP resume, WS answers, chat commands,
	// studio, MCP) leave this false.
	Automatic bool
}

// RunSummary is the lightweight per-row shape returned by List.
// Heavier fields (events, artifacts, checkpoint detail) live in
// RunSnapshot — call Snapshot for the full view.
type RunSummary struct {
	ID string `json:"id"`
	// Name is the deterministic, human-friendly label for the run.
	// Empty for legacy runs persisted before this field existed.
	Name         string `json:"name,omitempty"`
	WorkflowName string `json:"workflow_name"`
	// BundleName is the bot/bundle label (e.g. "docs-refresh"). Sourced
	// from the persisted Run.BundleName; falls back server-side to
	// basename(BundlePath) (stripped of `.botz`) for legacy runs.
	// Empty for plain .bot runs with no bundle.
	BundleName string `json:"bundle_name,omitempty"`
	// BundleDisplayName is the bot's persona label (e.g. "Nexie") from
	// the bundle manifest. Empty for plain runs; the studio falls back
	// to BundleName then WorkflowName. Carried so the run list can
	// render readable bot-filter chips without a per-run fetch.
	BundleDisplayName string `json:"bundle_display_name,omitempty"`
	// SourceKind classifies how the run was triggered, for list filtering /
	// grouping: "manual" | "webhook" | "dispatcher" | "fork" | "shard".
	// Derived server-side from the run's source/owner; never persisted.
	SourceKind string          `json:"source_kind,omitempty"`
	Status     store.RunStatus `json:"status"`
	FilePath   string          `json:"file_path,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Error      string          `json:"error,omitempty"`
	// FailureCode is Error's machine-readable classification (ADR-095);
	// empty = unknown/legacy.
	FailureCode store.FailureCode `json:"failure_code,omitempty"`
	// Active reports whether the run is currently held by this
	// process's manager. A run with status "running" but Active=false
	// belongs to another process or to a previous boot — Cancel won't
	// reach it from here.
	Active bool `json:"active"`
	// RetryAfter is when a failed_resumable run will be resumed
	// automatically, once its provider quota window reopens. Nil means no
	// retry is armed — which is the whole point of surfacing it: a row
	// that will resume in 33h and a row that never will are otherwise
	// indistinguishable, both just "failed_resumable". RetryAttempts is
	// the run's position in its attempt budget.
	RetryAfter    *time.Time `json:"retry_after,omitempty"`
	RetryAttempts int        `json:"retry_attempts,omitempty"`
	// Worktree finalization summary (only populated for `worktree:
	// auto` runs that reached a clean exit). See store.Run for the
	// full semantics.
	FinalCommit      string              `json:"final_commit,omitempty"`
	FinalBranch      string              `json:"final_branch,omitempty"`
	FinalBranchError string              `json:"final_branch_error,omitempty"`
	MergedInto       string              `json:"merged_into,omitempty"`
	MergedCommit     string              `json:"merged_commit,omitempty"`
	MergeStrategy    store.MergeStrategy `json:"merge_strategy,omitempty"`
	MergeStatus      store.MergeStatus   `json:"merge_status,omitempty"`
	AutoMerge        bool                `json:"auto_merge,omitempty"`
	// WorkDir / RepoRoot locate where a local run executed: WorkDir is
	// the absolute exec dir (worktree path or inherited cwd), RepoRoot
	// the main git repo it was forked from. The studio uses these to
	// offer a "by folder" run filter in desktop/local mode. Empty for
	// cloud runs (which carry ProjectPath instead) and legacy runs.
	WorkDir  string `json:"work_dir,omitempty"`
	RepoRoot string `json:"repo_root,omitempty"`
	// ProjectPath is the stable forge slug ("group/project") for
	// cloud webhook-launched runs, powering the "by repo" filter.
	// Empty in local mode.
	ProjectPath string `json:"project_path,omitempty"`
	// QueuePosition is set only for cloud-mode runs whose Status is
	// "queued"; nil otherwise. 1 means "next to be picked up". Computed
	// by the server (Mongo aggregation), not persisted on the run doc.
	// See cloud-ready plan §F (T-03, T-31).
	QueuePosition *int `json:"queue_position,omitempty"`
	// Run-tree shard tuple (T4b, refs #125): the child←parent edge plus
	// the shard coordinates mirrored from the queue message. Empty for a
	// top-level (non-sharded) run. Carried so the run list / children
	// endpoint can project a run's shard/child subtree without a per-run
	// fetch. See store.Run.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// ParentNodeID is the IR node id of the subbot node in the parent
	// workflow that spawned this child run; empty for root runs and
	// non-subbot children. See store.Run.ParentNodeID.
	ParentNodeID string `json:"parent_node_id,omitempty"`
	ShardIndex   int    `json:"shard_index,omitempty"`
	ShardCount   int    `json:"shard_count,omitempty"`
	ShardLabel   string `json:"shard_label,omitempty"`
}

// ListFilter scopes a List request. Empty fields mean no filter.
type ListFilter struct {
	Status   store.RunStatus // exact match
	Workflow string          // exact match on WorkflowName
	Repo     string          // exact match on ProjectPath (cloud repo slug)
	// Bundle filters runs to those whose resolved bundle name (persisted
	// BundleName, falling back to basename(BundlePath) minus ".botz" —
	// see resolveBundleName) matches case-insensitively. Wire name: ?bot=.
	Bundle string
	Since  time.Time // UpdatedAt >= Since
	Limit  int       // 0 = no limit
	// Node filters runs to those whose persisted events include at
	// least one node_started for this IR node ID. Used by the studio
	// to surface "this node was touched by N runs" without scanning
	// every run on the client. Scanning happens at request time —
	// fine for hundreds of runs; wire an inverted index later if the
	// store grows past low thousands.
	Node string
}

// ArtifactSummary is the lightweight shape returned by ListArtifacts —
// just enough for the UI to populate a version selector without
// reading every artifact body.
type ArtifactSummary struct {
	Version   int       `json:"version"`
	WrittenAt time.Time `json:"written_at"`
}

// Service is the canonical façade over runtime + store + broker +
// manager. The HTTP server, the studio, and (optionally) the CLI all
// route through here — keeping a single source of truth for run
// lifecycle, validation, and event fan-out.
type Service struct {
	store    store.RunStore
	storeDir string
	// workspaceTracker versions the files a run produces, so a rewind can
	// undo a node's real work — the output map is only a summary for a
	// bot whose product is documentation or code. Filesystem-backed, so
	// nil in cloud mode (no local store dir) and for injected stores.
	workspaceTracker workspacetrack.Tracker
	// workspaceTrackDisabled is the per-service opt-out (see
	// WithoutWorkspaceTracking), checked alongside the env default.
	workspaceTrackDisabled bool
	// boardMCPHandler serves the board MCP routes for the per-run
	// gateway-reachable listener (C082); boardRegister mints per-node
	// board tokens. Both nil unless the server wires them via
	// WithBoardMCP — sandboxed board-emit then stays disabled.
	boardMCPHandler http.Handler
	boardRegister   func(caps []string, sourceIssueID string) string
	// workDir is the directory the engine should treat as ${PROJECT_DIR}
	// and as the repo-lookup seed for worktree: auto. Empty means
	// "default to os.Getwd() at Run() time" — the right thing for the
	// CLI (which runs in the user's cwd) but wrong for the desktop
	// server (whose process cwd is the user's home).
	workDir string
	logger  *iterlog.Logger
	broker  *EventBroker
	manager *Manager

	// skipRunLogged dedupes the "skip run" diagnostic emitted when a
	// listed run fails to load: the same stale/corrupt id is reported
	// once per service lifetime instead of on every UI poll.
	skipRunLogged sync.Map

	// sandboxDefault is the global sandbox default injected into every
	// in-process engine this service launches (see WithSandboxDefault).
	// Empty = neutral (no default), the contract tests rely on.
	sandboxDefault string

	// usageCapSource, when non-nil, is the LIVE usage-cap policy source
	// (the DB-backed runtime-settings resolver) consulted by the launch
	// preflight and threaded into every in-process executor's guard —
	// see WithUsageCapSource. Nil keeps the env-only resolution.
	usageCapSource usagecap.PolicySource

	// sbStore persists per-run Session-board specs (the LLM curation
	// layer's output). Nil when no on-disk store dir is available (cloud
	// mode) — curation then stays disabled. The deterministic task-list
	// board (Phase 1) is unaffected; it lives entirely in the studio.
	sbStore sessionboard.Store

	// alertSettings, when non-nil, requests construction of an alert
	// Manager that observes the run event stream (via the file-event
	// tail) and fans stall / budget / failure alerts out to a webhook,
	// the studio browser (broker → WS toast), and optionally a desktop
	// Wails sink. Set via WithAlerts.
	alertSettings *AlertSettings
	alertManager  *alert.Manager

	// recoveryDispatch is built once on construction so each Launch /
	// Resume reuses the same dispatcher rather than allocating a new
	// recipes map + closure on the per-run hot path.
	recoveryDispatch runtime.RecoveryDispatch

	// extraObservers are runtime EventObservers chained alongside
	// the broker fan-out. Used to attach Prometheus / OTLP / custom
	// observers when constructing a server-side service.
	extraObservers []func(store.Event)

	// wireWFCache memoises WireWorkflow projections by (filePath, hash)
	// so /api/runs/{id}/workflow doesn't re-parse + re-compile on every
	// request. Invalidated implicitly when the .bot source changes
	// (hash mismatch). See workflow_export.go.
	wireWFCache wireWorkflowCache

	// runLogs holds a per-run log buffer for the lifetime of each
	// in-process run. Created in spawnRun, removed when the run
	// goroutine exits. The buffer captures the iterion logger output
	// scoped to that run and fans it out to live WS subscribers; see
	// runlog.go and the /api/runs/{id}/log endpoint.
	runLogsMu sync.RWMutex
	runLogs   map[string]*RunLogBuffer
	// runEngines holds the live in-process Engine for each active run so
	// the active-duration stamping callback (activeDurationForRun) can
	// read the run's monotonic SharedBudget elapsed. Registered in
	// spawnRun, removed when the run goroutine exits. Same lifecycle as
	// runLogs, so it shares runLogsMu.
	runEngines map[string]*runtime.Engine
	// runSteer holds the send side of each live run's override channel
	// (live steering: bump_loop / raise_budget). Registered/removed
	// together with runEngines under runLogsMu; the engine holds the
	// receive side and drains it at its safe boundary.
	runSteer map[string]chan *runtime.OverrideMsg

	// draining is set by Drain at the start of graceful shutdown.
	// Once true, Launch and Resume early-return runtime.ErrServerDraining
	// so the HTTP layer can map it to 503 Service Unavailable.
	draining atomic.Bool

	// reconcileStop ends the periodic orphan-reconcile goroutine (see
	// startPeriodicReconcile). Closed by Drain and Stop via
	// reconcileStopOnce so double-teardown is safe.
	reconcileStop     chan struct{}
	reconcileStopOnce sync.Once

	// publisher, when non-nil, intercepts Launch/Resume/Cancel and
	// routes them through the cloud queue. When nil the service runs
	// the engine in-process (local mode). See LaunchPublisher and
	// WithLaunchPublisher.
	publisher LaunchPublisher
	// resumeFiller re-derives a bare ResumeSpec's source/bundle from the
	// persisted run (see WithResumeSourceFiller). Nil = no-op.
	resumeFiller ResumeSourceFiller

	// localSecrets + localSealer are the local (non-cloud) sealed secret
	// store and its AES-GCM sealer. When set (local studio / desktop),
	// in-process launches resolve the workflow's declared secrets from the
	// store into ctx via BuildExecutor. Nil in cloud mode (credentials are
	// injected by the runner from a sealed per-run bundle). Set via
	// WithLocalSecrets.
	localSecrets secrets.GenericSecretStore
	localSealer  secrets.Sealer

	// forgeToken resolves the forge credential for a repo-targeted run's
	// server-side merge (clone + push). Installed by the server via
	// WithForgeTokenResolver; nil on local studios, where merges happen
	// in the user's own checkout.
	forgeToken ForgeTokenResolver

	// completionNotifier POSTs a run-completion webhook when an
	// in-process run carrying a callback URL reaches a terminal state.
	// Default-constructed in NewService; never nil for in-process runs.
	completionNotifier *notify.Notifier

	// eventPublisher, when set, receives a trigger.Event on every run
	// terminal state (run.finished / run.failed / run.cancelled) — the
	// "runned by iterion" run-completion source feeding the trigger spine.
	// nil disables emission (the default). Wired post-construction via
	// SetEventPublisher by the server so it shares the trigger
	// coordinator's event bus.
	eventPublisher trigger.Publisher

	// injectedStore captures the WithStore option so NewService can
	// honour a caller-supplied store. nil → fall back to the
	// filesystem auto-discovery path (local mode).
	injectedStore store.RunStore

	// streamSrc, when non-nil, replaces the in-process EventBroker /
	// RunLogBuffer machinery for live + historical delivery. Cloud mode
	// injects a runstream.MongoSource via WithStreamSource so the WS
	// handler streams from change streams instead of relying on the
	// local broker (which only sees this process's writes). ADR-053.
	streamSrc runstream.Source

	// fileSrcs tracks on-demand events.jsonl tailers started by
	// ensureEventSource for runs not produced in this process (e.g.
	// dispatcher-spawned in-process runs, whose runtime observer feeds
	// the dispatcher heartbeat — not this broker). Refcounted across WS
	// subscribers; the tailer stops when the last subscriber releases.
	fileSrcMu sync.Mutex
	fileSrcs  map[string]*fileSrcHandle
	// logSrcs is the run.log twin of fileSrcs: on-demand run.log tailers
	// started by ensureLogSource for active runs not produced in this process,
	// so the WS log stream is live instead of a one-shot replay. Guarded by
	// fileSrcMu (same low-contention lock). Refcounted; the tailer + buffer are
	// torn down when the last WS subscriber releases.
	logSrcs map[string]*fileSrcHandle

	// maxCostPerDayUSD configures the per-(store, UTC-day) LLM spend cap
	// enforced across every run this service launches. 0 disables it.
	// Set via the ITERION_MAX_COST_PER_DAY_USD env default.
	maxCostPerDayUSD float64
	// dailyCap is the shared spend-cap guard built from maxCostPerDayUSD
	// + the wired SpendStore. nil when the cap is disabled (no limit, or
	// a store that can't persist a ledger — e.g. cloud Mongo).
	dailyCap *runtime.DailyCapGuard

	// maxConcurrentPipelines caps how many ROOT pipelines run at once on
	// this machine (the cross-run limit `max_parallel_branches` never
	// provided). 0 disables the cap. Set via WithMaxConcurrentPipelines
	// (CLI flag) or the ITERION_MAX_CONCURRENT_PIPELINES env.
	maxConcurrentPipelines int
	// pipelineQueue is the local admission gate + FIFO built from
	// maxConcurrentPipelines. nil = unlimited (in-process launches start
	// eagerly, exactly as before). Only the in-process (non-publisher,
	// non-detached) local path honours it.
	pipelineQueue *pipelineQueue
	// pipelineStop ends the pipeline scheduler goroutine (see
	// startPipelineScheduler). Closed by Drain/Stop via pipelineStopOnce.
	pipelineStop     chan struct{}
	pipelineStopOnce sync.Once
	// pipelineReservedInvalidate drops the reservation provider's cache; see
	// SetPipelineReservedProvider. Nil unless a board is wired.
	pipelineReservedMu         sync.RWMutex
	pipelineReservedInvalidate func()
}

// ServiceOption configures a Service at construction time.
type ServiceOption func(*Service)

// WithWorkDir sets the working directory the engine should use for
// `${PROJECT_DIR}` expansion and as the seed for the worktree git-repo
// lookup. Without this, the engine falls back to os.Getwd() at Run()
// time, which in the desktop server case is whatever cwd the desktop
// process was launched from (typically the user's home dir, not the
// project root). Set this to the same directory the host server's
// WorkDir was configured with.
func WithWorkDir(dir string) ServiceOption {
	return func(s *Service) {
		s.workDir = dir
	}
}

// WithLogger sets the logger used for service-level diagnostics.
func WithLogger(l *iterlog.Logger) ServiceOption {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithSandboxDefault sets the global sandbox default injected into
// every in-process engine this service launches (the studio/dispatch
// counterpart of `iterion run`'s ITERION_SANDBOX_DEFAULT resolution —
// product daemons pass runtime.ResolveGlobalSandboxDefault()). Empty
// keeps the service neutral: workflows without a sandbox: block run
// unsandboxed, the pre-sandbox-by-default behaviour.
func WithSandboxDefault(mode string) ServiceOption {
	return func(s *Service) {
		s.sandboxDefault = mode
	}
}

// WithUsageCapSource wires a LIVE usage-cap policy source (the DB-backed
// runtime-settings resolver, pkg/usagecap.Resolver) into the service: the
// launch-time preflight and every in-process executor's guard then read
// the effective (db-or-env) policy per evaluation instead of the env
// value frozen at process start. Nil keeps env-only resolution.
func WithUsageCapSource(src usagecap.PolicySource) ServiceOption {
	return func(s *Service) { s.usageCapSource = src }
}

// AlertSettings configures the run-health alert Manager the service
// builds when WithAlerts is supplied. The webhook + desktop sinks are
// optional; browser delivery (broker → WS toast) is always wired so
// studio sessions get alerts regardless of these fields.
type AlertSettings struct {
	// WebhookURL targets a generic incoming webhook (Slack/Discord
	// shape). Empty disables webhook delivery. Treated as a secret.
	WebhookURL string
	// StallTimeout is the no-activity window for stall alerts. Zero or
	// negative disables stall detection; the caller resolves the default.
	StallTimeout time.Duration
	// BaseURL is the origin used to build /runs/<id> deep links.
	BaseURL string
	// DesktopSink, when non-nil, is added as an extra sink — the desktop
	// app injects a Wails EventsEmit sink here for in-window
	// notifications. Nil in headless server / browser-only mode.
	DesktopSink alert.Sink
}

// WithAlerts enables run-health alerting. The service constructs an
// alert.Manager, attaches a browser-delivery sink (publishing an
// in-process `alert` event to the broker), optional webhook + desktop
// sinks, wires it into the file-event tail, and starts its poll loop.
func WithAlerts(set AlertSettings) ServiceOption {
	return func(s *Service) {
		cp := set
		s.alertSettings = &cp
	}
}

// AlertManager returns the service's alert Manager, or nil when alerts
// are disabled. Exposed for tests + shutdown.
func (s *Service) AlertManager() *alert.Manager { return s.alertManager }

// WithLaunchPublisher wires the cloud-mode publisher; when nil the
// service stays in local-mode (in-process engine).
func WithLaunchPublisher(p LaunchPublisher) ServiceOption {
	return func(s *Service) { s.publisher = p }
}

// ResumeSourceFiller re-derives a resume's Source/BundleDir/BotBundle from
// the persisted run when the caller supplied none. Resume invokes it for
// every bare ResumeSpec, so surfaces that answer a run without carrying
// source (answer-human HTTP/WS, the ADR-081 async auto-resume) resolve the
// same stored-bot tiers as an explicit resume — never the pod's baked twin.
// The returned cleanup (may be nil) releases any materialized bundle dir
// once the resume has been handed off.
type ResumeSourceFiller func(ctx context.Context, run *store.Run, spec *ResumeSpec) (func(), error)

// WithResumeSourceFiller wires the server-side resume-source resolution
// into every Resume call that arrives without an explicit source.
func WithResumeSourceFiller(fn ResumeSourceFiller) ServiceOption {
	return func(s *Service) { s.resumeFiller = fn }
}

// WithForgeTokenResolver wires the forge-credential lookup used to merge
// repo-targeted runs server-side. See ForgeTokenResolver.
func WithForgeTokenResolver(fn ForgeTokenResolver) ServiceOption {
	return func(s *Service) { s.forgeToken = fn }
}

// WithMaxConcurrentPipelines caps how many ROOT pipelines run at once on
// this machine. Over-limit launches wait in a FIFO (surfaced on the
// pipeline board's TODO lane) and start when a slot frees. 0 (or
// negative) disables the cap — every launch starts eagerly, as before.
// The CLI wires it from --max-concurrent-pipelines; a non-positive value
// leaves the ITERION_MAX_CONCURRENT_PIPELINES env default to apply.
func WithMaxConcurrentPipelines(n int) ServiceOption {
	return func(s *Service) {
		if n > 0 {
			s.maxConcurrentPipelines = n
		}
	}
}

// WithLocalSecrets wires the local (non-cloud) sealed secret store + its
// AES-GCM sealer so in-process launches resolve the workflow's declared
// secrets into ctx (BuildExecutor). No-op in cloud mode. Set by the local
// studio/desktop server wiring from server.Config.GenericSecrets + Sealer.
func WithLocalSecrets(store secrets.GenericSecretStore, sealer secrets.Sealer) ServiceOption {
	return func(svc *Service) {
		svc.localSecrets = store
		svc.localSealer = sealer
	}
}

// WithStore replaces the default filesystem store with a caller-
// supplied implementation. When set, NewService skips the store
// auto-discovery and uses the supplied store directly. Used by
// cloud-mode entry points to inject the Mongo+S3 store. Plan §F
// (T-19, T-30).
func WithStore(s store.RunStore) ServiceOption {
	return func(svc *Service) { svc.injectedStore = s }
}

// WithBoardMCP wires the board MCP HTTP transport for sandboxed
// board-capability nodes (C082). handler must serve ONLY the board MCP
// routes (it is exposed gateway-reachable, token-gated) — never the full
// server mux. register mints a per-node run token against the server's
// BoardMCPTokenRegistry. Both are threaded into the engine + executor so
// sandboxed claude_code can write the operator's board.
func WithBoardMCP(handler http.Handler, register func(caps []string, sourceIssueID string) string) ServiceOption {
	return func(svc *Service) {
		svc.boardMCPHandler = handler
		svc.boardRegister = register
	}
}

// WithStreamSource installs an alternative streaming source (typically
// runstream.MongoSource in cloud mode) so the WS handler streams from
// change streams instead of the in-process EventBroker / RunLogBuffer
// machinery, which only sees this process's writes. See ADR-053.
func WithStreamSource(src runstream.Source) ServiceOption {
	return func(svc *Service) { svc.streamSrc = src }
}

// workspaceTrackingEnabled reports whether runs should version their
// workspace. ITERION_WORKSPACE_TRACK=off|0|false disables it globally.
//
// Default ON: without it a rewind cannot undo what a node actually
// produced for the majority of bots, which is most of the point. The
// escape hatch exists because the cost scales with the workspace, not
// with the run.
func workspaceTrackingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_WORKSPACE_TRACK"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// WorkspaceTrackerFor builds the workspace tracker for a store dir, or
// nil when versioning is disabled. Exported so the CLI's own engine
// assembly (`iterion run` / `iterion resume`, which do not go through
// Service) enables the same thing on the same terms — without it, a run
// launched from the terminal captures nothing and a later rewind reports
// "no snapshots" for a run created seconds earlier.
func WorkspaceTrackerFor(storeDir string) workspacetrack.Tracker {
	if storeDir == "" || !workspaceTrackingEnabled() {
		return nil
	}
	return workspacetrack.NewNative(storeDir)
}

// WithoutWorkspaceTracking disables workspace versioning for this
// service, whatever the environment says.
func WithoutWorkspaceTracking() ServiceOption {
	return func(s *Service) { s.workspaceTrackDisabled = true }
}

// NewService constructs a Service rooted at storeDir. When the
// caller wires WithStore, storeDir may be "" — the service uses the
// injected store directly without resolving a filesystem path.
func NewService(storeDir string, opts ...ServiceOption) (*Service, error) {
	logger := iterlog.NewFromEnv(os.Stderr)

	s := &Service{
		storeDir:         storeDir,
		logger:           logger,
		broker:           NewEventBroker(),
		manager:          NewManager(),
		recoveryDispatch: recovery.Dispatch(recovery.DefaultRecipes()),
		runLogs:          make(map[string]*RunLogBuffer),
		runEngines:       make(map[string]*runtime.Engine),
		runSteer:         make(map[string]chan *runtime.OverrideMsg),
	}
	for _, opt := range opts {
		opt(s)
	}

	switch {
	case s.injectedStore != nil:
		s.store = s.injectedStore
	case storeDir != "":
		st, err := store.New(storeDir, store.WithLogger(s.logger))
		if err != nil {
			return nil, fmt.Errorf("runview: open store: %w", err)
		}
		s.store = st
	default:
		// Fall back to the prior implicit ".iterion" behaviour so
		// pre-existing local callers keep working.
		st, err := store.New(".iterion", store.WithLogger(s.logger))
		if err != nil {
			return nil, fmt.Errorf("runview: open store: %w", err)
		}
		s.store = st
		s.storeDir = ".iterion"
	}

	// Wire log-position stamping when the store is a local
	// FilesystemRunStore. The closure reads the current byte total
	// from the per-run RunLogBuffer (created lazily by
	// prepareRunLog); a missing entry returns 0, which the studio
	// interprets as "no offset info — show live tail". Cloud
	// (Mongo) stores skip this wiring — they have no on-host log
	// buffer to attach.
	if fs, ok := s.store.(*store.FilesystemRunStore); ok {
		fs.SetLogPositionFn(s.logPositionForRun)
		// Same seam: stamp Event.ActiveMs from the run's monotonic
		// SharedBudget so the studio active timer excludes OS-suspend.
		fs.SetActiveDurationFn(s.activeDurationForRun)
	}

	// Workspace versioning needs the same on-disk store dir. It is what
	// lets a rewind restore files for a run with NO isolated worktree —
	// the default shape, where git cannot serve because the workspace is
	// the operator's live checkout.
	// Off switch: this walks and hashes the workspace at every node
	// boundary of every non-worktree run, so an operator whose workspace is
	// large (or who simply does not use rewind) needs a way out that is not
	// "declare worktree: auto".
	if s.storeDir != "" && s.workspaceTracker == nil && !s.workspaceTrackDisabled && workspaceTrackingEnabled() {
		s.workspaceTracker = workspacetrack.NewNative(s.storeDir)
	}

	// Session-board curation needs an on-disk dir to persist specs
	// (runs/<id>/sessionboard.json). Skipped in cloud mode (no storeDir).
	if s.storeDir != "" {
		if sb, err := sessionboard.NewFileStore(s.storeDir); err == nil {
			s.sbStore = sb
		} else {
			s.logger.Warn("runview: session board store unavailable: %v", err)
		}
	}

	// Build the daily spend-cap guard from the ITERION_MAX_COST_PER_DAY_USD
	// env default. NewDailyCapGuard returns nil (cap disabled) when the
	// limit is non-positive or the store can't persist a ledger (cloud
	// Mongo stores).
	if s.maxCostPerDayUSD <= 0 {
		s.maxCostPerDayUSD = envDailyCostCap()
	}
	s.dailyCap = runtime.NewDailyCapGuard(
		store.AsSpendStore(s.store),
		clock.Default,
		runtime.DailyCapConfig{MaxCostPerDayUSD: s.maxCostPerDayUSD},
	)

	// Build the local pipeline-concurrency gate from the
	// ITERION_MAX_CONCURRENT_PIPELINES env default when no explicit cap was
	// wired. nil (unlimited) when the resolved value is non-positive. Only
	// in-process (local) mode has a gate: the cloud publisher path bypasses
	// Launch's in-process branch entirely, so a queue there would be inert.
	if s.maxConcurrentPipelines <= 0 {
		s.maxConcurrentPipelines = envMaxConcurrentPipelines()
	}
	if s.publisher == nil {
		s.pipelineQueue = newPipelineQueue(s.maxConcurrentPipelines)
	}

	if s.alertSettings != nil {
		s.alertManager = s.buildAlertManager(*s.alertSettings)
		s.alertManager.Start(context.Background())
	}

	if s.completionNotifier == nil {
		// Run-completion webhooks are off-by-default in behaviour (no-op
		// unless a launched run carries a callback URL) but the notifier
		// itself is always present so spawnRun can fire unconditionally.
		// ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE=1 relaxes the SSRF
		// guard for self-hosted deployments whose callback receiver lives
		// on a private network alongside iterion.
		allowPrivate := os.Getenv("ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE") == "1"
		// ITERION_COMPLETION_WEBHOOK_SECRET, when set, HMAC-signs every
		// outbound payload (X-Iterion-Signature) so receivers can
		// authenticate the delivery. Empty = unsigned (receiver must not
		// require a signature).
		secret := os.Getenv("ITERION_COMPLETION_WEBHOOK_SECRET")
		s.completionNotifier = notify.New(s.logger, 0,
			notify.WithAllowPrivate(allowPrivate),
			notify.WithSigningSecret(secret))
	}

	s.reconcileOrphans()
	s.reconcileSandboxContainers()
	s.reconcileSandboxK8sResources()
	s.startPeriodicReconcile()
	// Recover any pipelines left waiting in the queue by a previous
	// process lifetime (persisted as queued docs), then start the
	// scheduler that admits them as slots free. No-op when the cap is
	// disabled (pipelineQueue == nil).
	s.rebuildPipelineQueue()
	s.startPipelineScheduler()
	return s, nil
}

// buildAlertManager wires the alert Manager's sinks (webhook, optional
// desktop, and the always-on browser-broker sink), the run-name lookup,
// and the deep-link base URL. The manager itself is fed events by the
// file-event tail (see runstream.TailEventsFile).
func (s *Service) buildAlertManager(set AlertSettings) *alert.Manager {
	var sinks []alert.Sink
	if wh := alert.NewWebhookSink(set.WebhookURL, s.logger); wh != nil {
		sinks = append(sinks, wh)
	}
	if set.DesktopSink != nil {
		sinks = append(sinks, set.DesktopSink)
	}
	// Error tracking, when an operator configured a DSN: a failed or
	// stalled run reaches the same incident stream as a panic. nil (and
	// therefore absent) whenever tracking is off.
	if tk := alert.NewTrackerSink(); tk != nil {
		sinks = append(sinks, tk)
	}
	// Browser delivery: publish an in-process `alert` event to the
	// broker. It is NOT persisted to events.jsonl, so the file tail
	// never re-feeds it into Observe (no detection feedback loop).
	sinks = append(sinks, alert.FuncSink(func(_ context.Context, a alert.Alert) {
		if s.broker == nil {
			return
		}
		s.broker.Publish(store.Event{
			Type:      store.EventAlert,
			RunID:     a.RunID,
			NodeID:    a.NodeID,
			Timestamp: a.Timestamp,
			Data:      a.AsEventData(),
		})
	}))

	runLookup := func(id string) (string, bool) {
		if s.store == nil {
			return "", false
		}
		r, err := s.store.LoadRun(context.Background(), id)
		if err != nil || r == nil {
			return "", false
		}
		if r.Name != "" {
			return r.Name, true
		}
		return r.WorkflowName, true
	}

	opts := []alert.Option{
		alert.WithSinks(sinks...),
		alert.WithRunLookup(runLookup),
		alert.WithBaseURL(set.BaseURL),
		alert.WithStallTimeout(set.StallTimeout),
		alert.WithLogger(s.logger),
	}
	// Persisted twin: append every alert as a durable run_health event
	// so the stall episode survives a WS reconnect and shows in the
	// timeline. Loop-free by construction — Observe ignores
	// EventRunHealth — and best-effort: an append failure is logged,
	// never blocks alerting.
	if s.store != nil {
		opts = append(opts, alert.WithStoreSink(func(a alert.Alert) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.store.AppendEvent(ctx, a.RunID, store.Event{
				Type:      store.EventRunHealth,
				RunID:     a.RunID,
				NodeID:    a.NodeID,
				Timestamp: a.Timestamp,
				Data:      a.AsEventData(),
			}); err != nil && s.logger != nil {
				s.logger.Warn("alert: persist run_health for %s: %v", a.RunID, err)
			}
		}))
	}
	return alert.NewManager(opts...)
}

// Broker exposes the event broker for transports that need to
// subscribe directly (the WS handler).
func (s *Service) Broker() *EventBroker { return s.broker }

// DailyCap returns the service's shared spend-cap guard, or nil when the
// daily cap is disabled. The HTTP layer uses it to read status and apply
// per-day overrides.
func (s *Service) DailyCap() *runtime.DailyCapGuard { return s.dailyCap }

// SetEventPublisher wires the run-completion event source: every terminal run
// emits a trigger.Event onto p. Called once by the server after the trigger
// coordinator is up so runs publish onto the same bus the evaluator consumes.
// Passing nil disables emission. Safe to call before Serve; not safe to call
// concurrently with active runs (set it once at wiring time).
func (s *Service) SetEventPublisher(p trigger.Publisher) { s.eventPublisher = p }

// envDailyCostCap parses ITERION_MAX_COST_PER_DAY_USD as a float dollar
// amount, returning 0 (disabled) when unset or unparseable.
func envDailyCostCap() float64 {
	v := os.Getenv("ITERION_MAX_COST_PER_DAY_USD")
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// envMaxConcurrentPipelines parses ITERION_MAX_CONCURRENT_PIPELINES as a
// positive integer cap on concurrent root pipelines, returning 0
// (disabled) when unset or unparseable.
func envMaxConcurrentPipelines() int {
	v := os.Getenv("ITERION_MAX_CONCURRENT_PIPELINES")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// PipelineConcurrency returns the current state of the local pipeline
// concurrency gate (limit, active, waiting) for server_info. A disabled
// cap reports Enabled=false.
func (s *Service) PipelineConcurrency() PipelineConcurrencyStatus {
	if s == nil {
		return PipelineConcurrencyStatus{}
	}
	return s.pipelineQueue.status()
}

// SetPipelineReservedProvider wires the board-derived source of RESERVED
// concurrency slots — tickets whose pipeline died mid-flight and now sit in
// the board's needs-attention lane. Those runs consume nothing (no
// goroutine, no budget) but their slot is held so the operator's fix
// restarts into it instead of queueing behind whatever grabbed it.
//
// runview owns the cap but knows nothing about boards, so the predicate is
// injected by the server. Nil (the default, and every non-studio embedding)
// means no reservations and therefore no behaviour change at all.
//
// invalidate, when non-nil, drops whatever cache the provider keeps. The
// run teardown calls it just before releasing a slot: the release wakes the
// FIFO drain immediately, and a drain reading a set computed before the
// failure would hand out the slot that failure just reserved.
//
// Safe to call after construction and more than once — a studio project
// switch rebuilds the Service and re-points it at the new board.
func (s *Service) SetPipelineReservedProvider(reserved func() map[string]struct{}, invalidate func()) {
	if s == nil {
		return
	}
	s.pipelineReservedMu.Lock()
	s.pipelineReservedInvalidate = invalidate
	s.pipelineReservedMu.Unlock()
	s.pipelineQueue.setReservedProvider(reserved)
}

// invalidatePipelineReserved drops the reservation provider's cache, if it
// keeps one. Nil-safe in every direction so the launch teardown can call it
// unconditionally.
func (s *Service) invalidatePipelineReserved() {
	if s == nil {
		return
	}
	s.pipelineReservedMu.RLock()
	fn := s.pipelineReservedInvalidate
	s.pipelineReservedMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// inboxBinder returns the runtime's operator-chatbox plumbing
// scoped to this service's store + broker. Built once per Build-
// Executor call so the binder closures see a consistent store
// handle even when a service is hot-swapped (project switch).
func (s *Service) inboxBinder() model.InboxBinder {
	if s == nil || s.store == nil {
		return nil
	}
	binder := &model.StoreInboxBinder{Store: s.store}
	if s.broker != nil {
		binder.Publish = s.broker.Publish
	}
	return binder
}

// asyncAskBinder returns the async-question plumbing (ADR-081) scoped
// to this service's store + broker, the sibling of inboxBinder.
func (s *Service) asyncAskBinder() model.AsyncAskBinder {
	if s == nil || s.store == nil {
		return nil
	}
	binder := &model.StoreAsyncAskBinder{Store: s.store}
	if s.broker != nil {
		binder.Publish = s.broker.Publish
	}
	return binder
}

// StoreDir returns the on-disk store directory. Exposed so HTTP
// handlers can fall back to persisted run.log when the in-memory
// buffer is gone.
func (s *Service) StoreDir() string { return s.storeDir }

// WorkspaceTracker returns the service's workspace versioning tracker,
// or nil when versioning is disabled. Used by the review-scope panel to
// build gate-to-gate ranges for in-place runs.
func (s *Service) WorkspaceTracker() workspacetrack.Tracker {
	if s == nil {
		return nil
	}
	return s.workspaceTracker
}

// StoreRoot returns the filesystem root the underlying RunStore
// operates on, or empty when the store has no filesystem (cloud
// stores). Used by the upload handlers to materialise a staging
// directory.
func (s *Service) StoreRoot() string {
	if s == nil || s.store == nil {
		return ""
	}
	return s.store.Root()
}

// RunStore exposes the underlying store handle for callers that need
// to drive read-only iteration patterns (ScanEvents over many runs,
// for instance the /runs/stats aggregator). Mutators are intentionally
// gated behind Service methods so the broker / manager bookkeeping
// stays coherent — call those instead of reaching into the store for
// writes.
func (s *Service) RunStore() store.RunStore {
	if s == nil {
		return nil
	}
	return s.store
}

// validatePathComponent delegates to store.SanitizePathComponent so
// the validation rules stay in lock-step with the storage layer.
func validatePathComponent(name, component string) error {
	return store.SanitizePathComponent(name, component)
}
